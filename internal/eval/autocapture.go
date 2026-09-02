package eval

import (
	"context"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// AutoCaptureResult reports what one automatic collection pass did. Skipped is
// true when the run simply had nothing to freeze, which is the ordinary outcome
// for most runs and is not a failure.
type AutoCaptureResult struct {
	Captured int
	Pruned   int
	Skipped  bool
	Reason   string
	// PinWarning reports a diversified pin materialization that did not happen
	// before the prune. It is a warning rather than an error because the prune
	// is still worth running: an unpinned corpus stays bounded, it just loses
	// the holdout protection for this pass.
	PinWarning string
}

// AutoCapture freezes one finished run's review passes into the local corpus,
// materializes the diversified pins the retention cap has to protect, and then
// enforces that cap.
//
// It is the single entry point for collection that nobody asked for by hand, so
// it deliberately keeps the same Capture the CLI uses rather than a looser
// variant: an automatically collected case has to be exactly as trustworthy as
// one a person captured, or the corpus quietly becomes a different thing from
// what the eval commands report on.
//
// The caller owns the timeout and the decision to run at all. This function
// owns nothing of the pipeline: it opens its own registry, does its work, and
// closes it, so a failure here cannot reach the run that triggered it.
func AutoCapture(ctx context.Context, p *paths.Paths, database *db.DB, runID string, maxCases int) (AutoCaptureResult, error) {
	if p == nil || database == nil {
		return AutoCaptureResult{}, fmt.Errorf("eval auto-capture requires paths and a database")
	}
	store, err := Open(p.EvalDir())
	if err != nil {
		return AutoCaptureResult{}, err
	}
	defer store.Close()

	cases, err := Capture(ctx, store, p, database, runID)
	if err != nil {
		if errors.Is(err, ErrNoCapturableReview) {
			return AutoCaptureResult{Skipped: true, Reason: err.Error()}, nil
		}
		return AutoCaptureResult{}, err
	}
	result := AutoCaptureResult{Captured: len(cases)}
	// Prune protects diversified-pinned cases, and this is the only collection
	// path a machine that never runs `eval sets` by hand takes, so the pins are
	// materialized here before the cap is enforced.
	if pinErr := store.ensureDiversifiedPinsForRetention(maxCases); pinErr != nil {
		result.PinWarning = pinErr.Error()
	}
	pruned, err := store.Prune(ctx, maxCases)
	result.Pruned = pruned
	if err != nil {
		return result, err
	}
	return result, nil
}
