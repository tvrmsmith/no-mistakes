package steps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testguidance"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestStep runs baseline tests, gathers evidence for user intent, and optionally asks the agent to fix failures.
type TestStep struct{}

func (s *TestStep) Name() types.StepName { return types.StepTest }

func (s *TestStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return runValidationStep(sctx, s.Name(), s.execute)
}

// vacuousGreenFindingID marks the finding parkVacuousGreen raises so the fix
// round can tell "the units proved nothing" from "a test failed" and ask for
// the right repair. Finding.ID survives into the next round's
// PreviousFindings, while the description is agent-facing prose that will be
// reworded.
const vacuousGreenFindingID = "vacuous-green"

const (
	testFixTask = `Fix the failing tests in this repository. Reproduce the specific failure, identify the root cause, and fix either the tests or the code so that failure passes.`

	testFixReproduceRule = `- Reproduce the specific failing case first (the exact test, package, script, or check named in the findings), then re-run only that focused verification after the fix.`

	vacuousGreenFixTask = `Write the missing test for this change. The test commands already exited zero and proved nothing: either no test executed at all, or no executed test reached the code this change touched. There is no failing case to reproduce, so add coverage rather than looking for a broken test.`

	vacuousGreenFixWriteRule = `- Write a new test (or extend an existing one) that executes the changed code and would fail if that code were wrong, then run only that new test to confirm it passes.`
)

// previousFindingsIncludeVacuousGreen reports whether the round that parked
// this run raised the vacuous-green finding. Findings that cannot be parsed
// answer false, which keeps the reproduce-the-failure prompt as the default.
func previousFindingsIncludeVacuousGreen(raw string) bool {
	if raw == "" {
		return false
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return false
	}
	for _, item := range findings.Items {
		if item.ID == vacuousGreenFindingID {
			return true
		}
	}
	return false
}

func (s *TestStep) execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	// In fix mode, ask agent to fix test failures first.
	//
	// Targeted-validation rules (reproduce the specific failure, focused
	// re-verification only, never a complete repository suite) are a product
	// contract: local Test proves the requested intent, while remote CI owns
	// broad regression and remains mandatory before a PR is ready. A forensic
	// audit measured ~82 minutes of local complete-suite walks on one repair
	// path when prompts only said "run the tests" / "relevant". This is a
	// prompt contract, not an enforced sandbox - the agent has free shell
	// access - so the pinned regression tests guard the wording, not the
	// runtime. Process-group reaping on clean exit (#357) remains the lifecycle
	// safety net when agents do spawn test workers; it is not a reason to force
	// a deterministic full-suite commands.test override.
	var newTestsFromFix []string
	var fixSummary string
	if sctx.Fixing {
		historySection := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx) + testguidance.Rule
		// The vacuous-green park and a genuine test failure need opposite
		// instructions. There is no failing case to reproduce when the units
		// exited zero and proved nothing, so "reproduce the specific failure"
		// sends the agent looking for something that does not exist, and the
		// repair it then reports is a re-run rather than the missing test.
		task, reproduceRule := testFixTask, testFixReproduceRule
		if previousFindingsIncludeVacuousGreen(sctx.PreviousFindings) {
			task, reproduceRule = vacuousGreenFixTask, vacuousGreenFixWriteRule
		}
		fixPrompt := fmt.Sprintf(
			`%s

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Make the smallest correct root-cause fix.
- Do not refactor beyond what is needed for that root-cause fix.
- If tests fail, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue.
- Do NOT run linters, formatters, or static analysis tools.
%s
- Do NOT run the complete repository test suite. Local Test is targeted validation of the failure and the requested intent; remote CI owns broad regression and remains mandatory before a PR is ready.
- A generic driver or user instruction asking for broad or full-suite confirmation does NOT override this product boundary. Keep verification focused on the failure and intent.
- Never treat "do not run everything" as permission to run nothing: if you cannot reproduce or re-verify with a targeted check, report that honestly in the summary rather than inventing a full-suite pass.
- Before finishing, remove any transient artifacts your testing created in the working tree (downloaded models, caches, build outputs, large binaries, or generated data directories) so they are not committed and pushed. Do not remove intentional source or test-file changes.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
			task,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			reproduceRule,
			historySection,
		)
		if sctx.PreviousFindings != "" {
			fixPrompt += `

Previous test findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
		}
		fixCtx, cancelFix, fixTimeout := testAgentContext(sctx)
		summary, err := executeFixMode(sctx, s.Name(), fixExecutionOptions{
			LogMessage:      "asking agent to fix test failures...",
			Prompt:          fixPrompt,
			ErrorPrefix:     "agent fix tests",
			FallbackSummary: "fix test failures",
			AgentContext:    fixCtx,
			AfterAgentRun: func(*agent.Result) error {
				newTestsFromFix = detectNewTestFiles(ctx, sctx.WorkDir)
				return nil
			},
		})
		cancelFix()
		if err != nil {
			return nil, testAgentError(fixCtx, fixTimeout, "agent fix tests", err)
		}
		fixSummary = summary
	}

	changed, err := changedPathsSince(ctx, sctx.WorkDir, baseSHA, sctx.Run.HeadSHA, sctx.Fixing)
	if err != nil {
		return nil, err
	}

	var covered []config.TestUnit
	ran := map[string]bool{}

	// The changed-file list a unit command reads can lose paths: the whole list
	// when it exceeds the byte cap, and individual paths a newline or carriage
	// return makes unreadable. Either way a command that takes its targets from
	// the variable validates less than the change, so the omission is a warning
	// finding on every outcome rather than a log line alone. The count variable
	// still carries the true total, which is the machine-readable signal a
	// command can compare against.
	changedFilesEnv, omittedChangedFiles := changedFilesEnvValue(changed)
	var omissionFindings []Finding
	if omittedChangedFiles > 0 {
		omission := fmt.Sprintf("%s omits %d of %d changed paths, so a command that reads it validates less than the change; %s carries the true total", envTestChangedFiles, omittedChangedFiles, len(changed), envTestChangedFileCount)
		sctx.Log(omission)
		omissionFindings = []Finding{{
			Severity:    types.FindingSeverityWarning,
			Action:      types.ActionAskUser,
			Description: omission,
		}}
	}

	// withOmission puts the changed-file omission in front of whatever a path
	// found, so every outcome the step can return carries it.
	withOmission := func(items []Finding) []Finding {
		if len(omissionFindings) == 0 {
			return items
		}
		return append(append([]Finding{}, omissionFindings...), items...)
	}

	// tested renders the durable record a reviewer reads on the outcome and in
	// the PR body. It names the unit beside its command, because the command
	// alone does not say which unit it covered and two units may share one.
	tested := func() []string {
		entries := make([]string, 0, len(covered))
		for _, unit := range covered {
			entries = append(entries, unit.Name+": "+unit.Command)
		}
		return entries
	}

	// parkForMaintainer stops the step at a gate with one error finding rather
	// than failing the run, the posture every unusable discovery answer takes:
	// a maintainer fixes the configuration or the inferred layout and resumes.
	// It carries whatever already ran this attempt, so a park that follows a
	// green unit still names what it covered.
	parkForMaintainer := func(description string) (*pipeline.StepOutcome, error) {
		sctx.Log(description)
		findings := Findings{
			Items: withOmission([]Finding{{
				Severity:    types.FindingSeverityError,
				Action:      types.ActionAskUser,
				Description: description,
			}}),
			Tested: tested(),
		}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   false,
			Findings:      string(findingsJSON),
			FixSummary:    fixSummary,
		}, nil
	}

	// parkVacuousGreen stops the step at an auto-fixable gate when the units
	// ran, exited zero, and proved nothing: no test executed, or no test
	// touched the change. Unlike the maintainer parks above, an agent fix round
	// is the right answer here, because the repair is a test the change is
	// missing rather than a setting only a maintainer can change.
	parkVacuousGreen := func(description string) (*pipeline.StepOutcome, error) {
		sctx.Log(description)
		findings := Findings{
			Items: withOmission([]Finding{{
				ID:          vacuousGreenFindingID,
				Severity:    types.FindingSeverityError,
				Action:      types.ActionAutoFix,
				Description: description,
			}}),
			Tested: tested(),
		}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   true,
			Findings:      string(findingsJSON),
			FixSummary:    fixSummary,
		}, nil
	}

	discovery, err := discoverTestUnits(sctx, baseSHA, changed)
	if err != nil {
		// A discovery failure parks rather than returning a Go error: a
		// returned error would fail the run outright, while the acceptance
		// criterion here is that an unreadable or invalid layout parks for a
		// maintainer to fix the configuration or the inferred command.
		//
		// The exception is a failure to reach the discovery agent at all
		// (budget expiry, transport error). Parking there would hold the run
		// at a gate on an agent that never answered, so it fails the run like
		// every other agent invocation in this step.
		var resultErr discoveryResultError
		if !errors.As(err, &resultErr) {
			return nil, err
		}
		return parkForMaintainer(fmt.Sprintf("test unit discovery failed: %v", err))
	}

	multiUnit := len(discovery.Units) > 1

	// Resolve every selected name against the layout before running anything.
	// A name with no unit behind it has no command to run, and executing an
	// empty command would exit 0 and report the unit as tested, so an
	// unresolvable selection parks instead.
	selectedUnits := make([]config.TestUnit, 0, len(discovery.Selected))
	for _, name := range discovery.Selected {
		unit, ok := findTestUnit(discovery.Units, name)
		if !ok {
			return parkForMaintainer(fmt.Sprintf("test unit discovery selected %q, which is not in the discovered unit layout", name))
		}
		selectedUnits = append(selectedUnits, unit)
	}

	if len(discovery.Selected) == 0 {
		sctx.Log(fmt.Sprintf("no test units selected for the changed files (%s)", discovery.Source))
	} else {
		sctx.Log(fmt.Sprintf("selected test units (%s): %s", discovery.Source, strings.Join(discovery.Selected, ", ")))
		for _, unit := range selectedUnits {
			sctx.Log(fmt.Sprintf("unit %s: %s", unit.Name, unit.Command))
		}
	}

	// runUnit runs one unit's command exactly once per attempt, tracked by
	// name in ran so the under-selection expansion below can never re-run a
	// unit the first pass already covered. It returns a non-nil outcome only
	// on a failing exit code; a nil outcome with a nil error means the caller
	// should keep going.
	runUnit := func(unit config.TestUnit) (*pipeline.StepOutcome, error) {
		if ran[unit.Name] {
			return nil, nil
		}
		ran[unit.Name] = true
		unitDir, dirErr := testUnitCoverageDir(sctx, unit.Name)
		if dirErr != nil {
			return nil, dirErr
		}
		// A profile an earlier attempt of this same run left behind would
		// certify an attempt that wrote nothing, which is exactly the vacuous
		// green the guard exists to catch, so the directory starts empty every
		// time rather than being merged into.
		if err := os.RemoveAll(unitDir); err != nil {
			return nil, fmt.Errorf("clear test coverage dir: %w", err)
		}
		if err := os.MkdirAll(unitDir, 0o755); err != nil {
			return nil, fmt.Errorf("create test coverage dir: %w", err)
		}
		env := []string{
			envTestBaseSHA + "=" + baseSHA,
			envTestChangedFiles + "=" + changedFilesEnv,
			envTestChangedFileCount + "=" + strconv.Itoa(len(changed)),
			envTestCoverageDir + "=" + unitDir,
		}
		output, exitCode, runErr := runStepShellCommandEnv(sctx, unit.Command, env)
		if runErr != nil {
			return nil, fmt.Errorf("run test command: %w", runErr)
		}
		covered = append(covered, unit)
		if exitCode == 0 {
			return nil, nil
		}
		description := fmt.Sprintf("tests failed with exit code %d", exitCode)
		if multiUnit {
			description = fmt.Sprintf("unit %s: tests failed with exit code %d", unit.Name, exitCode)
		}
		projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepTest)
		findings := Findings{
			Items: withOmission([]Finding{{
				Severity:    types.FindingSeverityError,
				Description: description,
			}}),
			Summary: projectedOutput,
			Tested:  tested(),
		}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   true,
			Findings:      string(findingsJSON),
			ExitCode:      exitCode,
			FixSummary:    fixSummary,
		}, nil
	}

	for _, unit := range selectedUnits {
		if outcome, runErr := runUnit(unit); outcome != nil || runErr != nil {
			return outcome, runErr
		}
	}

	if missing := underSelectedUnits(discovery.Units, changed, discovery.Selected); len(missing) > 0 {
		missingNames := make([]string, len(missing))
		for i, u := range missing {
			missingNames[i] = u.Name
		}
		count := sctx.Shared.NoteTestScopeFault()
		if count >= 2 {
			// A second scope fault in the same run means discovery itself is
			// unreliable, not merely incomplete this once: expanding again
			// would keep papering over a systematic miss, so this parks for
			// a maintainer instead of running the missing units.
			return parkForMaintainer(fmt.Sprintf("test unit discovery under-selected twice in this run; changed files belong to units it did not select: %s", strings.Join(missingNames, ", ")))
		}

		sctx.Log(fmt.Sprintf("test scope fault: original selection %s", strings.Join(discovery.Selected, ", ")))
		sctx.Log(fmt.Sprintf("expanding selection with %s", strings.Join(missingNames, ", ")))

		expanded := discovery
		expanded.Selected = append(append([]string{}, discovery.Selected...), missingNames...)
		// Only the agent source reads the cache back; discoverTestUnits derives
		// the config and command selections fresh every time, so re-caching
		// them would write a record nothing consults.
		if discovery.Source == "agent" {
			sctx.Shared.SetTestDiscovery(changedFilesFingerprint(changed), expanded)
		}
		discovery = expanded

		for _, unit := range missing {
			if outcome, runErr := runUnit(unit); outcome != nil || runErr != nil {
				return outcome, runErr
			}
		}
	}

	// The vacuous-green guard. It sits after the selected-unit loop and after
	// the under-selection expansion, so it judges every command this attempt
	// ran, and before the evidence pass, because there is no point spending an
	// agent turn gathering intent evidence for a run that already failed this
	// gate.
	//
	// It is skipped entirely when no unit command ran, which is the
	// agent-evidence path: no command there could have exercised anything and
	// no artifact exists to read a verdict out of.
	if len(covered) > 0 {
		if outcome, guardErr := guardVacuousGreen(sctx, covered, changed, baseSHA, parkForMaintainer, parkVacuousGreen); outcome != nil || guardErr != nil {
			return outcome, guardErr
		}
	}

	// An agent-inferred layout does not replace evidence gathering. A
	// repository that configured neither test.units nor commands.test got a
	// full evidence pass before this step split, and discovery answering "here
	// is a command" says nothing about whether the change's intent is visibly
	// satisfied. So the pass runs on three disjuncts: discovery selected no
	// unit, discovery inferred the layout itself, or the run carries user
	// intent. A configured layout with a non-empty selection still gets the
	// pass only for intent.
	//
	// Whenever the selected units' commands already ran, the prompt tells the
	// agent to read and judge those results instead of running tests again and
	// not to widen past the selection. The step itself runs each unit's command
	// exactly once per attempt; the evidence half of that bound is a prompt
	// contract, not an enforced sandbox, and the pinned regression tests guard
	// the wording.
	useEvidenceAgent := len(discovery.Selected) == 0 || discovery.Source == "agent" || cleanedUserIntent(sctx) != ""
	if useEvidenceAgent {
		evidenceDir := testEvidenceDir(sctx)
		if evidenceDir == "" {
			return nil, fmt.Errorf("test evidence dir is not configured for this run")
		}
		if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
			return nil, fmt.Errorf("create test evidence dir: %w", err)
		}
		switch {
		case len(discovery.Selected) == 0:
			sctx.Log("no test units selected, asking agent to run tests...")
		case discovery.Source == "agent":
			sctx.Log("test units were inferred, asking agent to gather test evidence...")
		default:
			sctx.Log("user intent available, asking agent to gather test evidence...")
		}
		reassessHistory := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx) + testguidance.Rule
		evidenceGuidance := fmt.Sprintf("- Write new evidence files into this evidence directory, never into the worktree: %s", evidenceDir)
		if sctx.Config.Test.Evidence.StoreInRepo {
			evidenceGuidance = fmt.Sprintf("- Write new evidence files into this evidence directory, never into the worktree; they are published to the repository's %s branch automatically and linked from the PR: %s", sctx.Config.Test.Evidence.Branch, evidenceDir)
		}
		// What the agent is told to DO swaps once the selected units' commands
		// have already run this attempt: telling it to "run the smallest
		// relevant tests yourself" there would run those same suites a second
		// time, and nothing in the evidence prompt bounds it to the selection.
		//
		// The opening sentence stays the same in both modes and the direction
		// moves into the section below the Context block, so a reader (and a
		// scenario matching on the prompt) can identify the test step's
		// evidence pass without knowing which mode it took.
		const evidenceOpening = "You are validating a code change by testing it."
		existingTestsRule := "- Look for existing tests that would generate sufficient evidence. If they exist, run the smallest relevant set that proves the requested intent."
		baselineSection := "\nExamine the repository and run the smallest relevant tests yourself.\n"
		if len(covered) > 0 {
			quoted := make([]string, len(covered))
			for i, unit := range covered {
				quoted[i] = unit.Name + " (`" + unit.Command + "`)"
			}
			baselineSection = fmt.Sprintf("\nThe selected test units already ran to completion and passed in this attempt: %s\nRead and judge those results rather than running them again.\n", strings.Join(quoted, ", "))
			existingTestsRule = "- The selected units' test commands above already ran and passed. Treat their results as the baseline, do NOT run them again, and do NOT run tests for any unit outside that selection. Spend this pass on evidence those commands do not produce."
		}
		evidenceCtx, cancelEvidence, evidenceTimeout := testAgentContext(sctx)
		evidencePrompt := fmt.Sprintf(
			`%s

Context:
- branch: %s
- base commit: %s
- target commit: %s
%s

Task:
- Understand the user intent before testing it. If extracted user intent is present, use it as the primary hint for what success means.
- Decide what evidence or artifacts would clearly demonstrate the user intent is satisfied. Unit tests passing is not sufficient evidence by itself.
- Demonstrate the user intent working end-to-end in a way consistent with how an end user would actually experience it.
- Prefer product-level artifacts: screenshots, GIFs, videos, rendered UI, CLI transcripts, API responses, persisted database state, generated PR markdown, logs, or other outputs that directly show the intended behavior working.
- For UI, HTML, CSS, Electron renderer, browser, visual layout, or copy-placement changes, attempt to capture reviewer-visible visual evidence.
- Prefer screenshots, images, videos, GIFs, or rendered HTML artifacts that show the actual end-user surface.
- DOM snapshots, selector assertions, and text-only render summaries are not substitutes for visual evidence when a rendered surface is available.
- If a UI-facing change has no screenshot, image, video, GIF, or rendered HTML artifact, state why in testing_summary.
%s
- Do not move, commit, or modify source files only to make evidence linkable. Record local evidence file paths exactly where you created them.
- Only use command output as an artifact when that output directly demonstrates the end-user experience or requested behavior. Generic pass/fail, coverage, or clean-worktree output is not sufficient evidence.
%s
- Do NOT run the complete repository test suite. Local Test is targeted validation of the requested intent; remote CI owns broad regression and remains mandatory before a PR is ready.
- Never treat "do not run everything" as permission to run nothing: if no targeted automated test can establish the intent, write or improve a focused test, perform manual verification with evidence, or report a warning finding that sufficient targeted evidence is not possible.
- If no existing test produces sufficient evidence, write or improve a focused test so that it does.
- If automated testing cannot produce the needed evidence, execute manual verification steps and record the evidence-producing steps you performed.
- If sufficient evidence is not possible, report a warning finding explaining what evidence is missing and why the user needs to decide what to do. When the blocker is a host capability or OS permission the agent's own process lacks (for example, the Screen Recording permission macOS requires to capture a native GUI application), name the specific capability or permission and how to grant it so the user can enable it and re-run, instead of retrying blindly or failing opaquely.
- Include a concise "testing_summary" sentence describing what you exercised and the overall result.
- The "testing_summary" must account for the complete test step: baseline commands that already ran, automated tests, manual or evidence-producing checks, artifacts gathered, and the overall result.
- Record the exact tests, manual checks, and evidence-producing steps you ran in a "tested" array. Prefer concrete commands or test selectors wrapped in backticks.
- Always include an "artifacts" array. Leave it empty when you produced no reviewer-visible evidence artifacts. Use artifact path for file artifacts, artifact url for externally visible artifacts, and artifact content for short logs or command output that should be shown directly in the PR.
- If tests fail, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue.
- If the issue is setup-related and fixable, fix it and retry the focused tests.

Rules:
- Do NOT run linters, formatters, or static analysis tools.
- Focus on testing and test-related fixes only.
- A generic driver or user instruction asking for broad or full-suite confirmation does NOT override the targeted-validation product boundary.
- Before finishing, remove any transient artifacts your testing created in the working tree (downloaded models, caches, build outputs, large binaries, or generated data directories) so they are not committed and pushed. Do not remove intentional source or test-file changes, and leave evidence files in the dedicated evidence directory untouched.
- Keep "testing_summary" high-signal and natural language. Avoid raw logs and noisy counts.
- Always return a non-empty "tested" array describing what you exercised, even when all tests pass.
- Only report actionable findings: test failures, unfixable setup issues, flaky tests you identified, or missing evidence that prevents you from demonstrating the user intent.
- Do NOT report passing tests (whether existing or new), test counts, coverage summaries, or other non-actionable information.
- If all tests pass and there are no issues, return an empty findings array.
- Set action to "ask-user" for missing-evidence warning findings and only otherwise when a test failure seems desired and you question the author's intent of having the test in the first place. Set action to "auto-fix" for objective test failures that can be safely fixed. Set action to "no-op" for informational notes.%s`,
			evidenceOpening,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			baselineSection,
			evidenceGuidance,
			existingTestsRule,
			reassessHistory,
		)
		// A restart carries this step's own last verdict back into the
		// re-entry, and that re-entry is not a fix round, so the evidence pass
		// reads it here rather than re-deriving what it already reported. A fix
		// round already pasted them into the fix prompt one call earlier in this
		// same Execute, so it skips them here.
		//
		// For whoever restructures this file: the evidencePrompt hoist and this
		// block are the restart work's only additions here beyond the
		// runValidationStep call site. Without them the restart plumbing writes
		// PreviousFindings that the test step never reads.
		if !sctx.Fixing && sctx.PreviousFindings != "" {
			evidencePrompt += `

Previous test findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
		}
		result, err := sctx.RunAgentContext(evidenceCtx, agent.RunOpts{
			Prompt:     evidencePrompt,
			CWD:        sctx.WorkDir,
			JSONSchema: testFindingsSchema,
			OnChunk:    sctx.LogChunk,
		})
		runErr := testAgentError(evidenceCtx, evidenceTimeout, "agent run tests", err)
		cancelEvidence()
		if runErr != nil {
			return nil, runErr
		}

		var findings Findings
		if result.Output != nil {
			if err := json.Unmarshal(result.Output, &findings); err != nil {
				sctx.Log("could not parse structured output, using text response")
				findings = Findings{Summary: result.Text}
			}
		}
		if len(covered) > 0 {
			findings.Tested = append(tested(), findings.Tested...)
		}
		findings.Items = withOmission(findings.Items)

		needsApproval := hasBlockingFindings(findings.Items)
		autoFixable := needsApproval

		// Record any new test files the agent wrote as informational (no-op)
		// findings. Their presence alone is not an actionable problem, so they
		// must not force the test step into approval when tests pass (issue #140).
		newTests := detectNewTestFiles(ctx, sctx.WorkDir)
		for _, f := range newTests {
			findings.Items = append(findings.Items, Finding{
				Severity:    "info",
				Action:      types.ActionNoOp,
				File:        f,
				Description: fmt.Sprintf("new test file written by agent: %s", f),
			})
		}

		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: needsApproval,
			AutoFixable:   autoFixable,
			Findings:      string(findingsJSON),
			FixSummary:    fixSummary,
		}, nil
	}

	// In fix mode the agent may add new test files while making tests pass.
	// Record them as informational (no-op) findings but do not gate on them:
	// passing tests with only informational findings proceed automatically (issue #140).
	if sctx.Fixing && len(newTestsFromFix) > 0 {
		findings := Findings{
			Summary: "tests passed, but agent wrote new test files",
			Items:   withOmission(nil),
			Tested:  tested(),
		}
		for _, f := range newTestsFromFix {
			findings.Items = append(findings.Items, Finding{
				Severity:    "info",
				Action:      types.ActionNoOp,
				File:        f,
				Description: fmt.Sprintf("new test file written by agent: %s", f),
			})
		}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: hasBlockingFindings(findings.Items),
			Findings:      string(findingsJSON),
			FixSummary:    fixSummary,
		}, nil
	}

	sctx.Log("all tests passed")
	greenFindings := Findings{Items: withOmission(nil), Tested: tested()}
	findingsJSON, _ := json.Marshal(greenFindings)
	return &pipeline.StepOutcome{
		NeedsApproval: hasBlockingFindings(greenFindings.Items),
		Findings:      string(findingsJSON),
		FixSummary:    fixSummary,
	}, nil
}

// testStepPark is one of the Test step's two gate shapes, closed over the
// attempt's own findings and fix summary so the guard below can raise either
// without rebuilding them.
type testStepPark func(description string) (*pipeline.StepOutcome, error)

// testUnitCoverageDir is where one unit's command writes its coverage profile
// and test report. Every unit gets its own directory under the run's coverage
// directory, because a shared one would let a unit that wrote nothing be
// greened by the profile a different unit left there.
//
// The segment is the sanitized unit name, and when sanitizing changed the name
// or left something that is not a usable single path element it also carries a
// dash plus the first 8 hex characters of the name's sha256. Two distinct unit
// names can sanitize to one segment ("api/v1" and "api v1" both become
// "api-v1"), and sharing a directory between them is the same cross-unit leak.
// The suffix is conditional so the ordinary name stays readable in a log line
// and a gate finding, and only the colliding case pays for the distinction.
//
// The caller wipes the returned directory before the unit's command runs, so a
// name that resolves anywhere but strictly inside the run's coverage directory
// ("." is the run's own root, ".." is the root every run shares) is refused
// rather than sanitized into something plausible. Unit names are not fully
// maintainer-controlled: an agent-inferred layout names its own units.
func testUnitCoverageDir(sctx *pipeline.StepContext, unitName string) (string, error) {
	root := runCoverageDir(sctx)
	if root == "" {
		return "", fmt.Errorf("test coverage dir is not configured for this run")
	}
	segment := sanitizeEvidenceSegment(unitName)
	if segment != unitName || !isSingleSafePathSegment(segment) {
		sum := sha256.Sum256([]byte(unitName))
		suffix := hex.EncodeToString(sum[:])[:8]
		if isSingleSafePathSegment(segment) {
			segment += "-" + suffix
		} else {
			segment = "unit-" + suffix
		}
	}
	dir := filepath.Join(root, segment)
	if filepath.Dir(dir) != filepath.Clean(root) {
		return "", fmt.Errorf("test unit %q resolves to a coverage directory outside %s", unitName, root)
	}
	return dir, nil
}

// isSingleSafePathSegment reports whether s can be joined onto a directory as
// exactly one child of it. Empty, "." and ".." each resolve to a directory the
// unit does not own, and a separator would nest or escape.
func isSingleSafePathSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, `/\`) {
		return false
	}
	return !filepath.IsAbs(s)
}

// guardVacuousGreen decides whether the unit commands that just exited zero
// actually exercised the change. A command that runs no test exits zero, so
// does one whose every test was filtered out, and so does a suite that tests
// only code the change never touched; the exit code cannot tell any of those
// from a real pass.
//
// It returns a nil outcome and a nil error when the attempt clears the gate.
// A Go error is a filesystem fault reading the artifacts, never a verdict.
func guardVacuousGreen(
	sctx *pipeline.StepContext,
	covered []config.TestUnit,
	changed []string,
	baseSHA string,
	parkForMaintainer testStepPark,
	parkVacuousGreen testStepPark,
) (*pipeline.StepOutcome, error) {
	var merged coverageProfile
	// Kept beside the merged profile because the changed-function check reads
	// each unit's reporting convention on its own; see
	// coveredChangedFunctionsAcrossUnits.
	var unitProfiles []coverageProfile
	seenFiles := map[string]bool{}
	// Carried to every verdict below, including the auto-fixable ones: an agent
	// asked to write a missing test needs to see that a profile was skipped or
	// unparseable, because that may be the whole reason its coverage looks
	// absent.
	var artifactNotes []string
	// A unit that skipped an artifact, or whose directory held more artifacts
	// than the reader was willing to open, reported less than it measured, so
	// the two vacuity verdicts below stop being evidence about the tests: the
	// missing coverage may be sitting in the file the reader could not use or
	// never reached. That is a command to repair, never an agent fix round, so
	// the verdict routes to the maintainer instead.
	artifactUnread := false
	vacuityPark := func(description string) (*pipeline.StepOutcome, error) {
		if artifactUnread {
			return parkForMaintainer(description)
		}
		return parkVacuousGreen(description)
	}

	for _, unit := range covered {
		unitDir, err := testUnitCoverageDir(sctx, unit.Name)
		if err != nil {
			return nil, err
		}
		artifacts, err := readCoverageArtifacts(unitDir)
		if err != nil {
			return nil, fmt.Errorf("read coverage artifacts for test unit %q: %w", unit.Name, err)
		}

		// The maintainer parks below are all configuration problems: the
		// command does not emit the artifacts, or emits them in a shape the
		// guard cannot read. An agent fix round would paper over that by
		// changing the tests instead of the command that reports on them.
		//
		// What the guard needs is judged first, and an artifact it had to skip
		// or could not parse is carried as context on that verdict. A skipped
		// artifact alone is not a park: a runner that writes a giant log or a
		// large sibling directory beside a perfectly good profile has broken
		// nothing the guard reads.
		notes := unreadableArtifactNotes(artifacts)
		if notes != "" {
			artifactNotes = append(artifactNotes, fmt.Sprintf("test unit %q%s", unit.Name, notes))
		}
		if len(artifacts.Skipped) > 0 || artifacts.ScanLimited {
			artifactUnread = true
		}
		switch {
		case !artifacts.HasProfile:
			return parkForMaintainer(fmt.Sprintf("test unit %q wrote no coverage profile to %s; a test run that exercised nothing cannot report a passing gate%s", unit.Name, unitDir, notes))
		case !artifacts.HasReport:
			return parkForMaintainer(fmt.Sprintf("test unit %q wrote no test report to %s; without a reported test count a passing exit code proves nothing ran%s", unit.Name, unitDir, notes))
		case len(artifacts.Unparseable) > 0:
			return parkForMaintainer(fmt.Sprintf("test unit %q wrote a coverage artifact that could not be parsed: %s", unit.Name, strings.Join(artifacts.Unparseable, ", ")))
		}

		if artifacts.Report.Executed <= 0 {
			return vacuityPark(fmt.Sprintf("test unit %q reported %d executed tests; a command that runs no test cannot report a passing gate%s", unit.Name, artifacts.Report.Executed, notes))
		}

		unitProfiles = append(unitProfiles, artifacts.Profile)
		merged.Functions = append(merged.Functions, artifacts.Profile.Functions...)
		for _, file := range artifacts.Profile.Files {
			if seenFiles[file] {
				continue
			}
			seenFiles[file] = true
			merged.Files = append(merged.Files, file)
		}
	}

	// A profile that parses but names no source file describes nothing, so it
	// certifies nothing: the extension exemption below would read it as "this
	// change touches nothing a profile could describe" and green a run whose
	// instrumenter matched no source at all. That is a reporting problem of the
	// same class as a missing profile, so it goes to the maintainer.
	mergedNotes := ""
	if len(artifactNotes) > 0 {
		mergedNotes = " (" + strings.Join(artifactNotes, "; ") + ")"
	}

	if len(merged.Files) == 0 {
		return parkForMaintainer(fmt.Sprintf(
			"the coverage profiles the units that ran (%s) wrote name no source file; a profile describing nothing cannot certify the change%s",
			strings.Join(testUnitNames(covered), ", "),
			mergedNotes,
		))
	}

	// The changed-function check reads every unit's profile, because a change
	// can span two units and either one's tests may be the ones that exercise
	// it, but it reads them one unit at a time so no unit's reporting
	// convention decides how another's paths are matched.
	//
	// The diff is read before the files are classified, because a file the
	// change only deleted lines from carries nothing to exercise and so is not
	// a file coverage can be demanded for.
	//
	// A maintainer's ignore_patterns entry excuses the file it names from
	// needing a covered function, so the entry is read from the TRUSTED
	// default-branch copy, never the pushed one. Exempting a changed file from
	// the gate that judges it is the same authority review.path_instructions
	// splits on: a contributor who could add `ignore_patterns: ["**"]` to their
	// own branch would turn the guard off for the change it exists to check.
	guarded := reviewablePaths(changed, sctx.Config.TrustedIgnorePatterns)
	if len(guarded) == 0 {
		sctx.Log("every changed file matches a trusted ignore_patterns entry, so no covered function is required")
		return nil, nil
	}
	ranges, err := changedLineRanges(sctx.Ctx, sctx.WorkDir, baseSHA, sctx.Run.HeadSHA, sctx.Fixing, guarded)
	if err != nil {
		return nil, err
	}
	// One path set drives both halves of the verdict. changedLineRanges parses
	// the whole diff, so its map still carries the paths the ignore filter just
	// excluded; handing the unfiltered map to the coverage check let an
	// executed function in an ignored file certify an uncovered guarded one,
	// and let an ignored or deletion-only path collide with a guarded path over
	// one short profile key and park a change that really was tested.
	guardedPaths := changedWithHeadSideLines(ranges, guarded)
	guardedRanges := rangesForPaths(ranges, guardedPaths)
	classified, err := classifyChangedFiles(sctx.Ctx, sctx.WorkDir, merged, guardedPaths)
	if err != nil {
		return nil, err
	}
	if len(classified.Coverable) == 0 {
		// A change carrying source files no profile could describe means the
		// commands that ran cover a different project than the change. That is a
		// configuration problem, and greening it is exactly what issue 9 refuses.
		if len(classified.Unexplained) > 0 {
			return parkForMaintainer(fmt.Sprintf(
				"the coverage profiles the units that ran (%s) wrote describe none of the source files this change touches (%s); a command pointed at other code cannot certify it%s",
				strings.Join(testUnitNames(covered), ", "),
				summarizePaths(classified.Unexplained, 5),
				mergedNotes,
			))
		}
		// A documentation or configuration change touches nothing a coverage
		// profile could ever describe. Demanding a covered function there would
		// park every README edit.
		sctx.Log("no changed file has an extension the coverage profiles describe, so no covered function is required")
		return nil, nil
	}

	if len(coveredChangedFunctionsAcrossUnits(unitProfiles, sctx.WorkDir, guardedRanges)) == 0 {
		return vacuityPark(fmt.Sprintf(
			"no test exercised a changed function; the units that ran (%s) recorded no executed function in the changed lines of %s%s",
			strings.Join(testUnitNames(covered), ", "),
			summarizePaths(classified.Coverable, 5),
			mergedNotes,
		))
	}
	return nil, nil
}

// unreadableArtifactNotes renders what the reader could not use as a
// parenthesised suffix on another park's description, so a maintainer reading
// "wrote no coverage profile" also sees the oversized or unparseable file that
// may be why.
func unreadableArtifactNotes(artifacts coverageArtifacts) string {
	var notes []string
	if len(artifacts.Unparseable) > 0 {
		notes = append(notes, "artifacts that could not be parsed: "+strings.Join(artifacts.Unparseable, ", "))
	}
	if len(artifacts.Skipped) > 0 {
		notes = append(notes, "artifacts the guard could not read: "+strings.Join(artifacts.Skipped, ", "))
	}
	if artifacts.ScanLimited {
		notes = append(notes, fmt.Sprintf("stopped reading after %d coverage artifacts or %d files", maxCoverageFilesScanned, maxCoverageEntriesVisited))
	}
	if len(notes) == 0 {
		return ""
	}
	return " (" + strings.Join(notes, "; ") + ")"
}

func testUnitNames(units []config.TestUnit) []string {
	names := make([]string, len(units))
	for i, unit := range units {
		names[i] = unit.Name
	}
	return names
}

// summarizePaths renders at most limit paths and appends a count for the rest,
// so a gate finding names the files a maintainer should look at without
// pasting a large diff's whole file list into the PR body.
func summarizePaths(paths []string, limit int) string {
	if len(paths) <= limit {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:limit], ", "), len(paths)-limit)
}

func testAgentContext(sctx *pipeline.StepContext) (context.Context, context.CancelFunc, time.Duration) {
	timeout := config.DefaultTestAgentTimeout
	if sctx != nil && sctx.Config != nil && sctx.Config.TestAgentTimeout > 0 {
		timeout = sctx.Config.TestAgentTimeout
	}
	ctx, cancel := context.WithTimeoutCause(sctx.Ctx, timeout, errTestAgentTimeout)
	return ctx, cancel, timeout
}

// findTestUnit returns the unit named name from units and whether the layout
// has one. Discovery validates every selected name against its own layout (see
// validateDiscovery), so a miss means a caller looked up a name discovery never
// vouched for; the caller parks on that rather than substituting a unit with no
// command, which would exit 0 and report the unit as tested.
func findTestUnit(units []config.TestUnit, name string) (config.TestUnit, bool) {
	for _, unit := range units {
		if unit.Name == name {
			return unit, true
		}
	}
	return config.TestUnit{}, false
}

var errTestAgentTimeout = errors.New("test agent timeout")

// testAgentError renders a Test-invocation budget expiry. It keeps the agent's
// own error rather than replacing it with the bare context cause: for a native
// agent that error carries the killed subprocess's exit status and stderr, and
// is the only account of what the process was doing when the budget ran out.
func testAgentError(ctx context.Context, timeout time.Duration, prefix string, err error) error {
	if timeout > 0 && errors.Is(context.Cause(ctx), errTestAgentTimeout) {
		if err == nil {
			err = context.Cause(ctx)
		}
		return fmt.Errorf("%s timed out after %s: %w", prefix, timeout, err)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return nil
}
