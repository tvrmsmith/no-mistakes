package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestRequestDrainErrorIsLabelledOnce pins the shape of the error an operator
// reads when the managed pre-drain RPC fails. Every consumer of requestDrain
// runs its error through drainRequestError, so labelling it a second time at
// the source produced "drain request: drain request: ...".
func TestRequestDrainErrorIsLabelledOnce(t *testing.T) {
	// Short temp dir: the macOS Unix socket path limit is 104 bytes.
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodShutdown, func(context.Context, json.RawMessage) (interface{}, error) {
		return nil, fmt.Errorf("shutdown handler exploded")
	})
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeReady() }()
	t.Cleanup(srv.Close)

	_, err = requestDrain(p, StopOptions{Drain: true, DrainTimeout: time.Second})
	if err == nil {
		t.Fatal("requestDrain() = nil error, want the handler failure")
	}
	joined := joinStopErrors(err, nil, nil, nil)
	if joined == nil {
		t.Fatal("joinStopErrors() = nil, want the drain failure")
	}
	if got := strings.Count(joined.Error(), "drain request:"); got != 1 {
		t.Fatalf("joinStopErrors() = %v, want exactly one \"drain request:\" label, got %d", joined, got)
	}
	if !strings.Contains(joined.Error(), "shutdown handler exploded") {
		t.Fatalf("joinStopErrors() = %v, want it to carry the daemon's own message", joined)
	}
}

// TestJoinStopErrors covers the rule the managed stop path rests on: a stop
// failure is forgiven once the daemon is gone anyway, and a drain failure
// never is. On the managed path waitErr is nil precisely when the service
// manager stopped the daemon outright, which is the case where a swallowed
// drain error would hide that the drain never reached the daemon at all.
func TestJoinStopErrors(t *testing.T) {
	drain := errors.New("dial daemon: connection refused")
	managed := errors.New("launchctl bootout: exit status 1")
	detached := errors.New("kill: no such process")
	wait := errors.New("daemon still running after 30s")

	tests := []struct {
		name     string
		drain    error
		managed  error
		detached error
		wait     error
		wantNil  bool
		contains []string
		absent   []string
	}{
		{
			name:    "everything succeeded",
			wantNil: true,
		},
		{
			name:     "drain failed but the daemon exited",
			drain:    drain,
			wantNil:  false,
			contains: []string{"drain request", "connection refused"},
		},
		{
			name:    "stop errors are forgiven when the daemon exited",
			managed: managed, detached: detached,
			wantNil: true,
		},
		{
			name:    "stop errors surface when the daemon is still alive",
			managed: managed, detached: detached, wait: wait,
			wantNil:  false,
			contains: []string{"launchctl bootout", "detached shutdown", "wait for exit"},
			absent:   []string{"drain request"},
		},
		{
			name:  "a drain failure is never forgiven by a clean exit",
			drain: drain, managed: managed,
			wantNil:  false,
			contains: []string{"drain request"},
			absent:   []string{"launchctl bootout", "wait for exit"},
		},
		{
			name:  "every failure is reported together",
			drain: drain, managed: managed, detached: detached, wait: wait,
			wantNil:  false,
			contains: []string{"drain request", "launchctl bootout", "detached shutdown", "wait for exit"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := joinStopErrors(tc.drain, tc.managed, tc.detached, tc.wait)
			if tc.wantNil {
				if err != nil {
					t.Fatalf("joinStopErrors() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("joinStopErrors() = nil, want an error")
			}
			for _, want := range tc.contains {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("joinStopErrors() = %v, want it to mention %q", err, want)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(err.Error(), unwanted) {
					t.Fatalf("joinStopErrors() = %v, want it to omit %q", err, unwanted)
				}
			}
		})
	}
}
