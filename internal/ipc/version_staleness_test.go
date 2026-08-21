package ipc_test

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// The remedy must name the fix for whichever side is on the LOWER protocol
// version, not for whichever side happened to detect the skew. Branching on
// the detecting role is only accidentally correct while "stale" means
// "version 0", and starts prescribing the wrong command the day
// ProtocolVersion is bumped past 1.
func TestVersionMismatchError_RemedyNamesTheStaleSide(t *testing.T) {
	const (
		restartRemedy   = "Run 'no-mistakes daemon restart'"
		reinstallRemedy = "Invoke the installed no-mistakes binary"
	)

	tests := []struct {
		name       string
		err        *ipc.VersionMismatchError
		wantRemedy string
		wantNot    string
	}{
		{
			name:       "client detects an older daemon",
			err:        &ipc.VersionMismatchError{Local: 1, Remote: 0, RemoteRole: ipc.RoleDaemon},
			wantRemedy: restartRemedy,
			wantNot:    reinstallRemedy,
		},
		{
			name:       "daemon refuses an older client",
			err:        &ipc.VersionMismatchError{Local: 1, Remote: 0, RemoteRole: ipc.RoleClient},
			wantRemedy: reinstallRemedy,
			wantNot:    restartRemedy,
		},
		{
			// Forward skew: the CLI detects the mismatch, but the daemon is
			// the NEWER side, so restarting the daemon from this stale CLI is
			// the wrong fix.
			name:       "client detects a newer daemon",
			err:        &ipc.VersionMismatchError{Local: 1, Remote: 2, RemoteRole: ipc.RoleDaemon},
			wantRemedy: reinstallRemedy,
			wantNot:    restartRemedy,
		},
		{
			// A newer daemon refusing an older client still names the client
			// as stale, which is the same conclusion by the other route.
			name:       "daemon refuses a client one version behind",
			err:        &ipc.VersionMismatchError{Local: 2, Remote: 1, RemoteRole: ipc.RoleClient},
			wantRemedy: reinstallRemedy,
			wantNot:    restartRemedy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Error()
			if !strings.Contains(got, tt.wantRemedy) {
				t.Errorf("remedy %q missing from %q", tt.wantRemedy, got)
			}
			if strings.Contains(got, tt.wantNot) {
				t.Errorf("remedy %q must not appear in %q", tt.wantNot, got)
			}
		})
	}
}

// Whichever side detects the skew, the two versions are reported against the
// same named sides, so the message cannot invert who is on which version.
func TestVersionMismatchError_NamesEachSideItsOwnVersion(t *testing.T) {
	const want = "client speaks version 1, daemon speaks version 2"

	detectedByClient := (&ipc.VersionMismatchError{Local: 1, Remote: 2, RemoteRole: ipc.RoleDaemon}).Error()
	if !strings.Contains(detectedByClient, want) {
		t.Errorf("client-detected message = %q, want it to contain %q", detectedByClient, want)
	}

	detectedByDaemon := (&ipc.VersionMismatchError{Local: 2, Remote: 1, RemoteRole: ipc.RoleClient}).Error()
	if !strings.Contains(detectedByDaemon, want) {
		t.Errorf("daemon-detected message = %q, want it to contain %q", detectedByDaemon, want)
	}
}
