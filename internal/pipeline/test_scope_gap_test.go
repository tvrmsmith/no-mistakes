package pipeline

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const scopeGapFindings = `{"findings":[{"id":"test-scope-fault","severity":"error","description":"under-selected twice","action":"ask-user"}]}`

// respondWhenParked retries a response until the executor has registered the
// gate, then waits for the run to finish.
func respondWhenParked(t *testing.T, exec *Executor, done <-chan error, step types.StepName, action types.ApprovalAction) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for exec.Respond(step, action, nil) != nil {
		if time.Now().After(deadline) {
			select {
			case err := <-done:
				t.Fatalf("run ended before the %s gate parked: %v", step, err)
			default:
			}
			t.Fatalf("%s gate never accepted %s", step, action)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("executor timed out")
	}
}

// runScopeFaultParkAndRespond parks a Test step that recorded a scope gap,
// answers it, and reports whether the step after it sees the gap accepted.
func runScopeFaultParkAndRespond(t *testing.T, action types.ApprovalAction) bool {
	t.Helper()
	database, p, run, repo := setupTest(t)
	gap := TestScopeGap{Fingerprint: "fp", Units: []string{"web"}}
	parked := false
	test := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if parked {
				return &StepOutcome{}, nil
			}
			parked = true
			sctx.Shared.SetParkedTestScopeGap(gap)
			return &StepOutcome{NeedsApproval: true, Findings: scopeGapFindings}, nil
		},
	}
	var accepted bool
	after := &adaptiveCallStep{
		name: types.StepPush,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			accepted = sctx.Shared.TestScopeGapAccepted(gap.Fingerprint, gap.Units)
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{test, after}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(t.Context(), run, repo, t.TempDir()) }()
	respondWhenParked(t, exec, done, types.StepTest, action)
	return accepted
}

func TestExecutor_ApprovingAScopeFaultParkAcceptsItsGap(t *testing.T) {
	if !runScopeFaultParkAndRespond(t, types.ActionApprove) {
		t.Fatal("approving the Test scope-fault park did not accept its gap")
	}
}

// Skip says "do not run this step now", not "these units need not run", so
// the next attempt on the same changed-file set asks again.
func TestExecutor_SkippingAScopeFaultParkAcceptsNothing(t *testing.T) {
	if runScopeFaultParkAndRespond(t, types.ActionSkip) {
		t.Fatal("skipping the Test scope-fault park accepted its gap")
	}
}

// A daemon restart while parked must not lose the gap the park recorded, or
// approving the recovered gate would accept nothing and the next re-test
// would park again.
func TestExecutor_ApprovingARecoveredScopeFaultParkAcceptsItsGap(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := scopeGapFindings
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
	if _, err := database.InsertStepResult(run.ID, types.StepPush); err != nil {
		t.Fatal(err)
	}
	gap := TestScopeGap{Fingerprint: "fp", Units: []string{"web"}}
	seed, err := json.Marshal(map[string]any{"scope_faults": 2, "parked_scope_gap": gap})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunTestDiscovery(run.ID, string(seed)); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	var accepted bool
	after := &adaptiveCallStep{
		name: types.StepPush,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			accepted = sctx.Shared.TestScopeGapAccepted(gap.Fingerprint, gap.Units)
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{newApprovalStep(types.StepTest, scopeGapFindings), after}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Resume(t.Context(), run, repo, t.TempDir()) }()
	respondWhenParked(t, exec, done, types.StepTest, types.ActionApprove)
	if !accepted {
		t.Fatal("approving the recovered Test scope-fault park did not accept its gap")
	}
}

// Every Test round clears the parked gap before it starts, so approving a
// later gate of another kind accepts nothing.
func TestRunShared_ClearedParkedScopeGapIsNotAccepted(t *testing.T) {
	s := NewRunShared(newFakeSharedStore(), "run-1")
	s.SetParkedTestScopeGap(TestScopeGap{Fingerprint: "fp", Units: []string{"web"}})
	s.ClearParkedTestScopeGap()
	s.AcceptParkedTestScopeGap()
	if s.TestScopeGapAccepted("fp", []string{"web"}) {
		t.Fatal("an approval after the parked round ended accepted its gap")
	}
}

func TestRunShared_AcceptedScopeGapCoversOnlyItsOwnUnits(t *testing.T) {
	s := NewRunShared(newFakeSharedStore(), "run-1")
	s.SetParkedTestScopeGap(TestScopeGap{Fingerprint: "fp", Units: []string{"web", "worker"}})
	s.AcceptParkedTestScopeGap()
	cases := []struct {
		fingerprint string
		units       []string
		want        bool
	}{
		{"fp", []string{"web"}, true},
		{"fp", []string{"web", "worker"}, true},
		{"fp", []string{"web", "billing"}, false},
		{"fp-moved", []string{"web"}, false},
		{"fp", nil, false},
	}
	for _, c := range cases {
		if got := s.TestScopeGapAccepted(c.fingerprint, c.units); got != c.want {
			t.Errorf("TestScopeGapAccepted(%q, %v) = %v, want %v", c.fingerprint, c.units, got, c.want)
		}
	}
}

func TestRestoreRunShared_ResumedRunKeepsItsAcceptedScopeGap(t *testing.T) {
	store := newFakeSharedStore()
	started := NewRunShared(store, "run-1")
	started.SetParkedTestScopeGap(TestScopeGap{Fingerprint: "fp", Units: []string{"web"}})
	started.AcceptParkedTestScopeGap()

	if !RestoreRunShared(store, "run-1").TestScopeGapAccepted("fp", []string{"web"}) {
		t.Fatal("a resumed run forgot the scope gap an operator accepted")
	}
}

func TestRunShared_ResetTestDiscoveryForgetsTheLayoutAndItsBudgets(t *testing.T) {
	store := newFakeSharedStore()
	s := NewRunShared(store, "run-1")
	s.SetTestDiscovery("fp", TestDiscovery{
		Units:    []config.TestUnit{{Name: "api", Path: "services/api", Command: "go test"}},
		Selected: []string{"api"},
		Source:   "agent",
	})
	s.NoteTestScopeFault()
	s.NoteTestScopeFault()
	s.NoteTestRunnerFault()
	s.SetTestKeptCommand("go test")
	s.SetParkedTestScopeGap(TestScopeGap{Fingerprint: "fp", Units: []string{"web"}})
	s.AcceptParkedTestScopeGap()

	s.ResetTestDiscovery()

	resumed := RestoreRunShared(store, "run-1")
	if _, ok := resumed.TestDiscovery("fp"); ok {
		t.Error("reset kept the cached layout")
	}
	if resumed.TestKeptCommand("go test") {
		t.Error("reset kept the kept command")
	}
	if resumed.TestScopeGapAccepted("fp", []string{"web"}) {
		t.Error("reset kept the accepted scope gap")
	}
	if got := resumed.NoteTestScopeFault(); got != 1 {
		t.Errorf("scope faults after reset = %d, want 1", got)
	}
	if got := resumed.NoteTestRunnerFault(); got != 1 {
		t.Errorf("runner faults after reset = %d, want 1", got)
	}
}
