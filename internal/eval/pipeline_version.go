package eval

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PipelineVersion tags an eval corpus entry with the pipeline layout it was
// collected under, so a scope change to the pipeline (a step moving relative
// to Review) never reads as a review-quality regression.
type PipelineVersion string

const (
	// PipelineReviewEarly is the layout where Review runs before the cheap
	// gates (format, lint, test, metrics).
	PipelineReviewEarly PipelineVersion = "review-early"
	// PipelineCheapGatesFirst is the layout where every cheap gate runs
	// before Review.
	PipelineCheapGatesFirst PipelineVersion = "cheap-gates-first"
	// PipelineAny is the empty filter: it matches every tag.
	PipelineAny PipelineVersion = ""
)

// cheapGateNames are the steps cheap enough to gate before a full review
// pass. types.StepLint and types.StepTest are real constants; "format" and
// "metrics" are named directly by string because a sibling change is still
// adding them as types.StepName constants, and comparing by string keeps this
// derivation correct both before and after they land.
var cheapGateNames = map[string]bool{
	string(types.StepLint): true,
	string(types.StepTest): true,
	"format":               true,
	"metrics":              true,
}

// orderedStep is the shape both PipelineVersionFromSteps (from recorded db
// rows) and CurrentPipelineVersion (from types.AllSteps) reduce to, so the two
// derivations share one rule and can never drift apart.
type orderedStep struct {
	name  string
	order int
}

// pipelineVersionFromOrderedSteps is that shared rule: absent a review step
// the caller-supplied fallback applies, because the steps carry no evidence at
// all; otherwise a cheap gate still ordered after review means review ran early
// (conservative: a gate still follows review), and every cheap gate ordered
// before review means the cheap gates ran first. A run that recorded review but
// no cheap gate is partial evidence rather than none, so it resolves the
// conservative way too, to the pre-reorder layout: a review-only step set
// (a demo pipeline, a --skip of every gate, an eval-miss ingest) must never be
// stamped with whatever layout the capturing binary happens to build.
func pipelineVersionFromOrderedSteps(steps []orderedStep, fallback PipelineVersion) PipelineVersion {
	reviewOrder := 0
	haveReview := false
	for _, s := range steps {
		if s.name == string(types.StepReview) {
			reviewOrder = s.order
			haveReview = true
			break
		}
	}
	if !haveReview {
		return fallback
	}
	haveCheapGate := false
	anyGateAfterReview := false
	for _, s := range steps {
		if !cheapGateNames[s.name] {
			continue
		}
		// An order of 0 or less is no recorded position at all, which is what
		// types.StepName.Order() returns for a name its switch does not know.
		// Reading that as "ran before review" would stamp a still-review-early
		// run cheap-gates-first, so it counts as no evidence rather than as
		// evidence of the new layout.
		if s.order <= 0 {
			continue
		}
		haveCheapGate = true
		// A gate sharing review's order proves nothing about which ran first,
		// so it counts as unresolved and lands on the conservative side with
		// the gates that plainly follow review.
		if s.order >= reviewOrder {
			anyGateAfterReview = true
		}
	}
	if !haveCheapGate || anyGateAfterReview {
		return PipelineReviewEarly
	}
	return PipelineCheapGatesFirst
}

// PipelineVersionFromSteps derives the tag from the run's OWN recorded step
// rows. The running binary's step list is consulted only for a run that
// recorded no review step at all, which carries no evidence either way. It
// never errors and never panics on a nil or empty slice.
func PipelineVersionFromSteps(steps []*db.StepResult) PipelineVersion {
	ordered := make([]orderedStep, 0, len(steps))
	for _, step := range steps {
		if step == nil {
			continue
		}
		ordered = append(ordered, orderedStep{name: string(step.StepName), order: step.StepOrder})
	}
	return pipelineVersionFromOrderedSteps(ordered, CurrentPipelineVersion())
}

// CurrentPipelineVersion is the fallback only, derived from types.AllSteps().
// It applies pipelineVersionFromOrderedSteps to the build's own step list, so
// it is used only when a run recorded no review step at all.
func CurrentPipelineVersion() PipelineVersion {
	all := types.AllSteps()
	ordered := make([]orderedStep, 0, len(all))
	for i, name := range all {
		ordered = append(ordered, orderedStep{name: string(name), order: i + 1})
	}
	// The fallback here is PipelineReviewEarly, the pre-reorder default: today
	// types.AllSteps() always includes review and at least one cheap gate, so
	// this fallback is unreachable in practice, but it names the pre-reorder
	// pipeline rather than recursing back into this function.
	return pipelineVersionFromOrderedSteps(ordered, PipelineReviewEarly)
}

// ParsePipelineVersion parses a CLI filter value. It trims and lowercases; ""
// and "any" mean "match every tag".
func ParsePipelineVersion(raw string) (PipelineVersion, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "", "any":
		return PipelineAny, nil
	case string(PipelineReviewEarly):
		return PipelineReviewEarly, nil
	case string(PipelineCheapGatesFirst):
		return PipelineCheapGatesFirst, nil
	default:
		return "", fmt.Errorf("unknown pipeline version %q (use %s, %s, or any)", raw, PipelineReviewEarly, PipelineCheapGatesFirst)
	}
}

// normalizePipelineVersion resolves a stored value read off disk. An absent
// value means the entry was captured before this field existed, which was
// always the pre-reorder pipeline. Any other value, including one this build
// does not know, is returned verbatim: a forward-compatible tag is never
// silently reclassified as pre-reorder.
func normalizePipelineVersion(v PipelineVersion) PipelineVersion {
	if v == "" {
		return PipelineReviewEarly
	}
	return v
}
