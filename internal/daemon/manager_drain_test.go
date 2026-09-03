package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
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
	if err := parkRunAwaitingAgentErr(database, run); err != nil {
		t.Fatal(err)
	}
}

// parkRunAwaitingAgentErr is parkRunAwaitingAgent for callers that are not the
// test goroutine. t.Fatal from a spawned goroutine is undefined behavior (it
// calls runtime.Goexit on the wrong goroutine, so the test never fails and can
// hang instead), and the mid-drain reclassification tests park from a
// goroutine by construction.
func parkRunAwaitingAgentErr(database *db.DB, run *db.Run) error {
	sr, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		return err
	}
	findings := "[]"
	return database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, &findings)
}

// markCIMonitorActive gives a run a single active CI step and a PR URL, the
// shape Drain (and db.RecoverStaleRunsExcept) classify as a CI monitor run.
func markCIMonitorActive(t *testing.T, database *db.DB, run *db.Run) {
	t.Helper()
	if err := markCIMonitorActiveErr(database, run); err != nil {
		t.Fatal(err)
	}
}

// markCIMonitorActiveErr is markCIMonitorActive off the test goroutine; see
// parkRunAwaitingAgentErr for why the error is returned rather than fataled.
func markCIMonitorActiveErr(database *db.DB, run *db.Run) error {
	if err := markCIStepActiveErr(database, run); err != nil {
		return err
	}
	return database.UpdateRunPRURL(run.ID, "https://github.com/user/project/pull/7")
}

// markCIStepActive gives a run an active CI step and nothing else. Without a
// PR URL this is the pre-PR shape the CI step passes through while it builds
// its forge host, which Drain must NOT treat as a CI monitor.
func markCIStepActive(t *testing.T, database *db.DB, run *db.Run) {
	t.Helper()
	if err := markCIStepActiveErr(database, run); err != nil {
		t.Fatal(err)
	}
}

func markCIStepActiveErr(database *db.DB, run *db.Run) error {
	sr, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		return err
	}
	return database.StartStepWithAutoFixLimit(sr.ID, 0)
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

	_, err := m.startRun(context.Background(), repo, "main", "deadbeef", "cafef00d", "push", nil, "", "")
	if err == nil {
		t.Fatal("startRun after Drain: got nil error, want refusal")
	}
	// The reason matters, not just the failure: this manager's repo has no
	// gate bare repo, so a startRun that got past the latch would fail at the
	// fetch anyway, and both failures mark the row terminal.
	if !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("startRun after Drain: err = %v, want the drain latch refusal", err)
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

// withDrainLoopHooks installs the wait-loop and finish-delivery seams for one
// test and restores them afterwards.
func withDrainLoopHooks(t *testing.T, iteration, delivered func()) {
	t.Helper()
	prevIteration, prevDelivered := drainWaitIterationHook, drainFinishDeliveredHook
	drainWaitIterationHook, drainFinishDeliveredHook = iteration, delivered
	t.Cleanup(func() {
		drainWaitIterationHook, drainFinishDeliveredHook = prevIteration, prevDelivered
	})
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

// TestDrain_ParkedCIGateWinsOverCIMonitorClassification pins the precedence
// between the two exemptions when a run satisfies both: it has an open PR and
// an active CI step, AND it is parked awaiting an operator. Parked wins. A
// parked run is not monitoring anything, so there is nothing to cut; cancelling
// it with the CI-monitor cause would fail a run that the clean-stop path
// otherwise preserves and resumes with its PR re-checked on the next start.
func TestDrain_ParkedCIGateWinsOverCIMonitorClassification(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, ctx, _ := registerFakeRun(t, m, database, repo, "feature")
	// done is never closed: a parked run's goroutine blocks until an operator
	// responds, so a drain that waits on this one waits forever.
	markCIMonitorActive(t, database, run)
	parkRunAwaitingAgent(t, database, run)

	start := time.Now()
	report := m.Drain(context.Background(), 10*time.Second)

	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("Drain took %v, want it to skip the parked run immediately", elapsed)
	}
	if context.Cause(ctx) != nil {
		t.Fatalf("cancel cause = %v, want nil: a parked run is never cancelled by Drain, even one at a CI gate", context.Cause(ctx))
	}
	if len(report.Interrupted) != 0 {
		t.Fatalf("Interrupted = %v, want empty: a parked CI gate is preserved for resume, not reported as a cut monitor", report.Interrupted)
	}
	if containsRunID(report.Waited, run.ID) {
		t.Fatalf("Waited = %v, want it to exclude the parked run %s", report.Waited, run.ID)
	}
	if containsRunID(report.Finished, run.ID) {
		t.Fatalf("Finished = %v, want it to exclude the parked run %s", report.Finished, run.ID)
	}
}

// TestDrain_RunFinishingAtTheDeadlineIsNotReportedInterrupted covers the race
// between a run's done channel closing and the deadline firing. select picks
// uniformly among ready cases, so without a post-loop sweep of the funnel a
// run that completed cleanly is reported as forcibly stopped and the CLI
// exits nonzero over it.
func TestDrain_RunFinishingAtTheDeadlineIsNotReportedInterrupted(t *testing.T) {
	m, database, repo := newDrainTestManager(t)

	// One pass is not evidence: with the deadline already expired, the timer
	// case and the funnel case are both ready and select picks uniformly, so a
	// build with no post-loop sweep passes about half the time. Repeating the
	// race drives the chance of a false pass to nothing.
	const attempts = 30
	for i := 0; i < attempts; i++ {
		run, _, done := registerFakeRun(t, m, database, repo, fmt.Sprintf("feature-%d", i))
		close(done)

		report := m.Drain(context.Background(), time.Nanosecond)

		if _, ok := findInterrupted(report.Interrupted, run.ID); ok {
			t.Fatalf("attempt %d: Interrupted = %v, want empty: run %s finished before the deadline", i, report.Interrupted, run.ID)
		}
		if !containsRunID(report.Finished, run.ID) {
			t.Fatalf("attempt %d: Finished = %v, want it to contain %s", i, report.Finished, run.ID)
		}

		m.mu.Lock()
		delete(m.cancels, run.ID)
		delete(m.dones, run.ID)
		m.mu.Unlock()
		if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDrain_ShutdownSignalReportsRunsAsStoppedNotFinished pins the race the
// shutdown signal exists for. Shutdown() cancels every run and the cancelled
// runs' done channels then close, so a drain that learned about the shutdown
// only from the runs it was waiting on would report work the shutdown killed
// as "finished before the daemon stopped" and exit 0 over it.
func TestDrain_ShutdownSignalReportsRunsAsStoppedNotFinished(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, ctx, done := registerFakeRun(t, m, database, repo, "feature")
	go func() {
		// A cancelled run unwinds (terminal row, head reconciliation) before
		// its goroutine exits; the delay stands in for that work.
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		close(done)
	}()

	go func() {
		time.Sleep(50 * time.Millisecond)
		m.Shutdown()
	}()

	start := time.Now()
	report := m.Drain(context.Background(), 60*time.Second)

	if elapsed := time.Since(start); elapsed >= 10*time.Second {
		t.Fatalf("Drain took %v, want it ended by the shutdown rather than the 60s deadline", elapsed)
	}
	if containsRunID(report.Finished, run.ID) {
		t.Fatalf("Finished = %v, want it to exclude %s: the shutdown killed it mid-flight", report.Finished, run.ID)
	}
	entry, ok := findInterrupted(report.Interrupted, run.ID)
	if !ok || entry.Reason != ipc.DrainInterruptedShutdown {
		t.Fatalf("Interrupted = %v, want one %s entry for %s", report.Interrupted, ipc.DrainInterruptedShutdown, run.ID)
	}
}

// TestShutdown_GateParkedRunIsPreservedForResume pins the premise the parked
// carve-out rests on: a drain leaves a parked run to the clean-stop path, and
// that path has to actually preserve it. Cancelling it makes its executor fail
// the step and the run and clear awaiting_agent_since, which rules the run out
// of the next start's recoverableParkedRuns/prepareRecoveredRun path.
func TestShutdown_GateParkedRunIsPreservedForResume(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	parked, parkedCtx, _ := registerFakeRun(t, m, database, repo, "parked")
	if err := database.UpdateRunStatus(parked.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	parkRunAwaitingAgent(t, database, parked)
	working, workingCtx, workingDone := registerFakeRun(t, m, database, repo, "working")
	go func() {
		<-workingCtx.Done()
		close(workingDone)
	}()

	m.Shutdown()

	if cause := context.Cause(parkedCtx); cause != nil {
		t.Fatalf("cancel cause for the parked run = %v, want nil: cancelling it destroys the row the next start resumes", cause)
	}
	if context.Cause(workingCtx) == nil {
		t.Fatalf("run %s was not cancelled by Shutdown; only gate-parked runs are preserved", working.ID)
	}

	row, err := database.GetRun(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The two conditions prepareRecoveredRun requires of a run it resumes.
	if row.Status != types.RunRunning {
		t.Fatalf("parked run status = %s, want %s so the next start can resume it", row.Status, types.RunRunning)
	}
	if row.AwaitingAgentSince == nil {
		t.Fatal("parked run lost awaiting_agent_since; the next start would not resume it")
	}
}

// TestDrain_ExemptRunThatUnparksIsWaitedOnAgain covers the other half of the
// park reclassification. Exemption is not a latch: a run that parks and then
// unparks inside the drain window is real work again, and leaving it exempt
// puts a run the stop is about to kill in none of Waited, Finished, or
// Interrupted.
func TestDrain_ExemptRunThatUnparksIsWaitedOnAgain(t *testing.T) {
	const tick = 10 * time.Millisecond
	shortenDrainReclassify(t, tick)
	m, database, repo := newDrainTestManager(t)
	// The second run finishes shortly after the unpark. That is what makes the
	// re-admission observable: while the unparked run stays exempt, this
	// completion drops the wait to zero remaining and the drain returns right
	// there, long before its deadline.
	_, _, otherDone := registerFakeRun(t, m, database, repo, "other")
	run, _, _ := registerFakeRun(t, m, database, repo, "feature")

	const deadline = 3 * time.Second
	// Each step waits many reclassify ticks, so the drain has ample room to
	// observe the transition before the next one lands. This is margin, not
	// synchronization: a missed tick fails the test rather than passing it.
	const settle = 20 * tick
	gateErr := make(chan error, 1)
	go func() {
		if err := parkRunAwaitingAgentErr(database, run); err != nil {
			gateErr <- err
			return
		}
		time.Sleep(settle)
		// The operator answered: the run is working again.
		if err := database.CompleteRunAwaitingAgent(run.ID, 100); err != nil {
			gateErr <- err
			return
		}
		time.Sleep(settle)
		close(otherDone)
		gateErr <- nil
	}()

	start := time.Now()
	report := m.Drain(context.Background(), deadline)
	elapsed := time.Since(start)

	if err := <-gateErr; err != nil {
		t.Fatalf("park then unpark the run: %v", err)
	}
	if elapsed < deadline {
		t.Fatalf("Drain returned after %v, want it to keep waiting on the unparked run until the %v deadline", elapsed, deadline)
	}
	if !containsRunID(report.Waited, run.ID) {
		t.Fatalf("Waited = %v, want the unparked run %s back in the wait", report.Waited, run.ID)
	}
	entry, ok := findInterrupted(report.Interrupted, run.ID)
	if !ok || entry.Reason != ipc.DrainInterruptedDeadline {
		t.Fatalf("Interrupted = %v, want a %s entry for the unparked run %s", report.Interrupted, ipc.DrainInterruptedDeadline, run.ID)
	}
}

// TestDrain_InitiallyParkedRunThatUnparksIsWaitedOn is the same rule for a run
// that was already parked when the drain began. Such a run is exempt from the
// first classification, but the operator can answer its gate a second later,
// and a resumed run that the stop is about to kill must not be absent from
// Waited, Finished, and Interrupted alike.
func TestDrain_InitiallyParkedRunThatUnparksIsWaitedOn(t *testing.T) {
	const tick = 10 * time.Millisecond
	shortenDrainReclassify(t, tick)
	m, database, repo := newDrainTestManager(t)
	run, ctx, _ := registerFakeRun(t, m, database, repo, "feature")
	parkRunAwaitingAgent(t, database, run)
	// As in the mid-drain case, this run's completion is what makes the
	// re-admission observable: while the resumed run stays exempt, it drops
	// the wait to zero remaining and the drain returns right there.
	_, _, otherDone := registerFakeRun(t, m, database, repo, "other")

	const deadline = 2 * time.Second
	unparkErr := make(chan error, 1)
	go func() {
		// Margin for the drain to classify the already-parked run as exempt
		// before the operator answers its gate; that is the state this test is
		// about.
		time.Sleep(10 * tick)
		if err := database.CompleteRunAwaitingAgent(run.ID, 100); err != nil {
			unparkErr <- err
			return
		}
		time.Sleep(20 * tick)
		close(otherDone)
		unparkErr <- nil
	}()

	start := time.Now()
	report := m.Drain(context.Background(), deadline)
	elapsed := time.Since(start)

	if err := <-unparkErr; err != nil {
		t.Fatalf("unpark the run: %v", err)
	}
	if elapsed < deadline {
		t.Fatalf("Drain returned after %v, want it to wait on the resumed run until the %v deadline", elapsed, deadline)
	}
	if !containsRunID(report.Waited, run.ID) {
		t.Fatalf("Waited = %v, want the resumed run %s in the wait", report.Waited, run.ID)
	}
	entry, ok := findInterrupted(report.Interrupted, run.ID)
	if !ok || entry.Reason != ipc.DrainInterruptedDeadline {
		t.Fatalf("Interrupted = %v, want a %s entry for the resumed run %s", report.Interrupted, ipc.DrainInterruptedDeadline, run.ID)
	}
	if context.Cause(ctx) != nil {
		t.Fatalf("cancel cause = %v, want nil: Drain never cancels a run it waited on", context.Cause(ctx))
	}
}

// TestDrain_GateAnsweredWithNothingLeftToWaitOnIsNotAbandoned covers the run
// an operator answers in the window between two reclassify ticks, when the
// last run the drain was still waiting on finishes. Running out of runs to
// wait on is not the same as the work being over: that answered run is doing
// real git and agent work again, and the shutdown behind this drain is about
// to kill it. The drain waits it out like any other in-flight run instead.
//
// No tick fires during this drain, so the re-admission can only come from the
// pass the drain makes before it declares the wait finished.
func TestDrain_GateAnsweredWithNothingLeftToWaitOnIsNotAbandoned(t *testing.T) {
	shortenDrainReclassify(t, time.Hour)
	m, database, repo := newDrainTestManager(t)
	parked, parkedCtx, _ := registerFakeRun(t, m, database, repo, "parked")
	parkRunAwaitingAgent(t, database, parked)
	// registerFakeRun's done channels are unbuffered, so a send completes
	// exactly when Drain's funnel goroutine receives it. Those goroutines
	// start after classification, so the first send proves the parked run was
	// already classified as exempt, and the gate is answered before the second
	// send empties the wait. No sleep, and answering early fails the test
	// rather than passing it.
	_, _, controlDone := registerFakeRun(t, m, database, repo, "control")
	_, _, workingDone := registerFakeRun(t, m, database, repo, "working")

	const deadline = 2 * time.Second
	unparkErr := make(chan error, 1)
	go func() {
		controlDone <- struct{}{}
		if err := database.CompleteRunAwaitingAgent(parked.ID, 0); err != nil {
			unparkErr <- err
			return
		}
		workingDone <- struct{}{}
		unparkErr <- nil
	}()

	start := time.Now()
	report := m.Drain(context.Background(), deadline)
	elapsed := time.Since(start)

	if err := <-unparkErr; err != nil {
		t.Fatalf("unpark the run: %v", err)
	}
	if elapsed < deadline {
		t.Fatalf("Drain returned after %v, want it to wait on the answered run %s until the %v deadline", elapsed, parked.ID, deadline)
	}
	if !containsRunID(report.Waited, parked.ID) {
		t.Fatalf("Waited = %v, want the answered run %s back in the wait", report.Waited, parked.ID)
	}
	entry, ok := findInterrupted(report.Interrupted, parked.ID)
	if !ok || entry.Reason != ipc.DrainInterruptedDeadline {
		t.Fatalf("Interrupted = %v, want a %s entry for the answered run %s", report.Interrupted, ipc.DrainInterruptedDeadline, parked.ID)
	}
	if cause := context.Cause(parkedCtx); cause != nil {
		t.Fatalf("cancel cause = %v, want nil: Drain never cancels a run it waited on", cause)
	}
}

// TestDrain_RunThatParksAfterTheLastTickIsNotReportedAsCut covers the run that
// parks at a gate between the final reclassify tick and the deadline. Shutdown
// applies the same predicate moments later, sees it parked, and preserves it
// for the next start to resume, so a report calling it forcibly stopped
// contradicts what actually happens to it and exits nonzero over it.
func TestDrain_RunThatParksAfterTheLastTickIsNotReportedAsCut(t *testing.T) {
	// No tick fires, so nothing exempts the run before the deadline does.
	shortenDrainReclassify(t, time.Hour)
	m, database, repo := newDrainTestManager(t)
	run, runCtx, _ := registerFakeRun(t, m, database, repo, "feature")
	_, _, controlDone := registerFakeRun(t, m, database, repo, "control")

	parkErr := make(chan error, 1)
	go func() {
		// Completes when the funnel receives it, which proves the drain
		// already classified this run as ordinary in-flight work.
		controlDone <- struct{}{}
		parkErr <- parkRunAwaitingAgentErr(database, run)
	}()

	report := m.Drain(context.Background(), time.Second)

	if err := <-parkErr; err != nil {
		t.Fatalf("park the run: %v", err)
	}
	if entry, ok := findInterrupted(report.Interrupted, run.ID); ok {
		t.Fatalf("Interrupted = %v, want no entry for run %s, which the shutdown preserves and the next start resumes", entry, run.ID)
	}
	if containsRunID(report.Waited, run.ID) {
		t.Fatalf("Waited = %v, want it to exclude the parked run %s", report.Waited, run.ID)
	}
	if cause := context.Cause(runCtx); cause != nil {
		t.Fatalf("cancel cause = %v, want nil", cause)
	}
}

// TestDrain_CompletionsAlreadyDeliveredWhenTheDaemonStopsAreCredited covers a
// burst of runs finishing just as a SIGTERM or a concurrent stop lands. The
// wait loop reads one completion per pass, so the rest sit in the funnel's
// channel when the shutdown signal ends the wait. Reporting those as stopped
// mid-flight tells the operator that a drain which did its whole job finished
// nothing, and exits nonzero over it.
func TestDrain_CompletionsAlreadyDeliveredWhenTheDaemonStopsAreCredited(t *testing.T) {
	shortenDrainReclassify(t, time.Hour)
	m, database, repo := newDrainTestManager(t)

	const finishers = 3
	ids := make([]string, 0, finishers)
	for i := range finishers {
		run, _, done := registerFakeRun(t, m, database, repo, fmt.Sprintf("done-%d", i))
		ids = append(ids, run.ID)
		close(done)
	}
	// One run that never finishes, so the wait cannot end on its own.
	blocker, _, _ := registerFakeRun(t, m, database, repo, "blocker")

	entered := make(chan struct{})
	release := make(chan struct{})
	deliveries := make(chan struct{}, finishers)
	var once sync.Once
	withDrainLoopHooks(t,
		func() {
			once.Do(func() {
				close(entered)
				<-release
			})
		},
		func() { deliveries <- struct{}{} },
	)

	reports := make(chan DrainReport, 1)
	go func() { reports <- m.Drain(context.Background(), 30*time.Second) }()

	// The wait loop is held before its first pass, so nothing has consumed a
	// completion yet; waiting on the delivery hook proves all three are in the
	// channel before the shutdown signal ends the wait.
	<-entered
	for range finishers {
		<-deliveries
	}
	m.signalShutdown()
	close(release)

	report := <-reports
	for _, id := range ids {
		if !containsRunID(report.Finished, id) {
			t.Fatalf("Finished = %v, want it to contain %s, which finished before the daemon stopped", report.Finished, id)
		}
		if entry, ok := findInterrupted(report.Interrupted, id); ok {
			t.Fatalf("Interrupted = %v, want no entry for %s", entry, id)
		}
	}
	entry, ok := findInterrupted(report.Interrupted, blocker.ID)
	if !ok || entry.Reason != ipc.DrainInterruptedShutdown {
		t.Fatalf("Interrupted = %v, want a %s entry for the run that never finished", report.Interrupted, ipc.DrainInterruptedShutdown)
	}
}

// TestDrain_JustApprovedCIGateIsNotCutAsAMonitor pins the exclusion of
// awaiting_approval from the CI-monitor status set. CompleteRunAwaitingAgent
// clears awaiting_agent_since the moment the operator answers, while the CI
// step row stays awaiting_approval until the approval is applied. Classifying
// that window as an idle monitor cuts a run one step from finishing and throws
// away the answer the operator just gave.
func TestDrain_JustApprovedCIGateIsNotCutAsAMonitor(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, ctx, done := registerFakeRun(t, m, database, repo, "feature")
	sr, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	findings := "[]"
	if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, &findings); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(run.ID, "https://github.com/user/project/pull/7"); err != nil {
		t.Fatal(err)
	}
	// The operator answered: the gate signal is cleared, the step row is not.
	if err := database.CompleteRunAwaitingAgent(run.ID, 100); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(done)
	}()

	report := m.Drain(context.Background(), 5*time.Second)

	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("cancel cause = %v, want nil: an approval being applied is live work, not an idle CI monitor", cause)
	}
	if len(report.Interrupted) != 0 {
		t.Fatalf("Interrupted = %v, want empty", report.Interrupted)
	}
	if !containsRunID(report.Finished, run.ID) {
		t.Fatalf("Finished = %v, want it to contain %s", report.Finished, run.ID)
	}
}

// TestStartRun_RegisteringAfterTheDrainSnapshotIsRefused pins the window
// between startRun's own shuttingDown check and its registration into
// m.cancels/m.dones: minutes of fetching and worktree setup sit in between, so
// a run admitted just before a drain begins would otherwise register after the
// drain snapshotted the active set, be waited on and reported by nobody, and
// then be killed outright by the stop.
func TestStartRun_RegisteringAfterTheDrainSnapshotIsRefused(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, err := database.InsertRun(repo.ID, "feature", "deadbeef", "cafef00d")
	if err != nil {
		t.Fatal(err)
	}

	m.Drain(context.Background(), time.Millisecond)

	registered := m.registerActiveRun(run.ID, nil, func(error) {}, make(chan struct{}))
	if registered {
		t.Fatal("a run registering after the drain snapshot was accepted; the drain neither waited on it nor reported it")
	}
	m.mu.Lock()
	_, hasCancel := m.cancels[run.ID]
	_, hasDone := m.dones[run.ID]
	m.mu.Unlock()
	if hasCancel || hasDone {
		t.Fatal("a refused run was still registered in the active-run maps")
	}
}

// TestDrain_CIStepMidAutoFixRepairIsWaitedOnNotCut pins the narrower active-
// status set. A CI step in fixing has a live auto-fix agent partway through a
// repair; cutting it throws that work away, so it is waited on like any other
// in-flight run and bounded by the deadline instead.
func TestDrain_CIStepMidAutoFixRepairIsWaitedOnNotCut(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, ctx, done := registerFakeRun(t, m, database, repo, "feature")
	markCIMonitorActive(t, database, run)
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(steps[0].ID, types.StepStatusFixing); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(done)
	}()

	report := m.Drain(context.Background(), 5*time.Second)

	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("cancel cause = %v, want nil: a CI auto-fix repair is waited on, not cut", cause)
	}
	if len(report.Interrupted) != 0 {
		t.Fatalf("Interrupted = %v, want empty", report.Interrupted)
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
	parkErr := make(chan error, 1)
	go func() {
		time.Sleep(40 * time.Millisecond)
		parkErr <- parkRunAwaitingAgentErr(database, run)
	}()

	start := time.Now()
	report := m.Drain(context.Background(), 10*time.Second)
	elapsed := time.Since(start)

	if err := <-parkErr; err != nil {
		t.Fatalf("park run mid-drain: %v", err)
	}
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
	markErr := make(chan error, 1)
	go func() {
		time.Sleep(40 * time.Millisecond)
		if err := markCIMonitorActiveErr(database, run); err != nil {
			markErr <- err
			return
		}
		markErr <- nil
		<-ctx.Done()
		close(done)
	}()

	start := time.Now()
	report := m.Drain(context.Background(), 10*time.Second)
	elapsed := time.Since(start)

	if err := <-markErr; err != nil {
		t.Fatalf("mark run as a CI monitor mid-drain: %v", err)
	}
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
	// The reason must be shutdown, not deadline: the 60s deadline never fired,
	// and telling the operator it did points them at --drain-timeout for a
	// problem raising it cannot fix.
	entry, ok := findInterrupted(report.Interrupted, run.ID)
	if !ok || entry.Reason != ipc.DrainInterruptedShutdown {
		t.Fatalf("Interrupted = %v, want one %s entry for %s", report.Interrupted, ipc.DrainInterruptedShutdown, run.ID)
	}
}

// withDrainBeforeReportHook installs fn in the window between Drain's wait
// loop and the report it builds, and removes it afterwards.
func withDrainBeforeReportHook(t *testing.T, fn func()) {
	t.Helper()
	prev := drainBeforeReportHook
	drainBeforeReportHook = fn
	t.Cleanup(func() { drainBeforeReportHook = prev })
}

// TestDrain_RunWithAnotherActiveStepBesidesCIIsNotCut pins the half of the CI
// classification that protects real work: a run whose CI step row is running
// alongside another running step is not sitting in a CI monitor, it is doing
// something the drain must wait out. Cutting it would cancel that work and
// then tell the operator its PR is merely waiting on CI.
func TestDrain_RunWithAnotherActiveStepBesidesCIIsNotCut(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, ctx, done := registerFakeRun(t, m, database, repo, "feature")
	markCIMonitorActive(t, database, run)
	pushRow, err := database.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithAutoFixLimit(pushRow.ID, 0); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(done)
	}()

	report := m.Drain(context.Background(), 5*time.Second)

	if context.Cause(ctx) != nil {
		t.Fatalf("cancel cause = %v, want nil: a run with another running step is not a CI monitor", context.Cause(ctx))
	}
	if len(report.Interrupted) != 0 {
		t.Fatalf("Interrupted = %v, want empty", report.Interrupted)
	}
	if !containsRunID(report.Waited, run.ID) {
		t.Fatalf("Waited = %v, want it to include %s", report.Waited, run.ID)
	}
	if !containsRunID(report.Finished, run.ID) {
		t.Fatalf("Finished = %v, want it to include %s", report.Finished, run.ID)
	}
}

// TestDrain_ExemptRunThatFinishesIsReportedFinished covers the run that was
// parked when the drain started, whose gate an operator answered mid-drain,
// and which then completed. An exempt entry is released from the wait, so the
// wait can end without ever reading its finish off the funnel; reporting it as
// interrupted would tell the operator a successful run was stopped mid-flight
// and exit nonzero over it.
//
// The hook places the finish in the only window that produces that state, the
// gap between the wait ending and the report being built.
func TestDrain_ExemptRunThatFinishesIsReportedFinished(t *testing.T) {
	shortenDrainReclassify(t, time.Hour)
	m, database, repo := newDrainTestManager(t)
	parked, parkedCtx, parkedDone := registerFakeRun(t, m, database, repo, "parked")
	parkRunAwaitingAgent(t, database, parked)
	working, _, workingDone := registerFakeRun(t, m, database, repo, "working")

	withDrainBeforeReportHook(t, func() {
		if err := database.CompleteRunAwaitingAgent(parked.ID, 0); err != nil {
			t.Error(err)
			return
		}
		if err := database.UpdateRunStatus(parked.ID, types.RunCompleted); err != nil {
			t.Error(err)
			return
		}
		close(parkedDone)
	})
	close(workingDone)

	report := m.Drain(context.Background(), 5*time.Second)

	if context.Cause(parkedCtx) != nil {
		t.Fatalf("cancel cause = %v, want nil: the drain never cancels a run it merely stopped waiting on", context.Cause(parkedCtx))
	}
	if entry, ok := findInterrupted(report.Interrupted, parked.ID); ok {
		t.Fatalf("Interrupted = %v, want no entry for the run that finished (reason %q)", report.Interrupted, entry.Reason)
	}
	if !containsRunID(report.Finished, parked.ID) {
		t.Fatalf("Finished = %v, want it to include the resumed run %s", report.Finished, parked.ID)
	}
	if !containsRunID(report.Finished, working.ID) {
		t.Fatalf("Finished = %v, want it to include %s", report.Finished, working.ID)
	}
}

// TestShutdown_PreservedRunThatLeavesItsGateIsCancelled covers the run an
// operator answers while the shutdown is already running. The IPC server still
// accepts `axi respond` at that point (daemon.go stops runs before it closes
// the server), so a preserved run can start executing real git and agent work
// that nothing is waiting for, and the process would exit on top of it.
//
// The two control runs turn that race into a schedule: registerFakeRun's done
// channels are unbuffered, so a send on one completes exactly when the
// shutdown's wait receives it. The first send proves the preserve decision is
// already made, and the gate is answered before the second send releases the
// wait.
//
// The sweep belongs to the shutdown behind a drain, so the drain runs first.
func TestShutdown_PreservedRunThatLeavesItsGateIsCancelled(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	parked, parkedCtx, parkedDone := registerFakeRun(t, m, database, repo, "parked")
	if err := database.UpdateRunStatus(parked.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	parkRunAwaitingAgent(t, database, parked)

	// Drained first, and with nothing but the parked run registered: a drain
	// leaves a funnel goroutine on every run it saw, and one of those would
	// receive the control sends below instead of the shutdown wait.
	m.Drain(context.Background(), time.Millisecond)

	// Shutdown waits on its cancelled runs in sorted run-ID order.
	firstRun, _, firstDone := registerFakeRun(t, m, database, repo, "control-a")
	secondRun, _, secondDone := registerFakeRun(t, m, database, repo, "control-b")
	if firstRun.ID > secondRun.ID {
		firstDone, secondDone = secondDone, firstDone
	}

	unparked := make(chan error, 1)
	go func() {
		firstDone <- struct{}{}
		unparked <- database.CompleteRunAwaitingAgent(parked.ID, 0)
		secondDone <- struct{}{}
		// The sweep waits on what it cancels, the way the main wait does.
		close(parkedDone)
	}()

	m.Shutdown()

	select {
	case err := <-unparked:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("the gate was never answered; the control runs did not sequence the shutdown")
	}
	if context.Cause(parkedCtx) == nil {
		t.Fatal("a preserved run that left its gate during shutdown was not cancelled, so it kept working while the process exited")
	}
}

// TestShutdown_PlainStopKeepsARunThatLeavesItsGate is the other half: an
// ordinary `daemon stop` promises nothing about in-flight runs and has always
// preserved a parked one for the next start. A drain hands the operator a
// report and owes it the truth, so the sweep belongs to that path alone; here
// the run survives the stop untouched and stays resumable.
func TestShutdown_PlainStopKeepsARunThatLeavesItsGate(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	parked, parkedCtx, _ := registerFakeRun(t, m, database, repo, "parked")
	if err := database.UpdateRunStatus(parked.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	parkRunAwaitingAgent(t, database, parked)

	firstRun, _, firstDone := registerFakeRun(t, m, database, repo, "control-a")
	secondRun, _, secondDone := registerFakeRun(t, m, database, repo, "control-b")
	if firstRun.ID > secondRun.ID {
		firstDone, secondDone = secondDone, firstDone
	}

	unparked := make(chan error, 1)
	go func() {
		firstDone <- struct{}{}
		unparked <- database.CompleteRunAwaitingAgent(parked.ID, 0)
		secondDone <- struct{}{}
	}()

	start := time.Now()
	m.Shutdown()
	elapsed := time.Since(start)

	select {
	case err := <-unparked:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("the gate was never answered; the control runs did not sequence the shutdown")
	}
	if cause := context.Cause(parkedCtx); cause != nil {
		t.Fatalf("cancel cause = %v, want nil: a plain stop preserves a run that leaves its gate", cause)
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("Shutdown took %v, want a plain stop to skip the drain's second wait entirely", elapsed)
	}
	after, err := database.GetRun(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != types.RunRunning {
		t.Fatalf("run status = %s, want %s so the next start can pick it up", after.Status, types.RunRunning)
	}
}

// closeRecordingAgent is a no-op agent that remembers whether it was closed.
type closeRecordingAgent struct {
	agent.Agent
	closed atomic.Bool
}

func (a *closeRecordingAgent) Close() error {
	a.closed.Store(true)
	return a.Agent.Close()
}

// TestRefuseStartedRun_ReleasesEverythingTheRunAlreadyBuilt covers the run the
// refuse-new-runs latch turns away at registration, after startRun already
// resolved its agent. The pipeline goroutine that normally owns that teardown
// never launches, so nothing else closes the agent's subprocesses or the
// subscribers an attached TUI is waiting on, and the row must not read as a
// pipeline failure for work that never ran.
func TestRefuseStartedRun_ReleasesEverythingTheRunAlreadyBuilt(t *testing.T) {
	m, database, repo := newDrainTestManager(t)
	run, err := database.InsertRun(repo.ID, "feature", "deadbeef", "cafef00d")
	if err != nil {
		t.Fatal(err)
	}
	sub := subscribeDrained(t, m, run.ID)
	defer sub.Close()

	ag := &closeRecordingAgent{Agent: agent.NewNoop()}
	_, cancel := context.WithCancelCause(context.Background())
	refusal := fmt.Errorf("daemon is shutting down")

	m.refuseStartedRun(run.ID, ag, cancel, refusal)

	if !ag.closed.Load() {
		t.Fatal("the refused run's agent was never closed, so its subprocesses outlive the run")
	}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	for {
		event, ok := sub.Next(ctx)
		if !ok {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("the subscriber was never closed; it waits on a run that can no longer emit (last event %+v)", event)
		}
	}
	after, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != types.RunCancelled {
		t.Fatalf("run status = %s, want %s: nothing of the pipeline ran, so a failure is a false report", after.Status, types.RunCancelled)
	}
}

// TestDrainedAndAlive_SeparatesARecoverableDaemonFromAnExitingOne pins the two
// refusing states apart. An ordinary stop refuses runs on its way out and
// needs no operator; only a drain_only whose service-manager exit never landed
// does, and that is the one `daemon status` offers a restart for.
func TestDrainedAndAlive_SeparatesARecoverableDaemonFromAnExitingOne(t *testing.T) {
	m, _, _ := newDrainTestManager(t)

	if m.RefusingNewRuns() || m.DrainedAndAlive() {
		t.Fatalf("a fresh manager reports RefusingNewRuns=%v DrainedAndAlive=%v, want both false", m.RefusingNewRuns(), m.DrainedAndAlive())
	}

	m.Shutdown()

	if !m.RefusingNewRuns() {
		t.Fatal("a daemon that is shutting down still reports that it accepts runs")
	}
	if m.DrainedAndAlive() {
		t.Fatal("an ordinary stop reported the drained-and-alive state, so `daemon status` tells the operator to restart a daemon that is already exiting")
	}

	m.MarkDrainedAlive()

	if !m.DrainedAndAlive() {
		t.Fatal("a drain_only that left the process running did not report the state an operator has to recover from")
	}
}
