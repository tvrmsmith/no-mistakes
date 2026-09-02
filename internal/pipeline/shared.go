package pipeline

import (
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// HousekeepingLintResult is the lint assessment produced by the combined
// document+lint housekeeping pass: the document step performs both duties in
// one agent invocation and hands the lint half to the lint step so it does
// not pay a second cold agent pass.
type HousekeepingLintResult struct {
	// FindingsJSON holds the lint-category findings (possibly an empty set)
	// in the same JSON shape the lint step produces itself.
	FindingsJSON string
	// Summary is the housekeeping pass's one-line lint summary.
	Summary string
}

// TestDiscovery is one run's test-unit discovery: the repository's unit layout
// and the names of the units a change touches.
type TestDiscovery struct {
	Units    []config.TestUnit
	Selected []string
	Source   string // "config", "command", or "agent"
}

// copy returns a deep copy of d, so a caller mutating the slices it got from
// Set or TestDiscovery cannot corrupt the cached value, and setting a
// discovery cannot be corrupted by the caller mutating its own slices
// afterward.
func (d TestDiscovery) copy() TestDiscovery {
	out := TestDiscovery{Source: d.Source}
	if d.Units != nil {
		out.Units = append([]config.TestUnit{}, d.Units...)
	}
	if d.Selected != nil {
		out.Selected = append([]string{}, d.Selected...)
	}
	return out
}

// RunShared carries in-memory run-scoped results one step hands to a later
// step in the same run. It lives on the executor for the run's lifetime and
// is never persisted: on any process boundary the consuming step simply
// falls back to doing its own work.
type RunShared struct {
	mu               sync.Mutex
	housekeepingLint *HousekeepingLintResult
	// testDiscovery and its fingerprint cache the Test step's discovery result
	// for the run. Unlike housekeepingLint, this is read, not consumed: a
	// daemon restart re-enters the Test step and must reuse the discovered
	// layout rather than pay a second cold agent pass for a changed-file set
	// it has already resolved.
	testDiscovery            *TestDiscovery
	testDiscoveryFingerprint string
	// testScopeFaults counts under-selection faults noticed so far this run.
	testScopeFaults int
}

// SetTestDiscovery caches a discovery under a fingerprint of the changed-file
// set it was derived from, replacing any previous entry. A later Set with a
// different fingerprint (the changed-file set moved, most likely because a
// fix round added a commit) discards the stale entry along with it.
func (s *RunShared) SetTestDiscovery(fingerprint string, d TestDiscovery) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := d.copy()
	s.testDiscovery = &stored
	s.testDiscoveryFingerprint = fingerprint
}

// TestDiscovery returns the cached discovery when it was derived from the
// same changed-file set. A fingerprint mismatch reports a miss rather than
// the stale entry, so a changed-file set that moved recomputes discovery
// instead of reusing a selection that no longer describes the diff.
func (s *RunShared) TestDiscovery(fingerprint string) (TestDiscovery, bool) {
	if s == nil {
		return TestDiscovery{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.testDiscovery == nil || s.testDiscoveryFingerprint != fingerprint {
		return TestDiscovery{}, false
	}
	return s.testDiscovery.copy(), true
}

// NoteTestScopeFault records an under-selection fault and returns the run's
// running total. A nil receiver has no run-scoped state to remember a fault
// in, so it returns 1 on every call: the caller has no way to tell "first
// fault this run" from "first fault this call", and the safe default on that
// ambiguity is to treat the selection as under-proven and expand it, not to
// park on what might be a one-off.
func (s *RunShared) NoteTestScopeFault() int {
	if s == nil {
		return 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.testScopeFaults++
	return s.testScopeFaults
}

// SetHousekeepingLint records the combined pass's lint assessment for the
// lint step. It replaces any previous assessment (a document fix round
// re-runs the combined pass and re-stashes a fresh result).
func (s *RunShared) SetHousekeepingLint(result HousekeepingLintResult) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.housekeepingLint = &result
}

// ClearHousekeepingLint discards a previous combined-pass lint assessment
// before a document pass starts, so a later lint step never consumes stale
// findings.
func (s *RunShared) ClearHousekeepingLint() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.housekeepingLint = nil
}

// TakeHousekeepingLint returns and consumes the combined pass's lint
// assessment. The second call returns false so a lint fix round re-assesses
// with its own agent pass instead of trusting a stale result.
func (s *RunShared) TakeHousekeepingLint() (HousekeepingLintResult, bool) {
	if s == nil {
		return HousekeepingLintResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.housekeepingLint == nil {
		return HousekeepingLintResult{}, false
	}
	result := *s.housekeepingLint
	s.housekeepingLint = nil
	return result, true
}
