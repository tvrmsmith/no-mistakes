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

// markCIMonitorActive gives a run a single active CI step and a PR URL, the
// shape Drain (and db.RecoverStaleRunsExcept) classify as a CI monitor run.
func markCIMonitorActive(t *testing.T, database *db.DB, run *db.Run) {
	t.Helper()
	markCIStepActive(t, database, run)
	if err := database.UpdateRunPRURL(run.ID, "https://github.com/user/project/pull/7"); err != nil {
		t.Fatal(err)
	}
}

// markCIStepActive gives a run an active CI step and nothing else. Without a
// PR URL this is the pre-PR shape the CI step passes through while it builds
// its forge host, which Drain must NOT treat as a CI monitor.
func markCIStepActive(t *testing.T, database *db.DB, run *db.Run) {
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

	_, interrupted := findInterrupted(report.Interrupted, run.ID)
	if containsRunID(report.Waited, run.ID) {
		// Drain snapshotted the run before deregistration, so it legitimately
		// waited on it. Its done channel was already closed, so it must be
		// reported finished and never interrupted.
		if !containsRunID(report.Finished, run.ID) {
			t.Fatalf("waited run %s is missing from Finished: %+v", run.ID, report)
		}
		if interrupted {
			t.Fatalf("waited run %s finished but is reported interrupted: %+v", run.ID, report)
		}
		return
	}
	// Drain snapshotted after deregistration, so the run is not its business
	// and must appear nowhere in the report.
	if containsRunID(report.Finished, run.ID) || interrupted {
		t.Fatalf("report named run %s that was not in Waited: %+v", run.ID, report)
	}
}

// shortenDrainReclassify makes the reclassify ticker fire fast enough for a
// test to observe it without sleeping out the real interval.
func shortenDrainReclassify(t *testing.T, d time.Duration) {
	t.Helper()
	prev := drainReclassifyInterval
	drainReclassifyInterval = d
	t.Cleanup(func() { drainReclassifyInterval = prev })
}

// TestDrain_ActiveCIStepWithoutPRURLIsNotCut pins the pr_url half of the CI
// classification. The CI step row is already running while the step builds
// its forge host and before it bails out with "no PR URL found", and a drain
// landing there must wait the run out like any other, not cut it short and
// tell the operator a PR that does not exist is still open.
func TestDrain_ActiveCIStepWithoutPRURLIsNotCut(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, ctx, done := registerFakeRun(t, m, database, repo, "feature")
	markCIStepActive(t, database, run)
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(done)
	}()

	report := m.Drain(context.Background(), 5*time.Second)

	if context.Cause(ctx) != nil {
		t.Fatalf("cancel cause = %v, want nil: a CI step with no PR URL is not a CI monitor", context.Cause(ctx))
	}
	if len(report.Interrupted) != 0 {
		t.Fatalf("Interrupted = %v, want empty", report.Interrupted)
	}
	if !containsRunID(report.Finished, run.ID) {
		t.Fatalf("Finished = %v, want it to contain %s", report.Finished, run.ID)
	}
}

// TestDrain_CutCIMonitorIsNotCountedAsFinished pins the operator-facing
// count: a CI monitor exits promptly once the drain cancels it, but it exited
// because the drain cut it, not because its work completed, so it belongs in
// Interrupted alone. Counting it as finished contradicts the very next line
// of the CLI's own output.
func TestDrain_CutCIMonitorIsNotCountedAsFinished(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, _, done := registerFakeRun(t, m, database, repo, "feature")
	markCIMonitorActive(t, database, run)
	close(done)

	report := m.Drain(context.Background(), 5*time.Second)

	if containsRunID(report.Finished, run.ID) {
		t.Fatalf("Finished = %v, want it to exclude the cut CI monitor %s", report.Finished, run.ID)
	}
	if _, ok := findInterrupted(report.Interrupted, run.ID); !ok {
		t.Fatalf("Interrupted = %v, want an entry for %s", report.Interrupted, run.ID)
	}
}

// TestDrain_RunFinishingAtTheDeadlineIsNotReportedInterrupted covers the race
// between a run's done channel closing and the deadline firing. select picks
// uniformly among ready cases, so without a post-loop sweep of the funnel a
// run that completed cleanly is reported as forcibly stopped and the CLI
// exits nonzero over it.
func TestDrain_RunFinishingAtTheDeadlineIsNotReportedInterrupted(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, _, done := registerFakeRun(t, m, database, repo, "feature")

	// Close done and let the funnel goroutine deliver, then drain with an
	// already-expired deadline so the timer case is ready alongside it.
	close(done)
	time.Sleep(50 * time.Millisecond)
	report := m.Drain(context.Background(), time.Nanosecond)

	if _, ok := findInterrupted(report.Interrupted, run.ID); ok {
		t.Fatalf("Interrupted = %v, want empty: run %s finished before the deadline", report.Interrupted, run.ID)
	}
	if !containsRunID(report.Finished, run.ID) {
		t.Fatalf("Finished = %v, want it to contain %s", report.Finished, run.ID)
	}
}

// TestDrain_RunThatParksMidDrainIsReleased covers reclassification: a run that
// reaches a gate after the drain begins cannot finish without an operator, so
// holding the drain to its full deadline over it is the same mistake as
// waiting on a run parked before the drain started.
func TestDrain_RunThatParksMidDrainIsReleased(t *testing.T) {
	shortenDrainReclassify(t, 20*time.Millisecond)
	m, database, repo := newDrainTestManager(t)
	run, ctx, _ := registerFakeRun(t, m, database, repo, "feature")
	// done is never closed: the run parks and stays parked.
	go func() {
		time.Sleep(40 * time.Millisecond)
		parkRunAwaitingAgent(t, database, run)
	}()

	start := time.Now()
	report := m.Drain(context.Background(), 10*time.Second)
	elapsed := time.Since(start)

	if elapsed >= 5*time.Second {
		t.Fatalf("Drain took %v, want it released once the run parked rather than waiting out the deadline", elapsed)
	}
	if containsRunID(report.Waited, run.ID) {
		t.Fatalf("Waited = %v, want it to exclude the run that parked mid-drain", report.Waited)
	}
	if len(report.Interrupted) != 0 {
		t.Fatalf("Interrupted = %v, want empty: a parked run is left for Shutdown's preserve-and-resume path", report.Interrupted)
	}
	if context.Cause(ctx) != nil {
		t.Fatalf("cancel cause = %v, want nil: Drain never cancels a parked run", context.Cause(ctx))
	}
}

// TestDrain_RunThatReachesCIMidDrainIsCut is the other half of
// reclassification: a run that enters its CI monitor after the drain begins
// would otherwise be waited on for a PR merge that can take 12 hours.
func TestDrain_RunThatReachesCIMidDrainIsCut(t *testing.T) {
	shortenDrainReclassify(t, 20*time.Millisecond)
	m, database, repo := newDrainTestManager(t)
	run, ctx, done := registerFakeRun(t, m, database, repo, "feature")
	go func() {
		time.Sleep(40 * time.Millisecond)
		markCIMonitorActive(t, database, run)
		<-ctx.Done()
		close(done)
	}()

	start := time.Now()
	report := m.Drain(context.Background(), 10*time.Second)
	elapsed := time.Since(start)

	if elapsed >= 5*time.Second {
		t.Fatalf("Drain took %v, want the CI monitor cut once it was reclassified", elapsed)
	}
	if cause := context.Cause(ctx); cause == nil || cause.Error() != types.RunCIMonitorDrainedReason {
		t.Fatalf("cancel cause = %v, want %q", context.Cause(ctx), types.RunCIMonitorDrainedReason)
	}
	entry, ok := findInterrupted(report.Interrupted, run.ID)
	if !ok || entry.Reason != ipc.DrainInterruptedCIMonitor {
		t.Fatalf("Interrupted = %v, want one %s entry for %s", report.Interrupted, ipc.DrainInterruptedCIMonitor, run.ID)
	}
	if len(report.Interrupted) != 1 {
		t.Fatalf("Interrupted = %v, want exactly one entry (reclassification must not report a run twice)", report.Interrupted)
	}
}

// TestDrain_CancelledContextAbortsTheWait covers Drain's ctx branch directly.
// The integration path for it (a signal arriving mid-drain) cannot prove it:
// doShutdown runs mgr.Shutdown() before srv.Close(), so the blocked runs are
// cancelled and finish through the normal funnel before the connection
// context is ever cancelled. Only a unit test reaches the branch.
func TestDrain_CancelledContextAbortsTheWait(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, _, _ := registerFakeRun(t, m, database, repo, "feature")
	// done is never closed: only ctx can end this wait.

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	report := m.Drain(ctx, 60*time.Second)
	elapsed := time.Since(start)

	if elapsed >= 10*time.Second {
		t.Fatalf("Drain took %v, want it aborted by ctx rather than waiting out the 60s deadline", elapsed)
	}
	if containsRunID(report.Finished, run.ID) {
		t.Fatalf("Finished = %v, want it to exclude the still-running %s", report.Finished, run.ID)
	}
	entry, ok := findInterrupted(report.Interrupted, run.ID)
	if !ok || entry.Reason != ipc.DrainInterruptedDeadline {
		t.Fatalf("Interrupted = %v, want one %s entry for %s", report.Interrupted, ipc.DrainInterruptedDeadline, run.ID)
	}
}
