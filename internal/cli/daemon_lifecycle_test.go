package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDaemonStopRefusesWithActiveRunsAndListsThem(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		stopCalled = true
		return daemon.StopOutcome{}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop")
	if err == nil {
		t.Fatal("daemon stop should refuse while active runs exist")
	}
	if stopCalled {
		t.Fatal("daemon stop should not stop the daemon after refusing")
	}
	for _, want := range []string{
		"refusing daemon stop",
		"2 active pipeline runs",
		"feature-a",
		"aaa111",
		"feature-b",
		"bbb222",
		"--force",
	} {
		if !strings.Contains(out+err.Error(), want) {
			t.Fatalf("daemon stop refusal should contain %q, got output %q error %v", want, out, err)
		}
	}
}

func TestDaemonStopForceOverridesActiveRunGuard(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		stopCalled = true
		return daemon.StopOutcome{}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop", "--force")
	if err != nil {
		t.Fatalf("daemon stop --force failed: %v\n%s", err, out)
	}
	if !stopCalled {
		t.Fatal("daemon stop --force should stop the daemon")
	}
	if !strings.Contains(out, "FORCE: daemon stop") {
		t.Fatalf("force output should be loud, got %q", out)
	}
}

func TestDaemonRestartRefusesWithActiveRuns(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	stopCalled := false
	startCalled := false
	prevStop := daemonStopFn
	prevStart := daemonStartFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		stopCalled = true
		return daemon.StopOutcome{}, nil
	}
	daemonStartFn = func(*paths.Paths) error {
		startCalled = true
		return nil
	}
	t.Cleanup(func() {
		daemonStopFn = prevStop
		daemonStartFn = prevStart
	})

	out, err := executeCmd("daemon", "restart")
	if err == nil {
		t.Fatal("daemon restart should refuse while active runs exist")
	}
	if stopCalled || startCalled {
		t.Fatalf("daemon restart should not stop/start after refusing; stop=%t start=%t", stopCalled, startCalled)
	}
	if !strings.Contains(out+err.Error(), "refusing daemon restart") || !strings.Contains(out+err.Error(), "feature-a") {
		t.Fatalf("daemon restart refusal should list active runs, got output %q error %v", out, err)
	}
}

func TestLifecycleCommandsWriteCallerAttributionToCLILog(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	prevStop := daemonStopFn
	prevStart := daemonStartFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) { return daemon.StopOutcome{}, nil }
	daemonStartFn = func(*paths.Paths) error { return nil }
	t.Cleanup(func() {
		daemonStopFn = prevStop
		daemonStartFn = prevStart
	})

	out, err := executeCmd("daemon", "stop", "--force")
	if err != nil {
		t.Fatalf("daemon stop --force failed: %v\n%s", err, out)
	}
	out, err = executeCmd("daemon", "restart", "--force")
	if err != nil {
		t.Fatalf("daemon restart --force failed: %v\n%s", err, out)
	}
	out, err = executeCmd("update", "--force")
	if err != nil {
		t.Fatalf("update --force failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(filepath.Join(nmHome, "logs", "cli.log"))
	if err != nil {
		t.Fatalf("read cli.log: %v", err)
	}
	log := string(data)
	for _, want := range []string{
		"lifecycle FORCE command=daemon.stop",
		"lifecycle FORCE command=daemon.restart",
		"lifecycle FORCE command=update",
		"force=true",
		"pid=",
		"ppid=",
		"parent_cmdline=",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("cli.log should contain %q, got %q", want, log)
		}
	}
}

func createLifecycleGuardRuns(t *testing.T, p *paths.Paths) {
	t.Helper()
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	repo, err := database.InsertRepo("/tmp/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := database.InsertRun(repo.ID, "feature-a", "aaa111", "000"); err != nil {
		t.Fatalf("insert pending run: %v", err)
	}
	running, err := database.InsertRun(repo.ID, "feature-b", "bbb222", "000")
	if err != nil {
		t.Fatalf("insert running run: %v", err)
	}
	if err := database.UpdateRunStatus(running.ID, types.RunRunning); err != nil {
		t.Fatalf("mark running: %v", err)
	}
}

func TestDaemonStopDrainWithForceIsRejected(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		stopCalled = true
		return daemon.StopOutcome{}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop", "--drain", "--force")
	if err == nil {
		t.Fatal("daemon stop --drain --force should be rejected")
	}
	if !strings.Contains(err.Error(), "--drain") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("rejection should name both flags, got %v", err)
	}
	if stopCalled {
		t.Fatalf("daemonStopFn should not be called, output %q", out)
	}
}

func TestDaemonStopDrainTimeoutReachesDaemon(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	var gotOpts daemon.StopOptions
	prevStop := daemonStopFn
	daemonStopFn = func(_ *paths.Paths, opts daemon.StopOptions) (daemon.StopOutcome, error) {
		gotOpts = opts
		return daemon.StopOutcome{Drained: true}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	if _, err := executeCmd("daemon", "stop", "--drain", "--drain-timeout", "30s"); err != nil {
		t.Fatalf("daemon stop --drain failed: %v", err)
	}
	if !gotOpts.Drain || gotOpts.DrainTimeout != 30*time.Second {
		t.Fatalf("expected Drain=true DrainTimeout=30s, got %+v", gotOpts)
	}
}

func TestDaemonStopDrainDefaultTimeoutIsTenMinutes(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	var gotOpts daemon.StopOptions
	prevStop := daemonStopFn
	daemonStopFn = func(_ *paths.Paths, opts daemon.StopOptions) (daemon.StopOutcome, error) {
		gotOpts = opts
		return daemon.StopOutcome{Drained: true}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	if _, err := executeCmd("daemon", "stop", "--drain"); err != nil {
		t.Fatalf("daemon stop --drain failed: %v", err)
	}
	if !gotOpts.Drain || gotOpts.DrainTimeout != 10*time.Minute {
		t.Fatalf("expected Drain=true DrainTimeout=10m, got %+v", gotOpts)
	}
}

func TestDaemonStopDrainTimeoutNonPositiveIsRejected(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		stopCalled = true
		return daemon.StopOutcome{}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	_, err := executeCmd("daemon", "stop", "--drain", "--drain-timeout", "0s")
	if err == nil {
		t.Fatal("daemon stop --drain --drain-timeout 0s should be rejected")
	}
	if stopCalled {
		t.Fatal("daemonStopFn should not be called for a non-positive drain timeout")
	}
}

func TestDaemonStopDrainGuardDoesNotRefuse(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		stopCalled = true
		return daemon.StopOutcome{Drained: true}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop", "--drain")
	if err != nil {
		t.Fatalf("daemon stop --drain should proceed with active runs: %v\n%s", err, out)
	}
	if !stopCalled {
		t.Fatal("daemon stop --drain should call daemonStopFn")
	}
	if strings.Contains(out, "FORCE") {
		t.Fatalf("drain output should not contain FORCE, got %q", out)
	}
	if strings.Contains(out, "refusing") {
		t.Fatalf("drain output should not contain refusing, got %q", out)
	}
	if !strings.Contains(out, "2 active pipeline runs") {
		t.Fatalf("drain output should mention the number of active runs, got %q", out)
	}
	// The guard must not promise to wait on all of them. A parked run is
	// released and a CI monitor is cut, so "will wait on 2 active pipeline
	// runs" tells the operator to expect something the drain will not do.
	if strings.Contains(out, "will wait on") {
		t.Fatalf("drain guard must not promise to wait on every active run, got %q", out)
	}
}

// TestDaemonStopDrainEndedByShutdownExitsNonZeroWithoutBlamingTheDeadline
// covers the reason a drain reports when the daemon's own shutdown ends it: a
// signal, or a concurrent stop. It still exits nonzero (a run was stopped
// mid-flight), but pointing the operator at the deadline would send them to
// raise --drain-timeout for a problem raising it cannot fix.
func TestDaemonStopDrainEndedByShutdownExitsNonZeroWithoutBlamingTheDeadline(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		return daemon.StopOutcome{
			Drained: true,
			Interrupted: []ipc.DrainInterruptedRun{
				{RunID: "run-9", Branch: "feature-y", Reason: ipc.DrainInterruptedShutdown},
			},
		}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop", "--drain")
	if err == nil {
		t.Fatalf("a run stopped before it finished should exit nonzero, got output %q", out)
	}
	if !strings.Contains(err.Error(), "run-9") {
		t.Fatalf("error should name the run that did not finish, got %v", err)
	}
	if strings.Contains(out, "deadline") || strings.Contains(err.Error(), "deadline") {
		t.Fatalf("a shutdown-ended drain must not be reported as a deadline cut, got output %q and error %v", out, err)
	}
	if !strings.Contains(out, "shutdown") {
		t.Fatalf("output should say the drain ended by daemon shutdown, got %q", out)
	}
}

func TestDaemonStopCIMonitorInterruptionExitsZero(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		return daemon.StopOutcome{
			Drained:  true,
			Finished: []string{"run-1"},
			Interrupted: []ipc.DrainInterruptedRun{
				{RunID: "run-2", Branch: "feature-x", Reason: ipc.DrainInterruptedCIMonitor},
			},
		}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop", "--drain")
	if err != nil {
		t.Fatalf("a ci_monitor interruption alone should exit 0, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "run-2") {
		t.Fatalf("output should name the interrupted run, got %q", out)
	}
	if !strings.Contains(out, "PR") || !strings.Contains(out, "open") {
		t.Fatalf("output should say the PR is still open, got %q", out)
	}
}

func TestDaemonStopDeadlineInterruptionExitsNonZero(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		return daemon.StopOutcome{
			Drained: true,
			Interrupted: []ipc.DrainInterruptedRun{
				{RunID: "run-3", Branch: "feature-y", Reason: ipc.DrainInterruptedDeadline},
			},
		}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop", "--drain")
	if err == nil {
		t.Fatalf("a deadline interruption should exit nonzero, output %q", out)
	}
	if !strings.Contains(err.Error(), "run-3") {
		t.Fatalf("error should name the interrupted run, got %v", err)
	}
}

func TestDaemonRestartDrainStartsDaemonEvenAfterDeadlineCut(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	startCalled := false
	prevStop := daemonStopFn
	prevStart := daemonStartFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		return daemon.StopOutcome{
			Drained: true,
			Interrupted: []ipc.DrainInterruptedRun{
				{RunID: "run-4", Branch: "feature-z", Reason: ipc.DrainInterruptedDeadline},
			},
		}, nil
	}
	daemonStartFn = func(*paths.Paths) error {
		startCalled = true
		return nil
	}
	t.Cleanup(func() {
		daemonStopFn = prevStop
		daemonStartFn = prevStart
	})

	out, err := executeCmd("daemon", "restart", "--drain")
	if err == nil {
		t.Fatalf("restart --drain should still return the deadline error, output %q", out)
	}
	if !startCalled {
		t.Fatal("restart --drain should start the daemon even after a deadline cut")
	}
	if !strings.Contains(out, "daemon restarted") {
		t.Fatalf("restart --drain should still print the restarted line, got %q", out)
	}
	if !strings.Contains(err.Error(), "run-4") {
		t.Fatalf("restart --drain error should name the interrupted run, got %v", err)
	}
}

func TestDaemonStopPlainIsUnchanged(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	var gotOpts daemon.StopOptions
	gotOptsSet := false
	prevStop := daemonStopFn
	daemonStopFn = func(_ *paths.Paths, opts daemon.StopOptions) (daemon.StopOutcome, error) {
		gotOpts = opts
		gotOptsSet = true
		return daemon.StopOutcome{}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop")
	if err != nil {
		t.Fatalf("plain daemon stop failed: %v\n%s", err, out)
	}
	if !gotOptsSet || (gotOpts != daemon.StopOptions{}) {
		t.Fatalf("plain daemon stop should call daemonStopFn with a zero StopOptions, got %+v", gotOpts)
	}
	if !strings.Contains(out, "daemon stopped") {
		t.Fatalf("plain daemon stop should print the usual success line, got %q", out)
	}
}

// TestDaemonStopDrainReportsOutcomeEvenWhenTheStopErrors pins that a drain
// report is not thrown away by a later failure. selfexec returns a populated
// outcome alongside an error on purpose (a drain that ran in full, then a
// wait-for-exit timeout), and printing only the error hides which run was
// interrupted and that a PR was left open.
func TestDaemonStopDrainReportsOutcomeEvenWhenTheStopErrors(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		return daemon.StopOutcome{
			Drained: true,
			Interrupted: []ipc.DrainInterruptedRun{
				{RunID: "run-7", Branch: "feature-w", Reason: ipc.DrainInterruptedCIMonitor},
			},
		}, fmt.Errorf("wait for exit: daemon still running")
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop", "--drain")
	if err == nil {
		t.Fatalf("daemon stop --drain should surface the stop failure, got output %q", out)
	}
	if !strings.Contains(out, "run-7") {
		t.Fatalf("output should still report the drain, got %q", out)
	}
}

// TestDaemonRestartDrainReportsOutcomeEvenWhenTheStopErrors is the restart half.
func TestDaemonRestartDrainReportsOutcomeEvenWhenTheStopErrors(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	prevStop := daemonStopFn
	prevStart := daemonStartFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		return daemon.StopOutcome{
			Drained: true,
			Interrupted: []ipc.DrainInterruptedRun{
				{RunID: "run-8", Branch: "feature-v", Reason: ipc.DrainInterruptedDeadline},
			},
		}, fmt.Errorf("wait for exit: daemon still running")
	}
	daemonStartFn = func(*paths.Paths) error { return nil }
	t.Cleanup(func() {
		daemonStopFn = prevStop
		daemonStartFn = prevStart
	})

	out, err := executeCmd("daemon", "restart", "--drain")
	if err == nil {
		t.Fatalf("daemon restart --drain should surface the stop failure, got output %q", out)
	}
	if !strings.Contains(out, "run-8") {
		t.Fatalf("output should still report the drain, got %q", out)
	}
}

// TestDaemonStopDrainReportsWhenTheDaemonDidNotDrain pins that the CLI gates
// on what the daemon did (outcome.Drained), not on what the operator asked
// for (--drain). A new CLI against a pre-drain daemon, and the loser of two
// concurrent drains, both come back Drained:false after every run was
// cancelled outright; reporting either as a clean drain of zero runs claims
// the opposite of what happened.
func TestDaemonStopDrainReportsWhenTheDaemonDidNotDrain(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		return daemon.StopOutcome{Drained: false}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop", "--drain")
	if err == nil {
		t.Fatalf("daemon stop --drain should fail when the daemon did not drain, got output %q", out)
	}
	if !strings.Contains(err.Error(), "did not drain") {
		t.Fatalf("error should say the daemon did not drain, got %v", err)
	}
	if strings.Contains(out, "0 run(s) finished") {
		t.Fatalf("output must not claim a clean drain, got %q", out)
	}
}

// TestDaemonStopDrainWithNoDaemonRunningExitsZero separates "there was nothing
// to drain" from "the drain failed". Stopping an already-stopped daemon is a
// success without --drain, and adding --drain must not turn that same no-op
// into a nonzero exit - a stop script that always passes --drain would then
// fail on its second run.
func TestDaemonStopDrainWithNoDaemonRunningExitsZero(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		return daemon.StopOutcome{NoDaemon: true}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop", "--drain")
	if err != nil {
		t.Fatalf("daemon stop --drain with no daemon running should succeed, got %v (output %q)", err, out)
	}
	if !strings.Contains(out, "no daemon was running") {
		t.Fatalf("output should say there was no daemon to drain, got %q", out)
	}
	if strings.Contains(out, "daemon stopped") {
		t.Fatalf("output must not also claim a daemon was stopped, got %q", out)
	}
	if strings.Contains(out, "cancelled outright") {
		t.Fatalf("output must not claim runs were cancelled when there was no daemon, got %q", out)
	}
}

// TestDaemonRestartDrainReportsWhenTheDaemonDidNotDrain is the restart half:
// the daemon still restarts (the operator asked for a restart), but the exit
// status stays honest.
func TestDaemonRestartDrainReportsWhenTheDaemonDidNotDrain(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	started := false
	prevStop := daemonStopFn
	prevStart := daemonStartFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		return daemon.StopOutcome{Drained: false}, nil
	}
	daemonStartFn = func(*paths.Paths) error { started = true; return nil }
	t.Cleanup(func() {
		daemonStopFn = prevStop
		daemonStartFn = prevStart
	})

	out, err := executeCmd("daemon", "restart", "--drain")
	if err == nil {
		t.Fatalf("daemon restart --drain should fail when the daemon did not drain, got output %q", out)
	}
	if !started {
		t.Fatal("daemon restart --drain should still restart the daemon")
	}
	if !strings.Contains(out, "daemon restarted") {
		t.Fatalf("output should confirm the restart, got %q", out)
	}
}

// TestDaemonRestartDrainWithForceIsRejected covers the restart side of the
// flag validation. Restart wires drainStopOptions independently of stop, so a
// divergence there would otherwise go unnoticed.
func TestDaemonRestartDrainWithForceIsRejected(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	stopCalled := false
	prevStop := daemonStopFn
	prevStart := daemonStartFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		stopCalled = true
		return daemon.StopOutcome{Drained: true}, nil
	}
	daemonStartFn = func(*paths.Paths) error { return nil }
	t.Cleanup(func() {
		daemonStopFn = prevStop
		daemonStartFn = prevStart
	})

	out, err := executeCmd("daemon", "restart", "--drain", "--force")
	if err == nil {
		t.Fatal("daemon restart --drain --force should be rejected")
	}
	if !strings.Contains(err.Error(), "--drain") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("rejection should name both flags, got %v", err)
	}
	if stopCalled {
		t.Fatalf("daemonStopFn should not be called, output %q", out)
	}
}

// TestDaemonStopDrainTimeoutWithoutDrainIsRejected pins that a timeout the
// command would silently drop is refused instead of accepted and ignored.
func TestDaemonStopDrainTimeoutWithoutDrainIsRejected(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		stopCalled = true
		return daemon.StopOutcome{}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	_, err := executeCmd("daemon", "stop", "--drain-timeout", "30s")
	if err == nil {
		t.Fatal("--drain-timeout without --drain should be rejected")
	}
	if !strings.Contains(err.Error(), "--drain") {
		t.Fatalf("rejection should point at --drain, got %v", err)
	}
	if stopCalled {
		t.Fatal("daemonStopFn should not be called")
	}
}

// TestDaemonLifecycleRefusalOffersDrain pins that the guard's refusal names
// the non-destructive alternative, not only --force.
func TestDaemonLifecycleRefusalOffersDrain(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		return daemon.StopOutcome{}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	_, err := executeCmd("daemon", "stop")
	if err == nil {
		t.Fatal("daemon stop should refuse while active runs exist")
	}
	if !strings.Contains(err.Error(), "--drain") {
		t.Fatalf("refusal should offer --drain, got %v", err)
	}
}

// TestDrainLifecycleInvocationIsDistinguishableInTheCLILog pins the forensic
// record. A drain cuts CI monitors short, forcibly stops anything past its
// deadline, and passes the active-runs guard a bare stop would have refused,
// so its audit line must not read identically to an ordinary stop.
func TestDrainLifecycleInvocationIsDistinguishableInTheCLILog(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths, daemon.StopOptions) (daemon.StopOutcome, error) {
		return daemon.StopOutcome{Drained: true}, nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	if out, err := executeCmd("daemon", "stop", "--drain"); err != nil {
		t.Fatalf("daemon stop --drain failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(filepath.Join(nmHome, "logs", "cli.log"))
	if err != nil {
		t.Fatalf("read cli.log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "command=daemon.stop") || !strings.Contains(log, "drain=true") {
		t.Fatalf("cli.log should record the drain, got %q", log)
	}
}

// TestDaemonStatusReportsADrainedDaemon covers the state a managed-service
// drain can leave behind: the drain finished, the daemon deliberately stayed
// alive for its service manager to stop, and that stop never landed. The
// process answers health, so "daemon running" is true and useless; every push
// is refused and nothing in the status said why.
func TestDaemonStatusReportsADrainedDaemon(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())

	tests := []struct {
		name    string
		drained bool
		want    string
		absent  string
	}{
		{
			name:    "a working daemon",
			drained: false,
			want:    "daemon running",
			absent:  "drained",
		},
		{
			name:    "a drained daemon",
			drained: true,
			want:    "daemon drained, not accepting new runs",
			absent:  "daemon running",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prevRunning, prevDrained := daemonIsRunningFn, daemonIsDrainedFn
			daemonIsRunningFn = func(*paths.Paths) (bool, error) { return true, nil }
			daemonIsDrainedFn = func(*paths.Paths) (bool, error) { return tc.drained, nil }
			t.Cleanup(func() {
				daemonIsRunningFn = prevRunning
				daemonIsDrainedFn = prevDrained
			})

			out, err := executeCmd("daemon", "status")
			if err != nil {
				t.Fatalf("daemon status failed: %v\n%s", err, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("daemon status output = %q, want it to say %q", out, tc.want)
			}
			if strings.Contains(out, tc.absent) {
				t.Fatalf("daemon status output = %q, want it to omit %q", out, tc.absent)
			}
		})
	}
}

// TestDaemonStatusOnADrainedDaemonNamesTheRecovery keeps the status line
// actionable: the only way out of the drained-and-alive state is a restart.
func TestDaemonStatusOnADrainedDaemonNamesTheRecovery(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())

	prevRunning, prevDrained := daemonIsRunningFn, daemonIsDrainedFn
	daemonIsRunningFn = func(*paths.Paths) (bool, error) { return true, nil }
	daemonIsDrainedFn = func(*paths.Paths) (bool, error) { return true, nil }
	t.Cleanup(func() {
		daemonIsRunningFn = prevRunning
		daemonIsDrainedFn = prevDrained
	})

	out, err := executeCmd("daemon", "status")
	if err != nil {
		t.Fatalf("daemon status failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "daemon restart") {
		t.Fatalf("daemon status output = %q, want it to name the restart that recovers a drained daemon", out)
	}
}
