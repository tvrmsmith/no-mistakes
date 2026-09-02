package eval

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Once Review runs behind the cheap gates, a share of the old gold-labelled
// findings can no longer occur: blending pre-reorder and post-reorder cases
// or evaluations into one score reads as a review-quality regression that is
// really a scope change. These tests pin the selection and reporting contract
// that keeps the two populations apart by default.

func TestListCasesForPipelineNarrowsToOneLayout(t *testing.T) {
	store := openEvalStore(t)
	a := writeSyntheticCase(t, store, syntheticCaseSpec{id: "a", fingerprint: "repo-a", capturedAt: 1, changedLines: 10, pipelineVersion: PipelineReviewEarly})
	b := writeSyntheticCase(t, store, syntheticCaseSpec{id: "b", fingerprint: "repo-a", capturedAt: 2, changedLines: 10, pipelineVersion: PipelineReviewEarly})
	c := writeSyntheticCase(t, store, syntheticCaseSpec{id: "c", fingerprint: "repo-a", capturedAt: 3, changedLines: 10, pipelineVersion: PipelineCheapGatesFirst})
	_, _, _ = a, b, c

	reviewEarly, err := store.ListCasesForPipeline("all", PipelineReviewEarly)
	if err != nil {
		t.Fatal(err)
	}
	if got := caseIDs(reviewEarly); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("ListCasesForPipeline(review-early) = %v, want [a b]", got)
	}

	cheapGatesFirst, err := store.ListCasesForPipeline("all", PipelineCheapGatesFirst)
	if err != nil {
		t.Fatal(err)
	}
	if got := caseIDs(cheapGatesFirst); !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("ListCasesForPipeline(cheap-gates-first) = %v, want [c]", got)
	}

	any, err := store.ListCasesForPipeline("all", PipelineAny)
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(caseIDs(any), caseIDs(all)) {
		t.Fatalf("ListCasesForPipeline(any) = %v, want the same order and IDs as ListCases(all) = %v", caseIDs(any), caseIDs(all))
	}
	if len(any) != 3 {
		t.Fatalf("ListCasesForPipeline(any) = %d cases, want 3", len(any))
	}
}

// A pipeline filter is a view onto the resolved set, never a rebuild: the
// diversified pins (the held-out official set) must come out identical to
// what they were before a filtered call, even though the filtered call
// returns a narrower slice.
func TestListCasesForPipelineDoesNotDisturbTheDiversifiedPins(t *testing.T) {
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "review-early-1", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            []FindingGold{{ID: "g1", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g1", Severity: "error", Action: "auto-fix"}},
		roundFindings:   findingsJSON(findingSpec{ID: "g1", Severity: "error", File: "main.go", Line: 1, Description: "g1", Action: "auto-fix"}),
	})
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "review-early-2", fingerprint: "repo-b", capturedAt: 2, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            []FindingGold{{ID: "g2", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g2", Severity: "warning", Action: "auto-fix"}},
		roundFindings:   findingsJSON(findingSpec{ID: "g2", Severity: "warning", File: "main.go", Line: 1, Description: "g2", Action: "auto-fix"}),
	})
	cheapGatesFirst := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "cheap-gates-first-1", fingerprint: "repo-c", capturedAt: 3, changedLines: 10,
		pipelineVersion: PipelineCheapGatesFirst,
		gold:            []FindingGold{{ID: "g3", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g3", Severity: "error", Action: "auto-fix"}},
		roundFindings:   findingsJSON(findingSpec{ID: "g3", Severity: "error", File: "main.go", Line: 1, Description: "g3", Action: "auto-fix"}),
	})

	if _, err := store.ListCases("diversified"); err != nil {
		t.Fatal(err)
	}
	pinsBefore := mustDiversifiedPinRows(t, store)

	filtered, err := store.ListCasesForPipeline("diversified", PipelineCheapGatesFirst)
	if err != nil {
		t.Fatal(err)
	}
	if got := caseIDs(filtered); !reflect.DeepEqual(got, []string{cheapGatesFirst.ID}) {
		t.Fatalf("ListCasesForPipeline(diversified, cheap-gates-first) = %v, want only %q", got, cheapGatesFirst.ID)
	}

	pinsAfter := mustDiversifiedPinRows(t, store)
	if !reflect.DeepEqual(pinsBefore, pinsAfter) {
		t.Fatalf("a pipeline filter rebuilt the diversified pins:\nbefore: %#v\nafter: %#v", pinsBefore, pinsAfter)
	}
}

func TestListCasesForPipelineKeepsAnUnknownTagOutOfANarrowedSet(t *testing.T) {
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{id: "future", fingerprint: "repo-a", capturedAt: 1, changedLines: 10, pipelineVersion: PipelineVersion("future-layout")})

	narrowed, err := store.ListCasesForPipeline("all", PipelineReviewEarly)
	if err != nil {
		t.Fatal(err)
	}
	if len(narrowed) != 0 {
		t.Fatalf("ListCasesForPipeline(review-early) = %v, want the unrecognized tag excluded", caseIDs(narrowed))
	}

	any, err := store.ListCasesForPipeline("all", PipelineAny)
	if err != nil {
		t.Fatal(err)
	}
	if got := caseIDs(any); !reflect.DeepEqual(got, []string{"future"}) {
		t.Fatalf("ListCasesForPipeline(any) = %v, want the unrecognized tag included", got)
	}
}

func TestReplayEmptyFilteredSetNamesTheFilter(t *testing.T) {
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{id: "review-early-only", fingerprint: "repo-a", capturedAt: 1, changedLines: 10, pipelineVersion: PipelineReviewEarly})

	_, _, err := Replay(context.Background(), store, ReplayOptions{
		Set:       "all",
		Candidate: Candidate{Agent: types.AgentClaude, Model: "test"},
		Repeats:   1,
		Pipeline:  PipelineCheapGatesFirst,
	})
	if err == nil {
		t.Fatal("Replay = nil error, want the filtered set to be reported empty")
	}
	if !strings.Contains(err.Error(), "has no case tagged cheap-gates-first") {
		t.Fatalf("Replay error = %q, want it to name the filter", err.Error())
	}
}

// writeTwoPipelinePopulationEvaluations persists one evaluation each for one
// candidate and one cohort, tagged with different pipeline versions and
// different true-positive counts, so a report over them can only be correct
// if it keeps the two populations apart.
func writeTwoPipelinePopulationEvaluations(t *testing.T, store *Store) (reviewEarly, cheapGatesFirst Evaluation) {
	t.Helper()
	reviewEarlyCase := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "review-early-case", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            []FindingGold{{ID: "g1", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g1", Severity: "error", Action: "auto-fix"}},
	})
	cheapGatesFirstCase := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "cheap-gates-first-case", fingerprint: "repo-a", capturedAt: 2, changedLines: 10,
		pipelineVersion: PipelineCheapGatesFirst,
		gold:            []FindingGold{{ID: "g2", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g2", Severity: "error", Action: "auto-fix"}},
	})
	reviewEarly = Evaluation{
		ID: "eval-review-early", SessionID: "session", CaseID: reviewEarlyCase.ID, Candidate: "claude+test", Cohort: "cohort",
		Repeat: 1, Status: "completed", HasFindingGold: true, GoldCount: 1, TruePositive: 1,
		PipelineVersion: PipelineReviewEarly,
	}
	cheapGatesFirst = Evaluation{
		ID: "eval-cheap-gates-first", SessionID: "session", CaseID: cheapGatesFirstCase.ID, Candidate: "claude+test", Cohort: "cohort",
		Repeat: 1, Status: "completed", HasFindingGold: true, GoldCount: 2, TruePositive: 2,
		PipelineVersion: PipelineCheapGatesFirst,
	}
	if err := store.persistEvaluation(reviewEarlyCase, reviewEarly); err != nil {
		t.Fatal(err)
	}
	if err := store.persistEvaluation(cheapGatesFirstCase, cheapGatesFirst); err != nil {
		t.Fatal(err)
	}
	return reviewEarly, cheapGatesFirst
}

func TestReportSplitsTheTwoPipelinePopulations(t *testing.T) {
	store := openEvalStore(t)
	writeTwoPipelinePopulationEvaluations(t, store)

	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("Report = %d rows, want 2 (one per pipeline version)", len(reports))
	}
	if reports[0].PipelineVersion != PipelineCheapGatesFirst || reports[1].PipelineVersion != PipelineReviewEarly {
		t.Fatalf("Report order = [%s, %s], want cheap-gates-first before review-early", reports[0].PipelineVersion, reports[1].PipelineVersion)
	}
	combined := reports[0].Summary.TruePositive + reports[1].Summary.TruePositive
	for _, report := range reports {
		if report.Summary.TruePositive == combined {
			t.Fatalf("report for %s blended both populations: TruePositive %d equals the combined total %d", report.PipelineVersion, report.Summary.TruePositive, combined)
		}
	}
}

func TestReportReadsAnUntaggedEvaluationAsPreReorder(t *testing.T) {
	store := openEvalStore(t)
	c := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "untagged-case", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		gold: []FindingGold{{ID: "g1", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g1", Severity: "error", Action: "auto-fix"}},
	})
	// PipelineVersion left as the zero value: an evaluation JSON with no
	// pipeline_version key, exactly as one recorded before the field existed.
	if err := store.persistEvaluation(c, Evaluation{
		ID: "eval", SessionID: "session", CaseID: c.ID, Candidate: "claude+test", Cohort: "cohort",
		Repeat: 1, Status: "completed", HasFindingGold: true, GoldCount: 1, TruePositive: 1,
	}); err != nil {
		t.Fatal(err)
	}

	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatalf("Report = %d rows, want 1", len(reports))
	}
	if reports[0].PipelineVersion != PipelineReviewEarly {
		t.Fatalf("reports[0].PipelineVersion = %q, want %q for an untagged evaluation", reports[0].PipelineVersion, PipelineReviewEarly)
	}
}

func TestReportForPipelineNarrowsToOneLayout(t *testing.T) {
	store := openEvalStore(t)
	writeTwoPipelinePopulationEvaluations(t, store)

	reports, err := ReportForPipeline(store, PipelineReviewEarly)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].PipelineVersion != PipelineReviewEarly {
		t.Fatalf("ReportForPipeline(review-early) = %#v, want exactly one review-early row", reports)
	}
}

// A cheap-gates-first row's recall/cost relationship might make it dominate a
// review-early row IF they were compared, but comparing across pipeline
// versions is a category error: neither may be marked off the frontier
// because of the other.
func TestMarkFrontierNeverComparesAcrossPipelineVersions(t *testing.T) {
	cheap := 10.0
	expensive := 100.0
	reports := []CandidateReport{
		{PipelineVersion: PipelineCheapGatesFirst, Cohort: "cohort", Summary: EvaluationSummary{Labeled: 1, TruePositive: 1}, AverageTokens: &cheap},
		{PipelineVersion: PipelineReviewEarly, Cohort: "cohort", Summary: EvaluationSummary{Labeled: 1, TruePositive: 1}, AverageTokens: &expensive},
	}
	markFrontier(reports)
	if !reports[0].OnFrontier || !reports[1].OnFrontier {
		t.Fatalf("rows on different pipeline versions dominated each other: %#v", reports)
	}
}

func TestRenderReportNamesThePipelineVersion(t *testing.T) {
	output := RenderReport([]CandidateReport{{
		PipelineVersion: PipelineReviewEarly,
		Cohort:          "cohort",
		Summary:         EvaluationSummary{Candidate: "claude+test", Total: 1, Labeled: 1, TruePositive: 1},
	}})
	if !strings.Contains(output, "(pipeline review-early, cohort ") {
		t.Fatalf("report = %q, want the pipeline version named on the header line", output)
	}
}

func TestInspectSetsBucketsCasesByPipelineVersion(t *testing.T) {
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "review-early-gold", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            []FindingGold{{ID: "g1", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g1", Severity: "error", Action: "auto-fix"}},
	})
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "review-early-unlabeled", fingerprint: "repo-a", capturedAt: 2, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
	})
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "cheap-gates-first-gold", fingerprint: "repo-b", capturedAt: 3, changedLines: 10,
		pipelineVersion: PipelineCheapGatesFirst,
		gold:            []FindingGold{{ID: "g2", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g2", Severity: "error", Action: "auto-fix"}},
	})

	all := mustSetSummary(t, store, "all")
	want := []PipelineCountRow{
		{PipelineVersion: PipelineCheapGatesFirst, Cases: 1, GoldCases: 1},
		{PipelineVersion: PipelineReviewEarly, Cases: 2, GoldCases: 1},
	}
	if !reflect.DeepEqual(all.Pipelines, want) {
		t.Fatalf("all.Pipelines = %#v, want %#v", all.Pipelines, want)
	}
}

// Capture and relabel must stay idempotent with the pipeline tag present:
// running each twice must never duplicate gold, rewrite a manifest already on
// disk, or double-append evaluation rows.
func TestEvalCommandsStayIdempotentWithThePipelineTag(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	for _, name := range []types.StepName{"format", types.StepLint, types.StepTest} {
		step, err := sourceDB.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		forceStepOrder(t, p, step.ID, 1)
	}
	forceStepOrder(t, p, reviewRound.StepResultID, 10)

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("first capture = %d cases, want 1", len(first))
	}
	if got := manifestPipelineVersion(t, first[0].Dir); got != string(PipelineCheapGatesFirst) {
		t.Fatalf("manifest pipeline_version = %q, want %q", got, PipelineCheapGatesFirst)
	}

	second, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("second capture = %#v, want the same single case %q", second, first[0].ID)
	}
	manifestAfterSecondCapture := mustReadFile(t, filepath.Join(first[0].Dir, "manifest.json"))

	if err := sourceDB.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	firstRelabel, err := RelabelRun(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	labelsAfterFirstRelabel := mustReadFile(t, filepath.Join(first[0].Dir, "labels.json"))
	secondRelabel, err := RelabelRun(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstRelabel) != len(secondRelabel) {
		t.Fatalf("relabel case counts differ: %d vs %d", len(firstRelabel), len(secondRelabel))
	}
	if got := mustReadFile(t, filepath.Join(first[0].Dir, "labels.json")); got != labelsAfterFirstRelabel {
		t.Fatalf("second relabel rewrote labels.json:\nfirst: %s\nsecond: %s", labelsAfterFirstRelabel, got)
	}
	if got := mustReadFile(t, filepath.Join(first[0].Dir, "manifest.json")); got != manifestAfterSecondCapture {
		t.Fatalf("relabel rewrote manifest.json:\nbefore: %s\nafter: %s", manifestAfterSecondCapture, got)
	}
	if got := manifestPipelineVersion(t, first[0].Dir); got != string(PipelineCheapGatesFirst) {
		t.Fatalf("manifest pipeline_version after relabel = %q, want unchanged %q", got, PipelineCheapGatesFirst)
	}

	all, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("corpus has %d cases after double capture/relabel, want 1", len(all))
	}
}
