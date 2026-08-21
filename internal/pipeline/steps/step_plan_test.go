package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAllStepNamesMatchesTheStepNameOrdering pins the two owners of the
// pipeline's ordered layout to each other: the steps the daemon actually runs
// and the canonical name list a lifecycle guard compares a persisted step plan
// against. Adding a step to one only would silently make every parked run read
// as drifted.
func TestAllStepNamesMatchesTheStepNameOrdering(t *testing.T) {
	got := AllStepNames()
	want := types.AllSteps()
	if len(got) != len(want) {
		t.Fatalf("AllStepNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllStepNames()[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

// TestAllStepNamesFollowsDemoMode proves the guard's comparison plan tracks the
// steps the binary would really run, so a run parked in demo mode is not
// misread as recorded under a drifted layout.
func TestAllStepNamesFollowsDemoMode(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	got := AllStepNames()
	demo := DemoSteps()
	if len(got) != len(demo) {
		t.Fatalf("AllStepNames() under demo mode = %v, want %d demo steps", got, len(demo))
	}
	for i, step := range demo {
		if got[i] != step.Name() {
			t.Fatalf("AllStepNames()[%d] = %s, want %s", i, got[i], step.Name())
		}
	}
}
