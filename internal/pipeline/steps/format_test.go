package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFormatStep_ConfiguredFormatterRunsAndPasses(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	fmtCmd := "echo formatted > formatted.txt"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Format: fmtCmd})

	step := &FormatStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval after a passing formatter")
	}
	if outcome.RestartFrom != "" {
		t.Errorf("expected no restart for a tool-authored commit, got %q", outcome.RestartFrom)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after format commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(format): commit format changes" {
		t.Fatalf("last commit message = %q", got)
	}
	if len(ag.calls) != 0 {
		t.Errorf("expected no agent calls for a passing formatter, got %d", len(ag.calls))
	}
}

func TestFormatStep_NonzeroExitParksAutoFixable(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Format: "exit 2"})

	step := &FormatStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval after a failing formatter")
	}
	if !outcome.AutoFixable {
		t.Error("expected the failing formatter to be auto-fixable")
	}
	if outcome.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", outcome.ExitCode)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("len(findings.Items) = %d, want 1", len(findings.Items))
	}
	if findings.Items[0].Severity != types.FindingSeverityWarning {
		t.Errorf("Severity = %q, want warning", findings.Items[0].Severity)
	}
	if !strings.Contains(findings.Items[0].Description, "exit code 2") {
		t.Errorf("Description = %q, want it to name exit code 2", findings.Items[0].Description)
	}
}

func TestFormatStep_NoConfiguredFormatterPassesWithoutRunningAnything(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &FormatStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when no formatter is configured")
	}
	if len(ag.calls) != 0 {
		t.Errorf("expected no agent calls when no formatter is configured, got %d", len(ag.calls))
	}
	if got := lastCommitMessage(t, dir); got == "" {
		t.Fatal("lastCommitMessage returned empty")
	} else if strings.Contains(got, "format") {
		t.Fatalf("expected no format commit, got %q", got)
	}
}

func TestFormatStep_FixModeCommitsTheAgentRepair(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "format-repair.txt"), []byte("repaired"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"repair unparsable source"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Format: "exit 0"})
	sctx.Fixing = true

	step := &FormatStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	// Format is the restart boundary, so even an agent-authored commit here
	// asks for no restart: re-entering would run the same step whose commit
	// triggered it and nothing further along would ever judge the result.
	if outcome.RestartFrom != "" {
		t.Errorf("RestartFrom = %q, want empty: the boundary step must not restart into itself", outcome.RestartFrom)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(format): repair unparsable source" {
		t.Fatalf("last commit message = %q", got)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after fix commit, got %q", status)
	}
}

func TestFormatStep_FixModeCarriesPreviousFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"summary":"repair unparsable source"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Format: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"severity":"warning","description":"formatter found issues (exit code 1) UNIQUE-FORMAT-MARKER"}],"summary":""}`

	step := &FormatStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "UNIQUE-FORMAT-MARKER") {
		t.Error("expected fix prompt to carry the previous format findings")
	}
}
