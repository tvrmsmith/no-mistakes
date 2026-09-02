package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newDrainTestManager builds a bare RunManager over a fresh temp-rooted
// database, with no daemon service and no IPC listener - Drain only ever
// touches m.db, m.cancels, and m.dones, so registering fake runs directly
// against those is enough to exercise it without a real pipeline execution.
func newDrainTestManager(t *testing.T) (*RunManager, *db.DB, *db.Repo) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	repo, err := database.InsertRepo(filepath.Join(t.TempDir(), "repo"), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}

	m := NewRunManager(database, p, nil)
	return m, database, repo
}

// registerFakeRun inserts a run row and registers a cancel/done pair for it
// directly on the manager, mimicking what startRun does after launching its
// background goroutine, without actually running a pipeline.
func registerFakeRun(t *testing.T, m *RunManager, database *db.DB, repo *db.Repo, branch string) (*db.Run, context.Context, chan struct{}) {
	t.Helper()
	run, err := database.InsertRun(repo.ID, branch, "deadbeef", "cafef00d")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	m.mu.Lock()
	m.cancels[run.ID] = cancel
	m.dones[run.ID] = done
	m.mu.Unlock()
	return run, ctx, done
}

// parkRunAwaitingAgent marks a run as parked at an approval gate, the way the
// executor does on gate entry.
func parkRunAwaitingAgent(t *testing.T, database *db.DB, run *db.Run) {
	t.Helper()
	sr, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := "[]"
	if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, &findings); err != nil {
		t.Fatal(err)
	}
}

// markCIMonitorActive gives a run a single active CI step, the shape Drain
// (and db.RecoverStaleRunsExcept) classify as a CI monitor run.
func markCIMonitorActive(t *testing.T, database *db.DB, run *db.Run) {
	t.Helper()
	sr, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithAutoFixLimit(sr.ID, 0); err != nil {
		t.Fatal(err)
	}
}

func containsRunID(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

func findInterrupted(interrupted []ipc.DrainInterruptedRun, id string) (ipc.DrainInterruptedRun, bool) {
	for _, ir := range interrupted {
		if ir.RunID == id {
			return ir, true
		}
	}
	return ipc.DrainInterruptedRun{}, false
}

// TestDrain_RefusesNewRunsImmediately pins that a push arriving after Drain
// begins never starts a run, mirroring the existing shuttingDown check that
// startRun already performs for Shutdown().
func TestDrain_RefusesNewRunsImmediately(t *testing.T) {
	m, database, repo := newDrainTestManager(t)

	report := m.Drain(context.Background(), 5*time.Second)
	if len(report.Waited) != 0 {
		t.Fatalf("Waited = %v, want empty (no runs registered)", report.Waited)
	}

	_, err := m.startRun(context.Background(), repo, "main", "deadbeef", "cafef00d", "push", nil, "")
	if err == nil {
		t.Fatal("startRun after Drain: got nil error, want refusal")
	}
	run, dbErr := database.GetActiveRun(repo.ID, "main")
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if run != nil {
		t.Fatalf("startRun after Drain registered a run: %+v", run)
	}
}

// TestDrain_GateParkedRunDoesNotHoldUpDrain covers scenario 2: a run parked
// at an approval gate must never be waited on, cancelled, or reported.
func TestDrain_GateParkedRunDoesNotHoldUpDrain(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, _, _ := registerFakeRun(t, m, database, repo, "feature")
	parkRunAwaitingAgent(t, database, run)

	start := time.Now()
	report := m.Drain(context.Background(), 5*time.Second)
	elapsed := time.Since(start)

	if elapsed >= 2*time.Second {
		t.Fatalf("Drain took %v, want under 2s for a parked-only run", elapsed)
	}
	if containsRunID(report.Waited, run.ID) {
		t.Fatalf("Waited = %v, want it to exclude parked run %s", report.Waited, run.ID)
	}
	if len(report.Interrupted) != 0 {
		t.Fatalf("Interrupted = %v, want empty", report.Interrupted)
	}
}

// TestDrain_CIMonitorIsCutNotWaitedFor covers scenario 3.
func TestDrain_CIMonitorIsCutNotWaitedFor(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, ctx, done := registerFakeRun(t, m, database, repo, "feature")
	markCIMonitorActive(t, database, run)
	close(done) // goroutine "exits promptly" once cancelled

	start := time.Now()
	report := m.Drain(context.Background(), 5*time.Second)
	elapsed := time.Since(start)
	if elapsed >= 2*time.Second {
		t.Fatalf("Drain took %v, want promptly for a cut CI monitor", elapsed)
	}

	cause := context.Cause(ctx)
	if cause == nil || cause.Error() != types.RunCIMonitorDrainedReason {
		t.Fatalf("cancel cause = %v, want %q", cause, types.RunCIMonitorDrainedReason)
	}
	if len(report.Interrupted) != 1 {
		t.Fatalf("Interrupted = %v, want exactly one entry", report.Interrupted)
	}
	entry := report.Interrupted[0]
	if entry.RunID != run.ID || entry.Reason != ipc.DrainInterruptedCIMonitor {
		t.Fatalf("Interrupted[0] = %+v, want run %s reason %s", entry, run.ID, ipc.DrainInterruptedCIMonitor)
	}
}

// TestDrain_NormalInFlightRunIsWaitedFor covers scenario 4.
func TestDrain_NormalInFlightRunIsWaitedFor(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, ctx, done := registerFakeRun(t, m, database, repo, "feature")
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(done)
	}()

	start := time.Now()
	report := m.Drain(context.Background(), 5*time.Second)
	elapsed := time.Since(start)

	if elapsed < 200*time.Millisecond {
		t.Fatalf("Drain returned after %v, want it to wait for the run to exit (~200ms)", elapsed)
	}
	if !containsRunID(report.Finished, run.ID) {
		t.Fatalf("Finished = %v, want it to contain %s", report.Finished, run.ID)
	}
	if len(report.Interrupted) != 0 {
		t.Fatalf("Interrupted = %v, want empty", report.Interrupted)
	}
	if context.Cause(ctx) != nil {
		t.Fatalf("cancel cause = %v, want nil (Drain must not cancel a normal waited run)", context.Cause(ctx))
	}
}

// TestDrain_DeadlineExpiresWithRunStillInFlight covers scenario 5.
func TestDrain_DeadlineExpiresWithRunStillInFlight(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, ctx, _ := registerFakeRun(t, m, database, repo, "feature")
	// done is never closed: the run never exits on its own.

	start := time.Now()
	report := m.Drain(context.Background(), 100*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed >= 2*time.Second {
		t.Fatalf("Drain took %v, want it to return at about the 100ms deadline", elapsed)
	}
	if containsRunID(report.Finished, run.ID) {
		t.Fatalf("Finished = %v, want it to exclude %s", report.Finished, run.ID)
	}
	entry, ok := findInterrupted(report.Interrupted, run.ID)
	if !ok || entry.Reason != ipc.DrainInterruptedDeadline {
		t.Fatalf("Interrupted = %v, want one entry for %s with reason %s", report.Interrupted, run.ID, ipc.DrainInterruptedDeadline)
	}
	if context.Cause(ctx) != nil {
		t.Fatalf("cancel cause = %v, want nil (Drain must not cancel a deadline-expired run itself)", context.Cause(ctx))
	}

	// Shutdown() still cancels it afterwards, unchanged.
	m.Shutdown()
	if context.Cause(ctx) == nil {
		t.Fatal("Shutdown() after Drain did not cancel the still-in-flight run")
	}
}

// TestDrain_ConcurrentDeregistrationIsNotAPhantomRun covers scenario 7: a run
// finishing (deregistering) concurrently with Drain's classification snapshot
// must not be named in the report, and the race detector must stay clean.
func TestDrain_ConcurrentDeregistrationIsNotAPhantomRun(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, _, done := registerFakeRun(t, m, database, repo, "feature")

	deregistered := make(chan struct{})
	go func() {
		close(done)
		m.mu.Lock()
		delete(m.cancels, run.ID)
		delete(m.dones, run.ID)
		m.mu.Unlock()
		if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
			t.Error(err)
		}
		close(deregistered)
	}()

	report := m.Drain(context.Background(), 5*time.Second)
	<-deregistered

	for _, id := range report.Waited {
		if id == run.ID {
			// Fine either way - Drain may have snapshotted the run before
			// deregistration and legitimately waited on (and finished) it.
			return
		}
	}
	if containsRunID(report.Finished, run.ID) || func() bool { _, ok := findInterrupted(report.Interrupted, run.ID); return ok }() {
		t.Fatalf("report named run %s that was not in Waited: %+v", run.ID, report)
	}
}
