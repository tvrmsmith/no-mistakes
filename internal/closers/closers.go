// Package closers releases a handle whose close error the caller cannot act on.
package closers

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
)

// Quiet closes c and reports a failure instead of returning it. Use it on a
// handle that has already yielded everything the caller needed: a read
// response body, a query's rows, a database at teardown. Closing only releases
// the descriptor there, so the caller has nothing left to retry or undo, but a
// failed close still means a leaked descriptor and is worth a line in the log.
//
// A handle whose close finishes a write is not this: check that error and
// return it, because the bytes may never have landed.
//
// A *sql.Rows or *sql.Stmt takes the closure form, defer func() {
// closers.Quiet(rows) }(), because the leak checker over those handles only
// recognises a Close it can see spelled out at the deferring site.
func Quiet(c io.Closer) {
	if err := c.Close(); err != nil {
		slog.Warn("close failed", "at", callSite(), "handle", fmt.Sprintf("%T", c), "error", err)
	}
}

// callSite names the function that deferred the close, which is the only way
// to tell one of these lines from another in a log.
func callSite() string {
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%s:%d", filepath.Base(file), line)
}
