package daemon

import (
	"errors"
	"strings"
	"testing"
)

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
