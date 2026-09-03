package daemon

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// TestDrainCallTimeoutOutlastsTheDrainItWaitsOn pins the budgets a drain's RPC
// and post-drain wait need. Both exist because ipc.Client.Call's default would
// otherwise hang up on a daemon that is draining exactly as instructed, and no
// drain test uses a window long enough to notice: the shipped default is ten
// minutes, and the longest deadline any test asks for is seconds.
func TestDrainCallTimeoutOutlastsTheDrainItWaitsOn(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
	}{
		{name: "the shipped default, taken from a zero timeout", timeout: 0},
		{name: "a negative timeout falls back the same way", timeout: -time.Second},
		{name: "a short explicit timeout", timeout: 2 * time.Second},
		{name: "an explicit timeout far past the ipc default", timeout: time.Hour},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deadline := drainTimeoutOrDefault(tc.timeout)
			call := drainCallTimeout(tc.timeout)
			if call <= deadline {
				t.Fatalf("drainCallTimeout(%v) = %v, want more than the %v drain deadline it waits on", tc.timeout, call, deadline)
			}
			if call <= deadline+daemonStopTimeout() {
				t.Fatalf("drainCallTimeout(%v) = %v, want more than the drain deadline plus the %v post-drain stop grace", tc.timeout, call, daemonStopTimeout())
			}
			if call <= ipc.DefaultCallTimeout {
				t.Fatalf("drainCallTimeout(%v) = %v, want more than the %v ipc default it exists to replace", tc.timeout, call, ipc.DefaultCallTimeout)
			}

			wait := drainStopWaitTimeout(tc.timeout)
			if wait <= deadline {
				t.Fatalf("drainStopWaitTimeout(%v) = %v, want more than the %v drain deadline that has to expire first", tc.timeout, wait, deadline)
			}
		})
	}
}

// TestDrainCallTimeoutCoversTheShippedDefault states the headline case as a
// number rather than a relation: `--drain-timeout` defaults to ten minutes,
// which the ipc default would cut off after thirty seconds.
func TestDrainCallTimeoutCoversTheShippedDefault(t *testing.T) {
	if defaultDrainTimeout <= ipc.DefaultCallTimeout {
		t.Fatalf("defaultDrainTimeout = %v, no longer outlasts the %v ipc default this budget exists for", defaultDrainTimeout, ipc.DefaultCallTimeout)
	}
	if got := drainCallTimeout(0); got < defaultDrainTimeout {
		t.Fatalf("drainCallTimeout(0) = %v, want at least the %v default drain deadline", got, defaultDrainTimeout)
	}
}
