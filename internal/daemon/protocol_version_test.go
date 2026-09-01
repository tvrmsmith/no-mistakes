package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// legacyHealthResult mimics a pre-handshake daemon whose health response
// carries no protocol_version field at all.
type legacyHealthResult struct {
	Status string `json:"status"`
}

// shortTempRoot returns a paths.Paths rooted in a short-named temp
// directory. Unix domain socket paths are bound by a small OS limit
// (~104 bytes on macOS), and t.TempDir() embeds the full test name in the
// path, so tests that bind a real socket use this instead.
func shortTempRoot(t *testing.T) *paths.Paths {
	t.Helper()
	dir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := paths.WithRoot(dir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return p
}

// startFakeDaemon spins up a real ipc.Server on p.Socket() with the given
// health handler, and returns it so the caller can stop it via t.Cleanup.
func startFakeDaemon(t *testing.T, p *paths.Paths, health ipc.HandlerFunc) *ipc.Server {
	t.Helper()
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, health)
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		_ = srv.ServeReady()
	}()
	t.Cleanup(srv.Close)
	return srv
}

func TestDaemonIsRunningViaIPC_OldDaemonReportsVersionMismatch(t *testing.T) {
	p := shortTempRoot(t)
	startFakeDaemon(t, p, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return legacyHealthResult{Status: "ok"}, nil
	})

	alive, err := daemonIsRunningViaIPC(p)
	if alive {
		t.Fatal("expected alive == false against a version-mismatched daemon")
	}
	wantMsg := (&ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: 0, RemoteRole: ipc.RoleDaemon}).Error()
	if err == nil || err.Error() != wantMsg {
		t.Fatalf("err = %v, want %q", err, wantMsg)
	}
	if !ipc.IsVersionMismatch(err) {
		t.Fatalf("expected IsVersionMismatch(err) to be true, got %v", err)
	}
}

func TestEnsureDaemon_OldDaemonReturnsMismatchWithoutStartingReplacement(t *testing.T) {
	p := shortTempRoot(t)
	startFakeDaemon(t, p, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return legacyHealthResult{Status: "ok"}, nil
	})

	startCalled := false
	originalStart := daemonStart
	daemonStart = func(*paths.Paths) error {
		startCalled = true
		return nil
	}
	defer func() { daemonStart = originalStart }()

	err := EnsureDaemon(p)
	if startCalled {
		t.Fatal("expected daemonStart not to be called on a version mismatch")
	}
	wantMsg := (&ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: 0, RemoteRole: ipc.RoleDaemon}).Error()
	if err == nil || err.Error() != wantMsg {
		t.Fatalf("err = %v, want %q", err, wantMsg)
	}
}

func TestDaemonIsRunningViaIPC_MatchedVersionReportsAlive(t *testing.T) {
	p := shortTempRoot(t)
	startFakeDaemon(t, p, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.HealthResult{Status: "ok", ProtocolVersion: ipc.ProtocolVersion}, nil
	})

	alive, err := daemonIsRunningViaIPC(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !alive {
		t.Fatal("expected alive == true for a matched-version daemon")
	}
}

func TestRegisterHandlers_HealthReportsProtocolVersion(t *testing.T) {
	srv := ipc.NewServer()
	mgr := NewRunManager(nil, nil, nil)
	registerHandlers(srv, mgr, nil, func() {})

	dir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	if err := srv.Listen(sock); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		_ = srv.ServeReady()
	}()
	t.Cleanup(srv.Close)

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	var result ipc.HealthResult
	if err := client.Call(ipc.MethodHealth, &ipc.HealthParams{}, &result); err != nil {
		t.Fatalf("health call: %v", err)
	}
	if result.ProtocolVersion != ipc.ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", result.ProtocolVersion, ipc.ProtocolVersion)
	}
}

// TestStart_VersionMismatchReturnsErrorWithoutLaunching covers the fail-open
// hole in Start: a version-mismatched daemon is fully alive, so demoting it
// to "absent" and launching a replacement would either race the live daemon
// or spawn a duplicate. Start must surface the mismatch unchanged instead.
func TestStart_VersionMismatchReturnsErrorWithoutLaunching(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	// Safety net matching TestStartWithUnstubbedPathsDoesNotInvokeRealServiceCommands:
	// if the bug under test lets Start fall through to the detached re-exec
	// path, the child must exit immediately rather than recursively running
	// the whole test suite (see helpers_test.go TestMain).
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "1")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "200ms")

	mismatchErr := &ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: 0, RemoteRole: ipc.RoleDaemon}
	oldHealth := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return false, mismatchErr }
	defer func() { daemonHealthCheck = oldHealth }()

	listCalled := false
	oldList := daemonListDaemonProcesses
	daemonListDaemonProcesses = func() ([]daemonProcessInfo, error) { listCalled = true; return nil, nil }
	defer func() { daemonListDaemonProcesses = oldList }()

	err := Start(p)
	wantMsg := mismatchErr.Error()
	if err == nil || err.Error() != wantMsg {
		t.Fatalf("Start() err = %v, want %q", err, wantMsg)
	}
	if !ipc.IsVersionMismatch(err) {
		t.Fatalf("expected IsVersionMismatch(err) to be true, got %v", err)
	}
	if listCalled {
		t.Fatal("expected reconcileCollidingDaemons not to run against a version-mismatched daemon")
	}
}

// TestReconcileCollidingDaemons_VersionMismatchedStrayIsNeverKilled covers the
// same fail-open hole as TestStart_VersionMismatchReturnsErrorWithoutLaunching,
// one layer down: a stray daemon under an alternate root spelling that speaks
// an incompatible protocol version is fully alive. Treating the health-check
// error as "unhealthy" would fall through to the daemonKillPID loop and kill a
// healthy process instead of refusing like the plain-healthy-stray case.
func TestReconcileCollidingDaemons_VersionMismatchedStrayIsNeverKilled(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	p := paths.WithRoot(root) // canonical start uses the real path
	strayRoot := link         // stray was started via the symlink spelling

	oldList := daemonListDaemonProcesses
	daemonListDaemonProcesses = func() ([]daemonProcessInfo, error) {
		return []daemonProcessInfo{{PID: 99999, Root: strayRoot}}, nil
	}
	defer func() { daemonListDaemonProcesses = oldList }()

	mismatchErr := &ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: 0, RemoteRole: ipc.RoleDaemon}
	oldHealth := daemonHealthCheck
	daemonHealthCheck = func(p *paths.Paths) (bool, error) {
		if p.Root() == strayRoot {
			return false, mismatchErr
		}
		return false, nil
	}
	defer func() { daemonHealthCheck = oldHealth }()

	killed := 0
	oldKill := daemonKillPID
	daemonKillPID = func(int) error { killed++; return nil }
	defer func() { daemonKillPID = oldKill }()

	err := reconcileCollidingDaemons(p)
	if !errors.Is(err, errDaemonCollisionHealthy) {
		t.Fatalf("expected errDaemonCollisionHealthy, got %v", err)
	}
	if !ipc.IsVersionMismatch(err) {
		t.Fatalf("expected IsVersionMismatch(err) to be true, got %v", err)
	}
	if !strings.Contains(err.Error(), "Run 'no-mistakes daemon restart'") {
		t.Fatalf("expected collision error to carry the actionable restart guidance, got %v", err)
	}
	if killed != 0 {
		t.Fatalf("expected daemonKillPID to never be called against a version-mismatched stray, got %d calls", killed)
	}
}

// TestStart_InstallFailureBranchReturnsVersionMismatchWithoutDetachedFallback
// covers the same fail-open hole one step later than
// TestStart_VersionMismatchReturnsErrorWithoutLaunching: the daemon is absent
// at the top-of-function check, but comes up with an incompatible protocol
// version by the time installManagedService fails. The install-failure branch
// must re-check health, surface the mismatch unchanged, and never fall
// through to startDetachedDaemon.
func TestStart_InstallFailureBranchReturnsVersionMismatchWithoutDetachedFallback(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "1")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "200ms")

	mismatchErr := &ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: 0, RemoteRole: ipc.RoleDaemon}
	healthCalls := 0
	oldHealth := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		healthCalls++
		if healthCalls == 1 {
			// Top-of-function check: absent, not mismatched yet.
			return false, nil
		}
		// Second check, from the install-failure branch: a competitor came
		// up in between and speaks an incompatible protocol version.
		return false, mismatchErr
	}
	defer func() { daemonHealthCheck = oldHealth }()

	oldList := daemonListDaemonProcesses
	daemonListDaemonProcesses = func() ([]daemonProcessInfo, error) { return nil, nil }
	defer func() { daemonListDaemonProcesses = oldList }()

	oldInstall := daemonInstallManagedService
	daemonInstallManagedService = func(*paths.Paths) (bool, error) { return false, errors.New("install boom") }
	defer func() { daemonInstallManagedService = oldInstall }()

	detachedCalled := false
	oldDetached := daemonStartDetachedDaemon
	daemonStartDetachedDaemon = func(*paths.Paths) error { detachedCalled = true; return nil }
	defer func() { daemonStartDetachedDaemon = oldDetached }()

	err := Start(p)
	wantMsg := mismatchErr.Error()
	if err == nil || err.Error() != wantMsg {
		t.Fatalf("Start() err = %v, want %q", err, wantMsg)
	}
	if !ipc.IsVersionMismatch(err) {
		t.Fatalf("expected IsVersionMismatch(err) to be true, got %v", err)
	}
	if detachedCalled {
		t.Fatal("expected startDetachedDaemon not to be called after the install-failure branch detects a version-mismatched daemon")
	}
	if healthCalls < 2 {
		t.Fatalf("expected daemonHealthCheck to be called at least twice (top guard, then install-failure branch), got %d", healthCalls)
	}
}

// TestStart_PreFallbackRecheckSkipsTheFallbackWhenADaemonCameUp covers the
// plain-alive half of the hoisted pre-fallback re-check. Its mismatch half is
// covered above; without this one, a re-check that only ever answered "mismatch
// or absent" would still pass while `Start` launched a second daemon on top of
// a healthy one that appeared while the managed install was failing.
func TestStart_PreFallbackRecheckSkipsTheFallbackWhenADaemonCameUp(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "1")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "200ms")

	healthCalls := 0
	oldHealth := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		healthCalls++
		// Absent at the top guard; healthy by the time the install fails.
		return healthCalls > 1, nil
	}
	defer func() { daemonHealthCheck = oldHealth }()

	oldList := daemonListDaemonProcesses
	daemonListDaemonProcesses = func() ([]daemonProcessInfo, error) { return nil, nil }
	defer func() { daemonListDaemonProcesses = oldList }()

	installs := 0
	oldInstall := daemonInstallManagedService
	daemonInstallManagedService = func(*paths.Paths) (bool, error) {
		installs++
		return false, errors.New("install boom")
	}
	defer func() { daemonInstallManagedService = oldInstall }()

	detachedCalled := false
	oldDetached := daemonStartDetachedDaemon
	daemonStartDetachedDaemon = func(*paths.Paths) error { detachedCalled = true; return nil }
	defer func() { daemonStartDetachedDaemon = oldDetached }()

	if err := Start(p); err != nil {
		t.Fatalf("Start() = %v, want nil when a healthy daemon is observed before the fallback", err)
	}
	if detachedCalled {
		t.Fatal("expected startDetachedDaemon not to be called once the re-check saw a healthy daemon")
	}
	// The managed cleanup on this path uninstalls the service definition to
	// clear the way for a detached fallback that no longer happens. Returning
	// success without restoring it silently costs the user login auto-start.
	if installs < 2 {
		t.Fatalf("managed service definition installs = %d, want the definition restored before Start reports success", installs)
	}
}

// TestDaemonPIDRecordMatchesProcess_SkewedDaemonCountsAsAlive covers the
// legacy-record branch's use of the alive bool alone. A skewed daemon answers
// health, so it is running; reading only the bool made the record unvalidatable
// and left `daemon stop` unable to prove the process it must wait for exited.
func TestDaemonPIDRecordMatchesProcess_SkewedDaemonCountsAsAlive(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	mismatchErr := &ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: 0, RemoteRole: ipc.RoleDaemon}
	oldHealth := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return false, mismatchErr }
	defer func() { daemonHealthCheck = oldHealth }()

	actualStart := time.Now().Add(-time.Minute).UTC()
	matches, err := daemonPIDRecordMatchesProcess(p, daemonPIDFile{PID: 4242}, actualStart)
	if err != nil {
		t.Fatalf("daemonPIDRecordMatchesProcess() err = %v, want nil against a live skewed daemon", err)
	}
	if !matches {
		t.Fatal("expected the legacy PID record to validate against a live version-skewed daemon")
	}

	upgraded, err := readDaemonPIDFile(p.PIDFile())
	if err != nil {
		t.Fatalf("read upgraded pid file: %v", err)
	}
	if upgraded.PID != 4242 || upgraded.StartedAt.IsZero() {
		t.Fatalf("expected the legacy record to be upgraded with a start time, got %+v", upgraded)
	}
}

// TestDaemonIsRunningViaIPC_NewerDaemonReportsVersionMismatch drives the real
// detector against a peer that declares a version rather than omitting one.
// Every other skew test in this package exercises the version-0 legacy shape,
// so a detector narrowed to "reported == 0" would still pass them all while
// treating a future daemon as compatible.
func TestDaemonIsRunningViaIPC_NewerDaemonReportsVersionMismatch(t *testing.T) {
	p := shortTempRoot(t)
	startFakeDaemon(t, p, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.HealthResult{Status: "ok", ProtocolVersion: ipc.ProtocolVersion + 1}, nil
	})

	alive, err := daemonIsRunningViaIPC(p)
	if alive {
		t.Fatal("expected alive == false against a newer-version daemon")
	}
	if !ipc.IsVersionMismatch(err) {
		t.Fatalf("expected IsVersionMismatch(err) to be true, got %v", err)
	}
	wantMsg := (&ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: ipc.ProtocolVersion + 1, RemoteRole: ipc.RoleDaemon}).Error()
	if err.Error() != wantMsg {
		t.Fatalf("err = %q, want %q", err.Error(), wantMsg)
	}
}

// TestWaitForDaemonStart_SkewedDaemonReturnsImmediately pins the early exit: a
// skewed daemon has answered, so the verdict is settled and no further waiting
// can change it. Without the early return the caller burns the whole readiness
// budget before reporting a mismatch it already knew about.
func TestWaitForDaemonStart_SkewedDaemonReturnsImmediately(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "10s")

	mismatchErr := &ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: 0, RemoteRole: ipc.RoleDaemon}
	checks := 0
	oldHealth := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		checks++
		return false, mismatchErr
	}
	defer func() { daemonHealthCheck = oldHealth }()

	started := time.Now()
	err := waitForDaemonStart(p, 0, time.Time{})
	elapsed := time.Since(started)

	if !ipc.IsVersionMismatch(err) {
		t.Fatalf("expected a version mismatch error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected an immediate return on a settled skew, waited %v", elapsed)
	}
	if checks != 1 {
		t.Fatalf("expected exactly one health check before returning, got %d", checks)
	}
}

// TestStartDetachedDaemon_SkewStillKillsAndReapsTheChild pins the reap on the
// skew exit. The skewed daemon that answered health is not the child launched
// here (that one loses the singleton lock), so abandoning the start without
// killing the child leaks a live process that races rollback or a fallback
// service for the socket the caller is about to rebuild - the exact leak the
// timeout path already reaps.
func TestStartDetachedDaemon_SkewStillKillsAndReapsTheChild(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_START_DAEMON", "1")
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "block")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "10s")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "5ms")

	mismatchErr := &ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: 0, RemoteRole: ipc.RoleDaemon}
	oldHealth := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return false, mismatchErr }
	t.Cleanup(func() { daemonHealthCheck = oldHealth })

	oldStartTime := daemonProcessStartTime
	startedPID := 0
	var childStartedAt time.Time
	daemonProcessStartTime = func(pid int) (time.Time, error) {
		observed, err := oldStartTime(pid)
		if err == nil {
			startedPID, childStartedAt = pid, observed
		}
		return observed, err
	}
	t.Cleanup(func() { daemonProcessStartTime = oldStartTime })

	started := time.Now()
	err := startDetachedDaemon(p)
	if !ipc.IsVersionMismatch(err) {
		t.Fatalf("startDetachedDaemon error = %v, want a version mismatch", err)
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("expected the settled skew to short-circuit the readiness budget, waited %v", elapsed)
	}
	assertTestDaemonNotRunning(t, startedPID, childStartedAt)
}

// TestHookRepliesCarryTheProtocolVersionStamp pins the stamp the git hooks read
// off the replies they are already waiting for. A dropped stamp decodes as
// version 0, so ipc.DaemonVersionMismatch would report a skew and the
// pre-receive hook would refuse every push against a perfectly current daemon.
func TestHookRepliesCarryTheProtocolVersionStamp(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "hook-stamp-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var admit ipc.AdmitPushResult
	if err := client.Call(ipc.MethodAdmitPush, &ipc.AdmitPushParams{Gate: p.RepoDir("hook-stamp-repo")}, &admit); err != nil {
		t.Fatalf("admit_push: %v", err)
	}
	if err := ipc.DaemonVersionMismatch(admit.ProtocolVersion); err != nil {
		t.Fatalf("pre-receive hook would refuse this push: %v", err)
	}

	var push ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("hook-stamp-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &push); err != nil {
		t.Fatalf("push_received: %v", err)
	}
	if err := ipc.DaemonVersionMismatch(push.ProtocolVersion); err != nil {
		t.Fatalf("post-receive hook would report a false skew: %v", err)
	}
	waitForRunTerminalState(t, d, push.RunID)
}

func TestEnsureDaemon_MatchedVersionReturnsNilWithoutStarting(t *testing.T) {
	p := shortTempRoot(t)
	startFakeDaemon(t, p, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.HealthResult{Status: "ok", ProtocolVersion: ipc.ProtocolVersion}, nil
	})

	startCalled := false
	originalStart := daemonStart
	daemonStart = func(*paths.Paths) error {
		startCalled = true
		return nil
	}
	defer func() { daemonStart = originalStart }()

	if err := EnsureDaemon(p); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if startCalled {
		t.Fatal("expected daemonStart not to be called when the daemon is already alive")
	}
}

// Self-update replaces the executable and then resets the daemon, so the CLI
// supervising the launch is the OLD binary and the child it starts is the NEW
// one. The child answers on the newer protocol, which the readiness probe sees
// as a skew. Treating that as a foreign daemon reaps the very daemon the update
// installed and reports failure, leaving the user with no daemon at all.
func TestStartDetachedDaemon_SkewFromTheChildThisCallLaunchedIsReadiness(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_START_DAEMON", "1")
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "block")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "3s")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "10ms")

	newerDaemon := &ipc.VersionMismatchError{
		Local:      ipc.ProtocolVersion,
		Remote:     ipc.ProtocolVersion + 1,
		RemoteRole: ipc.RoleDaemon,
	}
	serving := false
	oldHealth := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		if !serving {
			return false, nil
		}
		return false, newerDaemon
	}
	t.Cleanup(func() { daemonHealthCheck = oldHealth })

	oldStartTime := daemonProcessStartTime
	startedPID := 0
	daemonProcessStartTime = func(pid int) (time.Time, error) {
		startedAt, err := oldStartTime(pid)
		if err == nil && startedPID == 0 {
			startedPID = pid
			// The daemon publishes its PID record before it serves IPC, which
			// is what makes the answering peer identifiable at all.
			if writeErr := writeDaemonPIDFile(p.PIDFile(), daemonPIDFile{PID: pid, StartedAt: startedAt.UTC()}); writeErr != nil {
				t.Errorf("publish daemon pid record: %v", writeErr)
			}
			serving = true
		}
		return startedAt, err
	}
	t.Cleanup(func() { daemonProcessStartTime = oldStartTime })
	t.Cleanup(func() {
		if startedPID > 0 {
			if proc, err := os.FindProcess(startedPID); err == nil {
				_ = proc.Kill()
			}
		}
	})

	if err := startDetachedDaemon(p); err != nil {
		t.Fatalf("startDetachedDaemon = %v, want readiness for the newer child this call launched", err)
	}
	if startedPID <= 0 {
		t.Fatal("test did not capture the launched child pid")
	}
	running, err := daemonProcessRunning(startedPID)
	if err != nil {
		t.Fatalf("check child pid %d: %v", startedPID, err)
	}
	if !running {
		t.Fatalf("the newly installed daemon child pid %d was reaped", startedPID)
	}
}

// The managed path has no process handle, so the child is identified by the PID
// record it published: a live process that started no earlier than this launch.
func TestWaitForManagedDaemonStart_SkewFromTheChildThisCallLaunchedIsReadiness(t *testing.T) {
	p, child := managedSkewFixture(t, time.Now().UTC())

	if err := waitForDaemonStart(p, 0, time.Time{}); err != nil {
		t.Fatalf("waitForDaemonStart = %v, want readiness for the newer child this call launched", err)
	}
	running, err := daemonProcessRunning(child)
	if err != nil {
		t.Fatalf("check child pid %d: %v", child, err)
	}
	if !running {
		t.Fatalf("the newly installed daemon child pid %d was reaped", child)
	}
}

// A daemon that was already running long before this launch is a foreign peer,
// not the child that was just installed, so its skew stays terminal.
func TestWaitForManagedDaemonStart_SkewFromAForeignDaemonStaysAMismatch(t *testing.T) {
	p, _ := managedSkewFixture(t, time.Now().UTC().Add(-time.Hour))

	err := waitForDaemonStart(p, 0, time.Time{})
	if !ipc.IsVersionMismatch(err) {
		t.Fatalf("waitForDaemonStart = %v, want the mismatch from a daemon this call did not launch", err)
	}
}

// managedSkewFixture publishes a PID record for a live helper child claiming the
// given start time, and makes the socket answer with a newer-daemon skew.
func managedSkewFixture(t *testing.T, recordedStart time.Time) (*paths.Paths, int) {
	t.Helper()
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "3s")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "10ms")

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "NM_DAEMON_HELPER_PROCESS=block")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if err := writeDaemonPIDFile(p.PIDFile(), daemonPIDFile{PID: cmd.Process.Pid, StartedAt: recordedStart}); err != nil {
		t.Fatal(err)
	}

	newerDaemon := &ipc.VersionMismatchError{
		Local:      ipc.ProtocolVersion,
		Remote:     ipc.ProtocolVersion + 1,
		RemoteRole: ipc.RoleDaemon,
	}
	oldHealth := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return false, newerDaemon }
	t.Cleanup(func() { daemonHealthCheck = oldHealth })

	return p, cmd.Process.Pid
}
