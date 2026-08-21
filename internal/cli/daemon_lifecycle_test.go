package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle/lifecycletest"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDaemonStopRefusesWithActiveRunsAndListsThem(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
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
	p := paths.WithRoot(nmHome)
	createLifecycleGuardParkedRun(t, p)
	createLifecycleGuardSingleRunningRun(t, p)

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop", "--force")
	if err != nil {
		t.Fatalf("daemon stop --force failed: %v\n%s", err, out)
	}
	if !stopCalled {
		t.Fatal("daemon stop --force should stop the daemon")
	}
	for _, want := range []string{
		"FORCE: daemon stop",
		// Only the run the force actually disrupts is counted.
		"1 active pipeline run is in progress",
		"feature-running",
		// The parked run is preserved by this stop, so the promise belongs here.
		"will be preserved and resumed",
		"feature-parked",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("force output should contain %q, got %q", want, out)
		}
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
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
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
	daemonStopFn = func(*paths.Paths) error { return nil }
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

func TestDaemonStopAllowsGateParkedRunsWithoutForce(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardParkedRun(t, paths.WithRoot(nmHome))

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop")
	if err != nil {
		t.Fatalf("daemon stop should allow gate-parked runs without --force: %v\n%s", err, out)
	}
	if !stopCalled {
		t.Fatal("daemon stop should stop the daemon when only gate-parked runs are active")
	}
	if !strings.Contains(out, "will be preserved and resumed") {
		t.Fatalf("daemon stop output should mention preserved parked runs, got %q", out)
	}
}

func TestDaemonStopStillRefusesWithNonParkedActiveRun(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	createLifecycleGuardParkedRun(t, p)
	createLifecycleGuardSingleRunningRun(t, p)

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop")
	if err == nil {
		t.Fatal("daemon stop should refuse while a non-parked active run exists")
	}
	if stopCalled {
		t.Fatal("daemon stop should not stop the daemon after refusing")
	}
	if !strings.Contains(out+err.Error(), "1 active pipeline run is in progress") {
		t.Fatalf("daemon stop refusal should count only the non-parked run, got output %q error %v", out, err)
	}
	// Nothing is preserved by a refusal: the daemon keeps running, so the
	// promise must not be printed on that path.
	if strings.Contains(out+err.Error(), "will be preserved and resumed") {
		t.Fatalf("refusal should not promise preservation, got output %q error %v", out, err)
	}
}

// TestDaemonStopFailureDoesNotPromisePreservation proves the preservation
// promise tracks the action that honors it: when the stop itself fails the
// daemon keeps running, so nothing is preserved and nothing may be promised.
func TestDaemonStopFailureDoesNotPromisePreservation(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardParkedRun(t, paths.WithRoot(nmHome))

	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths) error {
		return errors.New("stop failed")
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop")
	if err == nil {
		t.Fatal("daemon stop should fail when the daemon cannot be stopped")
	}
	if strings.Contains(out+err.Error(), "will be preserved and resumed") {
		t.Fatalf("a failed stop must not promise preservation, got output %q error %v", out, err)
	}
}

// TestDaemonRestartAllowsGateParkedRunsWithoutForce mirrors the stop path on
// restart, which has its own guard call and its own notice print.
func TestDaemonRestartAllowsGateParkedRunsWithoutForce(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardParkedRun(t, paths.WithRoot(nmHome))

	stopCalled, startCalled := false, false
	prevStop, prevStart := daemonStopFn, daemonStartFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
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
	if err != nil {
		t.Fatalf("daemon restart should allow gate-parked runs without --force: %v\n%s", err, out)
	}
	if !stopCalled || !startCalled {
		t.Fatalf("daemon restart should stop and start the daemon; stop=%t start=%t", stopCalled, startCalled)
	}
	if !strings.Contains(out, "will be preserved and resumed") {
		t.Fatalf("daemon restart output should mention preserved parked runs, got %q", out)
	}
}

// TestDaemonRestartFailureDoesNotPromisePreservation covers the extra failure
// mode restart has between the guard and the promise: a daemon that stops but
// never comes back is not resuming anything, so nothing may be promised.
func TestDaemonRestartFailureDoesNotPromisePreservation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stopErr  error
		startErr error
	}{
		{name: "stop fails", stopErr: errors.New("stop failed")},
		{name: "start fails", startErr: errors.New("start failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nmHome := t.TempDir()
			t.Setenv("NM_HOME", nmHome)
			createLifecycleGuardParkedRun(t, paths.WithRoot(nmHome))

			stopCalled := false
			prevStop, prevStart := daemonStopFn, daemonStartFn
			daemonStopFn = func(*paths.Paths) error {
				stopCalled = true
				return tc.stopErr
			}
			daemonStartFn = func(*paths.Paths) error { return tc.startErr }
			t.Cleanup(func() {
				daemonStopFn = prevStop
				daemonStartFn = prevStart
			})

			out, err := executeCmd("daemon", "restart")
			if err == nil {
				t.Fatal("daemon restart should fail when the daemon cannot be restarted")
			}
			if !stopCalled {
				t.Fatal("daemon restart should have attempted the stop")
			}
			if strings.Contains(out+err.Error(), "will be preserved and resumed") {
				t.Fatalf("a failed restart must not promise preservation, got output %q error %v", out, err)
			}
		})
	}
}

// TestDaemonStopCountsStepPlanDriftedParkedRunAsBlocking proves stop applies the
// same resumability check update does. The executable that comes back is
// whatever is installed at that moment, which can have been replaced out of
// band since the run parked, so a run recorded under another layout is neither
// exempted from the guard nor promised preservation.
func TestDaemonStopCountsStepPlanDriftedParkedRunAsBlocking(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	createLifecycleGuardParkedRun(t, p)
	setLifecycleParkedRunStepPlan(t, p, []types.StepName{types.StepReview})

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop")
	if err == nil {
		t.Fatal("daemon stop should refuse a parked run recorded under a different step plan")
	}
	if stopCalled {
		t.Fatal("daemon stop should not stop the daemon after refusing")
	}
	if !strings.Contains(out+err.Error(), "1 active pipeline run is in progress") {
		t.Fatalf("refusal should count the drifted parked run, got output %q error %v", out, err)
	}
	if strings.Contains(out+err.Error(), "will be preserved and resumed") {
		t.Fatalf("a run that cannot be proven resumable must not be promised preservation, got output %q error %v", out, err)
	}
}

// TestDaemonStopRefusesAParkedRunRecoveryCouldNotResume proves the guard's
// promise is as strong as recovery's own rules. The gate row carries a stale
// agent PID, which startup recovery reads as an incomplete gate and refuses,
// so promising preservation here would hand the operator a run the next start
// would terminally fail and whose worktree it would then delete.
func TestDaemonStopRefusesAParkedRunRecoveryCouldNotResume(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	parked := createLifecycleGuardParkedRun(t, p)
	lifecycletest.SetGateAgentPID(t, p, parked.RunID, 4242)

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop")
	if err == nil {
		t.Fatal("daemon stop should refuse a parked run recovery could not resume")
	}
	if stopCalled {
		t.Fatal("daemon stop should not stop the daemon after refusing")
	}
	if !strings.Contains(out+err.Error(), "1 active pipeline run is in progress") {
		t.Fatalf("refusal should count the uncorroborated parked run, got output %q error %v", out, err)
	}
	if strings.Contains(out+err.Error(), "will be preserved and resumed") {
		t.Fatalf("a run recovery would fail must not be promised preservation, got output %q error %v", out, err)
	}
}

// TestDaemonStopRefusesAParkedRunWhoseWorktreeIsGone covers the other half of
// the same corroboration: recovery needs the worktree at the run's head, so a
// parked run without one is blocking rather than preserved.
func TestDaemonStopRefusesAParkedRunWhoseWorktreeIsGone(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	parked := createLifecycleGuardParkedRun(t, p)
	if err := os.RemoveAll(parked.WorkDir); err != nil {
		t.Fatal(err)
	}

	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths) error { return nil }
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop")
	if err == nil {
		t.Fatal("daemon stop should refuse a parked run whose worktree is gone")
	}
	if strings.Contains(out+err.Error(), "will be preserved and resumed") {
		t.Fatalf("a run with no worktree must not be promised preservation, got output %q error %v", out, err)
	}
}

func setLifecycleParkedRunStepPlan(t *testing.T, p *paths.Paths, plan []types.StepName) {
	t.Helper()
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	runs, err := database.GetActiveRuns()
	if err != nil {
		t.Fatalf("get active runs: %v", err)
	}
	for _, run := range runs {
		if run.Branch != "feature-parked" {
			continue
		}
		if err := database.SetRunStepPlan(run.ID, plan); err != nil {
			t.Fatalf("set step plan: %v", err)
		}
		return
	}
	t.Fatal("parked run not found")
}

func createLifecycleGuardParkedRun(t *testing.T, p *paths.Paths) lifecycletest.ParkedRun {
	t.Helper()
	return lifecycletest.SeedResumableParkedRun(t, p, "/tmp/project-parked", "feature-parked", steps.AllSteps())
}

func createLifecycleGuardSingleRunningRun(t *testing.T, p *paths.Paths) {
	t.Helper()
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	repo, err := database.InsertRepo("/tmp/project-running", "git@github.com:user/project-running.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	running, err := database.InsertRun(repo.ID, "feature-running", "ddd444", "000")
	if err != nil {
		t.Fatalf("insert running run: %v", err)
	}
	if err := database.UpdateRunStatus(running.ID, types.RunRunning); err != nil {
		t.Fatalf("mark running: %v", err)
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
