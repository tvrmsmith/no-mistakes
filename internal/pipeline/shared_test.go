package pipeline

import (
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

var errTestStoreUnavailable = errors.New("store unavailable")

func TestRunShared_TestDiscoveryIsReusedNotConsumed(t *testing.T) {
	s := &RunShared{}
	units := []config.TestUnit{{Name: "api", Path: "services/api", Command: "go test ./..."}}
	s.SetTestDiscovery("fp", TestDiscovery{Units: units, Selected: []string{"api"}, Source: "agent"})

	got1, ok1 := s.TestDiscovery("fp")
	if !ok1 {
		t.Fatal("first get: ok = false, want true")
	}
	got2, ok2 := s.TestDiscovery("fp")
	if !ok2 {
		t.Fatal("second get: ok = false, want true")
	}
	if len(got1.Units) != 1 || got1.Units[0].Name != "api" {
		t.Fatalf("first get units = %+v", got1.Units)
	}
	if len(got2.Units) != 1 || got2.Units[0].Name != "api" {
		t.Fatalf("second get units = %+v", got2.Units)
	}
}

func TestRunShared_TestDiscoveryFingerprintMismatchMisses(t *testing.T) {
	s := &RunShared{}
	s.SetTestDiscovery("abc", TestDiscovery{Units: []config.TestUnit{{Name: "repository", Path: "."}}, Selected: []string{"repository"}, Source: "command"})

	if _, ok := s.TestDiscovery("def"); ok {
		t.Fatal("ok = true for a mismatched fingerprint, want false")
	}
}

func TestRunShared_TestDiscoveryDoesNotAliasCallerSlices(t *testing.T) {
	s := &RunShared{}
	units := []config.TestUnit{{Name: "api", Path: "services/api", Command: "go test"}}
	selected := []string{"api"}
	s.SetTestDiscovery("fp", TestDiscovery{Units: units, Selected: selected, Source: "agent"})

	// Mutate the caller's own slices after Set.
	units[0].Name = "mutated-by-caller"
	selected[0] = "mutated-by-caller"

	got, ok := s.TestDiscovery("fp")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got.Units[0].Name != "api" || got.Selected[0] != "api" {
		t.Fatalf("cache aliased the caller's Set slices: units=%+v selected=%+v", got.Units, got.Selected)
	}

	// Mutate the slices returned by Get.
	got.Units[0].Name = "mutated-by-getter"
	got.Selected[0] = "mutated-by-getter"

	got2, ok2 := s.TestDiscovery("fp")
	if !ok2 {
		t.Fatal("ok = false, want true")
	}
	if got2.Units[0].Name != "api" || got2.Selected[0] != "api" {
		t.Fatalf("cache aliased the caller's Get slices: units=%+v selected=%+v", got2.Units, got2.Selected)
	}
}

func TestRunShared_NoteTestScopeFaultCounts(t *testing.T) {
	s := &RunShared{}
	if got := s.NoteTestScopeFault(); got != 1 {
		t.Fatalf("first call = %d, want 1", got)
	}
	if got := s.NoteTestScopeFault(); got != 2 {
		t.Fatalf("second call = %d, want 2", got)
	}
	if got := s.NoteTestScopeFault(); got != 3 {
		t.Fatalf("third call = %d, want 3", got)
	}
}

// fakeSharedStore is an in-memory stand-in for the run row, so the write-back
// and the restore can be exercised without a database.
type fakeSharedStore struct {
	rows     map[string]string
	getErr   error
	setErr   error
	setCalls int
}

func newFakeSharedStore() *fakeSharedStore {
	return &fakeSharedStore{rows: map[string]string{}}
}

func (f *fakeSharedStore) GetRunTestDiscovery(id string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	return f.rows[id], nil
}

func (f *fakeSharedStore) SetRunTestDiscovery(id, state string) error {
	f.setCalls++
	if f.setErr != nil {
		return f.setErr
	}
	f.rows[id] = state
	return nil
}

func TestRestoreRunShared_ResumesWithTheDiscoveryTheRunAlreadyPaidFor(t *testing.T) {
	store := newFakeSharedStore()
	units := []config.TestUnit{{Name: "api", Path: "services/api", Command: "go test ./services/api/..."}}

	started := NewRunShared(store, "run-1")
	started.SetTestDiscovery("fp", TestDiscovery{Units: units, Selected: []string{"api"}, Source: "agent"})
	started.NoteTestScopeFault()

	resumed := RestoreRunShared(store, "run-1")
	got, ok := resumed.TestDiscovery("fp")
	if !ok {
		t.Fatal("a resumed run did not find the discovery its earlier process stored")
	}
	if len(got.Units) != 1 || got.Units[0].Command != "go test ./services/api/..." {
		t.Fatalf("restored units = %+v", got.Units)
	}
	if len(got.Selected) != 1 || got.Selected[0] != "api" || got.Source != "agent" {
		t.Fatalf("restored selection = %v, source = %q", got.Selected, got.Source)
	}
	// The fault the earlier process saw still counts, so the next one parks
	// rather than expanding a second time.
	if count := resumed.NoteTestScopeFault(); count != 2 {
		t.Fatalf("resumed scope faults = %d, want 2", count)
	}
}

func TestRestoreRunShared_DoesNotReuseADiscoveryFromAnotherChangedFileSet(t *testing.T) {
	store := newFakeSharedStore()
	NewRunShared(store, "run-1").SetTestDiscovery("fp-old", TestDiscovery{
		Units:    []config.TestUnit{{Name: "api", Path: "services/api", Command: "go test"}},
		Selected: []string{"api"},
		Source:   "agent",
	})

	if _, ok := RestoreRunShared(store, "run-1").TestDiscovery("fp-new"); ok {
		t.Fatal("a resumed run reused a discovery derived from a different changed-file set")
	}
}

func TestRestoreRunShared_UnreadableStateStillResumesWithAnEmptyCache(t *testing.T) {
	store := newFakeSharedStore()
	store.rows["run-1"] = "{not json"

	resumed := RestoreRunShared(store, "run-1")
	if _, ok := resumed.TestDiscovery("fp"); ok {
		t.Fatal("ok = true from an undecodable payload")
	}
	if count := resumed.NoteTestScopeFault(); count != 1 {
		t.Fatalf("scope faults = %d, want a fresh count", count)
	}
}

func TestRestoreRunShared_UnreadableStoreStillResumesWithAnEmptyCache(t *testing.T) {
	store := newFakeSharedStore()
	store.getErr = errTestStoreUnavailable

	if _, ok := RestoreRunShared(store, "run-1").TestDiscovery("fp"); ok {
		t.Fatal("ok = true when the store could not be read")
	}
}

func TestRunShared_APersistenceFailureDoesNotBreakTheRun(t *testing.T) {
	store := newFakeSharedStore()
	store.setErr = errTestStoreUnavailable

	s := NewRunShared(store, "run-1")
	s.SetTestDiscovery("fp", TestDiscovery{Units: []config.TestUnit{{Name: "api", Path: ".", Command: "go test"}}, Selected: []string{"api"}, Source: "agent"})

	if _, ok := s.TestDiscovery("fp"); !ok {
		t.Fatal("the in-memory cache should still answer when the write fails")
	}
	if store.setCalls != 1 {
		t.Fatalf("set calls = %d, want 1", store.setCalls)
	}
}

func TestRunShared_WithoutAStoreStaysInMemory(t *testing.T) {
	s := NewRunShared(nil, "")
	s.SetTestDiscovery("fp", TestDiscovery{Units: []config.TestUnit{{Name: "api", Path: ".", Command: "go test"}}, Selected: []string{"api"}, Source: "agent"})
	if _, ok := s.TestDiscovery("fp"); !ok {
		t.Fatal("ok = false, want the in-memory cache to answer")
	}
}

func TestRunShared_NilReceiverIsSafe(t *testing.T) {
	var s *RunShared

	s.SetTestDiscovery("fp", TestDiscovery{Units: []config.TestUnit{{Name: "repository", Path: "."}}, Selected: []string{"repository"}})

	if _, ok := s.TestDiscovery("fp"); ok {
		t.Fatal("nil receiver TestDiscovery: ok = true, want false")
	}
	if got := s.NoteTestScopeFault(); got != 1 {
		t.Fatalf("nil receiver NoteTestScopeFault = %d, want 1", got)
	}
}
