package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

// shortNMHome points NM_HOME at a short-named temp dir. Unix domain socket
// paths have a small OS limit, so these tests cannot use t.TempDir() (which
// embeds the full test name in the path).
func shortNMHome(t *testing.T) *paths.Paths {
	t.Helper()
	nmHome, err := os.MkdirTemp("", "vmtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(nmHome) })
	t.Setenv("NM_HOME", nmHome)

	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return p
}

// startLegacyDaemonSocket serves health responses that carry no
// protocol_version field, the shape a pre-handshake daemon returns, so the
// CLI sees a live daemon that reads as version 0. gate_context is
// version-exempt on both sides of a skew, so this daemon still answers the
// containment query.
func startLegacyDaemonSocket(t *testing.T) *paths.Paths {
	t.Helper()
	return startLegacyDaemonSocketWithGateContext(t, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.GateContextResult{Nested: true, AgentDescendant: true, RunID: "outer-run"}, nil
	})
}

// startLegacyDaemonSocketWithGateContext is startLegacyDaemonSocket with a
// caller-chosen gate_context handler, or none at all when gateContext is nil -
// the shape of a daemon old enough to predate the method entirely.
func startLegacyDaemonSocketWithGateContext(t *testing.T, gateContext ipc.HandlerFunc) *paths.Paths {
	t.Helper()
	p := shortNMHome(t)
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return legacyDoctorHealthResult{Status: "ok"}, nil
	})
	if gateContext != nil {
		srv.Handle(ipc.MethodGateContext, gateContext)
	}
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.ServeReady() }()
	t.Cleanup(func() { srv.Close() })
	return p
}

// A version-mismatched daemon is alive but reports not-alive. Dropping to the
// local-only fallback there would classify from cwd with no authenticated
// peer, letting a step-spawned agent outside its managed worktree read as
// not-nested; refusing outright would deadlock every command that could
// resolve the skew. gate_context is version-exempt precisely so neither is
// necessary: the skewed daemon is still asked, and answers.
func TestClassifyGateControlCallerQueriesSkewedDaemonOverExemptMethod(t *testing.T) {
	startLegacyDaemonSocket(t)

	result, err := classifyGateControlCaller(context.Background())
	if err != nil {
		t.Fatalf("a skewed daemon must still answer the containment query, got: %v", err)
	}
	if !result.Nested || !result.AgentDescendant || result.RunID != "outer-run" {
		t.Fatalf("containment verdict degraded under skew: %+v", result)
	}
}

// lifecycleRepairCommand builds a real `no-mistakes <path...>` command, since
// the guard selects on the full command path.
func lifecycleRepairCommand(t *testing.T, path ...string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "no-mistakes"}
	parent := root
	for _, name := range path {
		child := &cobra.Command{Use: name}
		parent.AddCommand(child)
		parent = child
	}
	parent.SetContext(context.Background())
	parent.SetOut(&bytes.Buffer{})
	return parent
}

// The guard that runs before every pipeline-control command must reach a real
// containment verdict under skew rather than surfacing the mismatch itself.
// Refusing there deadlocks the remedy: `daemon stop`, `daemon restart` and
// `update` all sit behind this guard, so they would fail with the very error
// that tells the user to run them.
func TestGuardGateControlReachesAVerdictInsteadOfDeadlockingUnderSkew(t *testing.T) {
	startLegacyDaemonSocket(t)

	paths := [][]string{
		{"daemon", "restart"},
		{"daemon", "stop"},
		{"update"},
	}
	for _, path := range paths {
		cmd := lifecycleRepairCommand(t, path...)
		out := &bytes.Buffer{}
		cmd.SetOut(out)
		err := guardGateControl(cmd)
		if err == nil {
			t.Errorf("%s: fixture reports a nested caller, so the guard should refuse on containment", cmd.CommandPath())
			continue
		}
		// The fixture's gate_context answer says nested, so the one correct
		// outcome is the containment refusal carrying that verdict. A version
		// mismatch is the deadlock; a dial or getwd failure would satisfy a
		// bare "some error" assertion while never consulting the daemon at all.
		if ipc.IsVersionMismatch(err) {
			t.Errorf("%s deadlocked on protocol skew instead of using the daemon's verdict: %v", cmd.CommandPath(), err)
			continue
		}
		if !strings.Contains(out.String(), gatecontext.ErrorCode) {
			t.Errorf("%s: expected the containment refusal %q, got:\n%s", cmd.CommandPath(), gatecontext.ErrorCode, out.String())
		}
		if !strings.Contains(out.String(), "outer-run") {
			t.Errorf("%s: refusal must carry the daemon's own verdict, got:\n%s", cmd.CommandPath(), out.String())
		}
	}
}

// Skew and "the daemon predates gate_context" arrive together in practice:
// every pre-handshake daemon reads as version 0, and most are older than the
// method. Such a daemon answers the containment query with method-not-found,
// which must fall back to the local inspector rather than failing the guard -
// otherwise `daemon stop`, `daemon restart` and `update` all die on a method
// name instead of resolving the skew.
func TestClassifyGateControlCallerFallsBackWhenSkewedDaemonPredatesGateContext(t *testing.T) {
	startLegacyDaemonSocketWithGateContext(t, nil)

	result, err := classifyGateControlCaller(context.Background())
	if err != nil {
		t.Fatalf("a daemon without gate_context must fall back to local classification, got: %v", err)
	}
	if result.Nested {
		t.Fatalf("no registered gate here, so the local verdict must be not-nested: %+v", result)
	}

	cmd := lifecycleRepairCommand(t, "daemon", "restart")
	if err := guardGateControl(cmd); err != nil {
		t.Fatalf("the guard must reach a verdict rather than fail: %v", err)
	}
}

// startVersionedDaemonSocket serves a health response that DOES declare a
// protocol version, with a caller-chosen gate_context handler or none at all.
// It models a future daemon rather than a pre-handshake one.
func startVersionedDaemonSocket(t *testing.T, version int, gateContext ipc.HandlerFunc) *paths.Paths {
	t.Helper()
	p := shortNMHome(t)
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.HealthResult{Status: "ok", ProtocolVersion: version}, nil
	})
	if gateContext != nil {
		srv.Handle(ipc.MethodGateContext, gateContext)
	}
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.ServeReady() }()
	t.Cleanup(func() { srv.Close() })
	return p
}

// The method-not-found fallback exists for daemons that predate the handshake,
// which report no version at all. A daemon that DECLARES a version and still
// lacks gate_context is a future one that renamed or retired the method, and
// falling back for it degrades containment to the peerless cwd-only inspector
// - the exact bypass the exemption was added to avoid. It must refuse instead.
func TestClassifyGateControlCallerRefusesWhenAVersionedDaemonLacksGateContext(t *testing.T) {
	startVersionedDaemonSocket(t, ipc.ProtocolVersion+1, nil)

	_, err := classifyGateControlCaller(context.Background())
	if err == nil {
		t.Fatal("a versioned daemon without gate_context must not degrade to local cwd-only classification")
	}
	assertRefusedTheGateContextQuery(t, err)

	cmd := lifecycleRepairCommand(t, "daemon", "restart")
	guardErr := guardGateControl(cmd)
	if guardErr == nil {
		t.Fatal("the guard must refuse rather than admit an unclassifiable caller")
	}
	assertRefusedTheGateContextQuery(t, guardErr)
}

// The v0 fallback turns on BOTH the peer reporting no version and the query
// failing specifically with method-not-found. Without the second half every
// transient gate_context failure against a legacy daemon - a handler error, a
// malformed result - would silently degrade to the peerless local inspector.
func TestClassifyGateControlCallerRefusesWhenALegacyDaemonsGateContextFails(t *testing.T) {
	startLegacyDaemonSocketWithGateContext(t, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return nil, errors.New("boom")
	})

	result, err := classifyGateControlCaller(context.Background())
	if err == nil {
		t.Fatalf("a failed containment query must not read as a verdict, got: %+v", result)
	}
	assertRefusedTheGateContextQuery(t, err)
	if ipc.IsMethodNotFound(err) {
		t.Fatalf("this daemon HAS gate_context; the failure must not read as a missing method: %v", err)
	}
}

// assertRefusedTheGateContextQuery pins that the refusal came from the daemon's
// own gate_context answer rather than from a paths, getwd or dial failure that
// would satisfy a bare "some error" assertion without consulting the daemon.
func assertRefusedTheGateContextQuery(t *testing.T, err error) {
	t.Helper()
	if ipc.IsVersionMismatch(err) {
		t.Fatalf("the skew itself must not surface here; that is the deadlock: %v", err)
	}
	if !strings.Contains(err.Error(), "classify gate execution context") {
		t.Fatalf("error %v did not come from the gate_context query", err)
	}
}

// TestDaemonStopCompletesAgainstASkewedDaemon is the end-to-end proof of the
// remedy the mismatch message names. Every other skew test stops at one layer:
// the guard reaches a verdict, EnsureDaemon refuses, the error reads well. None
// of them run `daemon stop` all the way through to a daemon that is actually
// gone, which is the whole escape hatch - if it does not complete, a skewed
// daemon is unremovable by the command the tool tells the user to run.
//
// The PID record is the legacy bare-integer shape on purpose: it carries no
// start time, so validating it needs a health check, and a skewed daemon
// answers health. Reading only the alive bool there leaves the stop unable to
// prove the process it is waiting for ever exited.
func TestDaemonStopCompletesAgainstASkewedDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture parks a real process with sleep")
	}
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}

	// A real process stands in for the daemon process identity, so the stop has
	// something whose exit it must genuinely observe.
	child := exec.Command(sleepBin, "120")
	if err := child.Start(); err != nil {
		t.Fatalf("start stand-in daemon process: %v", err)
	}
	reaped := make(chan struct{})
	t.Cleanup(func() {
		_ = child.Process.Kill()
		select {
		case <-reaped:
		default:
			_, _ = child.Process.Wait()
		}
	})

	p := shortNMHome(t)
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return legacyDoctorHealthResult{Status: "ok"}, nil
	})
	srv.Handle(ipc.MethodGateContext, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.GateContextResult{}, nil
	})
	var shutdownRan atomic.Bool
	srv.Handle(ipc.MethodShutdown, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		shutdownRan.Store(true)
		// Model the real teardown from outside the handler: the daemon process
		// dies and the listener goes away, but only after this reply is written.
		go func() {
			time.Sleep(50 * time.Millisecond)
			_ = child.Process.Kill()
			_, _ = child.Process.Wait()
			close(reaped)
			srv.Close()
		}()
		return ipc.ShutdownResult{OK: true}, nil
	})
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.ServeReady() }()
	t.Cleanup(func() { srv.Close() })

	if err := os.WriteFile(p.PIDFile(), []byte(strconv.Itoa(child.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("daemon", "stop")
	if err != nil {
		t.Fatalf("daemon stop must complete against a skewed daemon, got %v:\n%s", err, out)
	}
	if !shutdownRan.Load() {
		t.Fatalf("the skewed daemon was never asked to shut down:\n%s", out)
	}
	if !strings.Contains(out, "daemon stopped") {
		t.Fatalf("expected the stop to report success, got:\n%s", out)
	}
	if _, statErr := os.Stat(p.PIDFile()); !os.IsNotExist(statErr) {
		t.Fatalf("a completed stop must clear the pid record, stat err = %v", statErr)
	}
}

// abort --run against a skewed daemon must not report the desired end state
// as already reached: the daemon is alive and may still be running the very
// monitor the command exists to reap.
func TestAxiAbortByRunIDFailsClosedOnVersionMismatch(t *testing.T) {
	startLegacyDaemonSocket(t)

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)

	err := runAxiAbortByRunID(cmd, "orphan-run")
	if err == nil {
		t.Fatalf("expected a non-zero abort failure, got success:\n%s", out.String())
	}
	var exit *exitError
	if !errors.As(err, &exit) || exit.code == 0 {
		t.Fatalf("expected a non-zero exit error, got %v", err)
	}
	got := out.String()
	if strings.Contains(got, "aborted: false") || strings.Contains(got, "no-op") {
		t.Fatalf("mismatch must not render as a successful no-op, got:\n%s", got)
	}
	if !strings.Contains(got, "ipc protocol version mismatch") {
		t.Fatalf("expected the mismatch to be named, got:\n%s", got)
	}
}

// EnsureDaemon returns a mismatch without ever attempting a start, so the
// caller must not tell the user a start was tried.
func TestEnsureDaemonErrorDoesNotClaimAStartOnVersionMismatch(t *testing.T) {
	mismatch := &ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: 0, RemoteRole: ipc.RoleDaemon}
	got := ensureDaemonError(mismatch)
	if strings.Contains(got.Error(), "start daemon") {
		t.Fatalf("mismatch error should not claim a start attempt, got: %v", got)
	}
	if !ipc.IsVersionMismatch(got) {
		t.Fatalf("mismatch classification must survive, got: %v", got)
	}

	other := ensureDaemonError(errors.New("socket gone"))
	if !strings.Contains(other.Error(), "start daemon: socket gone") {
		t.Fatalf("non-mismatch failure should keep its start-daemon framing, got: %v", other)
	}
	if ensureDaemonError(nil) != nil {
		t.Fatal("nil error must stay nil")
	}
}

// A live daemon lets a global-config load error be deferred rather than
// aborting the command. A skewed daemon is still live, so the same deferral
// must apply; reading not-alive here would flip the branch under skew.
//
// Driven through openAxiEnvWithOptions, the branch the skew actually changed:
// with the deferral the broken config is carried past, and the call fails (or
// not) on whatever comes after it; without one, the config error itself is
// what comes back.
func TestGlobalConfigErrorStaysDeferredForASkewedDaemon(t *testing.T) {
	const brokenConfig = "agent: [unterminated\n"
	const configLoadErrorMarker = "parse global config"

	configErrorFor := func(t *testing.T, p *paths.Paths) error {
		t.Helper()
		if err := os.WriteFile(p.ConfigFile(), []byte(brokenConfig), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := config.LoadGlobal(p.ConfigFile())
		if err == nil {
			t.Fatal("fixture precondition: the config must fail to load")
		}
		_, openErr := openAxiEnvWithOptions(axiEnvOptions{deferGlobalConfigErrorForRunningDaemon: true})
		return openErr
	}

	t.Run("no daemon surfaces the config error", func(t *testing.T) {
		p := shortNMHome(t)
		if err := configErrorFor(t, p); err == nil || !strings.Contains(err.Error(), configLoadErrorMarker) {
			t.Fatalf("with no daemon the config error must abort the command, got: %v", err)
		}
	})

	t.Run("skewed daemon defers it", func(t *testing.T) {
		p := startLegacyDaemonSocket(t)

		alive, err := daemon.IsRunning(p)
		if alive {
			t.Fatal("fixture precondition: a skewed daemon reports not-alive")
		}
		if !ipc.IsVersionMismatch(err) {
			t.Fatalf("fixture precondition: expected a mismatch, got %v", err)
		}

		if openErr := configErrorFor(t, p); openErr != nil && strings.Contains(openErr.Error(), configLoadErrorMarker) {
			t.Fatalf("a version-skewed daemon is live, so the config error must stay deferred, got: %v", openErr)
		}
	})
}

// A protocol skew is not a failed gate setup: the daemon is healthy and the
// gate this run just created is sound. Ejecting it would make the user redo
// init after the restart the mismatch already tells them to run.
func TestInitKeepsTheGateItCreatedWhenTheDaemonIsSkewed(t *testing.T) {
	repoDir := setupTestRepo(t)
	p := startLegacyDaemonSocketWithGateContext(t, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.GateContextResult{}, nil
	})

	out, err := executeCmd("init")
	if err == nil {
		t.Fatal("init must fail closed against a version-skewed daemon")
	}
	if !ipc.IsVersionMismatch(err) {
		t.Fatalf("expected the mismatch to be reported, got: %v", err)
	}
	if strings.Contains(err.Error(), "rollback init") {
		t.Fatalf("a skew must not trigger the created-gate rollback, got: %v", err)
	}
	// Keeping the gate silently reads as a failed setup, so the user needs to
	// be told the work survived and that re-running init after the restart is
	// safe rather than a second half-finished attempt.
	for _, want := range []string{"gate was created", "daemon restart", "idempotent"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the kept-gate notice to mention %q, got:\n%s", want, out)
		}
	}

	d, openErr := db.Open(p.DB())
	if openErr != nil {
		t.Fatalf("open db: %v", openErr)
	}
	defer d.Close()
	repos, repoErr := d.GetRepos()
	if repoErr != nil {
		t.Fatalf("get repos: %v", repoErr)
	}
	if len(repos) != 1 {
		t.Fatalf("the gate registration for %s was rolled back; a skew must not undo a sound init (repos: %d)", repoDir, len(repos))
	}
	if _, statErr := os.Stat(p.RepoDir(repos[0].ID)); statErr != nil {
		t.Fatalf("the gate bare repo was removed: %v", statErr)
	}
	if _, remoteErr := git.GetRemoteURL(context.Background(), repoDir, "no-mistakes"); remoteErr != nil {
		t.Fatalf("the gate remote was removed from the working clone: %v", remoteErr)
	}
}

// The remedy names whichever binary is STALE. Against a daemon on a newer
// protocol the stale side is this CLI, so the kept-gate notice must not tell
// the user to restart the daemon - the notice would then contradict the very
// error printed beside it.
func TestInitSkewNoticeNamesTheRemedyForTheStaleSide(t *testing.T) {
	setupTestRepo(t)
	startVersionedDaemonSocket(t, ipc.ProtocolVersion+1, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.GateContextResult{}, nil
	})

	out, err := executeCmd("init")
	if err == nil || !ipc.IsVersionMismatch(err) {
		t.Fatalf("init must fail closed with the mismatch, got: %v", err)
	}
	if !strings.Contains(out, "Invoke the installed no-mistakes binary") {
		t.Fatalf("notice must prescribe the stale CLI's remedy, got:\n%s", out)
	}
	if strings.Contains(out, "daemon restart") {
		t.Fatalf("notice must not tell the user to restart the newer daemon, got:\n%s", out)
	}
}

// startHookDaemonSocket serves the two methods the git hooks call, with
// caller-chosen results, so a test can present either a current daemon or one
// whose reply carries no protocol version at all.
func startHookDaemonSocket(t *testing.T, admit ipc.AdmitPushResult, push ipc.PushReceivedResult) *paths.Paths {
	t.Helper()
	p := shortNMHome(t)
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodAdmitPush, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return admit, nil
	})
	srv.Handle(ipc.MethodPushReceived, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return push, nil
	})
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.ServeReady() }()
	t.Cleanup(func() { srv.Close() })
	return p
}

func runHookCommand(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return out.String(), err
}

// The gate hooks are the pipeline's main entry path and the one caller with no
// health probe ahead of it, so an in-place upgrade runs the new hook binary
// against the still-old daemon. Without a version check on the reply, a
// drifted AdmitPushResult decodes as not-nested and the push is admitted.
func TestHookCommandsFailClosedAgainstAnUnversionedDaemonReply(t *testing.T) {
	gateDir := t.TempDir()

	t.Run("admit-push", func(t *testing.T) {
		startHookDaemonSocket(t,
			ipc.AdmitPushResult{}, // legacy reply: no protocol_version
			ipc.PushReceivedResult{RunID: "r1"})

		out, err := runHookCommand(t, newDaemonAdmitPushCmd(), "--gate", gateDir)
		if err == nil {
			t.Fatalf("a stale daemon's reply must not admit the push, got:\n%s", out)
		}
		if !ipc.IsVersionMismatch(err) {
			t.Fatalf("expected the actionable mismatch, got: %v", err)
		}
	})

	t.Run("notify-push", func(t *testing.T) {
		startHookDaemonSocket(t,
			ipc.AdmitPushResult{ProtocolVersion: ipc.ProtocolVersion},
			ipc.PushReceivedResult{RunID: "r1"}) // legacy reply: no protocol_version

		_, err := runHookCommand(t, newDaemonNotifyPushCmd(),
			"--gate", gateDir, "--ref", "refs/heads/x", "--old", "a", "--new", "b")
		if !ipc.IsVersionMismatch(err) {
			t.Fatalf("expected the actionable mismatch, got: %v", err)
		}
	})
}

// The check costs the hooks nothing on the happy path: it reads a field of the
// reply they already wait for, and a current daemon's verdict still decides.
func TestHookCommandsHonorACurrentDaemonsVerdict(t *testing.T) {
	gateDir := t.TempDir()

	t.Run("admits a non-nested push", func(t *testing.T) {
		startHookDaemonSocket(t,
			ipc.AdmitPushResult{ProtocolVersion: ipc.ProtocolVersion},
			ipc.PushReceivedResult{RunID: "r1", ProtocolVersion: ipc.ProtocolVersion})

		if out, err := runHookCommand(t, newDaemonAdmitPushCmd(), "--gate", gateDir); err != nil {
			t.Fatalf("a current daemon's not-nested verdict must admit the push, got %v:\n%s", err, out)
		}
		if _, err := runHookCommand(t, newDaemonNotifyPushCmd(),
			"--gate", gateDir, "--ref", "refs/heads/x", "--old", "a", "--new", "b"); err != nil {
			t.Fatalf("notify-push against a current daemon must succeed, got: %v", err)
		}
	})

	t.Run("refuses a nested push", func(t *testing.T) {
		startHookDaemonSocket(t,
			ipc.AdmitPushResult{
				Context:         ipc.GateContextResult{Nested: true, RunID: "outer-run"},
				ProtocolVersion: ipc.ProtocolVersion,
			},
			ipc.PushReceivedResult{RunID: "r1", ProtocolVersion: ipc.ProtocolVersion})

		out, err := runHookCommand(t, newDaemonAdmitPushCmd(), "--gate", gateDir)
		if err == nil {
			t.Fatalf("a nested verdict must refuse, got:\n%s", out)
		}
		if ipc.IsVersionMismatch(err) {
			t.Fatalf("a matched daemon must not read as skewed: %v", err)
		}
		if !strings.Contains(out, gatecontext.ErrorCode) {
			t.Fatalf("expected the containment refusal, got %v:\n%s", err, out)
		}
	})
}
