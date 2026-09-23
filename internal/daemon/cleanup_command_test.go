package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/closers"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// recordCwdCleanupCommand appends the directory it runs in to marker and exits
// with exitCode. The directory is the point: a real cleanup command resolves
// which container stack to tear down from its working directory, so it has to
// run inside the run worktree while that directory still exists.
func recordCwdCleanupCommand(marker string, exitCode int) string {
	if runtime.GOOS == "windows" {
		return "cd>>" + marker + " & exit /b " + strconv.Itoa(exitCode)
	}
	return "pwd -P >> " + marker + "; exit " + strconv.Itoa(exitCode)
}

// cleanupRanIn reads the directory the cleanup command recorded. It is
// compared against a path resolved BEFORE teardown, because by the time the
// marker is read the worktree it names is usually gone.
func cleanupRanIn(t *testing.T, marker string) string {
	t.Helper()
	contents, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("cleanup command did not run: %v", err)
	}
	return strings.TrimSpace(string(contents))
}

func resolved(t *testing.T, path string) string {
	t.Helper()
	out, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// cleanupTestRun creates a repository, a run, and a run worktree, and returns
// the manager, the run, and the worktree directory.
func cleanupTestRun(t *testing.T, repoID string) (*RunManager, *paths.Paths, *db.DB, *db.Run, string) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closers.Quiet(database) })
	repo, head := setupTestGitRepo(t, p, database, repoID)
	run, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	wtDir := p.WorktreeDir(repo.ID, run.ID)
	if err := database.SetRunWorktreeDir(run.ID, wtDir); err != nil {
		t.Fatal(err)
	}
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), wtDir, head); err != nil {
		t.Fatal(err)
	}
	return NewRunManager(database, p, nil), p, database, run, wtDir
}

// TestRemoveRunWorktreeReleasesExternalResourcesBeforeRemoval is the
// correctness half of the cleanup hook. A container stack a run started is a
// child of the container daemon, so the process sweep cannot see it, and the
// command that could remove it resolves its target from the worktree path -
// once the directory is gone, nothing can attribute those containers to the
// run again.
func TestRemoveRunWorktreeReleasesExternalResourcesBeforeRemoval(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleanup.log")
	m, p, _, run, wtDir := cleanupTestRun(t, "cleanup-removal")
	wantDir := resolved(t, wtDir)

	m.removeRunWorktree("cleanup-removal", run.ID, p.RepoDir("cleanup-removal"), wtDir, "test", recordCwdCleanupCommand(marker, 0))

	if got := cleanupRanIn(t, marker); got != wantDir {
		t.Fatalf("cleanup ran in %q, want the run worktree %q", got, wantDir)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("worktree survived removal: %v", err)
	}
}

// TestRemoveRunWorktreeFailingCleanupStillRemovesAndKeepsTheOutcome keeps the
// hook best effort. The run's outcome was decided before teardown began, and a
// teardown script that exits non-zero must not rewrite it or strand the
// directory.
func TestRemoveRunWorktreeFailingCleanupStillRemovesAndKeepsTheOutcome(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleanup.log")
	m, p, database, run, wtDir := cleanupTestRun(t, "cleanup-failure")
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	wantDir := resolved(t, wtDir)

	m.removeRunWorktree("cleanup-failure", run.ID, p.RepoDir("cleanup-failure"), wtDir, "test", recordCwdCleanupCommand(marker, 7))

	if got := cleanupRanIn(t, marker); got != wantDir {
		t.Fatalf("cleanup ran in %q, want the run worktree %q", got, wantDir)
	}
	after, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != types.RunCompleted {
		t.Fatalf("failing cleanup changed the run outcome to %s", after.Status)
	}
	if after.Error != nil {
		t.Fatalf("failing cleanup recorded a run error: %q", *after.Error)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("worktree survived removal: %v", err)
	}
}

// TestRemoveRunWorktreeReleasesExternalResourcesUnderRetention places the call
// ahead of every retention decision. Retention preserves the index and working
// FILES so an operator can inspect or resume them; it is not a reason to leave
// a container stack running for however long that takes.
func TestRemoveRunWorktreeReleasesExternalResourcesUnderRetention(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleanup.log")
	m, p, database, run, wtDir := cleanupTestRun(t, "cleanup-retained")
	wantDir := resolved(t, wtDir)

	// Park a step on an unresolved protected-path refusal, which is what makes
	// removeRunWorktree preserve the directory.
	step, err := database.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	outcome := pipeline.ProtectedPathOutcome(&pipeline.ProtectedPathError{Path: "secrets.env", Rule: "*.env"})
	if outcome == nil {
		t.Fatal("expected a protected-path outcome")
	}
	if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1, &outcome.Findings); err != nil {
		t.Fatal(err)
	}

	m.removeRunWorktree("cleanup-retained", run.ID, p.RepoDir("cleanup-retained"), wtDir, "test", recordCwdCleanupCommand(marker, 0))

	if got := cleanupRanIn(t, marker); got != wantDir {
		t.Fatalf("cleanup ran in %q, want the run worktree %q", got, wantDir)
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("retention did not preserve the refused worktree: %v", err)
	}
}

// TestRemoveRunWorktreeReleasesExternalResourcesForAnInterruptedCIMonitor
// covers the second retention return. An interrupted CI monitor keeps its
// worktree because it may hold an unpublished repair commit, but a resumed
// monitor only polls the forge, so the stack goes regardless.
func TestRemoveRunWorktreeReleasesExternalResourcesForAnInterruptedCIMonitor(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleanup.log")
	m, p, database, run, wtDir := cleanupTestRun(t, "cleanup-ci-monitor")
	wantDir := resolved(t, wtDir)
	if _, err := database.EndActiveRunWithStatus(run.ID, types.RunCIMonitorInterrupted, "worktree holds an unpublished repair commit"); err != nil {
		t.Fatal(err)
	}

	m.removeRunWorktree("cleanup-ci-monitor", run.ID, p.RepoDir("cleanup-ci-monitor"), wtDir, "test", recordCwdCleanupCommand(marker, 0))

	if got := cleanupRanIn(t, marker); got != wantDir {
		t.Fatalf("cleanup ran in %q, want the run worktree %q", got, wantDir)
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("an interrupted ci monitor lost its worktree: %v", err)
	}
}

// TestReleaseRunResourcesSkipsAMissingWorktree covers the teardown route where
// the directory is already gone: there is nothing to resolve a target from, so
// the command is not launched at all.
func TestReleaseRunResourcesSkipsAMissingWorktree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleanup.log")
	m, _, _, run, wtDir := cleanupTestRun(t, "cleanup-missing")
	if err := os.RemoveAll(wtDir); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	m.releaseRunResources(run.ID, wtDir, recordCwdCleanupCommand(marker, 0))

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("cleanup command ran without a worktree: %v", err)
	}
	if out := logged.String(); !strings.Contains(out, "skipping run cleanup command: worktree is gone") || strings.Contains(out, "run cleanup command failed") {
		t.Fatalf("log = %q, want the missing-worktree skip and no launch attempt", out)
	}
}

// cleanupCommandConfig is trusted repo config whose commands.cleanup records
// the directory it runs in to marker.
func cleanupCommandConfig(t *testing.T, marker string) string {
	return "commands:\n  cleanup: " + testJSONString(t, recordCwdCleanupCommand(marker, 0)) + "\n"
}

// waitForCleanupMarker waits for teardown, which runs after the run row is
// already terminal, to record where the cleanup command ran.
func waitForCleanupMarker(t *testing.T, marker string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if contents, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(contents)) != "" {
			return strings.TrimSpace(string(contents))
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("cleanup command never ran at teardown")
	return ""
}

// resolvedWorktreeDir resolves a run worktree path that may already be gone by
// resolving the managed worktree root it sits under.
func resolvedWorktreeDir(t *testing.T, p *paths.Paths, wtDir string) string {
	t.Helper()
	rel, err := filepath.Rel(p.WorktreesDir(), wtDir)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(resolved(t, p.WorktreesDir()), rel)
}

// TestFinishedRunReleasesExternalResourcesFromItsTrustedConfig drives the
// run-end wiring: the trusted default branch configures commands.cleanup, and
// a run that finishes must run it in its own worktree at teardown.
func TestFinishedRunReleasesExternalResourcesFromItsTrustedConfig(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	marker := filepath.Join(t.TempDir(), "cleanup.log")
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepoWithConfig(t, p, database, "cleanup-run-end", cleanupCommandConfig(t, marker))
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	t.Cleanup(manager.Shutdown)

	runID, err := manager.startRun(t.Context(), repo, "main", head, refreshTestZeroSHA, "test", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, database, runID)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want %s", run.Status, types.RunCompleted)
	}

	got := waitForCleanupMarker(t, marker)
	if want := resolvedWorktreeDir(t, p, p.WorktreeDir(repo.ID, runID)); got != want {
		t.Fatalf("cleanup ran in %q, want the run worktree %q", got, want)
	}
}

// TestSetupFailureAfterTrustedConfigReleasesExternalResources drives the
// setup-failure wiring: once the trusted configuration has resolved, a setup
// failure that removes the worktree still runs the configured cleanup in it.
func TestSetupFailureAfterTrustedConfigReleasesExternalResources(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	marker := filepath.Join(t.TempDir(), "cleanup.log")
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepoWithConfig(t, p, database, "cleanup-setup-failure", cleanupCommandConfig(t, marker))
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	t.Cleanup(manager.Shutdown)
	manager.persistSkippedSteps = func(string, []types.StepName) error {
		return errors.New("database is locked")
	}

	if _, err := manager.startRun(t.Context(), repo, "main", head, refreshTestZeroSHA, "test",
		[]types.StepName{types.StepPush}, "", ""); err == nil {
		t.Fatal("start run should fail when the skip set cannot be persisted")
	}

	runs, err := database.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("repo has %d runs, want the one aborted run", len(runs))
	}
	if got, want := cleanupRanIn(t, marker), resolvedWorktreeDir(t, p, p.WorktreeDir(repo.ID, runs[0].ID)); got != want {
		t.Fatalf("cleanup ran in %q, want the run worktree %q", got, want)
	}
}
