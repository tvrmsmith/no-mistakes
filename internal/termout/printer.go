// Package termout writes command output and keeps the first write error, so a
// command that prints many lines can still report a failed write once, at the
// end.
package termout

import (
	"fmt"
	"io"
)

// Printer writes command output and keeps the first write error. A command
// prints many lines and can act on a failed write only once, at the end: a
// closed pipe (`no-mistakes status | head`) or a full disk otherwise leaves the
// command reporting success while the operator read nothing.
//
// Err is the whole point of the type. A command that prints through a Printer
// must return it, so the write failure reaches the exit code.
type Printer struct {
	w   io.Writer
	err error
}

// New returns a Printer writing to w.
func New(w io.Writer) *Printer { return &Printer{w: w} }

func (p *Printer) Printf(format string, a ...any) {
	p.record(fmt.Fprintf(p.w, format, a...))
}

func (p *Printer) Println(a ...any) {
	p.record(fmt.Fprintln(p.w, a...))
}

func (p *Printer) Print(a ...any) {
	p.record(fmt.Fprint(p.w, a...))
}

// Write lets a Printer stand in for the io.Writer a helper or a tabwriter
// expects, so those writes are latched too.
func (p *Printer) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.record(n, err)
	return n, err
}

func (p *Printer) record(_ int, err error) {
	if err != nil && p.err == nil {
		p.err = err
	}
}

// Err reports the first failed write, if any.
func (p *Printer) Err() error {
	if p.err == nil {
		return nil
	}
	return fmt.Errorf("write output: %w", p.err)
}
