package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle/lifecycletest"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	pipelinesteps "github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// waitForRunAwaitingAgent polls until the run row records a parked gate, or
// fails the test if that never happens.
func waitForRunAwaitingAgent(t *testing.T, d *db.DB, runID string) *db.Run {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.AwaitingAgentSince != nil {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run never reached awaiting_agent")
	return nil
}

// waitForStepStatus polls until one step row of a run reaches a status, or
// fails the test.
func waitForStepStatus(t *testing.T, d *db.DB, runID string, step types.StepName, status types.StepStatus) *db.StepResult {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if row := findStepRow(t, d, runID, step); row != nil && row.Status == status {
			return row
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("step %s never reached %s", step, status)
	return nil
}

func findStepRow(t *testing.T, d *db.DB, runID string, step types.StepName) *db.StepResult {
	t.Helper()

	rows, err := d.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.StepName == step {
			return row
		}
	}
	return nil
}

// startParkedRun pushes to a gate repo and waits until the resulting run is
// parked at its first approval gate.
func startParkedRun(t *testing.T, p *paths.Paths, d *db.DB, repoID string, skipSteps []types.StepName) (*db.Repo, string) {
	t.Helper()

	repo, headSHA := setupTestGitRepo(t, p, d, repoID)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:      p.RepoDir(repoID),
		Ref:       "refs/heads/main",
		Old:       "0000000000000000000000000000000000000000",
		New:       headSHA,
		SkipSteps: skipSteps,
	}, &pushResult)
	if err != nil {
		t.Fatal(err)
	}

	waitForRunAwaitingAgent(t, d, pushResult.RunID)
	return repo, pushResult.RunID
}

// approveWhenResumed retries an approval until the resumed run's executor is
// registered and accepts it.
func approveWhenResumed(t *testing.T, p *paths.Paths, runID string, step types.StepName) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for {
		if time.Now().After(deadline) {
			t.Fatalf("resumed gate never accepted an approval: last error %v", lastErr)
		}
		c, err := ipc.Dial(p.Socket())
		if err == nil {
			var response ipc.RespondResult
			err = c.Call(ipc.MethodRespond, &ipc.RespondParams{
				RunID:  runID,
				Step:   step,
				Action: types.ActionApprove,
			}, &response)
			_ = c.Close()
			if err == nil {
				return
			}
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCleanShutdownLeavesGateParkedRunResumable verifies that stopping the
// daemon while a run is parked at a review gate preserves the run row, its
// worktree, and the gate step so the run can be resumed later, instead of
// destroying that state the way a mid-step cancellation does.
func TestCleanShutdownLeavesGateParkedRunResumable(t *testing.T) {
	daemon := startTestDaemonInstance(t, func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	})
	p, d := daemon.paths, daemon.db

	repo, runID := startParkedRun(t, p, d, "shutdown-park-repo", nil)

	// A live value on the gate step row proves the shutdown preserves the row
	// as it stands instead of rewriting or clearing it. It has to be a field a
	// resumable gate may carry: an agent PID would make the row unresumable,
	// which is the opposite of what this test claims.
	parkedStep := findStepRow(t, d, runID, types.StepReview)
	if parkedStep == nil {
		t.Fatal("review step row not found before shutdown")
	}
	const activity = "agent parked at the review gate"
	if err := d.SetStepAgentActivity(parkedStep.ID, activity, nil); err != nil {
		t.Fatal(err)
	}

	if err := daemon.stopAndWait(t); err != nil {
		t.Fatalf("daemon exited with error: %v", err)
	}

	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunRunning {
		t.Fatalf("run status = %s, want %s", run.Status, types.RunRunning)
	}
	if run.AwaitingAgentSince == nil {
		t.Fatal("run lost its awaiting_agent marker on clean shutdown")
	}
	if run.Error != nil {
		t.Fatalf("run error = %q, want nil", *run.Error)
	}

	worktree := p.WorktreeDir(repo.ID, runID)
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree removed on clean shutdown: %v", err)
	}

	reviewStep := findStepRow(t, d, runID, types.StepReview)
	if reviewStep == nil {
		t.Fatal("review step row not found")
	}
	if reviewStep.Status != types.StepStatusAwaitingApproval {
		t.Fatalf("review step status = %s, want %s", reviewStep.Status, types.StepStatusAwaitingApproval)
	}
	if reviewStep.LastActivity == nil || *reviewStep.LastActivity != activity {
		t.Fatalf("review step last activity = %v, want preserved %q", reviewStep.LastActivity, activity)
	}
	// The claim the test's name makes is resumability, so assert it with the
	// predicate the next daemon start actually applies rather than with a field
	// nothing in this run ever writes.
	if err := pipeline.ValidateRecoveredRun(d, run, []pipeline.Step{&mockApprovalStep{name: types.StepReview}}); err != nil {
		t.Fatalf("preserved run is not resumable: %v", err)
	}
}

// TestStartRunPersistsItsStepPlan proves the evidence the lifecycle guard
// consumes is actually produced: starting a real run records the ordered step
// plan of the factory that run executes, so a guard comparing it against the
// installed binary's layout sees a match rather than an unrecorded plan.
func TestStartRunPersistsItsStepPlan(t *testing.T) {
	plan := []types.StepName{types.StepReview, types.StepTest}
	daemon := startTestDaemonInstance(t, func() []pipeline.Step {
		return []pipeline.Step{
			&mockApprovalStep{name: types.StepReview},
			&mockApprovalStep{name: types.StepTest},
		}
	})
	p, d := daemon.paths, daemon.db

	_, runID := startParkedRun(t, p, d, "step-plan-repo", nil)

	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.StepPlan) != len(plan) {
		t.Fatalf("run step plan = %v, want %v", run.StepPlan, plan)
	}
	for i := range plan {
		if run.StepPlan[i] != plan[i] {
			t.Fatalf("run step plan = %v, want %v", run.StepPlan, plan)
		}
	}
}

// TestDefaultStepFactoryMatchesTheGuardStepPlan closes the loop the guard
// depends on, through the seam where the two sides could genuinely diverge: a
// run parked under the layout the daemon's own default factory produces is
// driven through the live guard, which reads the layout the lifecycle commands
// install. A divergence in either the recorded plan or the step rows recovery
// validates costs every parked run its exemption, so the run must come back
// preserved rather than blocking.
func TestDefaultStepFactoryMatchesTheGuardStepPlan(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	daemonFactoryPlan := NewRunManager(nil, nil, nil).steps()
	lifecycletest.SeedResumableParkedRun(t, p, "/tmp/default-factory-project", "feature", daemonFactoryPlan)

	decision, err := lifecycle.Decide(p, pipelinesteps.AllSteps(), lifecycle.SameBinary)
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.Parked) != 1 || len(decision.Blocking) != 0 {
		t.Fatalf("guard read the default factory's plan as %d parked / %d blocking, want 1 / 0",
			len(decision.Parked), len(decision.Blocking))
	}
	if !strings.Contains(decision.ParkedNotice(), "will be preserved and resumed") {
		t.Fatalf("guard notice = %q, want the preservation promise", decision.ParkedNotice())
	}
}

// TestNextDaemonStartResumesRunPreservedByCleanStop proves the run left
// parked by a clean shutdown is not merely intact but actually resumable: a
// fresh daemon over the same NM_HOME picks it up and drives it to completion
// once the gate is approved.
func TestNextDaemonStartResumesRunPreservedByCleanStop(t *testing.T) {
	steps := func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	}
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	_, runID := startParkedRun(t, p, d, "shutdown-park-resume-repo", nil)

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	restartTestDaemonInstance(t, p, d, steps)

	approveWhenResumed(t, p, runID, types.StepReview)

	completed := waitForRunTerminalState(t, d, runID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("resumed run status = %s, want %s", completed.Status, types.RunCompleted)
	}
	if completed.AwaitingAgentSince != nil {
		t.Fatal("resumed run remained parked after approval")
	}
	if completed.ReviewApprovedHeadSHA == nil || *completed.ReviewApprovedHeadSHA != completed.HeadSHA {
		t.Fatalf("resumed run review_approved_head_sha = %v, want %s", completed.ReviewApprovedHeadSHA, completed.HeadSHA)
	}
}

// TestSecondCleanStopPreservesRunParkedAtALaterGate proves preservation is not
// a one-shot property of the first stop: a run resumed from a preserved park,
// then parked again at a later gate, survives a second clean stop with its
// worktree, running status and awaiting-agent marker intact.
func TestSecondCleanStopPreservesRunParkedAtALaterGate(t *testing.T) {
	steps := func() []pipeline.Step {
		return []pipeline.Step{
			&mockApprovalStep{name: types.StepReview},
			&mockApprovalStep{name: types.StepTest},
		}
	}
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, runID := startParkedRun(t, p, d, "shutdown-park-twice-repo", nil)

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	second := restartTestDaemonInstance(t, p, d, steps)

	approveWhenResumed(t, p, runID, types.StepReview)
	waitForStepStatus(t, d, runID, types.StepTest, types.StepStatusAwaitingApproval)
	reparked := waitForRunAwaitingAgent(t, d, runID)

	if err := second.stopAndWait(t); err != nil {
		t.Fatalf("second daemon exited with error: %v", err)
	}

	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunRunning {
		t.Fatalf("run status after second stop = %s, want %s", run.Status, types.RunRunning)
	}
	if run.AwaitingAgentSince == nil || *run.AwaitingAgentSince != *reparked.AwaitingAgentSince {
		t.Fatalf("awaiting_agent_since after second stop = %v, want %d", run.AwaitingAgentSince, *reparked.AwaitingAgentSince)
	}
	if run.Error != nil {
		t.Fatalf("run error = %q, want nil", *run.Error)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil {
		t.Fatalf("worktree removed on second clean shutdown: %v", err)
	}
	if row := findStepRow(t, d, runID, types.StepTest); row == nil || row.Status != types.StepStatusAwaitingApproval {
		t.Fatalf("test step row = %v, want %s", row, types.StepStatusAwaitingApproval)
	}
}

// TestResumedRunStillHonorsItsRequestedSkipSet proves the skip set survives a
// stop: a run started with --skip must not run the skipped step just because a
// restart resumed it from its gate.
func TestResumedRunStillHonorsItsRequestedSkipSet(t *testing.T) {
	skipped := &mockPassStep{name: types.StepTest}
	steps := func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}, skipped}
	}
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	_, runID := startParkedRun(t, p, d, "shutdown-park-skip-repo", []types.StepName{types.StepTest})

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	restartTestDaemonInstance(t, p, d, steps)
	approveWhenResumed(t, p, runID, types.StepReview)

	completed := waitForRunTerminalState(t, d, runID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("resumed run status = %s, want %s", completed.Status, types.RunCompleted)
	}
	if got := skipped.execCnt.Load(); got != 0 {
		t.Fatalf("skipped step executed %d times after resume, want 0", got)
	}
	row := findStepRow(t, d, runID, types.StepTest)
	if row == nil || row.Status != types.StepStatusSkipped {
		t.Fatalf("test step row = %v, want %s", row, types.StepStatusSkipped)
	}
}

// TestUnresumableParkedRunRecordsWhyItCouldNotResume proves an operator is
// told the real reason a preserved run did not come back, instead of the
// blanket crash message a generic stale-run sweep would stamp on it.
func TestUnresumableParkedRunRecordsWhyItCouldNotResume(t *testing.T) {
	steps := func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	}
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, runID := startParkedRun(t, p, d, "shutdown-park-broken-repo", nil)

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	// Destroy the one precondition recovery cannot work around.
	if err := os.RemoveAll(p.WorktreeDir(repo.ID, runID)); err != nil {
		t.Fatal(err)
	}

	restartTestDaemonInstance(t, p, d, steps)

	run := waitForRunTerminalState(t, d, runID)
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %s, want %s", run.Status, types.RunFailed)
	}
	if run.Error == nil {
		t.Fatal("failed run recorded no error")
	}
	if *run.Error == "daemon crashed during execution" {
		t.Fatalf("run error = %q, want the concrete reason the parked run could not resume", *run.Error)
	}
	if !strings.Contains(*run.Error, "parked") || !strings.Contains(*run.Error, "worktree") {
		t.Fatalf("run error = %q, want it to name the parked-run resume failure and its cause", *run.Error)
	}
}

// TestCleanShutdownStillFailsRunCancelledMidStep verifies the fix is scoped
// to gate-parked runs: a run cancelled mid-step by the same clean shutdown
// still fails, and its worktree is still cleaned up, exactly as before.
func TestCleanShutdownStillFailsRunCancelledMidStep(t *testing.T) {
	started := make(chan struct{})
	daemon := startTestDaemonInstance(t, func() []pipeline.Step {
		return []pipeline.Step{&mockSlowStep{name: types.StepReview, started: started}}
	})
	p, d := daemon.paths, daemon.db

	repo, headSHA := setupTestGitRepo(t, p, d, "shutdown-midstep-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}

	var pushResult ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("shutdown-midstep-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow step never started")
	}

	// The daemon drains in-flight handlers as it exits, so an idle client
	// connection left open outlives the shutdown it is waiting for.
	client.Close()
	if err := daemon.stopAndWait(t); err != nil {
		t.Fatalf("daemon exited with error: %v", err)
	}

	run, err := d.GetRun(pushResult.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %s, want %s", run.Status, types.RunFailed)
	}
	if run.Error == nil || *run.Error != "daemon shutting down" {
		t.Fatalf("run error = %v, want %q", run.Error, "daemon shutting down")
	}

	worktree := p.WorktreeDir(repo.ID, pushResult.RunID)
	cleanupDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(worktree); os.IsNotExist(err) {
			break
		} else if err != nil {
			t.Fatalf("stat worktree: %v", err)
		}
		if time.Now().After(cleanupDeadline) {
			t.Fatalf("worktree still exists after mid-step shutdown cleanup: %s", worktree)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestResumedSkippedStepSurvivesASecondStop proves a run keeps resuming after
// its skip set has already resolved a post-gate step: the skipped row must be
// accepted at the next gate recovery rather than read as unexplained state.
func TestResumedSkippedStepSurvivesASecondStop(t *testing.T) {
	skipped := &mockPassStep{name: types.StepTest}
	steps := func() []pipeline.Step {
		return []pipeline.Step{
			&mockApprovalStep{name: types.StepReview},
			skipped,
			&mockApprovalStep{name: types.StepDocument},
		}
	}
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, runID := startParkedRun(t, p, d, "shutdown-park-skip-twice-repo", []types.StepName{types.StepTest})

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	second := restartTestDaemonInstance(t, p, d, steps)
	approveWhenResumed(t, p, runID, types.StepReview)
	waitForStepStatus(t, d, runID, types.StepTest, types.StepStatusSkipped)
	waitForStepStatus(t, d, runID, types.StepDocument, types.StepStatusAwaitingApproval)
	waitForRunAwaitingAgent(t, d, runID)

	if err := second.stopAndWait(t); err != nil {
		t.Fatalf("second daemon exited with error: %v", err)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil {
		t.Fatalf("worktree removed on second clean shutdown: %v", err)
	}

	restartTestDaemonInstance(t, p, d, steps)
	approveWhenResumed(t, p, runID, types.StepDocument)

	completed := waitForRunTerminalState(t, d, runID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("twice-resumed run status = %s (error %v), want %s", completed.Status, completed.Error, types.RunCompleted)
	}
	if got := skipped.execCnt.Load(); got != 0 {
		t.Fatalf("skipped step executed %d times, want 0", got)
	}
}

// TestOfflineStartDefersAPreservedRunInsteadOfFailingIt proves a transient
// cause never spends the preservation promise. The trusted default branch
// cannot be fetched, so the start neither resumes nor rejects the parked run:
// it keeps the row running, the marker set and the worktree on disk, and a
// later start that can reach the branch resumes it to completion.
func TestOfflineStartDefersAPreservedRunInsteadOfFailingIt(t *testing.T) {
	steps := func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	}
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, runID := startParkedRun(t, p, d, "shutdown-park-offline-repo", nil)
	parked := waitForRunAwaitingAgent(t, d, runID)

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	gateDir := p.RepoDir(repo.ID)
	reachableOrigin := gitOutput(t, gateDir, "remote", "get-url", "origin")
	gitCmd(t, gateDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "off-network.git"))

	offline := restartTestDaemonInstance(t, p, d, steps)

	// The row must stay untouched for the whole start, not merely at the
	// instant recovery finished.
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != types.RunRunning {
			t.Fatalf("offline start set run status to %s (error %v), want it left %s", run.Status, run.Error, types.RunRunning)
		}
		if run.AwaitingAgentSince == nil || *run.AwaitingAgentSince != *parked.AwaitingAgentSince {
			t.Fatalf("offline start changed awaiting_agent_since to %v, want %d", run.AwaitingAgentSince, *parked.AwaitingAgentSince)
		}
		if run.Error != nil {
			t.Fatalf("offline start recorded error %q, want none", *run.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil {
		t.Fatalf("offline start removed the preserved worktree: %v", err)
	}
	if row := findStepRow(t, d, runID, types.StepReview); row == nil || row.Status != types.StepStatusAwaitingApproval {
		t.Fatalf("gate step row = %v, want %s", row, types.StepStatusAwaitingApproval)
	}

	if err := offline.stopAndWait(t); err != nil {
		t.Fatalf("offline daemon exited with error: %v", err)
	}

	gitCmd(t, gateDir, "remote", "set-url", "origin", reachableOrigin)
	restartTestDaemonInstance(t, p, d, steps)
	approveWhenResumed(t, p, runID, types.StepReview)

	completed := waitForRunTerminalState(t, d, runID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("run resumed after the network returned = %s (error %v), want %s", completed.Status, completed.Error, types.RunCompleted)
	}
}

// TestStartRunAbortsWhenItCannotPersistTheSkipSet proves the skip-set write is
// authoritative rather than best-effort. --skip accepts delivery steps, so a
// run whose scope could not be recorded must not exist at all: a resume that
// lost the set would push a branch and open a PR the operator excluded.
func TestStartRunAbortsWhenItCannotPersistTheSkipSet(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "skip-set-persist-repo")

	executed := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{executed}
	})
	t.Cleanup(manager.Shutdown)
	manager.persistSkippedSteps = func(string, []types.StepName) error {
		return errors.New("database is locked")
	}

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test",
		[]types.StepName{types.StepPush, types.StepPR}, "")
	if err == nil {
		t.Fatal("start run should fail when the requested skip set cannot be persisted")
	}
	if !strings.Contains(err.Error(), "persist run skip set") {
		t.Fatalf("start run error = %v, want it to name the skip-set write", err)
	}
	if runID != "" {
		t.Fatalf("start run returned run ID %q, want none", runID)
	}

	runs, err := database.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("repo has %d runs, want the one aborted run", len(runs))
	}
	aborted := runs[0]
	if aborted.Status != types.RunFailed {
		t.Fatalf("aborted run status = %s, want %s", aborted.Status, types.RunFailed)
	}
	if aborted.Error == nil || !strings.Contains(*aborted.Error, "persist run skip set") {
		t.Fatalf("aborted run error = %v, want it to record the skip-set write failure", aborted.Error)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, aborted.ID)); !os.IsNotExist(err) {
		t.Fatalf("aborted run left a worktree behind: stat err = %v", err)
	}
	rows, err := database.GetStepsByRun(aborted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("aborted run ran %d steps, want none", len(rows))
	}
	if got := executed.execCnt.Load(); got != 0 {
		t.Fatalf("aborted run executed %d steps, want 0", got)
	}
}

// TestAbortTerminatesADeferredRun proves a deferred run is owned rather than
// unreachable. Recovery left the row running because the cause was transient,
// so an operator who aborts it must actually terminate it instead of getting a
// successful no-op over a row that stays running forever. An abort is a
// cancellation wherever it lands, so the deferred row records the same terminal
// status every other aborted run records rather than a pipeline failure.
func TestAbortTerminatesADeferredRun(t *testing.T) {
	steps := func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	}
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, runID := startParkedRun(t, p, d, "deferred-abort-repo", nil)
	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	gateDir := p.RepoDir(repo.ID)
	gitCmd(t, gateDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "off-network.git"))
	restartTestDaemonInstance(t, p, d, steps)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	var result ipc.CancelRunResult
	if err := client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: runID}, &result); err != nil {
		client.Close()
		t.Fatalf("abort of a deferred run failed: %v", err)
	}
	client.Close()

	aborted := waitForRunTerminalState(t, d, runID)
	if aborted.Status != types.RunCancelled {
		t.Fatalf("aborted deferred run status = %s, want %s", aborted.Status, types.RunCancelled)
	}
	if aborted.Error == nil || !strings.Contains(*aborted.Error, types.RunCancelReasonAbortedByUser) {
		t.Fatalf("aborted deferred run error = %v, want the user-abort reason", aborted.Error)
	}
}

// TestNewPushSupersedesWithoutDestroyingThePreservedRun proves branch
// contention is resolved in favour of the run that was promised preservation:
// a fresh push to the same branch is exactly what a preserved run's worktree
// must survive, since only that worktree holds its unpushed pipeline commits.
func TestNewPushSupersedesWithoutDestroyingThePreservedRun(t *testing.T) {
	steps := func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	}
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, parkedID := startParkedRun(t, p, d, "branch-conflict-repo", nil)
	parked := waitForRunAwaitingAgent(t, d, parkedID)

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	// A push that landed while the daemon was down leaves a second active row
	// on the same branch.
	newer, err := d.InsertRun(repo.ID, parked.Branch, parked.HeadSHA, parked.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}

	restartTestDaemonInstance(t, p, d, steps)

	superseded := waitForRunTerminalState(t, d, newer.ID)
	if superseded.Status != types.RunFailed {
		t.Fatalf("newer run status = %s, want %s", superseded.Status, types.RunFailed)
	}

	preserved, err := d.GetRun(parkedID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Status != types.RunRunning {
		t.Fatalf("preserved run status = %s (error %v), want %s", preserved.Status, preserved.Error, types.RunRunning)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, parkedID)); err != nil {
		t.Fatalf("a newer push destroyed the preserved worktree: %v", err)
	}

	approveWhenResumed(t, p, parkedID, types.StepReview)
	completed := waitForRunTerminalState(t, d, parkedID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("preserved run status = %s (error %v), want %s", completed.Status, completed.Error, types.RunCompleted)
	}
}

// staleMarkerRun leaves an active run row carrying the awaiting-agent marker
// and no gate step row at all: the best-effort marker write survived a crash
// that never parked anything.
func staleMarkerRun(t *testing.T, d *db.DB, repoID, branch, headSHA, baseSHA string) *db.Run {
	t.Helper()

	run, err := d.InsertRun(repoID, branch, headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	if rows, err := d.GetStepsByRun(run.ID); err != nil {
		t.Fatal(err)
	} else if len(rows) != 0 {
		t.Fatalf("stale-marker run has %d step rows, want none", len(rows))
	}
	return run
}

// TestStaleMarkerRunLosesAContendedBranchToTheParkedRun proves branch
// contention takes the same two facts as every other parked claim in this
// package. A marker with no gate step row behind it is not a preserved run, so
// it must not make the branch look ambiguous and cost the genuinely parked run
// its worktree.
func TestStaleMarkerRunLosesAContendedBranchToTheParkedRun(t *testing.T) {
	steps := func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	}
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, parkedID := startParkedRun(t, p, d, "stale-marker-branch-repo", nil)
	parked := waitForRunAwaitingAgent(t, d, parkedID)

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	stale := staleMarkerRun(t, d, repo.ID, parked.Branch, parked.HeadSHA, parked.BaseSHA)

	restartTestDaemonInstance(t, p, d, steps)

	superseded := waitForRunTerminalState(t, d, stale.ID)
	if superseded.Status == types.RunRunning {
		t.Fatalf("stale-marker run status = %s, want it superseded", superseded.Status)
	}

	preserved, err := d.GetRun(parkedID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Status != types.RunRunning {
		t.Fatalf("preserved run status = %s (error %v), want %s", preserved.Status, preserved.Error, types.RunRunning)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, parkedID)); err != nil {
		t.Fatalf("a stale marker cost the preserved run its worktree: %v", err)
	}

	approveWhenResumed(t, p, parkedID, types.StepReview)
	completed := waitForRunTerminalState(t, d, parkedID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("preserved run status = %s (error %v), want %s", completed.Status, completed.Error, types.RunCompleted)
	}
}

// TestStaleMarkerRunDoesNotBlockALivePush is the live-path half of the same
// rule: a marker nothing parked must not wedge a branch, refusing every push to
// it until an operator resolves a run that is not actually preserved.
func TestStaleMarkerRunDoesNotBlockALivePush(t *testing.T) {
	steps := func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	}
	instance := startTestDaemonInstance(t, steps)
	p, d := instance.paths, instance.db

	repo, headSHA := setupTestGitRepo(t, p, d, "stale-marker-live-repo")
	staleMarkerRun(t, d, repo.ID, "main", headSHA, headSHA)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult); err != nil {
		t.Fatalf("push over a stale marker was refused: %v", err)
	}
	if pushResult.RunID == "" {
		t.Fatal("push over a stale marker started no run")
	}
	waitForRunAwaitingAgent(t, d, pushResult.RunID)
}

// TestLivePushDoesNotDestroyAParkedRunOnTheSameBranch mirrors the startup rule
// on the live path: while the daemon is up, a fresh push to a branch whose run
// is parked at a gate must lose, because only that run's worktree holds its
// unpushed pipeline commits.
func TestLivePushDoesNotDestroyAParkedRunOnTheSameBranch(t *testing.T) {
	steps := func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	}
	instance := startTestDaemonInstance(t, steps)
	p, d := instance.paths, instance.db

	repo, parkedID := startParkedRun(t, p, d, "live-branch-conflict-repo", nil)
	parked := waitForRunAwaitingAgent(t, d, parkedID)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	pushErr := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/" + parked.Branch,
		Old:  parked.HeadSHA,
		New:  parked.HeadSHA,
	}, &pushResult)
	if pushErr == nil {
		t.Fatalf("push over a parked run started run %q, want it refused", pushResult.RunID)
	}

	preserved, err := d.GetRun(parkedID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Status != types.RunRunning {
		t.Fatalf("preserved run status = %s (error %v), want %s", preserved.Status, preserved.Error, types.RunRunning)
	}
	if preserved.AwaitingAgentSince == nil {
		t.Fatal("a newer push cleared the preserved run's awaiting-agent marker")
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, parkedID)); err != nil {
		t.Fatalf("a newer push destroyed the preserved worktree: %v", err)
	}

	approveWhenResumed(t, p, parkedID, types.StepReview)
	completed := waitForRunTerminalState(t, d, parkedID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("preserved run status = %s (error %v), want %s", completed.Status, completed.Error, types.RunCompleted)
	}
}
