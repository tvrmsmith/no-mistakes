package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// untrustedWorkspaceFixture is a VERBATIM copy of the stderr line from run
// 01M1FAF1H15SSVAHRHKDEY6BBG: see
// ~/.no-mistakes/logs/01M1FAF1H15SSVAHRHKDEY6BBG/review.log line 292. That
// run was not actually wedged by this warning (it was a slow review that hit
// its own 30-minute budget); the fixture is kept as the verbatim source of a
// permissions.allow warning, the category that --dangerously-skip-permissions
// (the default agent's bypass) makes inert.
const untrustedWorkspaceFixture = `Ignoring 8 permissions.allow entries from .claude/settings.json: this workspace has not been trusted. Run Claude Code interactively here once and accept the trust dialog, or set projects["/Users/trevor.smith/.no-mistakes/repos/871d740473c0.git"].hasTrustDialogAccepted: true in /Users/trevor.smith/.claude.json.
`

// untrustedWorkspaceAdditionalDirectoriesFixture is the one category that
// still costs the run under bypass: --dangerously-skip-permissions grants
// approval, not extra read roots, so a discarded permissions.additionalDirectories
// entry really does shrink what the agent can read.
const untrustedWorkspaceAdditionalDirectoriesFixture = `Ignoring 1 permissions.additionalDirectories entry from .claude/settings.json: this workspace has not been trusted. Run Claude Code interactively here once and accept the trust dialog, or set projects["/Users/trevor.smith/.no-mistakes/repos/871d740473c0.git"].hasTrustDialogAccepted: true in /Users/trevor.smith/.claude.json.
`

// wantRemedySubstring is the exact remedy text the assignment pins, derived
// from claudetrust.Remedy for the workspace path shared by both fixtures
// above.
const wantRemedySubstring = `run claude interactively in /Users/trevor.smith/.no-mistakes/repos/871d740473c0.git once and accept the trust dialog, or set projects["/Users/trevor.smith/.no-mistakes/repos/871d740473c0.git"].hasTrustDialogAccepted to true in`

// newClaudeConfigDir points CLAUDE_CONFIG_DIR at a directory holding a
// .claude.json, so claudetrust.ConfigPath resolves deterministically in the
// child helper process instead of falling back to the real user home.
func newClaudeConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write fake .claude.json: %v", err)
	}
	return dir
}

// TestClaudeAgent_UntrustedWorkspaceAbortsWithoutWaitingForTheProcess is the
// whole point of the abort: claude hangs for 60s after writing the
// additionalDirectories fixture (a category that bites even under the
// default agent's bypass), and the call still has to return in well under
// 10s because the adapter tears the process down itself instead of waiting
// for it to exit or for the run's own deadline.
func TestClaudeAgent_UntrustedWorkspaceAbortsWithoutWaitingForTheProcess(t *testing.T) {
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "untrusted-then-hang")
	t.Setenv("CLAUDE_CONFIG_DIR", newClaudeConfigDir(t))
	a := newClaudeStdinHelperAgent(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	started := time.Now()
	res, err := a.runOnce(ctx, RunOpts{Prompt: "review", CWD: t.TempDir()})
	elapsed := time.Since(started)

	if elapsed > 10*time.Second {
		t.Fatalf("runOnce took %v, want well under 10s: it must abort, not wait out the hang", elapsed)
	}
	if res != nil {
		t.Fatalf("result = %+v, want nil on abort", res)
	}
	if !errors.Is(err, errClaudeWorkspaceUntrusted) {
		t.Fatalf("error %v does not wrap errClaudeWorkspaceUntrusted", err)
	}
	if !strings.Contains(err.Error(), wantRemedySubstring) {
		t.Fatalf("error %q missing remedy substring %q", err, wantRemedySubstring)
	}
}

// TestClaudeAgent_UntrustedWorkspaceWinsOverASuccessfulResult pins that the
// abort is not merely a fallback for a run that otherwise failed: even a
// complete, valid, successful result event loses to the trust abort, for a
// warning category (permissions.additionalDirectories) that bites under the
// default agent's bypass.
func TestClaudeAgent_UntrustedWorkspaceWinsOverASuccessfulResult(t *testing.T) {
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "untrusted-with-result")
	t.Setenv("CLAUDE_CONFIG_DIR", newClaudeConfigDir(t))
	a := newClaudeStdinHelperAgent(t)

	res, err := a.runOnce(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir()})

	if res != nil {
		t.Fatalf("result = %+v, want nil: the abort must win over a parsed success", res)
	}
	if !errors.Is(err, errClaudeWorkspaceUntrusted) {
		t.Fatalf("error %v does not wrap errClaudeWorkspaceUntrusted", err)
	}
}

// TestClaudeAgent_AdditionalDirectoriesAbortsUnderBypass is the direct
// coverage for the bites-under-bypass rule: even with the default agent (no
// pinned permission mode, so buildArgs adds --dangerously-skip-permissions),
// a discarded permissions.additionalDirectories entry still aborts the run,
// because bypass grants tool-call approval, not extra read roots.
func TestClaudeAgent_AdditionalDirectoriesAbortsUnderBypass(t *testing.T) {
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "untrusted-with-result")
	t.Setenv("CLAUDE_CONFIG_DIR", newClaudeConfigDir(t))
	a := newClaudeStdinHelperAgent(t)

	res, err := a.runOnce(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir()})

	if res != nil {
		t.Fatalf("result = %+v, want nil on abort", res)
	}
	if !errors.Is(err, errClaudeWorkspaceUntrusted) {
		t.Fatalf("error %v does not wrap errClaudeWorkspaceUntrusted", err)
	}
}

// TestClaudeAgent_PermissionsAllowUnderBypassDoesNotAbort is the regression
// guard for the whole correction: a discarded permissions.allow entry is
// inert once --dangerously-skip-permissions is on the command line (the
// default for an agent with no pinned permission mode), because permission
// checking itself is off. Aborting here used to be the very bug this file
// was rewritten to fix - the run must reach its normal successful outcome,
// and the dropped category must not go unreported, so it is surfaced once
// through opts.OnChunk instead.
func TestClaudeAgent_PermissionsAllowUnderBypassDoesNotAbort(t *testing.T) {
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "untrusted-allow-with-result")
	t.Setenv("CLAUDE_CONFIG_DIR", newClaudeConfigDir(t))
	a := newClaudeStdinHelperAgent(t)

	var chunks []string
	res, err := a.runOnce(context.Background(), RunOpts{
		Prompt: "review",
		CWD:    t.TempDir(),
		OnChunk: func(chunk string) {
			chunks = append(chunks, chunk)
		},
	})

	if err != nil {
		t.Fatalf("runOnce: %v, want the run to reach its normal outcome", err)
	}
	if res == nil {
		t.Fatal("result is nil, want the parsed successful result")
	}

	found := false
	for _, c := range chunks {
		if strings.Contains(c, "permissions.allow") && strings.Contains(c, wantRemedySubstring) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("OnChunk chunks %v never reported the discarded permissions.allow warning", chunks)
	}
}

// TestClaudeAgent_PermissionsAllowAbortsWithoutBypass proves the other half
// of the rule: with the operator pinning their own --permission-mode
// (so buildArgs does not add --dangerously-skip-permissions), permission
// checking is genuinely in effect, and the same permissions.allow fixture
// that is inert under bypass now aborts the run.
func TestClaudeAgent_PermissionsAllowAbortsWithoutBypass(t *testing.T) {
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "untrusted-allow-with-result")
	t.Setenv("CLAUDE_CONFIG_DIR", newClaudeConfigDir(t))
	a := newClaudeStdinHelperAgentPinningPermissionMode(t)

	res, err := a.runOnce(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir()})

	if res != nil {
		t.Fatalf("result = %+v, want nil on abort", res)
	}
	if !errors.Is(err, errClaudeWorkspaceUntrusted) {
		t.Fatalf("error %v does not wrap errClaudeWorkspaceUntrusted", err)
	}
}

// TestClaudeAgent_UnrelatedStderrFailureIsUnchanged makes sure the new stderr
// scanner didn't disturb the existing exit-error path: ordinary stderr noise
// unrelated to workspace trust still produces "claude exited: ...: <stderr>"
// and never matches the trust sentinel.
func TestClaudeAgent_UnrelatedStderrFailureIsUnchanged(t *testing.T) {
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "unrelated-stderr-fail")
	t.Setenv("CLAUDE_CONFIG_DIR", newClaudeConfigDir(t))
	a := newClaudeStdinHelperAgent(t)

	_, err := a.runOnce(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir()})

	if err == nil {
		t.Fatal("expected the non-zero exit to fail")
	}
	if errors.Is(err, errClaudeWorkspaceUntrusted) {
		t.Fatalf("error %v unexpectedly matched the trust sentinel", err)
	}
	if !strings.Contains(err.Error(), "claude exited:") {
		t.Fatalf("error %q lost the exit-error wrapping", err)
	}
	if !strings.Contains(err.Error(), "warning: some unrelated deprecation notice") {
		t.Fatalf("error %q dropped the stderr detail", err)
	}
}

// TestClaudeAgent_CleanRunWithEmptyStderrStillSucceeds is the regression
// guard for the stderr scanner rewrite: a normal successful run with no
// stderr output at all must be completely unaffected.
func TestClaudeAgent_CleanRunWithEmptyStderrStillSucceeds(t *testing.T) {
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "skill")
	t.Setenv("CLAUDE_CONFIG_DIR", newClaudeConfigDir(t))
	a := newClaudeStdinHelperAgent(t)

	res, err := a.runOnce(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if res == nil {
		t.Fatal("result is nil for a clean successful run")
	}
}

// TestClaudeRetryClassifier_DoesNotRetryUntrustedWorkspace pins that the
// abort is never spent as a retry: continuing would waste the run's whole
// budget on an agent that can never make progress against the same
// untrusted repo path.
func TestClaudeRetryClassifier_DoesNotRetryUntrustedWorkspace(t *testing.T) {
	_, retry := claudeRetryClassifier(errClaudeWorkspaceUntrusted)
	if retry {
		t.Fatal("claudeRetryClassifier retried the untrusted-workspace abort")
	}
}
