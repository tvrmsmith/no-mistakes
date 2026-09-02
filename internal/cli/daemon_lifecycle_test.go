package cli

import (
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
		return daemon.StopOutcome{}, nil
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
		return daemon.StopOutcome{}, nil
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
		return daemon.StopOutcome{}, nil
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
