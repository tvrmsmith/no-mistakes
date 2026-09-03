package pipeline

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
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

// RunSharedStore is the durable half of RunShared: the run row that outlives
// the process. Only the Test step's discovery uses it, and the payload is
// opaque to the store.
type RunSharedStore interface {
	GetRunTestDiscovery(id string) (string, error)
	SetRunTestDiscovery(id, state string) error
}

// testDiscoveryRecord is the persisted shape of the Test step's discovery. It
// is a private wire format between RunShared and the run row, so no other
// package decodes it.
type testDiscoveryRecord struct {
	Fingerprint string            `json:"fingerprint"`
	Units       []config.TestUnit `json:"units"`
	Selected    []string          `json:"selected"`
	Source      string            `json:"source"`
	ScopeFaults int               `json:"scope_faults"`
}

// RunShared carries run-scoped results one step hands to a later step in the
// same run. It lives on the executor for the run's lifetime.
//
// Most of it is in-memory only: on any process boundary the consuming step
// falls back to doing its own work. The Test step's discovery is the
// exception. It writes through to the run row so a daemon restart resumes with
// the layout it already paid an agent pass for, and the row dies with the run,
// so nothing carries across runs.
type RunShared struct {
	mu               sync.Mutex
	housekeepingLint *HousekeepingLintResult
	// store and runID address the run row the test discovery writes through
	// to. Both are empty for a RunShared built without a store, which keeps
	// discovery purely in-memory.
	store RunSharedStore
	runID string
	// testDiscovery and its fingerprint cache the Test step's discovery result
	// for the run. Unlike housekeepingLint, this is read, not consumed: a
	// daemon restart re-enters the Test step and must reuse the discovered
	// layout rather than pay a second cold agent pass for a changed-file set
	// it has already resolved.
	testDiscovery            *TestDiscovery
	testDiscoveryFingerprint string
	// testScopeFaults counts under-selection faults noticed so far this run.
	testScopeFaults int
	// restartTrees remembers, per step, the tree its last restart-triggering
	// commit produced, so a later round of that step committing an identical
	// tree is recognised as churn rather than progress.
	restartTrees map[types.StepName]string
}

// NewRunShared returns the run-scoped results holder a fresh run starts with.
// It is empty, and its test discovery writes through to the run row.
func NewRunShared(store RunSharedStore, runID string) *RunShared {
	return &RunShared{store: store, runID: runID}
}

// RestoreRunShared returns the run-scoped results holder a recovered run
// resumes with: the housekeeping half is empty, and the Test step's discovery
// is read back from the run row so the resumed run reuses the layout instead
// of paying a second cold agent pass.
//
// A read or decode failure only costs that reuse, so it warns and returns an
// empty holder rather than refusing the resume.
func RestoreRunShared(store RunSharedStore, runID string) *RunShared {
	s := NewRunShared(store, runID)
	if store == nil || runID == "" {
		return s
	}
	payload, err := store.GetRunTestDiscovery(runID)
	if err != nil {
		slog.Warn("could not read this run's test discovery, the Test step will discover again", "run", runID, "error", err)
		return s
	}
	if strings.TrimSpace(payload) == "" {
		return s
	}
	var record testDiscoveryRecord
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		slog.Warn("could not decode this run's test discovery, the Test step will discover again", "run", runID, "error", err)
		return s
	}
	s.testScopeFaults = record.ScopeFaults
	if record.Fingerprint != "" && len(record.Units) > 0 {
		stored := TestDiscovery{Units: record.Units, Selected: record.Selected, Source: record.Source}.copy()
		s.testDiscovery = &stored
		s.testDiscoveryFingerprint = record.Fingerprint
	}
	return s
}

// persistTestDiscoveryLocked writes the discovery half of the run's shared
// state through to the run row. The caller holds s.mu.
//
// The write is best-effort. Losing it costs a recovered run one cold discovery
// pass and one expansion's worth of patience, which is strictly better than
// failing a run over a cache.
func (s *RunShared) persistTestDiscoveryLocked() {
	if s.store == nil || s.runID == "" {
		return
	}
	record := testDiscoveryRecord{ScopeFaults: s.testScopeFaults}
	if s.testDiscovery != nil {
		record.Fingerprint = s.testDiscoveryFingerprint
		record.Units = s.testDiscovery.Units
		record.Selected = s.testDiscovery.Selected
		record.Source = s.testDiscovery.Source
	}
	payload, err := json.Marshal(record)
	if err != nil {
		slog.Warn("could not encode this run's test discovery", "run", s.runID, "error", err)
		return
	}
	if err := s.store.SetRunTestDiscovery(s.runID, string(payload)); err != nil {
		slog.Warn("could not persist this run's test discovery", "run", s.runID, "error", err)
	}
}

// SetTestDiscovery caches a discovery under a fingerprint of the changed-file
// set it was derived from, replacing any previous entry, and writes it through
// to the run row. A later Set with a different fingerprint (the changed-file
// set moved, most likely because a fix round added a commit) discards the
// stale entry along with it.
func (s *RunShared) SetTestDiscovery(fingerprint string, d TestDiscovery) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := d.copy()
	s.testDiscovery = &stored
	s.testDiscoveryFingerprint = fingerprint
	s.persistTestDiscoveryLocked()
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
	s.persistTestDiscoveryLocked()
	return s.testScopeFaults
}

// LastRestartTree returns the tree a step's previous restart-triggering commit
// produced, or "" when that step has not restarted the run yet.
func (s *RunShared) LastRestartTree(step types.StepName) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restartTrees[step]
}

// SetLastRestartTree records the tree a step's restart-triggering commit
// produced, so a later round of the same step that commits an identical tree
// is recognised as churn rather than progress. Unlike the housekeeping stash
// this is not consume-once: the comparison must survive every later round of
// the run.
//
// Only the most recent tree per step is kept, and RunShared is rebuilt on
// every Execute and Resume, so the guard catches a consecutive repeat within
// one daemon process and nothing more. An A/B/A oscillation and a loop that
// spans a daemon restart both slip past it; runs.restart_count is what stays
// durable across those.
func (s *RunShared) SetLastRestartTree(step types.StepName, tree string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restartTrees == nil {
		s.restartTrees = make(map[types.StepName]string)
	}
	s.restartTrees[step] = tree
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
