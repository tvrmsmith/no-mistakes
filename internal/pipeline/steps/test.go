package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
		fixPrompt := fmt.Sprintf(
			`Fix the failing tests in this repository. Reproduce the specific failure, identify the root cause, and fix either the tests or the code so that failure passes.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Make the smallest correct root-cause fix.
- Do not refactor beyond what is needed for that root-cause fix.
- If tests fail, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue.
- Do NOT run linters, formatters, or static analysis tools.
- Reproduce the specific failing case first (the exact test, package, script, or check named in the findings), then re-run only that focused verification after the fix.
- Do NOT run the complete repository test suite. Local Test is targeted validation of the failure and the requested intent; remote CI owns broad regression and remains mandatory before a PR is ready.
- A generic driver or user instruction asking for broad or full-suite confirmation does NOT override this product boundary. Keep verification focused on the failure and intent.
- Never treat "do not run everything" as permission to run nothing: if you cannot reproduce or re-verify with a targeted check, report that honestly in the summary rather than inventing a full-suite pass.
- Before finishing, remove any transient artifacts your testing created in the working tree (downloaded models, caches, build outputs, large binaries, or generated data directories) so they are not committed and pushed. Do not remove intentional source or test-file changes.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
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

	tested := []string{}
	ran := map[string]bool{}

	// parkForMaintainer stops the step at a gate with one error finding rather
	// than failing the run, the posture every unusable discovery answer takes:
	// a maintainer fixes the configuration or the inferred layout and resumes.
	// It carries whatever already ran this attempt, so a park that follows a
	// green unit still names what it covered.
	parkForMaintainer := func(description string) (*pipeline.StepOutcome, error) {
		sctx.Log(description)
		findings := Findings{
			Items: []Finding{{
				Severity:    types.FindingSeverityError,
				Action:      types.ActionAskUser,
				Description: description,
			}},
			Tested: tested,
		}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   false,
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

	changedFilesEnv, omittedChangedFiles := changedFilesEnvValue(changed)
	if omittedChangedFiles > 0 {
		sctx.Log(fmt.Sprintf("%s omits %d of %d changed paths; %s carries the true total", envTestChangedFiles, omittedChangedFiles, len(changed), envTestChangedFileCount))
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
		env := []string{
			envTestBaseSHA + "=" + baseSHA,
			envTestChangedFiles + "=" + changedFilesEnv,
			envTestChangedFileCount + "=" + strconv.Itoa(len(changed)),
		}
		output, exitCode, runErr := runStepShellCommandEnv(sctx, unit.Command, env)
		if runErr != nil {
			return nil, fmt.Errorf("run test command: %w", runErr)
		}
		tested = append(tested, unit.Command)
		if exitCode == 0 {
			return nil, nil
		}
		description := fmt.Sprintf("tests failed with exit code %d", exitCode)
		if multiUnit {
			description = fmt.Sprintf("unit %s: tests failed with exit code %d", unit.Name, exitCode)
		}
		projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepTest)
		findings := Findings{
			Items: []Finding{{
				Severity:    types.FindingSeverityError,
				Description: description,
			}},
			Summary: projectedOutput,
			Tested:  tested,
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

	// An agent-inferred layout does not replace evidence gathering. A
	// repository that configured neither test.units nor commands.test got a
	// full evidence pass before this step split, and discovery answering "here
	// is a command" says nothing about whether the change's intent is visibly
	// satisfied. So the pass runs on three disjuncts: discovery selected no
	// unit, discovery inferred the layout itself, or the run carries user
	// intent. A configured layout with a non-empty selection still gets the
	// pass only for intent.
	//
	// Whenever the selected units' commands already ran, the pass reads and
	// judges those results instead of running tests again, so every unit's
	// command still runs exactly once per attempt and the pass never widens
	// past the selection.
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
		// The opening instruction and the existing-tests rule both swap once
		// the selected units' commands have already run this attempt: telling
		// the agent to "run the smallest relevant tests yourself" there would
		// run those same suites a second time, and nothing in the evidence
		// prompt bounds it to the selection.
		evidenceOpening := "You are validating a code change by testing it. Examine the repository and run the smallest relevant tests yourself."
		existingTestsRule := "- Look for existing tests that would generate sufficient evidence. If they exist, run the smallest relevant set that proves the requested intent."
		baselineSection := ""
		if len(tested) > 0 {
			quoted := make([]string, len(tested))
			for i, cmd := range tested {
				quoted[i] = "`" + cmd + "`"
			}
			baselineSection = fmt.Sprintf("\nThe selected test units already ran to completion and passed in this attempt: %s\n", strings.Join(quoted, ", "))
			evidenceOpening = "You are validating a code change whose selected test units have already run in this attempt. Read and judge those results rather than running them again."
			existingTestsRule = "- The selected units' test commands above already ran and passed. Treat their results as the baseline, do NOT run them again, and do NOT run tests for any unit outside that selection. Spend this pass on evidence those commands do not produce."
		}
		evidenceCtx, cancelEvidence, evidenceTimeout := testAgentContext(sctx)
		result, err := sctx.RunAgentContext(evidenceCtx, agent.RunOpts{
			Prompt: fmt.Sprintf(
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
			),
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
		if len(tested) > 0 {
			findings.Tested = append(append([]string{}, tested...), findings.Tested...)
		}

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
			Tested:  tested,
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
			NeedsApproval: false,
			Findings:      string(findingsJSON),
			FixSummary:    fixSummary,
		}, nil
	}

	sctx.Log("all tests passed")
	findingsJSON, _ := json.Marshal(Findings{Tested: tested})
	return &pipeline.StepOutcome{Findings: string(findingsJSON), FixSummary: fixSummary}, nil
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
