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
// agent pass, the combined document+lint housekeeping consume, and fix rounds
// - because the whole reason these linters exist is that nothing in the
// repository can discover them. A path that skipped them would report clean
// for the exact reason the operator configured them.
//
// It is additive. The lint duty's own findings, summary, exit code, and fix
// summary all survive unchanged; extra findings are appended, and the approval
// gate is only ever widened, never narrowed.
func applyExtraLinters(sctx *pipeline.StepContext, baseSHA string, outcome *pipeline.StepOutcome) (*pipeline.StepOutcome, error) {
	linters := sctx.Config.Lint.ExtraLinters
	if len(linters) == 0 || outcome == nil {
		return outcome, nil
	}

	// The extra linters are as entitled to installed dependencies as a
	// configured lint command is - an ESLint layer needs the package's own
	// node_modules. ensurePrepared is once-per-worktree, so the configured
	// path having already called it makes this a no-op.
	if err := ensurePrepared(sctx, types.StepLint); err != nil {
		return nil, fmt.Errorf("prepare extra linter dependencies: %w", err)
	}

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
// other question: which lines of a successful run are findings. Advisory rule
// sets exit 0 carrying their findings, so without a pattern a successful run
// contributes nothing.
func runExtraLinter(sctx *pipeline.StepContext, baseSHA string, linter config.ExtraLinter) ([]Finding, error) {
	sctx.Log(fmt.Sprintf("running extra linter %s: %s", linter.Name, linter.Command))
	env := append(stepEnvironment(sctx), extraLinterEnv(sctx, baseSHA)...)
	output, exitCode, err := runShellCommandWithEnv(sctx.Ctx, sctx.WorkDir, env, linter.Command)
	if err != nil {
		return nil, fmt.Errorf("run extra linter %s: %w", linter.Name, err)
	}

	projected := logCommandOutput(sctx, output, "extra linter "+linter.Name, types.StepLint)

	if exitCode != 0 {
		return []Finding{{
			Severity:    "warning",
			Description: fmt.Sprintf("extra linter %s failed (exit code %d), so its rules did not report on this change: %s", linter.Name, exitCode, projected),
			Source:      linter.Name,
		}}, nil
	}

	if linter.FindingsPattern == "" {
		sctx.Log(fmt.Sprintf("extra linter %s passed", linter.Name))
		return nil, nil
	}

	pattern, err := regexp.Compile(linter.FindingsPattern)
	if err != nil {
		// Unreachable through config load, which compiles the same pattern.
		return nil, fmt.Errorf("compile findings_pattern for extra linter %s: %w", linter.Name, err)
	}
	items := matchExtraLinterFindings(pattern, output, linter)
	sctx.Log(fmt.Sprintf("extra linter %s: %d finding(s)", linter.Name, len(items)))
	return items, nil
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
		items = append(items, extraLinterFinding(pattern, groups, line, linter))
	}
	if matched > len(items) {
		items = append(items, Finding{
			Severity:    linter.EffectiveSeverity(),
			Description: fmt.Sprintf("%s: %d more finding(s) not listed; see the lint step log for the full output", linter.Name, matched-len(items)),
			Source:      linter.Name,
		})
	}
	return items
}

func extraLinterFinding(pattern *regexp.Regexp, groups []string, line string, linter config.ExtraLinter) Finding {
	finding := Finding{
		Severity: linter.EffectiveSeverity(),
		Source:   linter.Name,
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
	finding.Description = linter.Name + ": " + message
	return finding
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
