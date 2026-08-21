package ipc_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// A daemon refusing a skewed client cannot restart anything on the caller's
// behalf, so its remedy names the caller's own binary.
func TestVersionMismatchError_Error_DaemonDetectsOldClient(t *testing.T) {
	err := &ipc.VersionMismatchError{Local: 1, Remote: 0, RemoteRole: ipc.RoleClient}
	want := "ipc protocol version mismatch: client speaks version 0, daemon speaks version 1; the CLI and daemon must come from the same no-mistakes binary. Invoke the installed no-mistakes binary rather than this stale one, then run 'no-mistakes init' in the repository to refresh its gate hooks."
	if got := err.Error(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestVersionMismatchError_Error_ClientDetectsOldDaemon(t *testing.T) {
	err := &ipc.VersionMismatchError{Local: 1, Remote: 0, RemoteRole: ipc.RoleDaemon}
	want := "ipc protocol version mismatch: client speaks version 1, daemon speaks version 0; the CLI and daemon must come from the same no-mistakes binary. Run 'no-mistakes daemon restart' to restart the daemon on the installed binary."
	if got := err.Error(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// An unset role names no side rather than defaulting into one of the two
// branches: assuming "remote is the daemon" for a pairing built the other way
// round would invert the versions and prescribe the wrong binary's remedy.
func TestVersionMismatchError_UnsetRoleRefusesToAttributeEitherSide(t *testing.T) {
	unset := &ipc.VersionMismatchError{Local: 2, Remote: 1}

	if got, want := unset.Error(), (&ipc.VersionMismatchError{Local: 2, Remote: 1, RemoteRole: ipc.RoleDaemon}).Error(); got == want {
		t.Fatalf("unset role silently rendered as a daemon-remote pairing: %q", got)
	}
	for _, text := range []string{unset.Error(), unset.Summary()} {
		if strings.Contains(text, "cli v") || strings.Contains(text, "client speaks") {
			t.Errorf("unset role must not attribute a version to a named side, got %q", text)
		}
		if !strings.Contains(text, "2") || !strings.Contains(text, "1") {
			t.Errorf("both observed versions must still be reported, got %q", text)
		}
	}
	if remedy := unset.Remedy(); strings.Contains(remedy, "rather than this stale one") {
		t.Errorf("unset role must not prescribe the stale-client remedy, got %q", remedy)
	}
	if !ipc.IsVersionMismatch(unset) {
		t.Error("an unattributed mismatch is still a mismatch")
	}
}

// A daemon reporting its version off a reply the caller already awaited is
// the client-side half of the handshake for callers with no health probe.
func TestDaemonVersionMismatch(t *testing.T) {
	if err := ipc.DaemonVersionMismatch(ipc.ProtocolVersion); err != nil {
		t.Errorf("matching version must not report a mismatch, got %v", err)
	}
	// A pre-handshake daemon omits the field, which decodes as zero.
	stale := ipc.DaemonVersionMismatch(0)
	if !ipc.IsVersionMismatch(stale) {
		t.Fatalf("an unversioned daemon reply must classify as a mismatch, got %v", stale)
	}
	if !strings.Contains(stale.Error(), "Run 'no-mistakes daemon restart'") {
		t.Errorf("the daemon is the stale side, so its remedy must be named, got %q", stale.Error())
	}
	if newer := ipc.DaemonVersionMismatch(ipc.ProtocolVersion + 1); !strings.Contains(newer.Error(), "Invoke the installed no-mistakes binary") {
		t.Errorf("against a newer daemon this binary is stale, got %q", newer.Error())
	}
}

func TestIsVersionMismatch(t *testing.T) {
	if !ipc.IsVersionMismatch(&ipc.VersionMismatchError{Local: 1, Remote: 0, RemoteRole: ipc.RoleClient}) {
		t.Error("expected VersionMismatchError to classify as version mismatch")
	}
	if !ipc.IsVersionMismatch(&ipc.RPCError{Code: ipc.ErrProtocolMismatch, Message: "mismatch"}) {
		t.Error("expected RPCError with ErrProtocolMismatch code to classify as version mismatch")
	}
	if ipc.IsVersionMismatch(errors.New("something else")) {
		t.Error("expected unrelated error to not classify as version mismatch")
	}
	if ipc.IsVersionMismatch(&ipc.RPCError{Code: ipc.ErrInternal, Message: "boom"}) {
		t.Error("expected unrelated RPCError code to not classify as version mismatch")
	}
}
