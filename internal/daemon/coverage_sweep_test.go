package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestSweepOrphanCoverageDirsRemovesOnlyTerminalRunDirectories covers the
// crash/kill case cleanupRunCoverage cannot reach: a directory no
// run-completion hook will ever clean up. A directory whose run is still
// active is left standing, exactly as a parked run's worktree survives the
// orphan-worktree sweep, while a terminal run's directory is removed.
func TestSweepOrphanCoverageDirsRemovesOnlyTerminalRunDirectories(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepoWithID("repo1", "/nonexistent/work", "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}

	active, err := d.InsertRun(repo.ID, "active-branch", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	terminal, err := d.InsertRun(repo.ID, "terminal-branch", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(terminal.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}

	activeDir := p.RunCoverageDir(active.ID)
	terminalDir := p.RunCoverageDir(terminal.ID)
	for _, dir := range []string{activeDir, terminalDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "coverage.lcov"), []byte("profile"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	sweepOrphanCoverageDirs(d, p)

	if _, err := os.Stat(filepath.Join(activeDir, "coverage.lcov")); err != nil {
		t.Errorf("sweep removed the active run's coverage directory: %v", err)
	}
	if _, err := os.Stat(terminalDir); !os.IsNotExist(err) {
		t.Errorf("sweep left the terminal run's coverage directory behind: err = %v", err)
	}
}

// TestSweepOrphanCoverageDirsSweepsNothingWhenActiveRunsCannotBeRead matches
// the posture recoverableParkedRuns takes for the same read: a failed
// listing is not a picture with no active runs in it, so the sweep must
// remove nothing rather than treat every directory as orphaned.
func TestSweepOrphanCoverageDirsSweepsNothingWhenActiveRunsCannotBeRead(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepoWithID("repo1", "/nonexistent/work", "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}

	restore := breakActiveRunListing(t, p, d, repo.ID)
	defer restore()

	dir := p.RunCoverageDir("some-run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	sweepOrphanCoverageDirs(d, p)

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("sweep removed a coverage directory while the active-run listing could not be read: %v", err)
	}
}
