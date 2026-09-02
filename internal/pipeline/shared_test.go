package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

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
