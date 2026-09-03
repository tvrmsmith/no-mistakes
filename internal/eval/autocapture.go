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
	// PinWarning reports a diversified pin materialization that failed, which
	// skips the prune for this pass. It is a warning rather than an error
	// because the capture itself succeeded: the corpus simply stays over its
	// retention target, which is already the posture for protected cases, and
	// that is cheaper than evicting an unprotected holdout for a disk figure.
	PinWarning string
}

// Retention is what one automatic pass is allowed to keep. The two numbers are
// both case counts and mean opposite things, so they are named rather than
// positional: transposing them silently re-plans the official holdout at the
// retention cap and bounds the corpus at the holdout size.
type Retention struct {
	// MaxCases bounds the whole corpus. Zero or less keeps every case.
	MaxCases int
	// DiversifiedSize caps the held-out diversified set. Zero means no cap
	// beyond one case per stratum.
	DiversifiedSize int
}

// AutoCapture freezes one finished run's review passes into the local corpus,
// materializes the diversified pins the retention cap has to protect, and then
// enforces that cap. The cap is enforced only once those pins exist, so a
// failed materialization leaves the corpus over its target rather than pruning
// a holdout nothing is protecting.
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
func AutoCapture(ctx context.Context, p *paths.Paths, database *db.DB, runID string, retention Retention) (AutoCaptureResult, error) {
	if p == nil || database == nil {
		return AutoCaptureResult{}, fmt.Errorf("eval auto-capture requires paths and a database")
	}
	store, err := Open(p.EvalDir())
	if err != nil {
		return AutoCaptureResult{}, err
	}
	defer store.Close()
	// The configured cap has to be applied before anything here resolves a set:
	// materializing the pins re-plans the whole holdout, so planning it at the
	// package default would silently resize the operator's official set.
	store.SetDiversifiedSize(retention.DiversifiedSize)

	cases, err := Capture(ctx, store, p, database, runID)
	if err != nil {
		if errors.Is(err, ErrNoCapturableReview) {
			return AutoCaptureResult{Skipped: true, Reason: err.Error()}, nil
		}
		return AutoCaptureResult{}, err
	}
	result := AutoCaptureResult{Captured: len(cases)}
	// The housekeeping runs before the pin gate because it owes nothing to the
	// pins: a staged deletion left half-finished and an expired reservation
	// both have to be reclaimed even on a pass that never reaches the cap.
	if err := store.sweepAbandonedStateLocking(ctx); err != nil {
		return result, err
	}
	if pinErr := store.ensureDiversifiedPinsForRetention(ctx, retention.MaxCases); pinErr != nil {
		result.PinWarning = pinErr.Error()
		return result, nil
	}
	pruned, err := store.Prune(ctx, retention.MaxCases)
	result.Pruned = pruned
	if err != nil {
		return result, err
	}
	return result, nil
}
