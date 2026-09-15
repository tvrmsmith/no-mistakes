// Package scratch discards the temporary files and directories a caller has
// finished with.
package scratch

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// Remove deletes one scratch file and reports a failure instead of returning
// it. Use it on a path whose work is already over, successfully or not: the
// caller has nothing left to retry, but a file left behind in a temp directory
// is a leak and is worth a line in the log.
//
// A removal the caller can still act on, such as rolling back a half-published
// artifact, is not this: check that error and return it.
func Remove(path string) {
	report(os.Remove(path), path)
}

// RemoveAll deletes one scratch directory and its contents, reporting a
// failure on the same terms as Remove.
func RemoveAll(path string) {
	report(os.RemoveAll(path), path)
}

func report(err error, path string) {
	if err == nil {
		return
	}
	slog.Warn("remove scratch path failed", "at", callSite(), "path", path, "error", err)
}

// callSite names the code that asked for the removal, which is the only way to
// tell one of these lines from another in a log.
func callSite() string {
	_, file, line, ok := runtime.Caller(3)
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%s:%d", filepath.Base(file), line)
}
