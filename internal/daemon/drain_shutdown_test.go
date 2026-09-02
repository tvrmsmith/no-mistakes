package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
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

	var result ipc.ShutdownResult
	start := time.Now()
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{Drain: true, DrainTimeoutMS: 5000}, &result); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed >= 2*time.Second {
		t.Fatalf("drain took %v, want a prompt cut of the CI monitor rather than waiting out the 5s deadline", elapsed)
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

// TestShutdown_SignalDuringDrainAbortsItPromptly covers scenario 6. There is
// no way to fire a real OS signal at this test: startTestDaemonWithSteps runs
// the daemon's RunWithOptions loop as a goroutine inside the test process
// itself, so a real SIGTERM would tear down the whole test binary rather than
// just the daemon. A concurrent bare (non-drain) shutdown call exercises the
// identical path a signal takes instead: both go through the same
// sync.Once-guarded doShutdown closure in runWithOptionsLocked, which cancels
// every connection's context (including the one blocked inside mgr.Drain) and
// closes the server.
func TestShutdown_SignalDuringDrainAbortsItPromptly(t *testing.T) {
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
	go func() {
		var result ipc.ShutdownResult
		drainDone <- drainClient.Call(ipc.MethodShutdown, &ipc.ShutdownParams{Drain: true, DrainTimeoutMS: 60000}, &result)
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
