package cli

import (
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// ensureDaemonError frames an EnsureDaemon failure for the user. A protocol
// version mismatch is returned unwrapped: the handshake guarantees no start
// was attempted for that error, so prefixing "start daemon" would report an
// action that never happened and bury the mismatch's own remedy.
func ensureDaemonError(err error) error {
	if err == nil {
		return nil
	}
	if ipc.IsVersionMismatch(err) {
		return err
	}
	return fmt.Errorf("start daemon: %w", err)
}

// versionMismatchRemedy renders the remedy err itself prescribes, which names
// whichever binary is STALE. Restating a fixed command instead would tell a
// stale CLI's user to restart the daemon, the wrong binary to touch. A
// peer-reported mismatch carries no struct to ask, so it falls back to the
// invariant both sides must satisfy.
func versionMismatchRemedy(err error) string {
	var mismatch *ipc.VersionMismatchError
	if errors.As(err, &mismatch) {
		return mismatch.Remedy()
	}
	return "Install the current no-mistakes binary and run 'no-mistakes daemon restart' so the CLI and daemon come from it."
}
