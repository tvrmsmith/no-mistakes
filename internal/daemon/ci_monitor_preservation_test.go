package daemon

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ciMonitorRows is the step history of a run sitting in its CI monitor: every
// earlier step done and a running ci row holding no agent pid.
func ciMonitorRows() []*db.StepResult {
	return []*db.StepResult{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepCI, Status: types.StepStatusRunning},
	}
}

func gateParkedRows() []*db.StepResult {
	return []*db.StepResult{
		{StepName: types.StepReview, Status: types.StepStatusAwaitingApproval},
	}
}

func prURL() *string {
	url := "https://github.com/o/r/pull/7"
	return &url
}

// TestBranchContentionPrefersAPreservedCIMonitor is the startup half of the
// branch-contention rule after a CI monitor became a resume point of its own:
// a leftover active row on the same branch must not supersede the monitor the
// previous clean stop promised to re-enter.
func TestBranchContentionPrefersAPreservedCIMonitor(t *testing.T) {
	monitor := &db.Run{ID: "monitor", RepoID: "repo1", Branch: "feature", Status: types.RunRunning, PRURL: prURL()}
	leftover := &db.Run{ID: "leftover", RepoID: "repo1", Branch: "feature", Status: types.RunRunning}
	stepsOf := func(runID string) ([]*db.StepResult, error) {
		if runID == "monitor" {
			return ciMonitorRows(), nil
		}
		return []*db.StepResult{{StepName: types.StepReview, Status: types.StepStatusRunning}}, nil
	}

	contention := branchContentionOf([]*db.Run{monitor, leftover}, stepsOf)

	if contention.superseded["monitor"] {
		t.Errorf("the preserved ci monitor was superseded, want it kept")
	}
	if !contention.superseded["leftover"] {
		t.Errorf("the leftover run was not superseded, want it cleared off the branch")
	}
	if len(contention.unresolved) != 0 {
		t.Errorf("unresolved = %v, want the monitor to win the branch outright", contention.unresolved)
	}
}

// TestBranchContentionResumesNeitherOfTwoPreservedShapes keeps the fail-closed
// posture: with a gate-parked run and a CI monitor on one branch there is no
// run to prefer, so neither is superseded and the group is unresolved.
func TestBranchContentionResumesNeitherOfTwoPreservedShapes(t *testing.T) {
	monitor := &db.Run{ID: "monitor", RepoID: "repo1", Branch: "feature", Status: types.RunRunning, PRURL: prURL()}
	marker := int64(1)
	parked := &db.Run{ID: "parked", RepoID: "repo1", Branch: "feature", Status: types.RunRunning, AwaitingAgentSince: &marker}
	stepsOf := func(runID string) ([]*db.StepResult, error) {
		if runID == "monitor" {
			return ciMonitorRows(), nil
		}
		return gateParkedRows(), nil
	}

	contention := branchContentionOf([]*db.Run{monitor, parked}, stepsOf)

	if len(contention.superseded) != 0 {
		t.Errorf("superseded = %v, want nothing destroyed when two preserved runs contend", contention.superseded)
	}
	if len(contention.unresolved) != 2 {
		t.Errorf("unresolved = %v, want both runs left for an operator", contention.unresolved)
	}
}

// TestLivePushStillSupersedesACIMonitor is the other half of the same rule: a
// push arriving while CI is being monitored is how an author corrects a red
// check, and the branch it moved makes what the monitor polls stale, so the
// live path admits only gates as preserved.
func TestLivePushStillSupersedesACIMonitor(t *testing.T) {
	monitor := &db.Run{ID: "monitor", RepoID: "repo1", Branch: "feature", Status: types.RunRunning, PRURL: prURL()}
	stepsOf := func(string) ([]*db.StepResult, error) { return ciMonitorRows(), nil }

	if kept := preservedBranchRuns([]*db.Run{monitor}, stepsOf, preservedGatesOnly); len(kept) != 0 {
		t.Errorf("live push path kept %d run(s), want a ci monitor to be supersedable", len(kept))
	}
	if kept := preservedBranchRuns([]*db.Run{monitor}, stepsOf, preservedGatesAndCIMonitors); len(kept) != 1 {
		t.Errorf("startup path kept %d run(s), want the ci monitor preferred", len(kept))
	}
}

// TestBranchContentionStepReadFailureLeavesACIMonitorUnproven covers the shape
// that cannot corroborate itself with the awaiting-agent marker, because a CI
// monitor never sets one. A transient step read failure must leave its claim
// unproven rather than refuted, so the group defers to an operator instead of
// superseding a run whose worktree can hold an unpushed repair commit.
func TestBranchContentionStepReadFailureLeavesACIMonitorUnproven(t *testing.T) {
	monitor := &db.Run{ID: "monitor", RepoID: "repo1", Branch: "feature", Status: types.RunRunning, PRURL: prURL()}
	other := &db.Run{ID: "other", RepoID: "repo1", Branch: "feature", Status: types.RunRunning}
	stepsOf := func(runID string) ([]*db.StepResult, error) {
		if runID == "monitor" {
			return nil, errors.New("database is locked")
		}
		return []*db.StepResult{{StepName: types.StepReview, Status: types.StepStatusRunning}}, nil
	}

	contention := branchContentionOf([]*db.Run{monitor, other}, stepsOf)

	if contention.superseded["monitor"] {
		t.Errorf("an unreadable ci monitor was superseded, want its claim treated as unproven")
	}
	if !contention.superseded["other"] {
		t.Errorf("the competing run was not superseded, want the sole candidate to win the branch")
	}
}

// TestBranchContentionStepReadFailureStillRefutesARunWithNoPreservedShape keeps
// the fallback from admitting everything: a run with no marker and no PR URL
// has no preserved shape left to establish once its step rows are unreadable.
func TestBranchContentionStepReadFailureStillRefutesARunWithNoPreservedShape(t *testing.T) {
	bare := &db.Run{ID: "bare", RepoID: "repo1", Branch: "feature", Status: types.RunRunning}
	stepsOf := func(string) ([]*db.StepResult, error) { return nil, errors.New("database is locked") }

	if kept := preservedBranchRuns([]*db.Run{bare}, stepsOf, preservedGatesAndCIMonitors); len(kept) != 0 {
		t.Errorf("kept %d run(s), want none: nothing establishes a preserved shape here", len(kept))
	}
}

// TestBranchContentionStepReadFailureStillSupersedesACIMonitorForALivePush is
// the live-push half of the same fallback. Admitting a CI monitor there would
// refuse an author's corrective push every time a step read transiently fails,
// which is the hazard preservedGatesOnly exists to avoid.
func TestBranchContentionStepReadFailureStillSupersedesACIMonitorForALivePush(t *testing.T) {
	monitor := &db.Run{ID: "monitor", RepoID: "repo1", Branch: "feature", Status: types.RunRunning, PRURL: prURL()}
	stepsOf := func(string) ([]*db.StepResult, error) { return nil, errors.New("database is locked") }

	if kept := preservedBranchRuns([]*db.Run{monitor}, stepsOf, preservedGatesOnly); len(kept) != 0 {
		t.Errorf("live push path kept %d run(s), want an unreadable ci monitor to stay supersedable", len(kept))
	}
}

// TestBranchContentionStepReadFailureRefutesATerminalRunHoldingAPRURL keeps the
// fallback tied to a live run: a run that already ended is no longer a monitor
// anything could re-enter, PR URL or not.
func TestBranchContentionStepReadFailureRefutesATerminalRunHoldingAPRURL(t *testing.T) {
	ended := &db.Run{ID: "ended", RepoID: "repo1", Branch: "feature", Status: types.RunFailed, PRURL: prURL()}
	stepsOf := func(string) ([]*db.StepResult, error) { return nil, errors.New("database is locked") }

	if kept := preservedBranchRuns([]*db.Run{ended}, stepsOf, preservedGatesAndCIMonitors); len(kept) != 0 {
		t.Errorf("kept %d run(s), want none: a terminal run is not a live monitor", len(kept))
	}
}

// TestAbortOfADeferredRunReportsAFailedWrite pins that an abort the daemon
// could not carry out is reported as one. A deferred run has no goroutine, so
// its cancel ends the row by writing it; swallowing that write's failure would
// tell the operator the run was terminated while it stayed running, with its
// manager entry gone and no way to try again.
func TestAbortOfADeferredRunReportsAFailedWrite(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepoWithID("repo1", filepath.Join(t.TempDir(), "src"), "https://github.com/o/r", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	m.registerDeferredRun(run)

	release := blockTerminalRunWrites(t, p.DB())
	if err := m.HandleCancel(run.ID); err == nil {
		t.Fatal("HandleCancel() = nil, want the failed write reported to the operator")
	}

	m.mu.Lock()
	_, stillOwned := m.cancels[run.ID]
	m.mu.Unlock()
	if !stillOwned {
		t.Fatal("the deferred run lost its manager entry after a failed abort, leaving the row unreachable")
	}

	// The write failure was transient, so the operator's second attempt has to
	// go through. A sync.Once around the teardown would leave this run
	// permanently un-abortable while the first assertion above still passed.
	release()
	if err := m.HandleCancel(run.ID); err != nil {
		t.Fatalf("second HandleCancel() = %v, want the retry to succeed", err)
	}
	ended, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ended.Status != types.RunCancelled {
		t.Errorf("run status = %s, want %s", ended.Status, types.RunCancelled)
	}
	m.mu.Lock()
	_, stillOwnedAfter := m.cancels[run.ID]
	m.mu.Unlock()
	if stillOwnedAfter {
		t.Error("the manager still owns a run it successfully ended")
	}
}

// blockTerminalRunWrites makes every attempt to write a run's terminal status
// fail, and returns the function that lifts it again. A schema trigger is
// visible to every connection, unlike a TEMP one, so the manager's own handle
// sees it; dropping it restores an ordinary working database, which is what
// makes the failure it models transient.
func blockTerminalRunWrites(t *testing.T, dbPath string) func() {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER block_run_status BEFORE UPDATE OF status ON runs BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() { raw.Close() })
	return func() {
		if released {
			return
		}
		released = true
		if _, err := raw.Exec(`DROP TRIGGER block_run_status`); err != nil {
			t.Fatal(err)
		}
	}
}

// TestALivePushIsRefusedWhenADeferredRunOnItsBranchCannotBeEnded pins the other
// consequence of a failed terminal write. A deferred run that could not be
// ended still owns its branch, so starting a newer push alongside it would put
// two worktrees behind one remote branch, driving push and PR from both.
func TestALivePushIsRefusedWhenADeferredRunOnItsBranchCannotBeEnded(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepoWithID("repo1", filepath.Join(t.TempDir(), "src"), "https://github.com/o/r", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPRURL(run.ID, "https://github.com/o/r/pull/7"); err != nil {
		t.Fatal(err)
	}
	ciRow, err := d.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(ciRow.ID); err != nil {
		t.Fatal(err)
	}
	live, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	m.registerDeferredRun(live)
	blockTerminalRunWrites(t, p.DB())

	err = m.cancelActiveRuns(repo.ID, "feature")

	if err == nil {
		t.Fatal("cancelActiveRuns() = nil, want the newer push refused while the branch is still owned")
	}
	if !strings.Contains(err.Error(), "could not supersede run "+run.ID) {
		t.Errorf("error = %v, want it to name the run that still owns the branch", err)
	}
	still, getErr := d.GetRun(run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if still.Status != types.RunRunning {
		t.Errorf("run status = %s, want %s: nothing ended it", still.Status, types.RunRunning)
	}
}

// TestInterruptedCIMonitorKeepsItsWorktreeAtStopTime covers the commit-then-stop
// window: steps.commitRepair commits a repair before the run records the new
// head, so a clean stop landing there finds work no push and no run row knows
// about. The run ends as an interrupted monitor rather than a failure, and the
// stop that ended it does not also delete the only copy of that commit.
func TestInterruptedCIMonitorKeepsItsWorktreeAtStopTime(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, headSHA := setupTestGitRepo(t, p, d, "repo1")
	run, err := d.InsertRun(repo.ID, "feature", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	wtDir := p.WorktreeDir(repo.ID, run.ID)
	gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", wtDir, headSHA)
	if _, err := d.EndActiveRunWithStatus(run.ID, types.RunCIMonitorInterrupted, "worktree holds an unpublished repair commit"); err != nil {
		t.Fatal(err)
	}

	NewRunManager(d, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), wtDir, "test")

	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("an interrupted ci monitor lost its worktree at stop time: %v", err)
	}
}

// TestAFailedRunStillLosesItsWorktreeAtStopTime is the other side of the same
// sparing rule. A CI refusal on a run that never opened a PR ends as an
// ordinary failure, and the sparing must not widen to cover it: nothing was
// published, the checkout is clean, and leaving it behind hands the operator a
// directory to reap for no gain.
func TestAFailedRunStillLosesItsWorktreeAtStopTime(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, headSHA := setupTestGitRepo(t, p, d, "repo1")
	run, err := d.InsertRun(repo.ID, "feature", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	wtDir := p.WorktreeDir(repo.ID, run.ID)
	gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", wtDir, headSHA)
	if _, err := d.EndActiveRunWithStatus(run.ID, types.RunFailed, "the run has no PR URL for a resumed monitor to poll"); err != nil {
		t.Fatal(err)
	}

	NewRunManager(d, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), wtDir, "test")

	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(worktree) error = %v, want the failed run's checkout reclaimed", err)
	}
}
