package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestExecutor_CleanShutdownPreservesGateParkedRun verifies that a clean
// daemon shutdown while a run is parked at an approval gate leaves the run
// and gate step intact for startup recovery, instead of failing them.
func TestExecutor_CleanShutdownPreservesGateParkedRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := newApprovalStep(types.StepReview, `{"findings":[{"severity":"warning","description":"needs a human","action":"ask-user"}],"summary":"1 issue"}`)
	// The real review step records the head it reviewed even when it parks;
	// a preserved round without it is not resumable.
	step.outcome.ReviewApprovedHeadSHA = run.HeadSHA
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	parked, err := waitForAwaitingAgentSince(t, database, run.ID)
	if err != nil {
		t.Fatalf("wait for parked run: %v", err)
	}
	parkedSince := *parked.AwaitingAgentSince

	// Stay parked long enough that folding the park would round to a non-zero
	// ParkedMS, so the assertion below can actually fail.
	time.Sleep(20 * time.Millisecond)
	cancel(ErrDaemonShutdown)

	select {
	case err := <-done:
		if !errors.Is(err, ErrParkPreserved) {
			t.Fatalf("Execute() error = %v, want ErrParkPreserved", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning {
		t.Errorf("run status = %s, want %s", got.Status, types.RunRunning)
	}
	if got.AwaitingAgentSince == nil || *got.AwaitingAgentSince != parkedSince {
		t.Errorf("AwaitingAgentSince = %v after preserved shutdown, want unchanged %d", got.AwaitingAgentSince, parkedSince)
	}
	if got.Error != nil {
		t.Errorf("run error = %q, want nil", *got.Error)
	}
	if got.ParkedMS != 0 {
		t.Errorf("ParkedMS = %d, want 0 (not folded on preserved shutdown)", got.ParkedMS)
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	gateStep := dbSteps[0]
	if gateStep.Status != types.StepStatusAwaitingApproval {
		t.Errorf("gate step status = %s, want %s", gateStep.Status, types.StepStatusAwaitingApproval)
	}
	if gateStep.FindingsJSON == nil {
		t.Error("gate step FindingsJSON = nil, want findings preserved")
	}

	rounds, err := database.GetRoundsByStep(gateStep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 {
		t.Fatalf("preserved gate rounds = %d, want 1", len(rounds))
	}
	if rounds[0].ReviewedHeadSHA == nil || *rounds[0].ReviewedHeadSHA != run.HeadSHA {
		t.Errorf("preserved round reviewed head = %v, want %s", rounds[0].ReviewedHeadSHA, run.HeadSHA)
	}

	if err := ValidateRecoveredRun(database, got, []Step{step}); err != nil {
		t.Errorf("ValidateRecoveredRun() error = %v, want nil", err)
	}
}

// TestExecutor_CleanShutdownPreservesFixReviewGateForResume covers the other
// gate a run can be parked at: a fix_review reached through an auto-fix round
// must survive the same clean shutdown and still be resumable to completion.
func TestExecutor_CleanShutdownPreservesFixReviewGateForResume(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{AutoFix: config.AutoFix{Review: 1}}
	calls := 0
	newStep := func() Step {
		return &adaptiveCallStep{
			name: types.StepReview,
			fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				if !sctx.Fixing {
					return &StepOutcome{
						NeedsApproval:         true,
						AutoFixable:           true,
						Findings:              `{"findings":[{"id":"f1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1 issue"}`,
						ReviewApprovedHeadSHA: run.HeadSHA,
					}, nil
				}
				return &StepOutcome{
					NeedsApproval:         true,
					Findings:              `{"findings":[{"id":"f2","severity":"warning","description":"confirm the fix","action":"ask-user"}],"summary":"1 issue"}`,
					ReviewApprovedHeadSHA: run.HeadSHA,
				}, nil
			},
		}
	}

	step := newStep()
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	if _, err := waitForAwaitingAgentSince(t, database, run.ID); err != nil {
		t.Fatalf("wait for parked run: %v", err)
	}
	cancel(ErrDaemonShutdown)

	select {
	case err := <-done:
		if !errors.Is(err, ErrParkPreserved) {
			t.Fatalf("Execute() error = %v, want ErrParkPreserved", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	preserved, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Status != types.RunRunning || preserved.AwaitingAgentSince == nil {
		t.Fatalf("preserved fix_review run = %s / %v, want running and parked", preserved.Status, preserved.AwaitingAgentSince)
	}

	// A fresh daemon resumes the preserved fix_review gate and drives it to
	// completion once the agent approves.
	resumed := NewExecutor(database, p, cfg, nil, []Step{newStep()}, nil)
	resumeDone := make(chan error, 1)
	go func() {
		resumeDone <- resumed.Resume(context.Background(), preserved, repo, workDir)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := resumed.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("preserved fix_review gate never accepted an approval")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-resumeDone; err != nil {
		t.Fatalf("Resume() error = %v, want nil", err)
	}

	final, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != types.RunCompleted {
		t.Fatalf("resumed run status = %s, want %s", final.Status, types.RunCompleted)
	}
	if final.AwaitingAgentSince != nil {
		t.Error("resumed run stayed parked after approval")
	}
	if final.ReviewApprovedHeadSHA == nil || *final.ReviewApprovedHeadSHA != run.HeadSHA {
		t.Errorf("resumed review approved head = %v, want %s", final.ReviewApprovedHeadSHA, run.HeadSHA)
	}
	// The initial review and its one auto-fix round, and nothing more: the
	// resumed gate is approved from its preserved findings, not re-reviewed.
	if calls != 2 {
		t.Errorf("step invocations = %d, want 2", calls)
	}
}

// TestExecutor_CleanShutdownFailsRunCancelledMidStep proves preservation is
// not over-broad: a run cancelled by the same shutdown cause while a step is
// actively executing (not parked at a gate) must still fail exactly as
// before.
func TestExecutor_CleanShutdownFailsRunCancelledMidStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			<-sctx.Ctx.Done()
			return nil, context.Cause(sctx.Ctx)
		},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	// Give the step a moment to start running before cancelling.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusRunning)
	cancel(ErrDaemonShutdown)

	select {
	case err := <-done:
		if errors.Is(err, ErrParkPreserved) {
			t.Fatalf("Execute() error = %v, want NOT ErrParkPreserved for a mid-step cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed {
		t.Errorf("run status = %s, want %s", got.Status, types.RunFailed)
	}
	if got.Error == nil || *got.Error != "daemon shutting down" {
		t.Errorf("run error = %v, want %q", got.Error, "daemon shutting down")
	}
}

// TestExecutor_ResumePreservesGateParkedRunOnCleanShutdown verifies the same
// preservation behavior when the parked gate is entered via Resume (startup
// recovery), not a fresh Execute.
func TestExecutor_ResumePreservesGateParkedRunOnCleanShutdown(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs a fix","action":"ask-user"}],"summary":"one issue"}`
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, "1111111111111111111111111111111111111111", 25); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	awaitingSince := *run.AwaitingAgentSince

	step := newApprovalStep(types.StepReview, findings)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	ec := collectEvents(exec)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Resume(ctx, run, repo, workDir)
	}()

	waitForEvent(t, ec, "step_completed", string(types.StepStatusAwaitingApproval))
	// Stay parked long enough that folding the park would round to a non-zero
	// ParkedMS, so the assertion below can actually fail.
	time.Sleep(20 * time.Millisecond)
	cancel(ErrDaemonShutdown)

	select {
	case err := <-done:
		if !errors.Is(err, ErrParkPreserved) {
			t.Fatalf("Resume() error = %v, want ErrParkPreserved", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resume timed out")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning {
		t.Errorf("run status = %s, want %s", got.Status, types.RunRunning)
	}
	if got.AwaitingAgentSince == nil || *got.AwaitingAgentSince != awaitingSince {
		t.Errorf("AwaitingAgentSince = %v, want unchanged %d", got.AwaitingAgentSince, awaitingSince)
	}
	if got.ParkedMS != 0 {
		t.Errorf("ParkedMS = %d, want 0 (not folded on preserved shutdown)", got.ParkedMS)
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].Status != types.StepStatusAwaitingApproval {
		t.Errorf("gate step status = %s, want %s", dbSteps[0].Status, types.StepStatusAwaitingApproval)
	}

	if err := ValidateRecoveredRun(database, got, []Step{step}); err != nil {
		t.Errorf("ValidateRecoveredRun() error = %v, want nil", err)
	}
}

// TestExecutor_UserAbortStillCancelsParkedRun proves the preservation path is
// specific to ErrDaemonShutdown: a user-initiated abort of a gate-parked run
// must still cancel it as before.
func TestExecutor_UserAbortStillCancelsParkedRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := newApprovalStep(types.StepReview, `{"findings":[{"severity":"warning","description":"needs a human","action":"ask-user"}],"summary":"1 issue"}`)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	cancel(errors.New(types.RunCancelReasonAbortedByUser))

	select {
	case err := <-done:
		if errors.Is(err, ErrParkPreserved) {
			t.Fatalf("Execute() error = %v, want NOT ErrParkPreserved for a user abort", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCancelled {
		t.Errorf("run status = %s, want %s", got.Status, types.RunCancelled)
	}
	if got.AwaitingAgentSince != nil {
		t.Errorf("AwaitingAgentSince = %v, want nil after cancellation", got.AwaitingAgentSince)
	}
}

// waitForAwaitingAgentSince polls the DB until the run's AwaitingAgentSince
// marker is set, returning the observed run row.
func waitForAwaitingAgentSince(t *testing.T, database *db.DB, runID string) (*db.Run, error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := database.GetRun(runID)
		if err != nil {
			return nil, err
		}
		if run.AwaitingAgentSince != nil {
			return run, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, errors.New("run never reached awaiting-agent state within timeout")
}

// TestExecutor_RecoveredPostGateSkippedStepNeedsTheRunSkipSet proves an
// already-resolved step row after the gate is only acceptable when the run's
// own skip set explains it: without that set the row is unexplained state and
// recovery must refuse rather than silently drop the step.
func TestExecutor_RecoveredPostGateSkippedStepNeedsTheRunSkipSet(t *testing.T) {
	database, _, run, _ := setupTest(t)

	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	gate, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(gate.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"r1","severity":"warning","description":"needs a human","action":"ask-user"}],"summary":"1 issue"}`
	if err := database.SetStepFindings(gate.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(gate.ID, 1, "initial", &findings, nil, run.HeadSHA, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(gate.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	later, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(later.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	plan := []Step{newApprovalStep(types.StepReview, findings), &adaptiveCallStep{name: types.StepTest}}
	if err := ValidateRecoveredRun(database, parked, plan); err == nil {
		t.Fatal("ValidateRecoveredRun() = nil for a post-gate skipped step the run never requested, want an error")
	}

	if err := database.SetRunSkippedSteps(run.ID, []types.StepName{types.StepTest}); err != nil {
		t.Fatal(err)
	}
	explained, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecoveredRun(database, explained, plan); err != nil {
		t.Errorf("ValidateRecoveredRun() error = %v for a skip the run requested, want nil", err)
	}
}

// TestExecutor_CancellationBeatsABufferedResponse pins the resolution of the
// race between a response the IPC caller was already told succeeded and a
// cancellation of the same gate wait: the cancellation wins, whatever its
// cause. Consuming the response instead would complete the gate under an
// already-cancelled context, and the step loop would then fail the run
// unparked; the decision is not lost, the resumed run presents the same gate
// again. A user abort must stay an abort for the same reason. The loop makes a
// random select win the race essentially every time.
func TestExecutor_CancellationBeatsABufferedResponse(t *testing.T) {
	database, p, _, _ := setupTest(t)

	abort := errors.New(types.RunCancelReasonAbortedByUser)
	for _, cause := range []error{ErrDaemonShutdown, abort} {
		for _, tc := range []struct {
			name string
			step Step
		}{
			{name: "plain gate", step: newApprovalStep(types.StepReview, "")},
			{name: "reconciling gate", step: &reconcilingApprovalStep{name: types.StepCI}},
		} {
			t.Run(cause.Error()+"/"+tc.name, func(t *testing.T) {
				exec := NewExecutor(database, p, nil, nil, []Step{tc.step}, nil)
				ctx, cancel := context.WithCancelCause(context.Background())
				cancel(cause)

				for i := 0; i < 50; i++ {
					exec.approvalCh <- approvalResponse{action: types.ActionApprove}
					response, reconciled, err := exec.waitForApprovalOrReconcile(ctx, tc.step, &StepContext{Ctx: ctx}, true)
					if !errors.Is(err, cause) {
						t.Fatalf("iteration %d: waitForApprovalOrReconcile() error = %v, want %v", i, err, cause)
					}
					if reconciled || response.action != "" {
						t.Fatalf("iteration %d: gate returned reconciled=%v response=%q, want neither", i, reconciled, response.action)
					}
				}
			})
		}
	}
}

// TestExecutor_ShutdownWithABufferedResponsePreservesTheParkedRun is the
// run-level consequence of that ordering: a response accepted in the instant
// before a clean shutdown lands does not complete the gate and then take the
// run down with it. The reconciliation blocks until the context is cancelled
// and both gate timings are far longer than the test, so the executor is
// provably not selecting on the response channel when the response is
// buffered: the ordering comes from the step's own synchronisation rather than
// from any timing window.
func TestExecutor_ShutdownWithABufferedResponsePreservesTheParkedRun(t *testing.T) {
	database, p, run, repo := setupTest(t)

	step := &reconcilingApprovalStep{name: types.StepCI, block: true, started: make(chan struct{})}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGateReconcileTimings(time.Hour, time.Hour)

	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, t.TempDir()) }()

	select {
	case <-step.started:
	case <-time.After(5 * time.Second):
		t.Fatal("gate reconciliation did not start")
	}
	if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
		t.Fatalf("Respond() error = %v, want the response accepted", err)
	}
	cancel(ErrDaemonShutdown)

	select {
	case err := <-done:
		if !errors.Is(err, ErrParkPreserved) {
			t.Fatalf("Execute() error = %v, want ErrParkPreserved", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning || got.AwaitingAgentSince == nil {
		t.Fatalf("preserved run = %s / %v, want running and still parked", got.Status, got.AwaitingAgentSince)
	}
	if got.Error != nil {
		t.Errorf("run error = %q, want nil", *got.Error)
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].Status != types.StepStatusAwaitingApproval {
		t.Errorf("gate step status = %s, want %s so the gate is presented again on resume", dbSteps[0].Status, types.StepStatusAwaitingApproval)
	}
	if dbSteps[0].FindingsJSON == nil {
		t.Error("gate step FindingsJSON = nil, want the findings the resumed gate re-presents")
	}
}

// TestExecutor_ShutdownBeatsAResolvedReconciliation covers the second way a
// gate can complete: reconciliation deciding the gate is obsolete. A clean
// shutdown that lands while that reconciliation is in flight must win, exactly
// as it does against a buffered response, so preservation is a property of the
// gate seam rather than of one path through it.
func TestExecutor_ShutdownBeatsAResolvedReconciliation(t *testing.T) {
	database, p, _, _ := setupTest(t)

	step := &reconcilingApprovalStep{name: types.StepCI, started: make(chan struct{}), release: make(chan struct{})}
	step.resolved.Store(true)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGateReconcileTimings(time.Hour, time.Hour)

	ctx, cancel := context.WithCancelCause(context.Background())
	exec.mu.Lock()
	exec.waiting = true
	exec.waitingStep = types.StepCI
	exec.mu.Unlock()

	type gateResult struct {
		reconciled bool
		err        error
	}
	done := make(chan gateResult, 1)
	go func() {
		_, reconciled, err := exec.waitForApprovalOrReconcile(ctx, step, &StepContext{Ctx: ctx}, true)
		done <- gateResult{reconciled: reconciled, err: err}
	}()

	select {
	case <-step.started:
	case <-time.After(5 * time.Second):
		t.Fatal("gate reconciliation did not start")
	}
	cancel(ErrDaemonShutdown)
	close(step.release)

	select {
	case got := <-done:
		if got.reconciled {
			t.Error("gate reported reconciled under a cancelled context, want the shutdown to win")
		}
		if !errors.Is(got.err, ErrDaemonShutdown) {
			t.Errorf("gate error = %v, want ErrDaemonShutdown", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gate wait timed out")
	}
}

// TestExecutor_ResumeShutdownBeatsThePreWaitReconciliation covers the same seam
// on the other path: Resume reconciles once before it ever waits, and a clean
// shutdown observed by then must preserve the recovered gate instead of
// completing it and running the rest of the pipeline.
func TestExecutor_ResumeShutdownBeatsThePreWaitReconciliation(t *testing.T) {
	database, p, run, repo := setupTest(t)

	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"ci-1","severity":"warning","description":"waiting","action":"ask-user"}],"summary":"waiting"}`
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, run.HeadSHA, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	step := &reconcilingApprovalStep{name: types.StepCI, started: make(chan struct{}), release: make(chan struct{})}
	step.resolved.Store(true)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGateReconcileTimings(time.Hour, time.Hour)

	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- exec.Resume(ctx, parked, repo, t.TempDir()) }()

	select {
	case <-step.started:
	case err := <-done:
		t.Fatalf("Resume() returned %v before reconciling the recovered gate", err)
	case <-time.After(5 * time.Second):
		t.Fatal("pre-wait reconciliation did not start")
	}
	cancel(ErrDaemonShutdown)
	close(step.release)

	select {
	case err := <-done:
		if !errors.Is(err, ErrParkPreserved) {
			t.Fatalf("Resume() error = %v, want ErrParkPreserved", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resume timed out")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning || got.AwaitingAgentSince == nil {
		t.Fatalf("preserved run = %s / %v, want running and still parked", got.Status, got.AwaitingAgentSince)
	}
	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].Status != types.StepStatusAwaitingApproval {
		t.Errorf("gate step status = %s, want %s so the gate is presented again on resume", dbSteps[0].Status, types.StepStatusAwaitingApproval)
	}
}

// TestExecutor_ShutdownStillCancelsAnUnansweredGate keeps the preservation
// branch intact: with nothing buffered, a clean shutdown still surfaces its
// own cause rather than blocking or inventing a response.
func TestExecutor_ShutdownStillCancelsAnUnansweredGate(t *testing.T) {
	database, p, _, _ := setupTest(t)

	step := newApprovalStep(types.StepReview, "")
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrDaemonShutdown)

	if _, _, err := exec.waitForApprovalOrReconcile(ctx, step, &StepContext{Ctx: ctx}, true); !errors.Is(err, ErrDaemonShutdown) {
		t.Fatalf("waitForApprovalOrReconcile() error = %v, want ErrDaemonShutdown", err)
	}
}
