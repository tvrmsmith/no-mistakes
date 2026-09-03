package eval

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

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

func pinnedCaseIDs(t *testing.T, store *Store) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, pin := range mustDiversifiedPinRows(t, store) {
		out[pin.CaseID] = true
	}
	return out
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

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, Retention{MaxCases: 1, DiversifiedSize: autoCaptureDefaultDiversifiedSize})
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
	cases, err := store.ListCases(context.Background(), "all")
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

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, Retention{MaxCases: 2, DiversifiedSize: autoCaptureDefaultDiversifiedSize})
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

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, Retention{MaxCases: 2, DiversifiedSize: autoCaptureDefaultDiversifiedSize})
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

// An unreadable case that is ALREADY pinned is the dangerous half of the same
// tolerance: skipping it while re-planning would rewrite the pin table without
// it, which takes it out of Prune's protection and lets oldest-first eviction
// delete a held-out case nobody can inspect to notice. Its pin row holds
// everything the table needs, so it is carried forward instead.
func TestAutoCaptureKeepsThePinOfAnAlreadyPinnedCaseThatBecomesUnreadable(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	seed, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	holdout := writeSyntheticCase(t, seed, syntheticCaseSpec{
		id: "pinned-holdout", fingerprint: "repo-holdout", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            goldSpec("holdout"),
		roundFindings:   goldRound("holdout"),
	})
	for i := 1; i <= 2; i++ {
		writeSyntheticCase(t, seed, syntheticCaseSpec{
			id: "filler-" + strconv.Itoa(i), fingerprint: "repo-holdout", capturedAt: int64(1 + i), changedLines: 10,
			pipelineVersion: PipelineReviewEarly,
			roundFindings:   findingsJSON(findingSpec{ID: "f" + strconv.Itoa(i), Severity: "error", File: "main.go", Line: 1, Description: "filler", Action: "ask-user"}),
		})
	}
	// Pin it the ordinary way, before anything is broken, so the test is about
	// a live pin surviving rather than about one being created.
	if _, err := seed.RefreshDiversified(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !pinnedCaseIDs(t, seed)["pinned-holdout"] {
		t.Fatal("setup did not pin the holdout case, so the regression cannot be observed")
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	// A build that cannot read a manifest it wrote earlier is the real shape of
	// this: the case is intact on disk, this binary just cannot load it.
	if err := os.WriteFile(filepath.Join(holdout.Dir, "manifest.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, Retention{MaxCases: 2, DiversifiedSize: autoCaptureDefaultDiversifiedSize})
	if err != nil {
		t.Fatalf("AutoCapture error = %v, want the unreadable pinned case tolerated", err)
	}
	if result.PinWarning != "" {
		t.Fatalf("AutoCapture pin warning = %q, want the pins materialized", result.PinWarning)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !pinnedCaseIDs(t, store)["pinned-holdout"] {
		t.Fatal("the pin of the unreadable holdout case was released, so nothing protected it from the cap")
	}
	if !caseRowExists(t, store, "pinned-holdout") {
		t.Fatal("the pinned holdout case was evicted after becoming unreadable")
	}
}

// The staged deletions and the expired replay reservations owe nothing to the
// pins, so a failed pin write must not strand them: a half-deleted case
// directory and a lease nobody holds both survive every later pass otherwise.
func TestAutoCaptureReclaimsAbandonedStateEvenWhenThePinsFail(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	seed, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	stranded := writeSyntheticCase(t, seed, syntheticCaseSpec{
		id: "half-deleted", fingerprint: "repo-stranded", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		roundFindings:   findingsJSON(findingSpec{ID: "f1", Severity: "error", File: "main.go", Line: 1, Description: "f1", Action: "ask-user"}),
	})
	reserved := writeSyntheticCase(t, seed, syntheticCaseSpec{
		id: "expired-lease", fingerprint: "repo-stranded", capturedAt: 2, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		roundFindings:   findingsJSON(findingSpec{ID: "f2", Severity: "error", File: "main.go", Line: 1, Description: "f2", Action: "ask-user"}),
	})
	// A crash between staging a deletion and finishing it leaves exactly this.
	if _, err := seed.db.Exec(`INSERT INTO pending_case_deletions (id, path, repo_fingerprint) VALUES (?, ?, ?)`, stranded.ID, stranded.Dir, "repo-stranded"); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.db.Exec(`DELETE FROM cases WHERE id = ?`, stranded.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.db.Exec(`INSERT INTO replay_case_reservations (session_id, case_id, reserved_until) VALUES (?, ?, ?)`, "dead-session", reserved.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.db.Exec(`CREATE TRIGGER refuse_pins BEFORE INSERT ON diversified_pins BEGIN SELECT RAISE(ABORT, 'pin write refused'); END`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, Retention{MaxCases: 1, DiversifiedSize: autoCaptureDefaultDiversifiedSize})
	if err != nil {
		t.Fatalf("AutoCapture error = %v, want the capture to succeed despite the pin failure", err)
	}
	if result.PinWarning == "" {
		t.Fatal("AutoCapture pin warning is empty, want the failed materialization surfaced")
	}

	if _, err := os.Stat(stranded.Dir); !os.IsNotExist(err) {
		t.Fatalf("stat of the half-deleted case directory = %v, want it removed even though the pins failed", err)
	}
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var pending int
	if err := store.db.QueryRow(`SELECT count(*) FROM pending_case_deletions`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("pending deletions = %d, want the staged deletion finished", pending)
	}
	var leases int
	if err := store.db.QueryRow(`SELECT count(*) FROM replay_case_reservations`).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("replay reservations = %d, want the expired lease released", leases)
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

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, Retention{MaxCases: 1, DiversifiedSize: autoCaptureDefaultDiversifiedSize})
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

	if _, err := AutoCapture(ctx, p, sourceDB, run.ID, Retention{MaxCases: 2, DiversifiedSize: 1}); err != nil {
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

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, Retention{MaxCases: 50, DiversifiedSize: autoCaptureDefaultDiversifiedSize})
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

// planReadableDiversified reads a cap of 0 as "no cap, one pin per stratum",
// so the readable half of the plan must never be handed a seat count derived
// from the carried pins. Carried pins ride ALONGSIDE a holdout planned at the
// full configured size; they neither displace readable gold nor open the plan
// up to one pin per stratum.
func TestAutoCapturePlansTheReadableHoldoutAtTheConfiguredSizeBesideCarriedPins(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	const configuredSize = 2
	seed, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	seed.SetDiversifiedSize(configuredSize)
	// One gold case per repository fingerprint, so every case is its own
	// stratum and an uncapped plan would pin all of them.
	dirs := map[string]string{}
	for i := 1; i <= 5; i++ {
		id := "gold-" + strconv.Itoa(i)
		c := writeSyntheticCase(t, seed, syntheticCaseSpec{
			id: id, fingerprint: "repo-" + strconv.Itoa(i), capturedAt: int64(i), changedLines: 10,
			pipelineVersion: PipelineReviewEarly,
			gold:            goldSpec(id),
			roundFindings:   goldRound(id),
		})
		dirs[id] = c.Dir
	}
	if _, err := seed.RefreshDiversified(context.Background()); err != nil {
		t.Fatal(err)
	}
	carried := pinnedCaseIDs(t, seed)
	if len(carried) != configuredSize {
		t.Fatalf("seeded pins = %v, want the configured size of %d", carried, configuredSize)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	// Both pinned cases become unloadable, so every pin the next pass sees is
	// carried forward rather than replanned, and the carried set alone fills
	// the configured size.
	for id := range carried {
		if err := os.WriteFile(filepath.Join(dirs[id], "manifest.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, Retention{MaxCases: 3, DiversifiedSize: configuredSize})
	if err != nil {
		t.Fatalf("AutoCapture error = %v, want the unreadable pinned cases tolerated", err)
	}
	if result.PinWarning != "" {
		t.Fatalf("AutoCapture pin warning = %q, want the pins materialized", result.PinWarning)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pinned := pinnedCaseIDs(t, store)
	for id := range carried {
		if !pinned[id] {
			t.Fatalf("carried pin %q was released, so nothing protects its case from the cap", id)
		}
	}
	readable := 0
	for id := range pinned {
		if !carried[id] {
			readable++
		}
	}
	// Four readable gold strata remain (three synthetic plus the captured run),
	// so a plan that fell through to the uncapped sentinel would pin all four.
	if readable != configuredSize {
		t.Fatalf("pin table holds %d readable pins (%v), want the configured size of %d planned at full cap", readable, pinned, configuredSize)
	}
}

// The pins a retention pass writes are only worth anything for the prune they
// gate, so the two have to be one instant. A set resolution replans the holdout
// and can release a pin, so an unserialized one landing between the gate and
// the prune drops a protected case back into the eviction window.
func TestAutoCaptureKeepsAPinnedCaseThroughARefreshRacingThePass(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	seed, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	// One synthetic gold case, the OLDEST in the corpus, so oldest-first
	// eviction aims straight at it and only its pin keeps it. With the captured
	// run's own gold case that is exactly two strata, which a holdout of two
	// seats pins in full however the pass is interleaved.
	writeSyntheticCase(t, seed, syntheticCaseSpec{
		// The fingerprint sorts last so a one-seat replan by the refresher
		// always drops THIS pin, which is what the pass has to defend.
		id: "gold-1", fingerprint: "zzz-gold", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            goldSpec("gold-1"),
		roundFindings:   goldRound("gold-1"),
	})
	for i := 1; i <= 3; i++ {
		writeSyntheticCase(t, seed, syntheticCaseSpec{
			id: "filler-" + strconv.Itoa(i), fingerprint: "repo-filler", capturedAt: int64(10 + i), changedLines: 10,
			pipelineVersion: PipelineReviewEarly,
			roundFindings:   findingsJSON(findingSpec{ID: "f" + strconv.Itoa(i), Severity: "error", File: "main.go", Line: 1, Description: "filler", Action: "ask-user"}),
		})
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	// A second registry on the same corpus with a SMALLER holdout, which is
	// what makes its replan release a pin the pass is relying on.
	racer, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer racer.Close()
	racer.SetDiversifiedSize(1)
	done := make(chan struct{})
	stopped := make(chan struct{})
	var replans atomic.Int64
	racerErr := make(chan error, 1)
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := racer.RefreshDiversified(context.Background()); err != nil {
				select {
				case racerErr <- err:
				default:
				}
				return
			}
			replans.Add(1)
			// Yield between replans, so the refresher contends with the pass
			// rather than starving it out of the corpus lock entirely.
			time.Sleep(time.Millisecond)
		}
	}()

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, Retention{MaxCases: 3, DiversifiedSize: 2})
	close(done)
	<-stopped
	if err != nil {
		t.Fatal(err)
	}
	select {
	case racerErr := <-racerErr:
		t.Fatalf("the racing refresher failed with %v, so the pass never had a contender", racerErr)
	default:
	}
	// Without a replan actually landing, this test degrades into "AutoCapture
	// ran alone" while still claiming to prove serialization.
	if n := replans.Load(); n == 0 {
		t.Fatal("the racing refresher completed no replan, so nothing contended with the retention pass")
	}
	if result.PinWarning != "" {
		t.Fatalf("AutoCapture pin warning = %q, want the pins materialized", result.PinWarning)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !caseRowExists(t, store, "gold-1") {
		t.Fatal("case \"gold-1\" was evicted, so a concurrent refresh released the pin the retention pass wrote")
	}
}

// Replanning the holdout is a pin-table write, so it has to be serialized with
// the retention pass that materializes the pins the cap respects. An
// unserialized replan can release a pin between the pass writing it and the
// prune reading it, which drops the case it protects back into the eviction
// window.
func TestRefreshDiversifiedWaitsForTheCorpusLock(t *testing.T) {
	ctx := context.Background()
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "gold-1", fingerprint: "repo-gold", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            goldSpec("gold-1"),
		roundFindings:   goldRound("gold-1"),
	})

	unlock, err := lockCorpus(ctx, store.root)
	if err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan error, 1)
	go func() {
		_, err := store.RefreshDiversified(context.Background())
		refreshed <- err
	}()
	select {
	case err := <-refreshed:
		unlock()
		t.Fatalf("RefreshDiversified replanned the holdout (err = %v) while another corpus writer held the lock", err)
	case <-time.After(250 * time.Millisecond):
	}

	unlock()
	select {
	case err := <-refreshed:
		if err != nil {
			t.Fatalf("RefreshDiversified error after the lock was released = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RefreshDiversified never completed after the corpus lock was released")
	}
	if !pinnedCaseIDs(t, store)["gold-1"] {
		t.Fatal("RefreshDiversified wrote no pin, so the serialization it waited for protected nothing")
	}
}

// ListCases resolves diversified and tune by WRITING the pin table, so the
// CLI's own read path is the same class of corpus writer as RefreshDiversified
// and has to queue behind the retention pass rather than read past it.
func TestListCasesDiversifiedWaitsForTheCorpusLock(t *testing.T) {
	ctx := context.Background()
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "gold-1", fingerprint: "repo-gold", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            goldSpec("gold-1"),
		roundFindings:   goldRound("gold-1"),
	})

	unlock, err := lockCorpus(ctx, store.root)
	if err != nil {
		t.Fatal(err)
	}
	type resolution struct {
		cases []Case
		err   error
	}
	resolved := make(chan resolution, 1)
	go func() {
		cases, err := store.ListCases(ctx, "diversified")
		resolved <- resolution{cases: cases, err: err}
	}()
	select {
	case got := <-resolved:
		unlock()
		t.Fatalf("ListCases(diversified) resolved %d case(s) (err = %v) while another corpus writer held the lock", len(got.cases), got.err)
	case <-time.After(250 * time.Millisecond):
	}

	unlock()
	select {
	case got := <-resolved:
		if got.err != nil {
			t.Fatalf("ListCases(diversified) error after the lock was released = %v", got.err)
		}
		if len(got.cases) != 1 || got.cases[0].ID != "gold-1" {
			t.Fatalf("ListCases(diversified) = %v, want the one gold case", caseIDs(got.cases))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListCases(diversified) never completed after the corpus lock was released")
	}
	if !pinnedCaseIDs(t, store)["gold-1"] {
		t.Fatal("ListCases(diversified) wrote no pin, so the serialization it waited for protected nothing")
	}
}

// A carried pin costs no seat. When the cases holding every configured seat
// become unreadable, the holdout used to consist only of them: an official set
// no eval command can load, with every readable gold case left outside the
// prune's protection as the oldest unevaluated rows.
func TestAutoCaptureStillPinsReadableGoldWhenCarriedPinsFillTheConfiguredSize(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	seed, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	seed.SetDiversifiedSize(1)
	// The one seat goes to the oldest gold case, which then becomes unreadable.
	unreadable := writeSyntheticCase(t, seed, syntheticCaseSpec{
		id: "gold-unreadable", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            goldSpec("gold-unreadable"),
		roundFindings:   goldRound("gold-unreadable"),
	})
	if _, err := seed.RefreshDiversified(ctx); err != nil {
		t.Fatal(err)
	}
	if !pinnedCaseIDs(t, seed)["gold-unreadable"] {
		t.Fatal("setup did not pin the oldest gold case into the single seat")
	}
	writeSyntheticCase(t, seed, syntheticCaseSpec{
		id: "gold-readable", fingerprint: "repo-b", capturedAt: 2, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            goldSpec("gold-readable"),
		roundFindings:   goldRound("gold-readable"),
	})
	// A stratum sibling, so the single readable seat lands on repo-b rather
	// than on the case this run itself captures.
	writeSyntheticCase(t, seed, syntheticCaseSpec{
		id: "gold-readable-sibling", fingerprint: "repo-b", capturedAt: 3, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            goldSpec("gold-readable-sibling"),
		roundFindings:   goldRound("gold-readable-sibling"),
	})
	for i := 1; i <= 2; i++ {
		writeSyntheticCase(t, seed, syntheticCaseSpec{
			id: "filler-" + strconv.Itoa(i), fingerprint: "repo-c", capturedAt: int64(2 + i), changedLines: 10,
			pipelineVersion: PipelineReviewEarly,
			roundFindings:   findingsJSON(findingSpec{ID: "f" + strconv.Itoa(i), Severity: "error", File: "main.go", Line: 1, Description: "filler", Action: "ask-user"}),
		})
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(unreadable.Dir, "manifest.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := AutoCapture(ctx, p, sourceDB, run.ID, Retention{MaxCases: 3, DiversifiedSize: 1})
	if err != nil {
		t.Fatalf("AutoCapture error = %v, want the unreadable pinned case tolerated", err)
	}
	if result.PinWarning != "" {
		t.Fatalf("AutoCapture pin warning = %q, want the pins materialized", result.PinWarning)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetDiversifiedSize(1)
	pinned := pinnedCaseIDs(t, store)
	if !pinned["gold-unreadable"] {
		t.Fatal("the carried pin was released, so nothing protects the unreadable case")
	}
	if !pinned["gold-readable"] {
		t.Fatalf("pin table = %v, want the readable gold case pinned alongside the carried pin", pinned)
	}
	if !caseRowExists(t, store, "gold-readable") {
		t.Fatal("the readable gold case was evicted, so the carried pin took its protection")
	}
	// The holdout has to hold evidence somebody can actually replay. Every pin
	// resting on a case this build cannot load is a set that resolves to
	// nothing, which is why the carried pin never takes the readable seat.
	readable, _, err := store.readableLabeledCases()
	if err != nil {
		t.Fatal(err)
	}
	loadable := 0
	for _, c := range readable {
		if pinned[c.ID] {
			loadable++
		}
	}
	if loadable == 0 {
		t.Fatalf("pin table = %v, want at least one pinned case this build can load", pinned)
	}

	// ListCases stays strict for every caller other than retention, so the
	// official holdout only resolves once the corrupt directory loads again.
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	holdout, err := store.ListCases(ctx, "diversified")
	if err != nil {
		t.Fatal(err)
	}
	if len(holdout) == 0 {
		t.Fatal("diversified resolved empty, so the official holdout holds only cases nothing can load")
	}
}
