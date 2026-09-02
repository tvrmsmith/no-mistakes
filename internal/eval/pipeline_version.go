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

// pipelineVersionFromOrderedSteps is that shared rule: absent a review step or
// a cheap gate among steps, the caller-supplied fallback applies; otherwise a
// cheap gate still ordered after review means review ran early (conservative:
// a gate still follows review), and every cheap gate ordered before review
// means the cheap gates ran first.
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
		haveCheapGate = true
		if s.order > reviewOrder {
			anyGateAfterReview = true
		}
	}
	if !haveCheapGate {
		return fallback
	}
	if anyGateAfterReview {
		return PipelineReviewEarly
	}
	return PipelineCheapGatesFirst
}

// PipelineVersionFromSteps derives the tag from the run's OWN recorded step
// rows, never from the running binary's step list. It never errors and never
// panics on a nil or empty slice.
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
// it is used only when a run's recorded rows give no evidence either way.
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
		return "", fmt.Errorf("unknown pipeline version %q (use %s or %s)", raw, PipelineReviewEarly, PipelineCheapGatesFirst)
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
