package daemon

import (
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestDaemonAnswering_SkewCountsAsPresent pins the distinction the shutdown
// paths need. daemonHealthCheck answers "is a COMPATIBLE daemon there", which
// is the wrong question before a stop: a skewed daemon is still a live process
// holding the socket, shutdown is version-exempt precisely so it can be told
// to exit, and reading the skew as absence leaves it running while the
// subsequent wait-for-exit times out.
func TestDaemonAnswering_SkewCountsAsPresent(t *testing.T) {
	tests := []struct {
		name  string
		alive bool
		err   error
		want  bool
	}{
		{name: "matched daemon", alive: true, want: true},
		{name: "skewed daemon", err: &ipc.VersionMismatchError{Local: 1, Remote: 0, RemoteRole: ipc.RoleDaemon}, want: true},
		{name: "absent daemon", want: false},
		{name: "unrelated read failure", err: errors.New("connect to daemon socket: no such file"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := daemonHealthCheck
			daemonHealthCheck = func(*paths.Paths) (bool, error) { return tt.alive, tt.err }
			t.Cleanup(func() { daemonHealthCheck = restore })

			if got := daemonAnswering(paths.WithRoot(t.TempDir())); got != tt.want {
				t.Fatalf("daemonAnswering() = %v, want %v", got, tt.want)
			}
		})
	}
}
