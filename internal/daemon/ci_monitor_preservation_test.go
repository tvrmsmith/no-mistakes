package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

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

	// A database that cannot be written is the reachable shape of the failure:
	// the abort has nowhere to record the run's terminal state.
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.HandleCancel(run.ID); err == nil {
		t.Fatal("HandleCancel() = nil, want the failed write reported to the operator")
	}

	m.mu.Lock()
	_, stillOwned := m.cancels[run.ID]
	m.mu.Unlock()
	if !stillOwned {
		t.Error("the deferred run lost its manager entry after a failed abort, leaving the row unreachable")
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
