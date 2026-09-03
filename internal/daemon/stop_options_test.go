package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// errStopRPC is the failure a fake daemon returns for a shutdown request.
var errStopRPC = errors.New("shutdown handler refused")

// stopOptionsRoot builds a paths root short enough for a unix socket path.
func stopOptionsRoot(t *testing.T) *paths.Paths {
	t.Helper()
	dir, err := os.MkdirTemp("", "stopopt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := paths.WithRoot(dir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return p
}

// serveFakeShutdown answers ipc.MethodShutdown with the caller's handler and
// nothing else, so a stop exercises the real client, wire, and error path
// without a real daemon behind it.
func serveFakeShutdown(t *testing.T, p *paths.Paths, handle func(ipc.ShutdownParams) (ipc.ShutdownResult, error)) {
	t.Helper()
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodShutdown, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var params ipc.ShutdownParams
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, err
			}
		}
		result, err := handle(params)
		if err != nil {
			return nil, err
		}
		return result, nil
	})
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.ServeReady()
	}()
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("fake IPC server did not stop")
		}
	})
}

// installFakeManagedService makes managedServiceInstalled report true through
// the real systemd branch and records every service-manager command, so a test
// can prove a stop never reached the service manager.
func installFakeManagedService(t *testing.T, p *paths.Paths) *[]string {
	t.Helper()
	restore := stubServiceRuntime(t)
	t.Cleanup(restore)
	runtimeGOOS = "linux"
	home := t.TempDir()
	serviceUserHomeDir = func() (string, error) { return home, nil }
	unit := systemdUserServicePath(p)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Service]\nExecStart=/x daemon run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var cmds []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		mu.Lock()
		cmds = append(cmds, name+" "+strings.Join(args, " "))
		mu.Unlock()
		return nil, nil
	}
	return &cmds
}

func TestStopWithOptions_NoDaemonIsReportedWithoutAnError(t *testing.T) {
	p := stopOptionsRoot(t)
	restore := stubServiceRuntime(t)
	defer restore()
	runtimeGOOS = "linux"
	serviceUserHomeDir = func() (string, error) { return t.TempDir(), nil }

	outcome, err := StopWithOptions(p, StopOptions{})
	if err != nil {
		t.Fatalf("stopping a root with no daemon should succeed, got %v", err)
	}
	if !outcome.NoDaemon {
		t.Fatalf("outcome = %+v, want NoDaemon", outcome)
	}
}

func TestStopWithOptions_FailedDrainRPCLeavesTheManagedDaemonAlone(t *testing.T) {
	p := stopOptionsRoot(t)
	cmds := installFakeManagedService(t, p)
	var drains int
	serveFakeShutdown(t, p, func(params ipc.ShutdownParams) (ipc.ShutdownResult, error) {
		if !params.Drain {
			t.Errorf("a failed drain must not be followed by a plain shutdown, got params %+v", params)
			return ipc.ShutdownResult{OK: true}, nil
		}
		drains++
		return ipc.ShutdownResult{}, errStopRPC
	})

	_, err := StopWithOptions(p, StopOptions{Drain: true, DrainTimeout: time.Second})
	if err == nil {
		t.Fatal("a failed drain RPC must fail the stop")
	}
	if drains != 1 {
		t.Fatalf("drain handler calls = %d, want 1", drains)
	}
	if len(*cmds) != 0 {
		t.Fatalf("the service manager was invoked after a failed drain: %v", *cmds)
	}
	for _, want := range []string{"drain request", "left running", "were not touched"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not tell the operator %q", err, want)
		}
	}
}

func TestStopWithOptions_ManagedServiceGoneAfterTheDrainFinishesDetached(t *testing.T) {
	p := stopOptionsRoot(t)
	installFakeManagedService(t, p)
	unit := systemdUserServicePath(p)
	var plainShutdowns int
	serveFakeShutdown(t, p, func(params ipc.ShutdownParams) (ipc.ShutdownResult, error) {
		if params.Drain {
			// The service manager forgets the daemon between the drain and
			// the stop, so stopManagedService reports managed=false.
			if err := os.Remove(unit); err != nil {
				t.Errorf("removing the unit file: %v", err)
			}
			return ipc.ShutdownResult{OK: true, Drained: true, Finished: []string{"run-1"}}, nil
		}
		plainShutdowns++
		return ipc.ShutdownResult{}, errStopRPC
	})

	outcome, err := StopWithOptions(p, StopOptions{Drain: true, DrainTimeout: time.Second})
	if err == nil {
		t.Fatal("a failed detached shutdown must fail the stop")
	}
	if plainShutdowns != 1 {
		t.Fatalf("plain shutdown calls = %d, want 1: the drain must not be repeated", plainShutdowns)
	}
	if !strings.Contains(err.Error(), "detached shutdown") {
		t.Errorf("error %q does not name the detached shutdown", err)
	}
	if strings.Contains(err.Error(), "drain request") {
		t.Errorf("error %q blames a drain that succeeded", err)
	}
	if !outcome.Drained || len(outcome.Finished) != 1 || outcome.Finished[0] != "run-1" {
		t.Errorf("outcome = %+v, want the drain report preserved", outcome)
	}
}
