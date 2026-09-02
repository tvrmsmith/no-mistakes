package eval

import (
	"context"
	"testing"
)

// Prune protects diversified-pinned cases, but only a set resolution writes
// those pins. A machine that never runs `eval sets` by hand therefore used to
// reach the cap with an empty pin table, and oldest-first eviction aims
// straight at the pre-reorder baseline the pipeline tag exists to keep. Auto
// capture is that machine's only collection path, so it materializes the pins
// itself before enforcing the cap.
func TestAutoCaptureProtectsAPinnedPreReorderCaseFromTheCap(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	seed, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	writeSyntheticCase(t, seed, syntheticCaseSpec{
		id: "pre-reorder-baseline", fingerprint: "repo-baseline", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            []FindingGold{{ID: "g1", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g1", Severity: "error", Action: "auto-fix"}},
		roundFindings:   findingsJSON(findingSpec{ID: "g1", Severity: "error", File: "main.go", Line: 1, Description: "g1", Action: "auto-fix"}),
	})
	if pins := mustDiversifiedPinRows(t, seed); len(pins) != 0 {
		t.Fatalf("pin table = %#v, want it empty before any set resolution", pins)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.PinWarning != "" {
		t.Fatalf("AutoCapture pin warning = %q, want the pins materialized", result.PinWarning)
	}
	if result.Captured != 1 {
		t.Fatalf("AutoCapture captured %d case(s), want 1", result.Captured)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cases {
		if c.ID == "pre-reorder-baseline" {
			found = true
		}
	}
	if !found {
		t.Fatalf("corpus = %v after auto capture at cap 1, want the pinned pre-reorder case kept", caseIDs(cases))
	}
}

// Materializing the pins is scoped to a pass that can actually evict something:
// a corpus still inside its cap leaves the pin table exactly as it found it.
func TestAutoCaptureUnderTheCapDoesNotPin(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	if _, err := AutoCapture(ctx, p, sourceDB, run.ID, 50); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if pins := mustDiversifiedPinRows(t, store); len(pins) != 0 {
		t.Fatalf("pin table = %#v, want no pin written while the corpus is inside its cap", pins)
	}
}
