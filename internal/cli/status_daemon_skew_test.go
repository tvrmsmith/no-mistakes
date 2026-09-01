package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// TestDaemonStatusRow_SkewedDaemonReadsAsRunning pins the one reading a
// version-skewed daemon must never get. It holds the socket and answers every
// exempt method, so calling it "stopped" hides a live process during exactly
// the window the handshake exists to make legible, and sends an operator
// looking for a daemon that is already there.
func TestDaemonStatusRow_SkewedDaemonReadsAsRunning(t *testing.T) {
	skew := &ipc.VersionMismatchError{Local: 1, Remote: 0, RemoteRole: ipc.RoleDaemon}

	tests := []struct {
		name      string
		alive     bool
		err       error
		wantState string
		wantIn    []string
	}{
		{
			name:      "matched daemon",
			alive:     true,
			wantState: "running",
		},
		{
			name:      "absent daemon",
			wantState: "stopped",
		},
		{
			name:      "skewed daemon",
			err:       skew,
			wantState: "running",
			// The skew is the actionable fact, so the row must carry it and
			// the command that resolves it rather than a bare "running".
			wantIn: []string{"cli v1", "daemon v0", "daemon restart"},
		},
		{
			name:      "unrelated read failure stays stopped",
			err:       errors.New("connect to daemon socket: no such file"),
			wantState: "stopped",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, text := daemonStatusRow(tt.alive, tt.err)
			if state != tt.wantState {
				t.Fatalf("state = %q, want %q", state, tt.wantState)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(text, want) {
					t.Errorf("row text %q is missing %q", text, want)
				}
			}
		})
	}
}
