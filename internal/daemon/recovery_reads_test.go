package daemon

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// parkedRunForRecovery leaves a real gate-parked run behind a stopped daemon,
// exactly as a clean stop does, so a recovery pass can be driven over it.
func parkedRunForRecovery(t *testing.T, repoID string) (*paths.Paths, *db.DB, *db.Repo, string, func() []pipeline.Step) {
	t.Helper()

	steps := func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	}
	instance := startTestDaemonInstance(t, steps)
	p, d := instance.paths, instance.db
	repo, runID := startParkedRun(t, p, d, repoID, nil)
	if err := instance.stopAndWait(t); err != nil {
		t.Fatalf("daemon exited with error: %v", err)
	}
	return p, d, repo, runID, steps
}

// hideTable renames a table out from under the daemon's open database handle,
// so every read that touches it fails for real instead of through a stub.
func hideTable(t *testing.T, p *paths.Paths, table string) {
	t.Helper()

	conn, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec("ALTER TABLE " + table + " RENAME TO " + table + "_hidden"); err != nil {
		t.Fatal(err)
	}
}

// breakActiveRunListing inserts an active run row whose parked_ms holds text,
// so scanning it fails and the whole active-run listing errors out while every
// other row and every write still works. It returns a function that removes the
// row again.
func breakActiveRunListing(t *testing.T, p *paths.Paths, d *db.DB, repoID string) func() {
	t.Helper()

	conn, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(
		`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, parked_ms, created_at, updated_at)
		 VALUES ('unscannable-run', ?, 'unscannable', 'a', 'b', ?, 'not-a-number', 1, 1)`,
		repoID, types.RunRunning,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetActiveRuns(); err == nil {
		t.Fatal("precondition failed: the active-run listing still succeeds")
	}
	return func() {
		conn, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Exec(`DELETE FROM runs WHERE id = 'unscannable-run'`); err != nil {
			t.Fatal(err)
		}
	}
}

// TestStartupDefersEveryPreservedRunWhenTheActiveRunListCannotBeRead covers the
// read every other recovery read depends on. A failed listing is not a picture
// with no parked runs in it, so a start that cannot read it must run no blanket
// crash sweep and no worktree cleanup at all: otherwise one failed query stamps
// every preserved run "daemon crashed during execution" and reaps the worktrees
// holding their unpushed commits.
func TestStartupDefersEveryPreservedRunWhenTheActiveRunListCannotBeRead(t *testing.T) {
	steps := func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	}
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, runID := startParkedRun(t, p, d, "recovery-active-list-repo", nil)
	parked := waitForRunAwaitingAgent(t, d, runID)
	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	// A second stale row the blanket sweep would certainly have taken, so the
	// sweep's absence is observable and not inferred from the parked run alone.
	crashed, err := d.InsertRun(repo.ID, "other-branch", parked.HeadSHA, parked.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(crashed.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	restore := breakActiveRunListing(t, p, d, repo.ID)

	blindManager := NewRunManager(d, p, steps)
	t.Cleanup(blindManager.Shutdown)
	recoverOnStartup(d, p, blindManager)

	if swept, err := d.GetRun(crashed.ID); err != nil {
		t.Fatal(err)
	} else if swept.Status != types.RunRunning {
		t.Fatalf("a blanket crash sweep ran over an unreadable listing: run status = %s (error %v)", swept.Status, swept.Error)
	}

	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunRunning {
		t.Fatalf("run status = %s (error %v), want it left %s", run.Status, run.Error, types.RunRunning)
	}
	if run.AwaitingAgentSince == nil || *run.AwaitingAgentSince != *parked.AwaitingAgentSince {
		t.Fatalf("awaiting_agent_since = %v, want it left %d", run.AwaitingAgentSince, *parked.AwaitingAgentSince)
	}
	if run.Error != nil {
		t.Fatalf("start recorded error %q on a run it never listed, want none", *run.Error)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil {
		t.Fatalf("start reaped the preserved worktree: %v", err)
	}
	if row := findStepRow(t, d, runID, types.StepReview); row == nil || row.Status != types.StepStatusAwaitingApproval {
		t.Fatalf("gate step row = %v, want %s", row, types.StepStatusAwaitingApproval)
	}

	restore()
	restartTestDaemonInstance(t, p, d, steps)
	approveWhenResumed(t, p, runID, types.StepReview)

	completed := waitForRunTerminalState(t, d, runID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("run resumed once the listing was readable = %s (error %v), want %s",
			completed.Status, completed.Error, types.RunCompleted)
	}
}

// assertRecoveryDefers drives a recovery pass and requires it to leave the
// parked run exactly as the stop left it: deferred to a later start, still
// running with its marker and no recorded error, and still holding the
// worktree that carries its unpushed pipeline commits.
func assertRecoveryDefers(t *testing.T, p *paths.Paths, d *db.DB, repo *db.Repo, runID string, steps func() []pipeline.Step) {
	t.Helper()

	manager := NewRunManager(d, p, steps)
	t.Cleanup(manager.Shutdown)

	plans, deferred, err := manager.recoverableParkedRuns(context.Background())
	if err != nil {
		t.Fatalf("recovery could not list active runs: %v", err)
	}
	if len(plans) != 0 {
		t.Fatalf("recovery planned %d resumes, want none while the read fails", len(plans))
	}
	if len(deferred) != 1 || deferred[0] != runID {
		t.Fatalf("deferred runs = %v, want [%s]", deferred, runID)
	}

	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunRunning {
		t.Fatalf("run status = %s (error %v), want it left %s", run.Status, run.Error, types.RunRunning)
	}
	if run.AwaitingAgentSince == nil {
		t.Fatal("recovery cleared the awaiting-agent marker of a run it could not read")
	}
	if run.Error != nil {
		t.Fatalf("recovery recorded error %q, want none", *run.Error)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil {
		t.Fatalf("recovery gave up the preserved worktree: %v", err)
	}
}

// TestRecoveryDefersWhenTheRepoRowCannotBeRead proves a failed database read is
// never adverse evidence about the run. The repo row cannot be read right now,
// which says nothing about whether the run is resumable, so recovery waits for
// a later start instead of spending the preservation promise.
func TestRecoveryDefersWhenTheRepoRowCannotBeRead(t *testing.T) {
	p, d, repo, runID, steps := parkedRunForRecovery(t, "recovery-repo-read-repo")

	hideTable(t, p, "repos")

	assertRecoveryDefers(t, p, d, repo, runID, steps)
}

// TestRecoveryDefersWhenTheStepRowsCannotBeRead covers the reads behind the
// resume preconditions: the gate step rows decide whether the run is resumable,
// so a read that never completed cannot be the reason it is terminally failed.
func TestRecoveryDefersWhenTheStepRowsCannotBeRead(t *testing.T) {
	p, d, repo, runID, steps := parkedRunForRecovery(t, "recovery-step-read-repo")

	hideTable(t, p, "step_results")

	assertRecoveryDefers(t, p, d, repo, runID, steps)
}

// TestRecoveryDefersWhenTheSessionRowsCannotBeRead covers the review-loop
// session read, which session_reuse reaches on nearly every recovery. Whether
// the run's sessions can still be resumed is unknown while the rows cannot be
// read, and an unknown is not adverse evidence.
func TestRecoveryDefersWhenTheSessionRowsCannotBeRead(t *testing.T) {
	p, d, repo, runID, steps := parkedRunForRecovery(t, "recovery-session-read-repo")

	// The session read sits behind agent resolution, so keep the agent
	// resolvable and let the hidden rows be the only thing that fails.
	t.Setenv("NM_DEMO", "1")
	hideTable(t, p, "run_agent_sessions")

	assertRecoveryDefers(t, p, d, repo, runID, steps)
}

// parkedTwinRun builds a second run that is parked at a gate on the same branch
// as an existing one, so a start faces two runs with an equal claim to it.
func parkedTwinRun(t *testing.T, d *db.DB, of *db.Run) *db.Run {
	t.Helper()

	twin, err := d.InsertRun(of.RepoID, of.Branch, of.HeadSHA, of.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(twin.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunAwaitingAgent(twin.ID); err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(twin.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatus(step.ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatal(err)
	}
	return twin
}

// TestAmbiguousContendedBranchResumesNeitherRun proves an unresolved branch is
// not left to sort itself out. Two runs with an equal parked claim have no
// winner to prefer, and resuming both would drive push, force-push and PR at
// one remote branch from two worktrees at two head SHAs - the data-loss shape
// this pipeline refuses on principle. Neither run resumes, both keep their
// worktrees, and an operator resolves it.
func TestAmbiguousContendedBranchResumesNeitherRun(t *testing.T) {
	p, d, repo, runID, steps := parkedRunForRecovery(t, "ambiguous-branch-repo")

	// Keep the parked run fully resumable, so the branch it contends for is the
	// only reason recovery leaves it alone.
	t.Setenv("NM_DEMO", "1")

	parked, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	twin := parkedTwinRun(t, d, parked)
	twinWorktree := p.WorktreeDir(repo.ID, twin.ID)
	if err := os.MkdirAll(twinWorktree, 0o755); err != nil {
		t.Fatal(err)
	}

	manager := NewRunManager(d, p, steps)
	t.Cleanup(manager.Shutdown)

	plans, deferred, err := manager.recoverableParkedRuns(context.Background())
	if err != nil {
		t.Fatalf("recovery could not list active runs: %v", err)
	}
	if len(plans) != 0 {
		t.Fatalf("recovery planned %d resumes for a branch two runs contend for, want none", len(plans))
	}
	if len(deferred) != 2 {
		t.Fatalf("deferred runs = %v, want both contending runs", deferred)
	}
	for _, id := range []string{runID, twin.ID} {
		found := false
		for _, got := range deferred {
			if got == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("deferred runs = %v, want %s among them", deferred, id)
		}
	}

	manager.mu.Lock()
	executors := len(manager.executors)
	manager.mu.Unlock()
	if executors != 0 {
		t.Fatalf("recovery registered %d executors, want none: an executor is what pushes the contended branch", executors)
	}

	for _, id := range []string{runID, twin.ID} {
		run, err := d.GetRun(id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != types.RunRunning {
			t.Fatalf("run %s status = %s (error %v), want it left %s", id, run.Status, run.Error, types.RunRunning)
		}
		if run.AwaitingAgentSince == nil {
			t.Fatalf("recovery cleared the awaiting-agent marker of run %s", id)
		}
		if run.Error != nil {
			t.Fatalf("recovery recorded error %q on run %s, want none", *run.Error, id)
		}
		if row := findStepRow(t, d, id, types.StepReview); row == nil || row.Status != types.StepStatusAwaitingApproval {
			t.Fatalf("gate step row of run %s = %v, want %s", id, row, types.StepStatusAwaitingApproval)
		}
		if _, err := os.Stat(p.WorktreeDir(repo.ID, id)); err != nil {
			t.Fatalf("recovery gave up the worktree of run %s: %v", id, err)
		}
	}
}

// assertResumeEntryDefers plans a real resume, then breaks a read the Resume
// entry performs, and requires the same deferral the planning side already
// makes. The classification has to hold across that seam: a read that did not
// complete says nothing about the run, whichever side of Resume it fails on.
func assertResumeEntryDefers(t *testing.T, p *paths.Paths, d *db.DB, repo *db.Repo, runID string, steps func() []pipeline.Step, table string) {
	t.Helper()

	manager := NewRunManager(d, p, steps)
	t.Cleanup(manager.Shutdown)

	plans, deferred, err := manager.recoverableParkedRuns(context.Background())
	if err != nil {
		t.Fatalf("recovery could not list active runs: %v", err)
	}
	if len(plans) != 1 || len(deferred) != 0 {
		t.Fatalf("precondition failed: %d plans and %d deferred runs, want the run planned for resume", len(plans), len(deferred))
	}

	hideTable(t, p, table)

	manager.resumeRecoveredRuns(plans)
	manager.wg.Wait()

	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunRunning {
		t.Fatalf("run status = %s (error %v), want it left %s", run.Status, run.Error, types.RunRunning)
	}
	if run.AwaitingAgentSince == nil {
		t.Fatal("resume cleared the awaiting-agent marker of a run it could not read")
	}
	if run.Error != nil {
		t.Fatalf("resume recorded error %q, want none", *run.Error)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil {
		t.Fatalf("resume gave up the preserved worktree: %v", err)
	}
}

// TestResumeEntryDefersWhenTheStepRowsCannotBeRead covers the step read Resume
// performs at entry, after planning already succeeded. Failing there used to
// reach the terminal branch and reap the worktree holding the run's unpushed
// pipeline commits.
func TestResumeEntryDefersWhenTheStepRowsCannotBeRead(t *testing.T) {
	p, d, repo, runID, steps := parkedRunForRecovery(t, "resume-entry-step-read-repo")

	t.Setenv("NM_DEMO", "1")
	assertResumeEntryDefers(t, p, d, repo, runID, steps, "step_results")
}

// TestResumeEntryDefersWhenTheRoundRowsCannotBeRead covers the second read of
// the same gate re-check, so the deferral is a property of the seam rather than
// of one query behind it.
func TestResumeEntryDefersWhenTheRoundRowsCannotBeRead(t *testing.T) {
	p, d, repo, runID, steps := parkedRunForRecovery(t, "resume-entry-round-read-repo")

	t.Setenv("NM_DEMO", "1")
	assertResumeEntryDefers(t, p, d, repo, runID, steps, "step_rounds")
}

// TestRecoveryDefersWhenTheWorktreeGitReadFails covers the git side of the same
// divider. With no git on PATH the worktree's head and its gate common dir
// cannot be read at all, which is a fact about this moment and not about the
// run, so the worktree must survive to be read on a later start.
func TestRecoveryDefersWhenTheWorktreeGitReadFails(t *testing.T) {
	p, d, repo, runID, steps := parkedRunForRecovery(t, "recovery-git-read-repo")

	t.Setenv("PATH", t.TempDir())

	assertRecoveryDefers(t, p, d, repo, runID, steps)
}
