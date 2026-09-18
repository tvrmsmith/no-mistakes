package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// extraLinterContext builds a lint-step context whose repository lint duty is
// whatever cmds says, plus the operator's global extra linters.
func extraLinterContext(t *testing.T, ag agent.Agent, cmds config.Commands, linters ...config.ExtraLinter) *pipeline.StepContext {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, cmds)
	sctx.Config.Lint = config.Lint{ExtraLinters: linters}
	return sctx
}

func outcomeFindings(t *testing.T, outcome *pipeline.StepOutcome) types.Findings {
	t.Helper()
	if strings.TrimSpace(outcome.Findings) == "" {
		return types.Findings{}
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse outcome findings %q: %v", outcome.Findings, err)
	}
	return findings
}

// The whole point of the feature: a linter the repository cannot declare still
// reports, and a green repo lint command does not hide it.
func TestLintStep_ExtraLinterReportsFindingsAGreenRepoCommandMisses(t *testing.T) {
	t.Parallel()
	sctx := extraLinterContext(t, &mockAgent{name: "test"}, config.Commands{Lint: "exit 0"}, config.ExtraLinter{
		Name:            "personal-dotnet",
		Command:         `echo "  src/Widget.cs(12,5): warning TVRM001: prefer a sealed class"`,
		FindingsPattern: `^\s*(?P<file>[^\s(]+)\((?P<line>\d+),\d+\): warning (?P<message>TVRM\d+: .*)$`,
		Severity:        "info",
	})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings := outcomeFindings(t, outcome)
	if len(findings.Items) != 1 {
		t.Fatalf("expected 1 extra-linter finding, got %d: %s", len(findings.Items), outcome.Findings)
	}
	got := findings.Items[0]
	if got.File != "src/Widget.cs" || got.Line != 12 {
		t.Errorf("expected the named capture groups to locate the finding, got %q:%d", got.File, got.Line)
	}
	if !strings.Contains(got.Description, "TVRM001") {
		t.Errorf("expected the rule id in the description, got %q", got.Description)
	}
	if got.Severity != "info" {
		t.Errorf("expected info severity, got %q", got.Severity)
	}
	if got.Source != "personal-dotnet" {
		t.Errorf("expected the linter name as the finding source, got %q", got.Source)
	}
}

// Info is the default and it reports without gating, so wiring a personal
// linter up never silently starts blocking pushes.
func TestLintStep_ExtraLinterInfoFindingsDoNotPark(t *testing.T) {
	t.Parallel()
	sctx := extraLinterContext(t, &mockAgent{name: "test"}, config.Commands{Lint: "exit 0"}, config.ExtraLinter{
		Name:            "personal-go",
		Command:         `echo "main.go:3:1: avoid init()"`,
		FindingsPattern: `^(?P<file>[^:]+):(?P<line>\d+):\d+: (?P<message>.*)$`,
		Severity:        "info",
	})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected info-severity extra findings to report without parking the step")
	}
	if len(outcomeFindings(t, outcome).Items) != 1 {
		t.Errorf("expected the finding to still be reported: %s", outcome.Findings)
	}
}

// Escalation is available, and only when the operator asks for it by name.
func TestLintStep_ExtraLinterWarningSeverityParksTheStep(t *testing.T) {
	t.Parallel()
	sctx := extraLinterContext(t, &mockAgent{name: "test"}, config.Commands{Lint: "exit 0"}, config.ExtraLinter{
		Name:            "personal-go",
		Command:         `echo "main.go:3:1: avoid init()"`,
		FindingsPattern: `^(?P<file>[^:]+):(?P<line>\d+):\d+: (?P<message>.*)$`,
		Severity:        "warning",
	})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected warning-severity extra findings to park the step for a decision")
	}
}

// A personal linter that could not run is indistinguishable from one that
// found nothing, which is the exact failure this feature exists to end. It
// must park regardless of the configured severity.
func TestLintStep_ExtraLinterBrokenRunParksEvenAtInfoSeverity(t *testing.T) {
	t.Parallel()
	sctx := extraLinterContext(t, &mockAgent{name: "test"}, config.Commands{Lint: "exit 0"}, config.ExtraLinter{
		Name:            "personal-dotnet",
		Command:         `echo "no analyzer binary" >&2; exit 3`,
		FindingsPattern: `: warning TVRM\d+`,
		Severity:        "info",
	})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a broken extra linter to park the step")
	}
	findings := outcomeFindings(t, outcome)
	if len(findings.Items) != 1 {
		t.Fatalf("expected 1 failure finding, got %d: %s", len(findings.Items), outcome.Findings)
	}
	if findings.Items[0].Severity != "warning" {
		t.Errorf("expected the failure finding to be a blocking warning, got %q", findings.Items[0].Severity)
	}
	if !strings.Contains(findings.Items[0].Description, "exit code 3") {
		t.Errorf("expected the exit code in the failure finding, got %q", findings.Items[0].Description)
	}
	if !strings.Contains(findings.Items[0].Description, "no analyzer binary") {
		t.Errorf("expected the command's own output in the failure finding, got %q", findings.Items[0].Description)
	}
}

// No pattern means "the exit code is the only signal", so a clean exit
// contributes nothing even when the command printed progress.
func TestLintStep_ExtraLinterWithoutPatternReportsNothingOnACleanExit(t *testing.T) {
	t.Parallel()
	sctx := extraLinterContext(t, &mockAgent{name: "test"}, config.Commands{Lint: "exit 0"}, config.ExtraLinter{
		Name:    "personal-ts",
		Command: `echo "=== packages/web ==="`,
	})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomeFindings(t, outcome).Items) != 0 {
		t.Errorf("expected no findings from a patternless clean run, got %s", outcome.Findings)
	}
	if outcome.NeedsApproval {
		t.Error("expected a patternless clean run to leave the step passing")
	}
}

// The failure the evidence log showed: no commands.lint, the agent discovers
// only repo-committed tooling and reports clean. The extra linter must still
// run on that path.
func TestLintStep_ExtraLinterRunsOnTheAgentPathWithNoLintCommand(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"lint clean"}`)}, nil
		},
	}
	sctx := extraLinterContext(t, ag, config.Commands{}, config.ExtraLinter{
		Name:            "personal-dotnet",
		Command:         `echo "src/Widget.cs(1,1): warning TVRM002: no"`,
		FindingsPattern: `warning (?P<message>TVRM\d+: .*)$`,
	})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings := outcomeFindings(t, outcome)
	if len(findings.Items) != 1 {
		t.Fatalf("expected the extra linter to report on the agent path, got %s", outcome.Findings)
	}
	if findings.Items[0].Severity != config.DefaultExtraLinterSeverity {
		t.Errorf("expected the default severity %q, got %q", config.DefaultExtraLinterSeverity, findings.Items[0].Severity)
	}
}

// The repository's own failing lint command keeps its finding, its exit code,
// and its output; the extra linter only appends.
func TestLintStep_ExtraLinterIsAdditiveToAFailingRepoLintCommand(t *testing.T) {
	t.Parallel()
	sctx := extraLinterContext(t, &mockAgent{name: "test"}, config.Commands{Lint: `echo "repo lint broke"; exit 1`}, config.ExtraLinter{
		Name:            "personal-go",
		Command:         `echo "main.go:1:1: no"`,
		FindingsPattern: `^(?P<file>[^:]+):(?P<line>\d+):\d+: (?P<message>.*)$`,
	})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ExitCode != 1 {
		t.Errorf("expected the repo lint command's exit code to survive, got %d", outcome.ExitCode)
	}
	findings := outcomeFindings(t, outcome)
	if len(findings.Items) != 2 {
		t.Fatalf("expected the repo finding plus the extra one, got %d: %s", len(findings.Items), outcome.Findings)
	}
	if !strings.Contains(findings.Summary, "repo lint broke") {
		t.Errorf("expected the repo command's output to survive in the summary, got %q", findings.Summary)
	}
}

// The diff base has to reach the command, since --since <ref> is the mode that
// fits a pipeline, and the adoption registry key has to be the registered
// checkout rather than the daemon's gate worktree.
func TestLintStep_ExtraLinterReceivesTheRunFactsInTheEnvironment(t *testing.T) {
	t.Parallel()
	sctx := extraLinterContext(t, &mockAgent{name: "test"}, config.Commands{Lint: "exit 0"}, config.ExtraLinter{
		Name:            "probe",
		Command:         `echo "base=$NO_MISTAKES_BASE_SHA head=$NO_MISTAKES_HEAD_SHA branch=$NO_MISTAKES_BRANCH repo=$NO_MISTAKES_REPO_PATH workdir=$NO_MISTAKES_WORKDIR"`,
		FindingsPattern: `^(?P<message>base=.*)$`,
	})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings := outcomeFindings(t, outcome)
	if len(findings.Items) != 1 {
		t.Fatalf("expected the probe finding, got %s", outcome.Findings)
	}
	reported := findings.Items[0].Description
	for _, want := range []string{
		"base=" + sctx.Run.BaseSHA,
		"head=" + sctx.Run.HeadSHA,
		"branch=" + sctx.Run.Branch,
		"repo=" + sctx.Repo.WorkingPath,
		"workdir=" + sctx.WorkDir,
	} {
		if !strings.Contains(reported, want) {
			t.Errorf("expected %q in the extra linter environment, got %q", want, reported)
		}
	}
}

// Findings ride the IPC event stream and the PR body, so one linter pointed at
// a large legacy tree must not make either unbounded - and the truncation is
// stated rather than silent.
func TestLintStep_ExtraLinterFindingsAreBoundedAndSaySo(t *testing.T) {
	t.Parallel()
	total := config.ExtraLinterMaxFindings + 7
	sctx := extraLinterContext(t, &mockAgent{name: "test"}, config.Commands{Lint: "exit 0"}, config.ExtraLinter{
		Name:            "flood",
		Command:         fmt.Sprintf(`seq 1 %d | sed 's/^/finding /'`, total),
		FindingsPattern: `^(?P<message>finding \d+)$`,
	})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	items := outcomeFindings(t, outcome).Items
	if len(items) != config.ExtraLinterMaxFindings+1 {
		t.Fatalf("expected %d findings plus one truncation notice, got %d", config.ExtraLinterMaxFindings, len(items))
	}
	last := items[len(items)-1].Description
	if !strings.Contains(last, "7 more finding(s) not listed") {
		t.Errorf("expected the truncation notice to state the remainder, got %q", last)
	}
}

// A machine with no extra linters configured behaves exactly as before.
func TestLintStep_NoExtraLintersLeavesTheOutcomeUntouched(t *testing.T) {
	t.Parallel()
	sctx := extraLinterContext(t, &mockAgent{name: "test"}, config.Commands{Lint: "exit 0"})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || strings.TrimSpace(outcome.Findings) != "" {
		t.Errorf("expected an unchanged passing outcome, got %+v", outcome)
	}
}
