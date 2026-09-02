package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func stepAt(name types.StepName, order int) *db.StepResult {
	return &db.StepResult{StepName: name, StepOrder: order}
}

func stepNamed(name string, order int) *db.StepResult {
	return &db.StepResult{StepName: types.StepName(name), StepOrder: order}
}

func TestPipelineVersionFromSteps_ReviewBeforeCheapGatesIsPreReorder(t *testing.T) {
	steps := []*db.StepResult{
		stepAt(types.StepIntent, 1),
		stepAt(types.StepRebase, 2),
		stepAt(types.StepReview, 3),
		stepAt(types.StepTest, 4),
		stepAt(types.StepDocument, 5),
		stepAt(types.StepLint, 6),
		stepAt(types.StepPush, 7),
		stepAt(types.StepPR, 8),
		stepAt(types.StepCI, 9),
	}
	if got := PipelineVersionFromSteps(steps); got != PipelineReviewEarly {
		t.Fatalf("PipelineVersionFromSteps() = %q, want %q", got, PipelineReviewEarly)
	}
}

func TestPipelineVersionFromSteps_CheapGatesBeforeReviewIsPostReorder(t *testing.T) {
	steps := []*db.StepResult{
		stepAt(types.StepIntent, 1),
		stepAt(types.StepRebase, 2),
		stepNamed("format", 3),
		stepAt(types.StepLint, 4),
		stepAt(types.StepTest, 5),
		stepNamed("metrics", 6),
		stepAt(types.StepDocument, 7),
		stepAt(types.StepReview, 8),
		stepAt(types.StepPush, 9),
		stepAt(types.StepPR, 10),
		stepAt(types.StepCI, 11),
	}
	if got := PipelineVersionFromSteps(steps); got != PipelineCheapGatesFirst {
		t.Fatalf("PipelineVersionFromSteps() = %q, want %q", got, PipelineCheapGatesFirst)
	}
}

func TestPipelineVersionFromSteps_AGateStillAfterReviewIsPreReorder(t *testing.T) {
	steps := []*db.StepResult{
		stepAt(types.StepIntent, 1),
		stepAt(types.StepRebase, 2),
		stepNamed("format", 3),
		stepAt(types.StepReview, 4),
		stepAt(types.StepTest, 5),
		stepAt(types.StepLint, 6),
	}
	if got := PipelineVersionFromSteps(steps); got != PipelineReviewEarly {
		t.Fatalf("PipelineVersionFromSteps() = %q, want %q", got, PipelineReviewEarly)
	}
}

// A gate recorded at review's own step order proves nothing about which of the
// two ran first, so the tag must not claim the cheap gates went first.
func TestPipelineVersionFromSteps_AGateTiedWithReviewIsPreReorder(t *testing.T) {
	steps := []*db.StepResult{
		stepAt(types.StepIntent, 1),
		stepAt(types.StepRebase, 2),
		stepNamed("format", 3),
		stepAt(types.StepLint, 4),
		stepAt(types.StepReview, 4),
	}
	if got := PipelineVersionFromSteps(steps); got != PipelineReviewEarly {
		t.Fatalf("PipelineVersionFromSteps() = %q, want %q", got, PipelineReviewEarly)
	}
}

// A run that recorded review but no cheap gate is partial evidence, not none:
// a review-only step set (a demo pipeline, a --skip of every gate, an
// eval-miss ingest) is a genuinely pre-reorder pass, and consulting the
// capturing binary would stamp it cheap-gates-first once the reorder lands and
// contaminate the very baseline the tag protects.
func TestPipelineVersionFromSteps_ReviewWithNoCheapGateIsPreReorder(t *testing.T) {
	steps := []*db.StepResult{
		stepAt(types.StepIntent, 1),
		stepAt(types.StepRebase, 2),
		stepAt(types.StepReview, 3),
		stepAt(types.StepDocument, 4),
		stepAt(types.StepPush, 5),
	}
	if got := PipelineVersionFromSteps(steps); got != PipelineReviewEarly {
		t.Fatalf("PipelineVersionFromSteps() = %q, want %q", got, PipelineReviewEarly)
	}
	if got := PipelineVersionFromSteps([]*db.StepResult{stepAt(types.StepReview, 1)}); got != PipelineReviewEarly {
		t.Fatalf("PipelineVersionFromSteps(review only) = %q, want %q", got, PipelineReviewEarly)
	}
}

// No review row at all is the one case with no evidence either way, so the
// build's own step order is the only thing left to read.
func TestPipelineVersionFromSteps_NoReviewRowFallsBackToTheBuildsOwnOrder(t *testing.T) {
	want := CurrentPipelineVersion()
	steps := []*db.StepResult{
		stepAt(types.StepIntent, 1),
		stepAt(types.StepRebase, 2),
		stepAt(types.StepTest, 3),
	}
	if got := PipelineVersionFromSteps(steps); got != want {
		t.Fatalf("PipelineVersionFromSteps() = %q, want %q", got, want)
	}
	if got := PipelineVersionFromSteps(nil); got != want {
		t.Fatalf("PipelineVersionFromSteps(nil) = %q, want %q", got, want)
	}
	if got := PipelineVersionFromSteps([]*db.StepResult{}); got != want {
		t.Fatalf("PipelineVersionFromSteps(empty) = %q, want %q", got, want)
	}
}

func TestCurrentPipelineVersionAgreesWithTheStepRuleAppliedToAllSteps(t *testing.T) {
	all := types.AllSteps()
	steps := make([]*db.StepResult, 0, len(all))
	for i, name := range all {
		steps = append(steps, stepAt(name, i+1))
	}
	if got, want := PipelineVersionFromSteps(steps), CurrentPipelineVersion(); got != want {
		t.Fatalf("PipelineVersionFromSteps(AllSteps) = %q, want %q (CurrentPipelineVersion)", got, want)
	}
}

func TestParsePipelineVersion(t *testing.T) {
	cases := []struct {
		raw     string
		want    PipelineVersion
		wantErr bool
	}{
		{raw: "review-early", want: PipelineReviewEarly},
		{raw: "cheap-gates-first", want: PipelineCheapGatesFirst},
		{raw: "  Review-Early  ", want: PipelineReviewEarly},
		{raw: "", want: PipelineAny},
		{raw: "any", want: PipelineAny},
		{raw: "ANY", want: PipelineAny},
		{raw: "v2", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParsePipelineVersion(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParsePipelineVersion(%q) = %q, nil, want error", tc.raw, got)
			}
			msg := err.Error()
			// "any" is an accepted value and the default, so an error that
			// lists only the two layouts tells a user with a typo that a valid
			// value does not exist.
			for _, want := range []string{`unknown pipeline version "v2"`, "review-early", "cheap-gates-first", "any"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("ParsePipelineVersion(%q) error = %q, want substring %q", tc.raw, msg, want)
				}
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParsePipelineVersion(%q) unexpected error: %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("ParsePipelineVersion(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestNormalizePipelineVersionReadsAnUntaggedEntryAsPreReorder(t *testing.T) {
	cases := []struct {
		in   PipelineVersion
		want PipelineVersion
	}{
		{in: "", want: PipelineReviewEarly},
		{in: PipelineCheapGatesFirst, want: PipelineCheapGatesFirst},
		{in: PipelineVersion("future-layout"), want: PipelineVersion("future-layout")},
	}
	for _, tc := range cases {
		if got := normalizePipelineVersion(tc.in); got != tc.want {
			t.Fatalf("normalizePipelineVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLoadCaseTagsAnUntaggedManifestAsPreReorderWithoutRewritingIt(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	store, err := Open(filepath.Join(p.Root(), "eval"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("captured %d cases, want 1", len(cases))
	}
	manifestPath := filepath.Join(cases[0].Dir, "manifest.json")

	// Simulate a manifest captured before pipeline_version existed: strip the
	// key entirely rather than merely zeroing it, so the round-trip proves
	// loadCase tolerates its total absence.
	raw := mustReadFile(t, manifestPath)
	var untagged map[string]any
	if err := json.Unmarshal([]byte(raw), &untagged); err != nil {
		t.Fatal(err)
	}
	delete(untagged, "pipeline_version")
	untaggedBytes, err := json.MarshalIndent(untagged, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	untaggedBytes = append(untaggedBytes, '\n')
	if err := os.WriteFile(manifestPath, untaggedBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	manifestBefore := mustReadFile(t, manifestPath)

	loaded, err := loadCase(cases[0].Dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PipelineVersion != PipelineReviewEarly {
		t.Fatalf("loaded.PipelineVersion = %q, want %q", loaded.PipelineVersion, PipelineReviewEarly)
	}
	if got := mustReadFile(t, manifestPath); got != manifestBefore {
		t.Fatalf("loadCase rewrote manifest.json:\nbefore: %s\nafter: %s", manifestBefore, got)
	}
}
