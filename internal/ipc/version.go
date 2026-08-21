package ipc

import (
	"errors"
	"fmt"
)

// Role names one side of an IPC pairing. The two constants below are the
// only valid values; any other value, including the zero Role, renders as an
// explicitly unattributed pairing rather than silently taking one of the two
// branches and prescribing the other side's remedy.
type Role string

const (
	RoleDaemon Role = "daemon"
	RoleClient Role = "client"
)

// VersionMismatchError reports an IPC pairing whose two sides speak
// different protocol versions.
type VersionMismatchError struct {
	Local      int  // version spoken by the side that detected the mismatch
	Remote     int  // version spoken by the peer
	RemoteRole Role // which side Remote describes
}

// Error always names the client's version before the daemon's, then
// prescribes the remedy for whichever side is STALE, meaning on the lower
// version. Branching on which side detected the skew instead would be only
// accidentally correct while "stale" means "version 0": once ProtocolVersion
// is bumped past 1, a client detecting a NEWER daemon would be told to
// restart the daemon, which is the wrong binary to touch.
func (e *VersionMismatchError) Error() string {
	clientVersion, daemonVersion, ok := e.versions()
	if !ok {
		return fmt.Sprintf(
			"ipc protocol version mismatch: this side speaks version %d, its unattributed peer speaks version %d; the CLI and daemon must come from the same no-mistakes binary. %s",
			e.Local, e.Remote, e.Remedy(),
		)
	}
	return fmt.Sprintf(
		"ipc protocol version mismatch: client speaks version %d, daemon speaks version %d; the CLI and daemon must come from the same no-mistakes binary. %s",
		clientVersion, daemonVersion, e.Remedy(),
	)
}

// Summary states the mismatch alone, short enough for a fixed-width status
// row that renders the remedy separately.
func (e *VersionMismatchError) Summary() string {
	clientVersion, daemonVersion, ok := e.versions()
	if !ok {
		return fmt.Sprintf("protocol version mismatch (local v%d, peer v%d)", e.Local, e.Remote)
	}
	return fmt.Sprintf("protocol version mismatch (cli v%d, daemon v%d)", clientVersion, daemonVersion)
}

// Remedy names the command that resolves the skew, for whichever side is on
// the lower version. Without a peer role there is no way to tell which binary
// is stale, so it names the invariant instead of guessing a command.
func (e *VersionMismatchError) Remedy() string {
	clientVersion, daemonVersion, ok := e.versions()
	if !ok {
		return "Install the current no-mistakes binary and run 'no-mistakes daemon restart' so the CLI and daemon come from it."
	}
	if clientVersion < daemonVersion {
		return "Invoke the installed no-mistakes binary rather than this stale one, then run 'no-mistakes init' in the repository to refresh its gate hooks."
	}
	return "Run 'no-mistakes daemon restart' to restart the daemon on the installed binary."
}

// versions resolves the pairing into (client, daemon) order regardless of
// which side detected the skew. ok is false when RemoteRole names neither
// side, because assuming one there would invert the pairing and prescribe the
// wrong side's remedy.
func (e *VersionMismatchError) versions() (client, daemon int, ok bool) {
	switch e.RemoteRole {
	case RoleClient:
		return e.Remote, e.Local, true
	case RoleDaemon:
		return e.Local, e.Remote, true
	default:
		return 0, 0, false
	}
}

// DaemonVersionMismatch converts a daemon-reported protocol version into a
// mismatch error, or nil when it matches. It is the client-side half of the
// handshake for callers that read the daemon's version off a reply they were
// already waiting for, rather than from a dedicated health probe.
func DaemonVersionMismatch(reported int) error {
	if reported == ProtocolVersion {
		return nil
	}
	return &VersionMismatchError{Local: ProtocolVersion, Remote: reported, RemoteRole: RoleDaemon}
}

// IsVersionMismatch reports whether err is a protocol version mismatch,
// whether detected locally or reported by the peer over the wire.
func IsVersionMismatch(err error) bool {
	var mismatch *VersionMismatchError
	if errors.As(err, &mismatch) {
		return true
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Code == ErrProtocolMismatch
	}
	return false
}
