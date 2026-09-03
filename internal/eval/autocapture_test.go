package eval

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

const autoCaptureDefaultDiversifiedSize = config.DefaultEvalDiversifiedSize

func goldSpec(id string) []FindingGold {
	return []FindingGold{{ID: "g-" + id, Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g-" + id, Severity: "error", Action: "auto-fix"}}
}

func goldRound(id string) string {
	return findingsJSON(findingSpec{ID: "g-" + id, Severity: "error", File: "main.go", Line: 1, Description: "g-" + id, Action: "auto-fix"})
}

func countCaseRows(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT count(*) FROM cases`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func caseRowExists(t *testing.T, store *Store, id string) bool {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT count(*) FROM cases WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

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
		gold:            goldSpec("baseline"),
		roundFindings:   goldRound("baseline"),
	})
	if pins := mustDiversifiedPinRows(t, seed); len(pins) != 0 {
		t.Fatalf("pin table = %#v, want it empty before any set resolution", pins)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, 1, autoCaptureDefaultDiversifiedSize)
	if err != nil {
		t.Fatal(err)
	}
	if result.PinWarning != "" {
		t.Fatalf("AutoCapture pin warning = %q, want the pins materialized", result.PinWarning)
	}
	if result.Captured != 1 {
		t.Fatalf("AutoCapture captured %d case(s), want 1", result.Captured)
	}
	// Both cases end up pinned, so the cap evicts nothing and the corpus stays
	// over its target. That is the documented posture for protected cases, and
	// it is only reachable because the pins exist by the time Prune runs.
	if result.Pruned != 0 {
		t.Fatalf("AutoCapture pruned %d case(s), want the protected corpus left alone", result.Pruned)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if n := countCaseRows(t, store); n != 2 {
		t.Fatalf("corpus = %d case(s) after auto capture at cap 1, want both protected cases kept", n)
	}
	pinned := map[string]bool{}
	for _, pin := range mustDiversifiedPinRows(t, store) {
		pinned[pin.CaseID] = true
	}
	if !pinned["pre-reorder-baseline"] {
		t.Fatalf("pin table = %v, want the pre-reorder baseline pinned before the cap ran", pinned)
	}
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

// The pin protection must not turn the cap off: once the pins are materialized,
// everything they do not cover is still evicted oldest-first.
func TestAutoCaptureStillEvictsUnpinnedCasesAfterMaterializingThePins(t *testing.T) {
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
		gold:            goldSpec("baseline"),
		roundFindings:   goldRound("baseline"),
	})
	for i := 1; i <= 3; i++ {
		writeSyntheticCase(t, seed, syntheticCaseSpec{
			id: "filler-" + strconv.Itoa(i), fingerprint: "repo-baseline", capturedAt: int64(1 + i), changedLines: 10,
			pipelineVersion: PipelineReviewEarly,
			roundFindings:   findingsJSON(findingSpec{ID: "f" + strconv.Itoa(i), Severity: "error", File: "main.go", Line: 1, Description: "filler", Action: "ask-user"}),
		})
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, 2, autoCaptureDefaultDiversifiedSize)
	if err != nil {
		t.Fatal(err)
	}
	if result.PinWarning != "" {
		t.Fatalf("AutoCapture pin warning = %q, want the pins materialized", result.PinWarning)
	}
	// 1 pinned baseline + 3 unlabeled fillers + 1 freshly captured case = 5,
	// against a cap of 2. Only the baseline is protected.
	if result.Pruned != 3 {
		t.Fatalf("AutoCapture pruned %d case(s), want the 3 oldest unprotected ones removed", result.Pruned)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if n := countCaseRows(t, store); n != 2 {
		t.Fatalf("corpus = %d case(s) after the cap, want 2", n)
	}
	if !caseRowExists(t, store, "pre-reorder-baseline") {
		t.Fatal("corpus lost the pinned pre-reorder case while enforcing the cap")
	}
}

// One case directory this build cannot read must not veto retention. Nothing
// repairs such a case, and Prune is what would eventually reclaim it, so a
// strict listing here would leave the corpus growing past its cap forever.
// Planning over the readable gold keeps both halves working.
func TestAutoCaptureStillPinsAndPrunesWithAnUnreadableCaseDirectory(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	seed, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	writeSyntheticCase(t, seed, syntheticCaseSpec{
		id: "old-gold", fingerprint: "repo-gold", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            goldSpec("old"),
		roundFindings:   goldRound("old"),
	})
	writeSyntheticCase(t, seed, syntheticCaseSpec{
		id: "filler", fingerprint: "repo-gold", capturedAt: 2, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		roundFindings:   findingsJSON(findingSpec{ID: "f1", Severity: "error", File: "main.go", Line: 1, Description: "f1", Action: "ask-user"}),
	})
	unreadable := writeSyntheticCase(t, seed, syntheticCaseSpec{
		id: "unreadable", fingerprint: "repo-broken", capturedAt: 3, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		roundFindings:   findingsJSON(findingSpec{ID: "f2", Severity: "error", File: "main.go", Line: 1, Description: "f2", Action: "ask-user"}),
	})
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unreadable.Dir, "manifest.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, 2, autoCaptureDefaultDiversifiedSize)
	if err != nil {
		t.Fatalf("AutoCapture error = %v, want an unreadable case tolerated on the retention path", err)
	}
	if result.PinWarning != "" {
		t.Fatalf("AutoCapture pin warning = %q, want the readable gold pinned anyway", result.PinWarning)
	}
	if result.Pruned == 0 {
		t.Fatal("AutoCapture pruned nothing, want the cap enforced despite the unreadable case")
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pinned := map[string]bool{}
	for _, pin := range mustDiversifiedPinRows(t, store) {
		pinned[pin.CaseID] = true
	}
	if !pinned["old-gold"] {
		t.Fatalf("pin table = %v, want the readable gold case pinned", pinned)
	}
	if !caseRowExists(t, store, "old-gold") {
		t.Fatal("the pinned gold case was evicted while enforcing the cap")
	}
	if caseRowExists(t, store, "filler") {
		t.Fatal("the oldest unprotected case survived the cap, so retention is still vetoed")
	}
}

// The prune is the half that destroys evidence, so it runs only once the pins
// that protect that evidence exist. A materialization failure leaves the corpus
// one pass over its retention target rather than evicting an unprotected
// holdout, and the capture and the run still succeed.
func TestAutoCaptureSkipsThePruneWhenThePinsCannotBeMaterialized(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	seed, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	writeSyntheticCase(t, seed, syntheticCaseSpec{
		id: "old-holdout", fingerprint: "repo-holdout", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            goldSpec("holdout"),
		roundFindings:   goldRound("holdout"),
	})
	// Refusing the pin write at the database is the one failure mode that hits
	// materialization and nothing else, so the assertion below is about
	// AutoCapture's ordering rather than about whichever error got it there.
	if _, err := seed.db.Exec(`CREATE TRIGGER refuse_pins BEFORE INSERT ON diversified_pins BEGIN SELECT RAISE(ABORT, 'pin write refused'); END`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, 1, autoCaptureDefaultDiversifiedSize)
	if err != nil {
		t.Fatalf("AutoCapture error = %v, want the capture to succeed despite the pin failure", err)
	}
	if result.PinWarning == "" {
		t.Fatal("AutoCapture pin warning is empty, want the failed materialization surfaced")
	}
	if result.Pruned != 0 {
		t.Fatalf("AutoCapture pruned %d case(s), want the prune skipped while the pins are unknown", result.Pruned)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !caseRowExists(t, store, "old-holdout") {
		t.Fatal("the oldest unprotected case was evicted with no pin protection in place")
	}
}

// The pins are re-planned every time they are materialized, so auto capture has
// to plan them at the operator's configured cap. Planning at the package
// default would resize the official held-out set behind their back.
func TestAutoCaptureHonorsTheConfiguredDiversifiedSize(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	seed, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		id := "gold-" + strconv.Itoa(i)
		writeSyntheticCase(t, seed, syntheticCaseSpec{
			id: id, fingerprint: "repo-" + strconv.Itoa(i), capturedAt: int64(i), changedLines: 10,
			pipelineVersion: PipelineReviewEarly,
			gold:            goldSpec(id),
			roundFindings:   goldRound(id),
		})
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := AutoCapture(ctx, p, sourceDB, run.ID, 2, 1); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if pins := mustDiversifiedPinRows(t, store); len(pins) != 1 {
		t.Fatalf("pin table = %#v, want the configured size of 1 rather than the package default of %d", pins, autoCaptureDefaultDiversifiedSize)
	}
}

// Materializing the pins is scoped to a pass that can actually evict something:
// a corpus still inside its cap leaves the pin table exactly as it found it.
func TestAutoCaptureUnderTheCapDoesNotPin(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	// Gold that WOULD be pinned if the pass materialized anything, so an empty
	// pin table proves the under-cap early return rather than an empty corpus.
	seed, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	writeSyntheticCase(t, seed, syntheticCaseSpec{
		id: "pinnable-gold", fingerprint: "repo-gold", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            goldSpec("pinnable"),
		roundFindings:   goldRound("pinnable"),
	})
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, 50, autoCaptureDefaultDiversifiedSize)
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped || result.Captured != 1 || result.PinWarning != "" {
		t.Fatalf("AutoCapture result = %+v, want one captured case and no pin warning", result)
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
