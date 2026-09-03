package pipeline

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// residueDiscardingStep is a mockStep that also implements
// ApprovalResidueDiscarder, so these tests can exercise the executor's
// ActionApprove discard path without a real worktree. Its round records residue
// on RunShared exactly as a real residue park does, unless parksClean is set,
// and it counts every discard call.
type residueDiscardingStep struct {
	*mockStep
	parksClean bool
	mu         sync.Mutex
	calls      int
	err        error
}

func (r *residueDiscardingStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if !r.parksClean {
		sctx.Shared.SetValidationResidue(r.Name(), ValidationResidue{Modified: []string{"feature.txt"}})
	}
	return r.mockStep.Execute(sctx)
}

func (r *residueDiscardingStep) DiscardApprovalResidue(_ *StepContext) error {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return r.err
}

func (r *residueDiscardingStep) observed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

const residueGateFindings = `{"findings":[{"id":"residue-tracked-1","severity":"warning","action":"no-op","file":"feature.txt","description":"left modified"}]}`

// TestExecutor_ApproveDiscardsResidue proves the live approval path actually
// calls the discarder. Approving a residue gate means discard, so a wiring
// this test does not cover is one where the gate's leftovers silently survive
// into whatever the next step commits.
func TestExecutor_ApproveDiscardsResidue(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &residueDiscardingStep{mockStep: newApprovalStep(types.StepReview, residueGateFindings)}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	waitForStepStatus(t, database, run.ID, step.Name(), types.StepStatusAwaitingApproval)
	if err := exec.Respond(step.Name(), types.ActionApprove, nil); err != nil {
		t.Fatalf("Respond(approve) error = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not complete after approval")
	}

	if calls := step.observed(); calls != 1 {
		t.Fatalf("DiscardApprovalResidue calls = %d, want exactly 1", calls)
	}
}

// TestExecutor_ApproveWithoutARecordedParkDiscardsNothing pins the gate on the
// call. Finding.ID is free-form agent text, so a round that parked for its own
// reasons can carry items spelled like a residue park's; only the record the
// step wrote from git decides, and a round that wrote none means an ordinary
// approval touches no files.
func TestExecutor_ApproveWithoutARecordedParkDiscardsNothing(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &residueDiscardingStep{
		mockStep:   newApprovalStep(types.StepReview, residueGateFindings),
		parksClean: true,
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	waitForStepStatus(t, database, run.ID, step.Name(), types.StepStatusAwaitingApproval)
	if err := exec.Respond(step.Name(), types.ActionApprove, nil); err != nil {
		t.Fatalf("Respond(approve) error = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not complete after approval")
	}

	if calls := step.observed(); calls != 0 {
		t.Fatalf("DiscardApprovalResidue calls = %d, want 0 for a gate that recorded no residue", calls)
	}
}

// TestExecutor_ApproveDiscardFailureFailsTheRun pins the fail-closed half. A
// discard that cannot complete leaves an uncommitted tree a later step would
// commit unjudged, so completing the step anyway is the outcome this rules out.
func TestExecutor_ApproveDiscardFailureFailsTheRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &residueDiscardingStep{
		mockStep: newApprovalStep(types.StepReview, residueGateFindings),
		err:      errors.New("worktree is locked"),
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	waitForStepStatus(t, database, run.ID, step.Name(), types.StepStatusAwaitingApproval)
	if err := exec.Respond(step.Name(), types.ActionApprove, nil); err != nil {
		t.Fatalf("Respond(approve) error = %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Execute() error = nil, want a failed discard to fail the run")
		}
		if !strings.Contains(err.Error(), "worktree is locked") {
			t.Fatalf("Execute() error = %v, want it to carry the discard failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not finish after approval")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == types.RunCompleted {
		t.Fatal("run status = completed, want the failed discard to stop the run")
	}
}

// TestExecutor_RecoveredGateApprovalDiscardsNothing covers the OTHER
// ActionApprove site, the daemon-restart recovery path in Resume. The record of
// what a park refused to commit lives on RunShared, which a new process rebuilds
// empty, so the recovered gate has no recorded list and approving it removes
// nothing; the leftovers ride into the next validation step's exit commit, which
// is what happened before the residue gate existed. Forgetting the record can
// only fail towards touching no files, never towards deleting one nothing
// recorded, and that direction is what this pins.
func TestExecutor_RecoveredGateApprovalDiscardsNothing(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := residueGateFindings
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	step := &residueDiscardingStep{mockStep: newApprovalStep(types.StepDocument, findings)}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()

	deadline := time.Now().Add(5 * time.Second)
	var respondErr error
	for time.Now().Before(deadline) {
		if respondErr = exec.Respond(types.StepDocument, types.ActionApprove, nil); respondErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if respondErr != nil {
		t.Fatalf("Respond(approve) error = %v", respondErr)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed executor did not complete after approval")
	}

	if calls := step.observed(); calls != 0 {
		t.Fatalf("recovered-path DiscardApprovalResidue calls = %d, want 0 with no recorded residue", calls)
	}
}

// TestExecutor_OnlyApprovalDiscardsResidue pins the other half of the trigger
// condition. Approving is what means discard; skipping the step says nothing
// about the leftovers, and discarding them there would destroy work no answer
// asked to throw away.
func TestExecutor_OnlyApprovalDiscardsResidue(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &residueDiscardingStep{mockStep: newApprovalStep(types.StepReview, residueGateFindings)}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	waitForStepStatus(t, database, run.ID, step.Name(), types.StepStatusAwaitingApproval)
	if err := exec.Respond(step.Name(), types.ActionSkip, nil); err != nil {
		t.Fatalf("Respond(skip) error = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not complete after the skip")
	}

	if calls := step.observed(); calls != 0 {
		t.Fatalf("DiscardApprovalResidue calls = %d, want 0 for a skipped gate", calls)
	}
}
