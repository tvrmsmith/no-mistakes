package agent

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
)

// scanStderrResult is what one scanClaudeStderr call produced.
type scanStderrResult struct {
	buf     string
	abort   error
	report  string
	aborted int
}

func runScanClaudeStderr(t *testing.T, r io.Reader, bypass bool) scanStderrResult {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", newClaudeConfigDir(t))

	var mu sync.Mutex
	var buf []byte
	var abort error
	var report string
	aborted := 0
	scanClaudeStderr(r, func() { aborted++ }, &mu, &buf, &abort, &report, bypass)
	return scanStderrResult{buf: string(buf), abort: abort, report: report, aborted: aborted}
}

// stepReader hands out a scripted sequence of reads, which is how a test
// drives a mid-stream read error the real *os.File pipe never produces.
type stepReader struct {
	steps []stepReaderStep
}

type stepReaderStep struct {
	data string
	err  error
}

func (r *stepReader) Read(p []byte) (int, error) {
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	step := r.steps[0]
	r.steps = r.steps[1:]
	n := copy(p, step.data)
	return n, step.err
}

// TestScanClaudeStderr_ReadErrorAnnotatesAndDrainsTheRemainder covers the
// scan-failure branch. A scanner that stops early would otherwise truncate the
// diagnostics the exit error is built from and stop draining the pipe, so a
// chatty child blocks on a full pipe until the run's deadline.
func TestScanClaudeStderr_ReadErrorAnnotatesAndDrainsTheRemainder(t *testing.T) {
	boom := errors.New("input/output error")
	got := runScanClaudeStderr(t, &stepReader{steps: []stepReaderStep{
		{data: "warning: first line\n"},
		{err: boom},
		{data: "tail the scanner never parsed"},
	}}, true)

	if !strings.Contains(got.buf, "warning: first line") {
		t.Errorf("stderr = %q, lost the lines read before the failure", got.buf)
	}
	if !strings.Contains(got.buf, "[stderr scan stopped: input/output error; remainder unparsed]") {
		t.Errorf("stderr = %q, want the scan failure recorded for the operator", got.buf)
	}
	if !strings.Contains(got.buf, "tail the scanner never parsed") {
		t.Errorf("stderr = %q, want the remainder drained rather than left in the pipe", got.buf)
	}
	if got.abort != nil {
		t.Errorf("abort = %v, want nil: a read failure is not a trust abort", got.abort)
	}
}

// TestScanClaudeStderr_ClosedPipeIsNotAScanFailure pins the exception: the
// biting-warning abort closes the pipes on purpose, so os.ErrClosed is the
// normal end of the scan and must not be annotated as a failure.
func TestScanClaudeStderr_ClosedPipeIsNotAScanFailure(t *testing.T) {
	got := runScanClaudeStderr(t, &stepReader{steps: []stepReaderStep{
		{data: "warning: first line\n"},
		{err: os.ErrClosed},
	}}, true)

	if got.buf != "warning: first line\n" {
		t.Errorf("stderr = %q, want only the line read before the deliberate close", got.buf)
	}
}

// TestScanClaudeStderr_CRLFAndUnterminatedFinalLineReachTheExitError pins what
// the line-oriented scan does to bytes io.ReadAll used to pass through: a
// trailing carriage return is dropped and a final line with no newline still
// gets one, and both lines still reach the operator through the exit error.
func TestScanClaudeStderr_CRLFAndUnterminatedFinalLineReachTheExitError(t *testing.T) {
	got := runScanClaudeStderr(t, strings.NewReader("warning: crlf line\r\nwarning: unterminated line"), true)

	if got.buf != "warning: crlf line\nwarning: unterminated line\n" {
		t.Errorf("stderr = %q, want both lines newline-normalized", got.buf)
	}

	var stream claudeStream
	err := claudeExitError(errors.New("exit status 1"), []byte(got.buf), &stream)
	if !strings.Contains(err.Error(), "warning: crlf line") {
		t.Errorf("exit error %q dropped the first line", err)
	}
	if !strings.Contains(err.Error(), "warning: unterminated line") {
		t.Errorf("exit error %q dropped the unterminated final line", err)
	}
}

// TestScanClaudeStderr_BitingLineAfterAnInertOneStillAborts covers the
// multi-line shape production actually emits: Claude Code prints one
// console.error per discarded category, so an inert permissions.allow line
// routinely precedes the permissions.additionalDirectories line that bites.
// Recording the inert warning first must not stop the later line from
// aborting.
func TestScanClaudeStderr_BitingLineAfterAnInertOneStillAborts(t *testing.T) {
	got := runScanClaudeStderr(t, strings.NewReader(untrustedWorkspaceFixture+untrustedWorkspaceAdditionalDirectoriesFixture), true)

	if !errors.Is(got.abort, errClaudeWorkspaceUntrusted) {
		t.Fatalf("abort = %v, want the later biting line to abort", got.abort)
	}
	if got.aborted != 1 {
		t.Errorf("abortProcess called %d times, want exactly 1", got.aborted)
	}
}

// TestScanClaudeStderr_FirstBitingLineWins pins the `if *abort == nil` guard:
// with two biting lines, the operator reads the first workspace's remedy
// rather than having it overwritten by whichever line happened to come last.
func TestScanClaudeStderr_FirstBitingLineWins(t *testing.T) {
	got := runScanClaudeStderr(t, strings.NewReader(untrustedWorkspaceAdditionalDirectoriesFixture+untrustedWorkspaceOtherGateFixture), true)

	if !errors.Is(got.abort, errClaudeWorkspaceUntrusted) {
		t.Fatalf("abort = %v, want an untrusted-workspace abort", got.abort)
	}
	if !strings.Contains(got.abort.Error(), "871d740473c0.git") {
		t.Errorf("abort = %q, want the first biting line's workspace", got.abort)
	}
	if strings.Contains(got.abort.Error(), "000000000000.git") {
		t.Errorf("abort = %q, the second biting line overwrote the first", got.abort)
	}
}
