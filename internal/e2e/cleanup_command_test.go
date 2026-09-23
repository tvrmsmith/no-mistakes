//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestCleanupCommandJourney drives commands.cleanup through a real daemon run.
// The trusted default branch configures a cleanup script that records its
// working directory and whether the worktree still holds the repository's
// files. A completed run must invoke it twice, once on entry to push and once
// at run teardown, both times in the run worktree while that directory still
// exists, and a cleanup that exits non-zero must not change the run's outcome.
func TestCleanupCommandJourney(t *testing.T) {
	t.Run("runs_at_push_entry_and_teardown_in_the_run_worktree", func(t *testing.T) {
		h, logPath := cleanupHarness(t, "exit 0")
		run := pushAndWaitForCleanup(t, h, "cleanup-happy")
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}
		assertCleanupRanInWorktree(t, h, logPath, run.ID)
		pushLog, err := os.ReadFile(filepath.Join(h.NMHome, "logs", run.ID, "push.log"))
		if err != nil {
			t.Fatalf("read push log: %v", err)
		}
		if !strings.Contains(string(pushLog), "releasing external run resources") {
			t.Fatalf("push log does not record the cleanup invocation:\n%s", pushLog)
		}
	})

	t.Run("failing_cleanup_does_not_change_the_run_outcome", func(t *testing.T) {
		h, logPath := cleanupHarness(t, "echo teardown broke >&2; exit 7")
		run := pushAndWaitForCleanup(t, h, "cleanup-failing")
		if run.Status != types.RunCompleted {
			t.Fatalf("a failing cleanup must not fail the run: status=%s error=%v", run.Status, deref(run.Error))
		}
		push, ok := findStep(run.Steps, types.StepPush)
		if !ok || push.Status != types.StepStatusCompleted {
			t.Fatalf("push step should complete despite the failing cleanup, got %+v", push)
		}
		assertCleanupRanInWorktree(t, h, logPath, run.ID)
		daemonLog := waitForFileContaining(t, filepath.Join(h.NMHome, "logs", "daemon.log"), "run cleanup command exited non-zero", 30*time.Second)
		if !strings.Contains(daemonLog, "teardown broke") {
			t.Fatalf("daemon log should carry the cleanup output tail:\n%s", daemonLog)
		}
	})

	t.Run("pushed_branch_cannot_supply_a_cleanup_command", func(t *testing.T) {
		optOut := false
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})
		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}
		marker := filepath.Join(t.TempDir(), "pushed-cleanup-ran")
		branch := "cleanup-pushed"
		h.CommitChange(branch, branch+".txt", "change to gate\n", "add change")
		cfg := fmt.Sprintf("allow_repo_commands: false\ncommands:\n  cleanup: \"echo ran > %s\"\n", marker)
		h.CommitChange(branch, ".no-mistakes.yaml", cfg, "pushed cleanup")
		h.PushToGate(branch)
		run := h.WaitForRun(branch, 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}
		waitForWorktreeGone(t, h, run.ID)
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("SECURITY REGRESSION: pushed-branch commands.cleanup executed (marker %s exists)", marker)
		}
	})
}

// cleanupHarness commits a trusted default-branch config whose commands.cleanup
// appends "<physical cwd>|<README present or missing>" to a log, then runs
// `body`. allow_repo_commands is false, so the command can only come from the
// trusted copy.
func cleanupHarness(t *testing.T, body string) (*Harness, string) {
	t.Helper()
	optOut := false
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})
	logPath := filepath.Join(t.TempDir(), "cleanup.log")
	script := filepath.Join(h.BinDir, "nm-e2e-cleanup")
	content := fmt.Sprintf("#!/bin/sh\nif [ -f README.md ]; then files=present; else files=missing; fi\nprintf '%%s|%%s\\n' \"$(pwd -P)\" \"$files\" >> %q\n%s\n", logPath, body)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write cleanup script: %v", err)
	}
	cfg := fmt.Sprintf("ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\nallow_repo_commands: false\ncommands:\n  cleanup: %s\n", script)
	h.CommitChange("main", ".no-mistakes.yaml", cfg, "configure trusted cleanup")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push trusted config: %v\n%s", err, out)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	return h, logPath
}

func pushAndWaitForCleanup(t *testing.T, h *Harness, branch string) *ipc.RunInfo {
	t.Helper()
	h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 90*time.Second)
	waitForWorktreeGone(t, h, run.ID)
	return run
}

func assertCleanupRanInWorktree(t *testing.T, h *Harness, logPath, runID string) {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("cleanup command never ran: %v", err)
	}
	t.Logf("cleanup invocations (cwd|worktree files):\n%s", data)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("cleanup should run exactly twice (push entry + teardown), got %d:\n%s", len(lines), data)
	}
	wantDir, err := filepath.EvalSymlinks(filepath.Join(h.NMHome, "worktrees"))
	if err != nil {
		t.Fatalf("resolve worktrees dir: %v", err)
	}
	for i, line := range lines {
		cwd, files, _ := strings.Cut(line, "|")
		if filepath.Base(cwd) != runID || filepath.Dir(filepath.Dir(cwd)) != wantDir {
			t.Errorf("invocation %d cwd = %s, want %s/<repo>/%s", i+1, cwd, wantDir, runID)
		}
		if files != "present" {
			t.Errorf("invocation %d ran after the worktree lost its files (%s)", i+1, files)
		}
	}
}

// waitForWorktreeGone waits until run teardown has removed the run worktree,
// which is after the teardown cleanup invocation has returned.
func waitForWorktreeGone(t *testing.T, h *Harness, runID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(h.NMHome, "worktrees", "*", runID))
		if len(matches) == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("run worktree for %s was not removed", runID)
}

func waitForFileContaining(t *testing.T, path, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(path)
		if strings.Contains(string(data), want) {
			return string(data)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s never contained %q:\n%s", path, want, data)
	return ""
}
