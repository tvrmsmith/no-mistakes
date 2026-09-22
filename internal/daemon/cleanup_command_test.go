package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

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

	m.releaseRunResources(run.ID, wtDir, recordCwdCleanupCommand(marker, 0))

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("cleanup command ran without a worktree: %v", err)
	}
}
