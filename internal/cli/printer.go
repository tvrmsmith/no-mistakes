package cli

import (
	"io"

	"github.com/kunchenguid/no-mistakes/internal/termout"
)

// printer is this package's name for the shared latching writer. Command
// output goes through it so a failed write reaches the exit code instead of
// leaving the command reporting success while the operator read nothing.
type printer = termout.Printer

func newPrinter(w io.Writer) *printer { return termout.New(w) }
