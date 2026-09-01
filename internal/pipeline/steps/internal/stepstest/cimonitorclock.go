package stepstest

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PinnedCIMonitorClock returns the two stubs that take a CI-monitor test off
// the wall clock and off the network, for the caller to wire through SetNow and
// SetBaseBranchTip.
//
// The monitor measures its idle timeout with step.now, and every poll re-arms
// that timeout by resolving the upstream default-branch tip - a git shell-out,
// and a real fetch over the network for a repo whose upstream is a live forge.
// Leaving both live makes a test assert that that round trip plus several
// fake-CLI subprocess spawns all finish inside ci_timeout. They do when the test
// runs alone and they do not on a loaded runner, and the failure does not look
// like a timing failure: the monitor returns its timeout outcome in place of
// the outcome under test, so the test reports the wrong defect.
//
// Freezing is the cure rather than a raised bound, which is still a bet on
// machine speed, and rather than config.CITimeoutUnlimited, which makes the
// monitor skip the base-branch re-arm block entirely so the test stops covering
// the path it runs today. A test that genuinely asserts the timeout drives its
// own advancing clock instead.
func PinnedCIMonitorClock() (now func() time.Time, baseBranchTip func(context.Context) (string, bool)) {
	frozen := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return frozen },
		func(context.Context) (string, bool) { return "base-tip-sha", true }
}

// AssertShortCITimeoutTestsPinTheClock fails when a test in dir sets a
// seconds-scale ci_timeout and leaves the monitor on the real clock. It is the
// standing guard for the drift PinnedCIMonitorClock exists to undo: both CI-step
// test packages had accumulated it silently, and a flake proves nothing by
// passing once, so the bar is the source rather than the runtime.
func AssertShortCITimeoutTestsPinTheClock(t *testing.T, dir string) {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no test files found under %s to check", dir)
	}

	fset := token.NewFileSet()
	var unpinned []string
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			var body bytes.Buffer
			if err := printer.Fprint(&body, fset, fn.Body); err != nil {
				t.Fatal(err)
			}
			text := body.String()
			if !setsSecondsScaleCITimeout(text) {
				continue
			}
			if strings.Contains(text, "pinCIMonitorClock(") || strings.Contains(text, "SetNow(") {
				continue
			}
			unpinned = append(unpinned, filepath.Base(path)+":"+fn.Name.Name)
		}
	}

	if len(unpinned) > 0 {
		t.Fatalf("these tests set a seconds-scale ci_timeout against the real clock, so they fail once the runner is slower than the bound; call pinCIMonitorClock(step), or drive SetNow yourself if the test asserts the timeout:\n  %s",
			strings.Join(unpinned, "\n  "))
	}
}

// setsSecondsScaleCITimeout reports whether body assigns a ci_timeout measured
// in seconds. Minutes and hours sit far enough above any plausible runner
// slowdown to be safe, and the unlimited sentinel is already off the clock.
func setsSecondsScaleCITimeout(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		_, rhs, found := strings.Cut(line, "CITimeout =")
		if !found {
			continue
		}
		if strings.Contains(rhs, "time.Second") {
			return true
		}
	}
	return false
}
