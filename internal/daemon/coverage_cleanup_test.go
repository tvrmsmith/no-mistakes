package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRunCleanupRemovesCoverageDirAndLeavesEvidenceAlone covers the per-run
// half of coverage ownership. Unlike evidence, coverage is dead the moment a
// run ends - nothing outside the run reads it - so cleanup removes the whole
// directory regardless of what it holds, and it must not reach into the
// unrelated evidence directory the same run may have written.
func TestRunCleanupRemovesCoverageDirAndLeavesEvidenceAlone(t *testing.T) {
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
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}

	coverageDir := p.RunCoverageDir(run.ID)
	if err := os.MkdirAll(coverageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coverageDir, "coverage.lcov"), []byte("profile"), 0o644); err != nil {
		t.Fatal(err)
	}

	evidenceDir := filepath.Join(p.EvidenceDir(), run.ID)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "screenshot.png"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewRunManager(d, p, nil)
	m.cleanupRunCoverage(run.ID)

	if _, err := os.Stat(coverageDir); !os.IsNotExist(err) {
		t.Errorf("coverage directory survived cleanup: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(evidenceDir, "screenshot.png")); err != nil {
		t.Errorf("cleanupRunCoverage touched the unrelated evidence directory: %v", err)
	}
}

// TestRunCleanupCoverageIsSafeForUnknownRuns keeps the cleanup path from ever
// being the thing that fails a finished run: a run that never wrote a
// coverage directory (e.g. it never ran a test command) has nothing to
// remove, and removing "nothing" must stay scoped to that run rather than
// reaching the shared root or a concurrent run's artifacts.
func TestRunCleanupCoverageIsSafeForUnknownRuns(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	otherDir := p.RunCoverageDir("another-live-run")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	otherProfile := filepath.Join(otherDir, "coverage.lcov")
	if err := os.WriteFile(otherProfile, []byte("profile"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootFile := filepath.Join(p.CoverageDir(), "root-level.txt")
	if err := os.WriteFile(rootFile, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewRunManager(d, p, nil)
	m.cleanupRunCoverage("run-that-never-existed")

	if _, err := os.Stat(otherProfile); err != nil {
		t.Errorf("cleanup for an unknown run removed another run's coverage: %v", err)
	}
	if _, err := os.Stat(rootFile); err != nil {
		t.Errorf("cleanup for an unknown run reached the shared coverage root: %v", err)
	}
	if _, err := os.Stat(p.RunCoverageDir("run-that-never-existed")); !os.IsNotExist(err) {
		t.Errorf("cleanup created the unknown run's coverage directory: err = %v", err)
	}
}
