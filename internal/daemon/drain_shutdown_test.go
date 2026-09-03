package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestShutdown_BareRequestReturnsImmediatelyWithoutDrain pins that an absent
// Drain field means today's behavior exactly: the daemon starts shutting
// down in the background and the RPC returns right away.
func TestShutdown_BareRequestReturnsImmediatelyWithoutDrain(t *testing.T) {
	p, _ := startTestDaemon(t)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer client.Close()

	var result ipc.ShutdownResult
	start := time.Now()
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, &result); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	elapsed := time.Since(start)

	if !result.OK {
		t.Fatalf("result.OK = false, want true")
	}
	if result.Drained {
		t.Fatalf("result.Drained = true, want false")
	}
	if len(result.Finished) != 0 || len(result.Interrupted) != 0 {
		t.Fatalf("result = %+v, want empty Finished/Interrupted", result)
	}
	if elapsed > time.Second {
		t.Fatalf("shutdown took %v, want an immediate return", elapsed)
	}
}

// TestShutdown_DrainWithNoInFlightRunsReportsDrainedEmpty covers scenario 2.
func TestShutdown_DrainWithNoInFlightRunsReportsDrainedEmpty(t *testing.T) {
	p, _ := startTestDaemon(t)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer client.Close()

	var result ipc.ShutdownResult
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{Drain: true, DrainTimeoutMS: 2000}, &result); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if !result.OK || !result.Drained {
		t.Fatalf("result = %+v, want OK and Drained", result)
	}
	if len(result.Interrupted) != 0 {
		t.Fatalf("Interrupted = %v, want empty", result.Interrupted)
	}
}

// TestShutdown_DrainCutsCIMonitorAndReportsIt covers scenario 3: a drain
// classifies a run whose only active step is CI as a CI monitor, cuts it
// immediately, and reports it over the wire rather than waiting.
func TestShutdown_DrainCutsCIMonitorAndReportsIt(t *testing.T) {
	started := make(chan struct{})
	ciStep := &mockSlowStep{name: types.StepCI, started: started}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{ciStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "drain-ci-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("drain-ci-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult); err != nil {
		t.Fatalf("push received: %v", err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("ci step never started")
	}
	// A CI monitor is an active CI step AND a PR to monitor; the real step
	// writes the URL before it starts watching checks.
	if err := d.UpdateRunPRURL(pushResult.RunID, "https://github.com/user/project/pull/7"); err != nil {
		t.Fatalf("set pr url: %v", err)
	}

	var result ipc.ShutdownResult
	start := time.Now()
	// The bound is a generous fraction of the deadline, not a tight one: the
	// cut run still has to unwind (head reconciliation, terminal row) before
	// its done channel closes, and a loaded CI machine makes that slow. What
	// the test proves is that the drain did not sit out its own deadline.
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{Drain: true, DrainTimeoutMS: 20000}, &result); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed >= 10*time.Second {
		t.Fatalf("drain took %v, want a prompt cut of the CI monitor rather than waiting out the 20s deadline", elapsed)
	}
	if !result.OK || !result.Drained {
		t.Fatalf("result = %+v, want OK and Drained", result)
	}
	if len(result.Interrupted) != 1 {
		t.Fatalf("Interrupted = %v, want exactly one entry", result.Interrupted)
	}
	entry := result.Interrupted[0]
	if entry.RunID != pushResult.RunID || entry.Reason != ipc.DrainInterruptedCIMonitor {
		t.Fatalf("Interrupted[0] = %+v, want run %s reason %s", entry, pushResult.RunID, ipc.DrainInterruptedCIMonitor)
	}

	// What the wire says the drain did must match what the run's own row says
	// happened to it. A cut CI monitor is ci_monitor_interrupted, not failed:
	// the PR is still open and the operator is told so, and a `failed` row
	// here would show up in axi and the TUI as work that broke.
	waitForRunStatus(t, d, pushResult.RunID, types.RunCIMonitorInterrupted)
}

// waitForRunStatus polls a run's terminal status. The drain reports the run as
// cut the moment its goroutine exits; the terminal row is written on that same
// unwind, so a direct read can race it.
func waitForRunStatus(t *testing.T, d *db.DB, runID string, want types.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last types.RunStatus
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatalf("get run %s: %v", runID, err)
		}
		if run == nil {
			t.Fatalf("run %s not found", runID)
		}
		last = run.Status
		if last == want {
			if run.Error == nil || *run.Error != types.RunCIMonitorDrainedReason {
				t.Fatalf("run %s error = %v, want %q", runID, run.Error, types.RunCIMonitorDrainedReason)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s status = %s, want %s", runID, last, want)
}

// TestShutdown_DrainReportsDeadlineCutForNonCIRun covers scenario 4: a
// non-CI in-flight run that never finishes on its own is reported as a
// deadline cut, and the daemon still exits.
func TestShutdown_DrainReportsDeadlineCutForNonCIRun(t *testing.T) {
	started := make(chan struct{})
	step := &mockSlowStep{name: types.StepReview, started: started}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "drain-deadline-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("drain-deadline-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult); err != nil {
		t.Fatalf("push received: %v", err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("review step never started")
	}

	var result ipc.ShutdownResult
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{Drain: true, DrainTimeoutMS: 100}, &result); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if !result.OK || !result.Drained {
		t.Fatalf("result = %+v, want OK and Drained", result)
	}
	if len(result.Interrupted) != 1 {
		t.Fatalf("Interrupted = %v, want exactly one entry", result.Interrupted)
	}
	entry := result.Interrupted[0]
	if entry.RunID != pushResult.RunID || entry.Reason != ipc.DrainInterruptedDeadline {
		t.Fatalf("Interrupted[0] = %+v, want run %s reason %s", entry, pushResult.RunID, ipc.DrainInterruptedDeadline)
	}
}

// TestShutdown_DrainDoesNotStarveOtherRPCs covers scenario 5: a drain
// blocking one connection's handler goroutine must not stall another
// connection, because each connection runs its own goroutine in
// internal/ipc/server.go.
func TestShutdown_DrainDoesNotStarveOtherRPCs(t *testing.T) {
	started := make(chan struct{})
	step := &mockSlowStep{name: types.StepReview, started: started}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "drain-concurrent-repo")

	pushClient, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer pushClient.Close()

	var pushResult ipc.PushReceivedResult
	if err := pushClient.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("drain-concurrent-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult); err != nil {
		t.Fatalf("push received: %v", err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("review step never started")
	}

	drainClient, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer drainClient.Close()

	healthClient, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer healthClient.Close()

	// The drain blocks its handler for its whole 2s deadline on a run that
	// never finishes. Health is probed continuously across that window and
	// each response is timestamped, so the proof is an overlap in time rather
	// than a sleep guessing when the drain reached the handler: if the server
	// serialized handlers, every probe would only answer after the drain
	// returned.
	const drainWindow = 2 * time.Second
	drainStart := time.Now()
	drainDone := make(chan error, 1)
	go func() {
		var result ipc.ShutdownResult
		drainDone <- drainClient.Call(ipc.MethodShutdown, &ipc.ShutdownParams{Drain: true, DrainTimeoutMS: drainWindow.Milliseconds()}, &result)
	}()

	probeStop := make(chan struct{})
	type probe struct{ at time.Time }
	probes := make(chan probe, 256)
	probeErr := make(chan error, 1)
	go func() {
		defer close(probes)
		// A probe failing after the drain returned is the daemon shutting
		// down, which is expected; only a failure while the drain still holds
		// its handler is the starvation this test is about.
		fail := func(err error) {
			select {
			case <-probeStop:
			default:
				probeErr <- err
			}
		}
		for {
			select {
			case <-probeStop:
				return
			default:
			}
			var health ipc.HealthResult
			if err := healthClient.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, 5*time.Second); err != nil {
				fail(err)
				return
			}
			if health.Status != "ok" {
				fail(fmt.Errorf("health.Status = %q, want ok", health.Status))
				return
			}
			probes <- probe{at: time.Now()}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
	case err := <-probeErr:
		t.Fatalf("health call during drain: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("drain never completed")
	}
	drainReturned := time.Now()
	close(probeStop)

	if drainElapsed := drainReturned.Sub(drainStart); drainElapsed < drainWindow {
		t.Fatalf("the drain returned after %v, before its %v deadline; it never blocked, so this proves nothing about starvation", drainElapsed, drainWindow)
	}
	during := 0
	for pr := range probes {
		if pr.at.After(drainStart) && pr.at.Before(drainReturned) {
			during++
		}
	}
	if during < 3 {
		t.Fatalf("%d health responses landed while the drain held its handler, want at least 3", during)
	}
}

// waitForDrainInFlight blocks until a drain is running inside the daemon's
// shutdown handler, the positive signal a test needs before it can race
// something against that drain. It reads that state instead of provoking it:
// Drain sets the refuse-new-runs latch as its very first act, before it
// classifies anything, and the health check reports the latch. A probe that
// sent a drain request of its own could win the handler's
// draining.CompareAndSwap against a real drain already on the wire, which
// would answer the real drain with Drained:false and end it on the spot, so
// the observation must stay read-only. drainDone reports a drain that already
// returned, which is a failure rather than something to wait out.
func waitForDrainInFlight(t *testing.T, socket string, drainDone <-chan error) {
	t.Helper()
	client, err := ipc.Dial(socket)
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer client.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-drainDone:
			t.Fatalf("the drain returned (err=%v) before it was ever observed in flight", err)
		default:
		}
		var health ipc.HealthResult
		if err := client.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, 5*time.Second); err != nil {
			t.Fatalf("health probe: %v", err)
		}
		if health.Drained {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no drain was in flight after 10s")
}

// TestShutdown_ShutdownConcurrentWithDrainEndsItPromptly covers scenario 6: a
// shutdown arriving mid-drain (what a SIGTERM becomes) ends the drain rather
// than leaving it to sit out its deadline, the drain still answers its caller,
// and it reports the run the shutdown killed as stopped mid-flight rather than
// as finished work.
//
// It reaches Drain through mgr.Shutdown's own signal, not the connection
// context: doShutdown runs mgr.Shutdown() before srv.Close(), so by the time
// ctx is cancelled the runs are already gone. TestDrain_CancelledContextAborts
// TheWait covers the ctx branch directly.
//
// A real OS signal is also out of reach here: startTestDaemonWithSteps runs
// the daemon's RunWithOptions loop as a goroutine inside the test process, so
// a SIGTERM would tear down the whole test binary. A bare (non-drain)
// shutdown call reaches the same sync.Once-guarded doShutdown closure a
// signal handler does.
func TestShutdown_ShutdownConcurrentWithDrainEndsItPromptly(t *testing.T) {
	started := make(chan struct{})
	step := &mockSlowStep{name: types.StepReview, started: started}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "drain-signal-repo")

	pushClient, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer pushClient.Close()

	var pushResult ipc.PushReceivedResult
	if err := pushClient.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("drain-signal-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult); err != nil {
		t.Fatalf("push received: %v", err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("review step never started")
	}

	drainClient, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer drainClient.Close()

	drainStart := time.Now()
	drainDone := make(chan error, 1)
	drainResult := make(chan ipc.ShutdownResult, 1)
	go func() {
		var result ipc.ShutdownResult
		err := drainClient.Call(ipc.MethodShutdown, &ipc.ShutdownParams{Drain: true, DrainTimeoutMS: 60000}, &result)
		drainResult <- result
		drainDone <- err
	}()

	waitForDrainInFlight(t, p.Socket(), drainDone)

	// The signal-equivalent: a bare shutdown request on a third connection,
	// funneling through the same doShutdown as a real SIGTERM would.
	sigClient, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer sigClient.Close()
	if err := sigClient.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil); err != nil {
		t.Fatalf("signal-equivalent shutdown: %v", err)
	}

	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain never returned after the signal-equivalent shutdown")
	}
	if elapsed := time.Since(drainStart); elapsed >= 10*time.Second {
		t.Fatalf("drain took %v, want it aborted promptly rather than waiting out its 60s deadline", elapsed)
	}
	// The drain still answers its caller, and says it drained. The run it was
	// waiting on is reported as stopped by the shutdown: mgr.Shutdown killed
	// it mid-step, so counting it among the runs that finished before the
	// daemon stopped would tell the operator the opposite of what happened,
	// and exit 0 over it.
	result := <-drainResult
	if !result.Drained {
		t.Fatalf("result = %+v, want Drained", result)
	}
	for _, id := range result.Finished {
		if id == pushResult.RunID {
			t.Fatalf("Finished = %v, want it to exclude run %s that the shutdown killed", result.Finished, pushResult.RunID)
		}
	}
	entry, ok := findInterruptedWire(result.Interrupted, pushResult.RunID)
	if !ok || entry.Reason != ipc.DrainInterruptedShutdown {
		t.Fatalf("Interrupted = %v, want a %s entry for %s", result.Interrupted, ipc.DrainInterruptedShutdown, pushResult.RunID)
	}
}

// TestStop_UnchangedSignatureAndBehavior covers scenario 7: Stop keeps its
// exact signature and today's plain-shutdown behavior, delegating to
// StopWithOptions with zero-value options.
func TestStop_UnchangedSignatureAndBehavior(t *testing.T) {
	p, _ := startTestDaemon(t)

	if err := Stop(p); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if _, err := os.Stat(p.Socket()); !os.IsNotExist(err) {
		t.Fatalf("socket stat after Stop() = %v, want not-exist", err)
	}
}

// TestStopWithOptions_DrainReturnsOutcome covers scenario 8.
func TestStopWithOptions_DrainReturnsOutcome(t *testing.T) {
	p, _ := startTestDaemon(t)

	outcome, err := StopWithOptions(p, StopOptions{Drain: true, DrainTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("StopWithOptions() error = %v", err)
	}
	if !outcome.Drained {
		t.Fatalf("outcome = %+v, want Drained", outcome)
	}
	if _, err := os.Stat(p.Socket()); !os.IsNotExist(err) {
		t.Fatalf("socket stat after StopWithOptions() = %v, want not-exist", err)
	}
}

// TestShutdown_ConcurrentDrainsOnlyOneDrains covers the single-drain guard:
// two overlapping drain requests must not both run a drain over the same run
// set. The loser gets Drained:false and the CLI reports that rather than
// claiming a clean drain of zero runs.
func TestShutdown_ConcurrentDrainsOnlyOneDrains(t *testing.T) {
	started := make(chan struct{})
	step := &mockSlowStep{name: types.StepReview, started: started}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "drain-twice-repo")

	pushClient, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer pushClient.Close()

	var pushResult ipc.PushReceivedResult
	if err := pushClient.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("drain-twice-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult); err != nil {
		t.Fatalf("push received: %v", err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("review step never started")
	}

	// Each drain needs its own connection: the handler blocks for the whole
	// drain, and one connection serves its requests in sequence. Both are
	// dialed before either call goes out, so the losing drain cannot arrive
	// after the winner already tore the daemon down and fail on the dial
	// instead of on the single-drain guard. Both are DrainOnly for the same
	// reason: a plain drain triggers a shutdown the moment it finishes.
	results := make(chan ipc.ShutdownResult, 2)
	errs := make(chan error, 2)
	clients := make([]*ipc.Client, 0, 2)
	for i := 0; i < 2; i++ {
		client, dialErr := ipc.Dial(p.Socket())
		if dialErr != nil {
			t.Fatalf("dial daemon: %v", dialErr)
		}
		defer client.Close()
		clients = append(clients, client)
	}
	launch := make(chan struct{})
	for _, client := range clients {
		go func(client *ipc.Client) {
			<-launch
			var result ipc.ShutdownResult
			if callErr := client.CallWithTimeout(ipc.MethodShutdown, &ipc.ShutdownParams{Drain: true, DrainTimeoutMS: 3000, DrainOnly: true}, &result, 30*time.Second); callErr != nil {
				errs <- callErr
				return
			}
			results <- result
		}(client)
	}
	close(launch)

	drained := 0
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if !result.OK {
				t.Fatalf("result = %+v, want OK", result)
			}
			if result.Drained {
				drained++
			}
		case err := <-errs:
			t.Fatalf("concurrent drain: %v", err)
		case <-time.After(15 * time.Second):
			t.Fatal("concurrent drains never both returned")
		}
	}
	if drained != 1 {
		t.Fatalf("%d of 2 concurrent drains reported Drained, want exactly 1", drained)
	}
}

// TestStopWithOptions_ManagedServiceDrainsBeforeSignalling covers the default
// install on macOS and Linux. A service manager's stop carries no drain
// semantics and waits the daemon out, so the drain RPC has to go out before
// it. The test proves the ordering by timing: the drain waits out its own
// deadline on a run that never finishes, and the launchctl invocation cannot
// have happened before that.
func TestStopWithOptions_ManagedServiceDrainsBeforeSignalling(t *testing.T) {
	started := make(chan struct{})
	step := &mockSlowStep{name: types.StepReview, started: started}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "drain-managed-repo")

	pushClient, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	var pushResult ipc.PushReceivedResult
	if err := pushClient.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("drain-managed-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult); err != nil {
		t.Fatalf("push received: %v", err)
	}
	pushClient.Close()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("review step never started")
	}

	// Present a managed install: a plist on disk under a stubbed home, with
	// every service command captured instead of run.
	cleanup := stubServiceRuntime(t)
	defer cleanup()
	home := t.TempDir()
	runtimeGOOS = "darwin"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	if err := os.MkdirAll(filepath.Dir(launchAgentPath(p)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launchAgentPath(p), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	const drainTimeout = 500 * time.Millisecond
	start := time.Now()
	var launchctlAfter time.Duration
	var launchctlRan bool
	var daemonAliveAtStop bool
	serviceCommandRunner = func(string, ...string) ([]byte, error) {
		if !launchctlRan {
			launchctlRan = true
			launchctlAfter = time.Since(start)
			// The daemon must still be reachable for this stop to be what
			// ends it. TestShutdown_DrainOnlyDrainsWithoutExiting is the
			// regression for the DrainOnly flag itself; this only checks that
			// the composed managed path did not hand the service manager an
			// already-dead daemon.
			if probe, dialErr := ipc.Dial(p.Socket()); dialErr == nil {
				var health ipc.HealthResult
				daemonAliveAtStop = probe.Call(ipc.MethodHealth, &ipc.HealthParams{}, &health) == nil
				// Stand in for what launchctl bootout would do: actually stop
				// the daemon. Without this the in-process daemon under test
				// outlives StopWithOptions, which unlinks its socket on the
				// way out and leaves nothing able to shut it down.
				_ = probe.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, &ipc.ShutdownResult{})
				probe.Close()
			}
		}
		return nil, nil
	}
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return false, nil }

	outcome, err := StopWithOptions(p, StopOptions{Drain: true, DrainTimeout: drainTimeout})
	if err != nil {
		t.Fatalf("StopWithOptions on a managed service: %v", err)
	}
	if !launchctlRan {
		t.Fatal("the managed service was never stopped")
	}
	if !outcome.Drained {
		t.Fatalf("outcome = %+v, want Drained: the drain must reach the daemon before the service manager signals it", outcome)
	}
	if !daemonAliveAtStop {
		t.Fatal("the daemon had already exited when the service manager was told to stop it; the pre-drain must be drain-only so the supervisor owns the exit")
	}
	if launchctlAfter < drainTimeout {
		t.Fatalf("the service manager was signalled %v in, before the %v drain finished", launchctlAfter, drainTimeout)
	}
	if _, ok := findInterruptedWire(outcome.Interrupted, pushResult.RunID); !ok {
		t.Fatalf("Interrupted = %v, want the never-finishing run reported", outcome.Interrupted)
	}
}

// TestShutdown_DrainOnlyDrainsWithoutExiting covers the managed-service path's
// requirement. Under launchd KeepAlive or systemd Restart=always, a daemon that
// exits the instant its drain finishes is respawned into the window before the
// service manager's own stop lands, and that replacement daemon accepts new
// runs - the exact thing the drain was asked to prevent. DrainOnly drains,
// keeps the refuse-new-runs latch set, and leaves the exit to the supervisor.
func TestShutdown_DrainOnlyDrainsWithoutExiting(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockSlowStep{name: types.StepReview}}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "drain-only-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer client.Close()

	var result ipc.ShutdownResult
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{Drain: true, DrainTimeoutMS: 2000, DrainOnly: true}, &result); err != nil {
		t.Fatalf("drain-only shutdown: %v", err)
	}
	if !result.OK || !result.Drained {
		t.Fatalf("result = %+v, want OK and Drained", result)
	}

	// Still accepting NEW connections: the supervisor, not the daemon, owns
	// the exit. This has to be a fresh dial, not another call on the
	// connection above - a shutting-down daemon serves its already-open
	// connections to completion while its listener is gone, so reusing this
	// one would pass against the very bug the test exists for. Sleep first to
	// give a self-exiting daemon time to actually go away.
	time.Sleep(500 * time.Millisecond)
	fresh, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial after drain-only: %v; want the daemon still listening for its service manager to stop", err)
	}
	defer fresh.Close()
	var health ipc.HealthResult
	if err := fresh.Call(ipc.MethodHealth, &ipc.HealthParams{}, &health); err != nil {
		t.Fatalf("health after drain-only: %v; want the daemon still running", err)
	}
	// Alive and drained are different states, and only the health answer can
	// tell them apart. Without it, a supervisor that never performed the exit
	// leaves an operator with a daemon that `daemon status` calls running and
	// that refuses every push.
	if !health.Drained {
		t.Fatalf("health = %+v, want Drained: a daemon left alive by drain_only accepts nothing", health)
	}
	// The narrower bit, and the one `daemon status` offers a restart for. An
	// ordinary stop sets Drained too and needs no operator; only this state
	// lasts until somebody acts on it.
	if !health.DrainedAlive {
		t.Fatalf("health = %+v, want DrainedAlive: only this tells an operator the daemon is stuck rather than exiting", health)
	}

	// Still refusing work: a drained daemon that kept serving must not accept
	// a push in the meantime.
	var pushResult ipc.PushReceivedResult
	err = fresh.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("drain-only-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult)
	if err == nil {
		t.Fatalf("push after drain-only was accepted as run %s, want it refused", pushResult.RunID)
	}
	// The refusal is the one message an operator in this state actually sees,
	// so it has to name the way out. "daemon is shutting down" describes a
	// daemon that is about to be gone, which this one is not.
	if !strings.Contains(err.Error(), "daemon restart") {
		t.Fatalf("push refusal = %v, want it to name `no-mistakes daemon restart` as the recovery", err)
	}
}

func findInterruptedWire(runs []ipc.DrainInterruptedRun, id string) (ipc.DrainInterruptedRun, bool) {
	for _, run := range runs {
		if run.RunID == id {
			return run, true
		}
	}
	return ipc.DrainInterruptedRun{}, false
}
