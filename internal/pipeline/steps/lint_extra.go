package steps

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// applyExtraLinters runs the operator's globally configured extra linters
// (config.ExtraLinter) and folds their findings into the outcome the Lint
// step's own duty produced.
//
// It runs on every path through the step - configured commands.lint, the cold
// agent pass, and fix rounds - because the whole reason these linters exist is
// that nothing in the repository can discover them. A path that skipped them
// would report clean for the exact reason the operator configured them.
//
// It is additive. The lint duty's own findings, summary, exit code, and fix
// summary all survive unchanged; extra findings are appended, and the approval
// gate is only ever widened, never narrowed.
func applyExtraLinters(sctx *pipeline.StepContext, baseSHA string, outcome *pipeline.StepOutcome) (*pipeline.StepOutcome, error) {
	linters := sctx.Config.Lint.ExtraLinters
	if len(linters) == 0 || outcome == nil {
		return outcome, nil
	}

	// No ensurePrepared here. Configuring a personal linter must not change a
	// repository's own Lint behaviour, and commands.prepare runs in this step
	// only on the path lintDuty already runs it on.
	var extra []Finding
	for _, linter := range linters {
		items, err := runExtraLinter(sctx, baseSHA, linter)
		if err != nil {
			return nil, err
		}
		extra = append(extra, items...)
	}
	if len(extra) == 0 {
		return outcome, nil
	}
	extra = boundExtraLinterFindings(extra)

	findings, err := parseOutcomeFindings(outcome.Findings)
	if err != nil {
		return nil, fmt.Errorf("merge extra linter findings: %w", err)
	}
	findings.Items = append(findings.Items, extra...)
	merged, err := json.Marshal(findings)
	if err != nil {
		return nil, fmt.Errorf("serialize extra linter findings: %w", err)
	}
	outcome.Findings = string(merged)
	// Only ever widen. An info-severity extra linter leaves a passing step
	// passing; a warning or error one parks it, which is what the operator
	// asked for by setting that severity.
	outcome.NeedsApproval = outcome.NeedsApproval || hasBlockingFindings(extra)
	return outcome, nil
}

// runExtraLinter runs one extra linter and converts its output into findings.
//
// Two signals, and they answer different questions. A non-zero exit means the
// run itself broke - an unbuilt dependency, a missing binary, an unreadable
// config - and is reported as a blocking warning no matter what severity the
// entry carries, because a personal linter that failed to run is
// indistinguishable from one that found nothing, and that indistinguishability
// is the failure this whole feature exists to end. FindingsPattern answers the
// other question: which lines of a successful run are findings.
//
// The pattern is matched against stdout alone. Both streams reach the log and
// the failure finding, but one combined pipe interleaves stderr into stdout,
// which splits a diagnostic across two lines and drops it silently - the same
// reason the Metrics step reads its report through runStepShellCommandEnvSplit.
func runExtraLinter(sctx *pipeline.StepContext, baseSHA string, linter config.ExtraLinter) ([]Finding, error) {
	name := linter.EffectiveName()
	command := linter.EffectiveCommand()
	sctx.Log(fmt.Sprintf("running extra linter %s: %s", name, command))
	stdout, stderr, exitCode, err := runStepShellCommandEnvSplit(sctx, command, extraLinterEnv(sctx, baseSHA))
	if err != nil {
		return nil, fmt.Errorf("run extra linter %s: %w", name, err)
	}

	projected := logCommandOutput(sctx, joinCommandStreams(stdout, stderr), "extra linter "+name, types.StepLint)

	if exitCode != 0 {
		return []Finding{{
			ID:          extraLinterFindingID(name, "failed"),
			Severity:    types.FindingSeverityWarning,
			Action:      types.ActionAskUser,
			Description: clampExtraLinterDescription(fmt.Sprintf("extra linter %s failed (exit code %d), so its rules did not report on this change: %s", name, exitCode, projected)),
			Source:      name,
		}}, nil
	}

	pattern, err := regexp.Compile(linter.EffectivePattern())
	if err != nil {
		// Unreachable through config load, which compiles the same pattern.
		return nil, fmt.Errorf("compile findings_pattern for extra linter %s: %w", name, err)
	}
	items := matchExtraLinterFindings(pattern, stdout, linter)
	sctx.Log(fmt.Sprintf("extra linter %s: %d finding(s)", name, len(items)))
	return items, nil
}

// extraLinterAction keeps the configured severity and the finding's action in
// step. An empty action reads as ask-user downstream, which would park the
// step on the reporting-only default the operator did not ask to gate on.
func extraLinterAction(severity string) string {
	if types.NormalizeFindingSeverity(severity) == types.FindingSeverityInfo {
		return types.ActionNoOp
	}
	return types.ActionAskUser
}

// extraLinterFindingID names one extra-linter finding stably across rounds.
// The locator is the finding's ordinal within its own linter's output, so the
// IDs findings are selected and filtered by do not move when the lint duty's
// own finding list changes length between rounds.
func extraLinterFindingID(linterName, locator string) string {
	slug := config.ExtraLinterIDSlug(linterName)
	if slug == "" {
		slug = "unnamed"
	}
	return "lint-extra-" + slug + "-" + locator
}

// matchExtraLinterFindings turns each matching output line into one finding.
// Named capture groups file, line, and message supply the finding's location
// and text when the pattern declares them; otherwise the matched line is the
// text. The result is bounded by config.ExtraLinterMaxFindings, with the
// truncation stated in a final finding rather than left silent.
func matchExtraLinterFindings(pattern *regexp.Regexp, output string, linter config.ExtraLinter) []Finding {
	var items []Finding
	matched := 0
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		groups := pattern.FindStringSubmatch(line)
		if groups == nil {
			continue
		}
		matched++
		if len(items) >= config.ExtraLinterMaxFindings {
			continue
		}
		items = append(items, extraLinterFinding(pattern, groups, line, linter, matched))
	}
	if matched > len(items) {
		items = append(items, Finding{
			ID:          extraLinterFindingID(linter.EffectiveName(), "truncated"),
			Severity:    linter.EffectiveSeverity(),
			Action:      extraLinterAction(linter.EffectiveSeverity()),
			Description: fmt.Sprintf("%s: %d more finding(s) not listed; see the lint step log for the full output", linter.EffectiveName(), matched-len(items)),
			Source:      linter.EffectiveName(),
		})
	}
	return items
}

func extraLinterFinding(pattern *regexp.Regexp, groups []string, line string, linter config.ExtraLinter, index int) Finding {
	finding := Finding{
		Severity: linter.EffectiveSeverity(),
		Action:   extraLinterAction(linter.EffectiveSeverity()),
		Source:   linter.EffectiveName(),
	}
	message := ""
	for i, name := range pattern.SubexpNames() {
		if i == 0 || i >= len(groups) || name == "" {
			continue
		}
		switch name {
		case "file":
			finding.File = strings.TrimSpace(groups[i])
		case "line":
			if n, err := strconv.Atoi(strings.TrimSpace(groups[i])); err == nil {
				finding.Line = n
			}
		case "message":
			message = strings.TrimSpace(groups[i])
		}
	}
	if message == "" {
		message = strings.TrimSpace(line)
	}
	finding.Description = clampExtraLinterDescription(linter.EffectiveName() + ": " + message)
	finding.ID = extraLinterFindingID(linter.EffectiveName(), strconv.Itoa(index))
	return finding
}

// clampExtraLinterDescription bounds one finding's text. A linter's output line
// is arbitrary text from a command the pipeline does not control, and findings
// ride the IPC event stream whose reader frames at 1 MiB, so an
// unbounded description can push the whole event past the frame and lose every
// finding in it rather than one line's tail. The marker keeps the truncation
// visible to whoever reads the finding.
func clampExtraLinterDescription(description string) string {
	if len(description) <= extraLinterMaxDescriptionBytes {
		return description
	}
	return strings.ToValidUTF8(description[:extraLinterMaxDescriptionBytes], "") + extraLinterTruncationMarker
}

const (
	// extraLinterMaxDescriptionBytes bounds one finding's description, and
	// config.ExtraLinterMaxFindings bounds how many findings one linter
	// contributes. The product is the worst case one linter can add, and
	// extraLinterMaxTotalBytes then bounds the whole list however many linters
	// the operator configured, so no configuration can approach the frame limit.
	extraLinterMaxDescriptionBytes = 2000
	extraLinterMaxTotalBytes       = 128 * 1024
	extraLinterTruncationMarker    = " [truncated]"
)

// boundExtraLinterFindings keeps the operator's whole list within
// extraLinterMaxTotalBytes. The per-linter cap alone bounds each linter, not
// the list: enough linters each reporting their full 50 findings still add up.
// What is dropped is stated in a final finding rather than left silent.
func boundExtraLinterFindings(items []Finding) []Finding {
	total := 0
	for i, item := range items {
		total += len(item.Description)
		if total <= extraLinterMaxTotalBytes {
			continue
		}
		kept := items[:i:i]
		return append(kept, Finding{
			ID:          "lint-extra-findings-truncated",
			Severity:    types.FindingSeverityWarning,
			Action:      types.ActionAskUser,
			Description: fmt.Sprintf("extra linters reported more than %d bytes of findings; %d finding(s) are not listed, see the lint step log for the full output", extraLinterMaxTotalBytes, len(items)-i),
			Source:      "extra linters",
		})
	}
	return items
}

// extraLinterEnv is how the run's facts reach the command. Environment rather
// than template substitution into the command string: the diff base is the
// one value an extra linter genuinely needs (`--since "$NO_MISTAKES_BASE_SHA"`
// is the mode that fits a pipeline), and passing it this way leaves no
// quoting or interpolation question to get wrong.
//
// NO_MISTAKES_REPO_PATH is the registered checkout, not the run worktree. A
// personal linter is usually gated on an adoption registry keyed on the
// operator's real clone, and the run worktree is a detached worktree of the
// daemon's bare gate repository under NM_HOME - so a registry lookup that
// resolves the path itself finds an unadopted directory and skips silently.
func extraLinterEnv(sctx *pipeline.StepContext, baseSHA string) []string {
	env := []string{
		"NO_MISTAKES_BASE_SHA=" + baseSHA,
		"NO_MISTAKES_HEAD_SHA=" + sctx.Run.HeadSHA,
		"NO_MISTAKES_BRANCH=" + sctx.Run.Branch,
		"NO_MISTAKES_WORKDIR=" + sctx.WorkDir,
	}
	if sctx.Repo != nil {
		env = append(env, "NO_MISTAKES_REPO_PATH="+sctx.Repo.WorkingPath)
	}
	return env
}

func parseOutcomeFindings(raw string) (types.Findings, error) {
	if strings.TrimSpace(raw) == "" {
		return types.Findings{}, nil
	}
	return types.ParseFindingsJSON(raw)
}
