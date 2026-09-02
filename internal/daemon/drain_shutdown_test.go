package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

	drainDone := make(chan error, 1)
	go func() {
		var result ipc.ShutdownResult
		drainDone <- drainClient.Call(ipc.MethodShutdown, &ipc.ShutdownParams{Drain: true, DrainTimeoutMS: 3000}, &result)
	}()

	// Give the drain time to start blocking on the still-running review step.
	time.Sleep(200 * time.Millisecond)

	healthClient, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer healthClient.Close()

	var health ipc.HealthResult
	if err := healthClient.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, time.Second); err != nil {
		t.Fatalf("health call during drain: %v", err)
	}
	if health.Status != "ok" {
		t.Fatalf("health.Status = %q, want ok", health.Status)
	}

	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain never completed")
	}
}

// TestShutdown_ShutdownConcurrentWithDrainEndsItPromptly covers scenario 6: a
// shutdown arriving mid-drain (what a SIGTERM becomes) ends the drain rather
// than leaving it to sit out its deadline, and the drain still answers its
// caller.
//
// It does NOT exercise Drain's ctx-cancellation branch, and the name and
// comment must not claim otherwise. doShutdown runs mgr.Shutdown() before
// srv.Close(), and mgr.Shutdown() cancels every run and waits: the blocked
// run's goroutine exits, its done channel closes, and Drain returns through
// its normal funnel case before the connection context is ever cancelled.
// TestDrain_CancelledContextAbortsTheWait covers the ctx branch directly.
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

	// Give the drain time to start blocking on the still-running review step.
	// A second probing drain call can't be used to confirm this the way
	// TestShutdown_DrainDoesNotStarveOtherRPCs confirms health responsiveness:
	// a drain request always triggers a real shutdown afterward (see the
	// handler comment), so a probe would itself shut the daemon down.
	time.Sleep(200 * time.Millisecond)

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
	// The drain still answers its caller, and says it drained. It reports the
	// run as finished, which is what it observed: mgr.Shutdown cancelled the
	// run and its goroutine exited normally. Drain distinguishes only the
	// cuts it makes itself; a concurrent shutdown is the operator overriding
	// the drain, and the daemon exits either way.
	result := <-drainResult
	if !result.Drained {
		t.Fatalf("result = %+v, want Drained", result)
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
	// drain, and one connection serves its requests in sequence.
	results := make(chan ipc.ShutdownResult, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			client, dialErr := ipc.Dial(p.Socket())
			if dialErr != nil {
				errs <- dialErr
				return
			}
			defer client.Close()
			var result ipc.ShutdownResult
			if callErr := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{Drain: true, DrainTimeoutMS: 1000}, &result); callErr != nil {
				errs <- callErr
				return
			}
			results <- result
		}()
	}

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
	serviceCommandRunner = func(string, ...string) ([]byte, error) {
		if !launchctlRan {
			launchctlRan = true
			launchctlAfter = time.Since(start)
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
	if launchctlAfter < drainTimeout {
		t.Fatalf("the service manager was signalled %v in, before the %v drain finished", launchctlAfter, drainTimeout)
	}
	if _, ok := findInterruptedWire(outcome.Interrupted, pushResult.RunID); !ok {
		t.Fatalf("Interrupted = %v, want the never-finishing run reported", outcome.Interrupted)
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
