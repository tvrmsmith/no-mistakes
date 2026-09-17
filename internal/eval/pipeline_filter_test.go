package eval

import (
	"context"
	"path/filepath"
	"reflect"
	"strconv"
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
	writeSyntheticCase(t, store, syntheticCaseSpec{id: "a", fingerprint: "repo-a", capturedAt: 1, changedLines: 10, pipelineVersion: PipelineReviewEarly})
	writeSyntheticCase(t, store, syntheticCaseSpec{id: "b", fingerprint: "repo-a", capturedAt: 2, changedLines: 10, pipelineVersion: PipelineReviewEarly})
	writeSyntheticCase(t, store, syntheticCaseSpec{id: "c", fingerprint: "repo-a", capturedAt: 3, changedLines: 10, pipelineVersion: PipelineCheapGatesFirst})

	reviewEarly, err := store.ListCasesForPipeline(context.Background(), "all", PipelineReviewEarly)
	if err != nil {
		t.Fatal(err)
	}
	if got := caseIDs(reviewEarly); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("ListCasesForPipeline(review-early) = %v, want [a b]", got)
	}

	cheapGatesFirst, err := store.ListCasesForPipeline(context.Background(), "all", PipelineCheapGatesFirst)
	if err != nil {
		t.Fatal(err)
	}
	if got := caseIDs(cheapGatesFirst); !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("ListCasesForPipeline(cheap-gates-first) = %v, want [c]", got)
	}

	unfiltered, err := store.ListCasesForPipeline(context.Background(), "all", PipelineAny)
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.ListCases(context.Background(), "all")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(caseIDs(unfiltered), caseIDs(all)) {
		t.Fatalf("ListCasesForPipeline(any) = %v, want the same order and IDs as ListCases(all) = %v", caseIDs(unfiltered), caseIDs(all))
	}
	if len(unfiltered) != 3 {
		t.Fatalf("ListCasesForPipeline(any) = %d cases, want 3", len(unfiltered))
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

	if _, err := store.ListCases(context.Background(), "diversified"); err != nil {
		t.Fatal(err)
	}
	pinsBefore := mustDiversifiedPinRows(t, store)
	if len(pinsBefore) == 0 {
		t.Fatal("no diversified pin exists to protect, so this test would pass vacuously")
	}

	filtered, err := store.ListCasesForPipeline(context.Background(), "diversified", PipelineCheapGatesFirst)
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

	narrowed, err := store.ListCasesForPipeline(context.Background(), "all", PipelineReviewEarly)
	if err != nil {
		t.Fatal(err)
	}
	if len(narrowed) != 0 {
		t.Fatalf("ListCasesForPipeline(review-early) = %v, want the unrecognized tag excluded", caseIDs(narrowed))
	}

	any, err := store.ListCasesForPipeline(context.Background(), "all", PipelineAny)
	if err != nil {
		t.Fatal(err)
	}
	if got := caseIDs(any); !reflect.DeepEqual(got, []string{"future"}) {
		t.Fatalf("ListCasesForPipeline(any) = %v, want the unrecognized tag included", got)
	}
}

// The positive side of the replay filter: a filtered plan must reserve exactly
// the tagged cases, not merely fail loudly when none match.
func TestPrepareReplayPlansOnlyTheTaggedCases(t *testing.T) {
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{id: "review-early-1", fingerprint: "repo-a", capturedAt: 1, changedLines: 10, pipelineVersion: PipelineReviewEarly})
	writeSyntheticCase(t, store, syntheticCaseSpec{id: "cheap-gates-first-1", fingerprint: "repo-a", capturedAt: 2, changedLines: 10, pipelineVersion: PipelineCheapGatesFirst})
	writeSyntheticCase(t, store, syntheticCaseSpec{id: "review-early-2", fingerprint: "repo-b", capturedAt: 3, changedLines: 10, pipelineVersion: PipelineReviewEarly})
	writeSyntheticCase(t, store, syntheticCaseSpec{id: "cheap-gates-first-2", fingerprint: "repo-b", capturedAt: 4, changedLines: 10, pipelineVersion: PipelineCheapGatesFirst})

	planned, session, err := store.prepareReplay(context.Background(), ReplayOptions{
		Set:       "all",
		Candidate: Candidate{Agent: types.AgentClaude, Model: "test"},
		Repeats:   1,
		Pipeline:  PipelineCheapGatesFirst,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cheap-gates-first-1", "cheap-gates-first-2"}
	if got := caseIDs(planned); !reflect.DeepEqual(got, want) {
		t.Fatalf("prepareReplay(cheap-gates-first) planned %v, want exactly %v", got, want)
	}
	if !reflect.DeepEqual(session.CaseIDs, want) {
		t.Fatalf("session.CaseIDs = %v, want exactly %v", session.CaseIDs, want)
	}
}

// A filtered `eval sets` narrows every per-set figure to the requested layout,
// but the layout breakdown itself stays whole: it is what an operator reads to
// decide which layout is worth a filtered report at all.
func TestInspectSetsForPipelineNarrowsTheFiguresAndKeepsTheWholeBreakdown(t *testing.T) {
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "review-early-gold", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            []FindingGold{{ID: "g1", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g1", Severity: "error", Action: "auto-fix"}},
		roundFindings:   findingsJSON(findingSpec{ID: "g1", Severity: "error", File: "main.go", Line: 1, Description: "g1", Action: "auto-fix"}),
	})
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "cheap-gates-first-gold", fingerprint: "repo-b", capturedAt: 2, changedLines: 10,
		pipelineVersion: PipelineCheapGatesFirst,
		gold:            []FindingGold{{ID: "g2", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g2", Severity: "warning", Action: "auto-fix"}},
		// The recorded review missed this one, so the filtered self-score can
		// only be right if it scored this case alone.
		roundFindings: findingsJSON(),
	})

	summaries, err := InspectSetsForPipeline(context.Background(), store, PipelineCheapGatesFirst)
	if err != nil {
		t.Fatal(err)
	}
	var all SetSummary
	for _, summary := range summaries {
		if summary.Name == "all" {
			all = summary
		}
	}
	if all.Cases != 1 || all.GoldCases != 1 {
		t.Fatalf("filtered all summary = %d case(s), %d gold, want 1 and 1", all.Cases, all.GoldCases)
	}
	if all.SelfScore.TruePositive != 0 || all.SelfScore.FalseNegative != 1 {
		t.Fatalf("filtered self-score = TP %d / FN %d, want the cheap-gates-first case alone (TP 0 / FN 1)", all.SelfScore.TruePositive, all.SelfScore.FalseNegative)
	}
	wantPipelines := []PipelineCountRow{
		{PipelineVersion: PipelineCheapGatesFirst, Cases: 1, GoldCases: 1},
		{PipelineVersion: PipelineReviewEarly, Cases: 1, GoldCases: 1},
	}
	if !reflect.DeepEqual(all.Pipelines, wantPipelines) {
		t.Fatalf("filtered all.Pipelines = %#v, want the unfiltered breakdown %#v", all.Pipelines, wantPipelines)
	}
	if all.ScoredLayouts != 1 {
		t.Fatalf("filtered all.ScoredLayouts = %d, want only the scored layout counted", all.ScoredLayouts)
	}
}

// The self-score caveat has to describe the population the score folded, not
// the whole set. A filter that leaves one layout makes the score comparable,
// even though the breakdown beside it still lists both layouts.
func TestInspectSetsForPipelineScopesTheScoredBreakdownToTheFilter(t *testing.T) {
	store := openEvalStore(t)
	for i := 1; i <= 20; i++ {
		id := "review-early-" + strconv.Itoa(i)
		writeSyntheticCase(t, store, syntheticCaseSpec{
			id: id, fingerprint: "repo-early-" + strconv.Itoa(i), capturedAt: int64(i), changedLines: 10,
			pipelineVersion: PipelineReviewEarly,
			gold:            []FindingGold{{ID: id, Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: id, Severity: "error", Action: "auto-fix"}},
			roundFindings:   findingsJSON(findingSpec{ID: id, Severity: "error", File: "main.go", Line: 1, Description: id, Action: "auto-fix"}),
		})
	}
	for i := 1; i <= 10; i++ {
		id := "cheap-gates-first-" + strconv.Itoa(i)
		writeSyntheticCase(t, store, syntheticCaseSpec{
			id: id, fingerprint: "repo-late-" + strconv.Itoa(i), capturedAt: int64(100 + i), changedLines: 10,
			pipelineVersion: PipelineCheapGatesFirst,
			gold:            []FindingGold{{ID: id, Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: id, Severity: "error", Action: "auto-fix"}},
			roundFindings:   findingsJSON(findingSpec{ID: id, Severity: "error", File: "main.go", Line: 1, Description: id, Action: "auto-fix"}),
		})
	}

	summaries, err := InspectSetsForPipeline(context.Background(), store, PipelineReviewEarly)
	if err != nil {
		t.Fatal(err)
	}
	var diversified SetSummary
	for _, summary := range summaries {
		if summary.Name == "diversified" {
			diversified = summary
		}
	}
	if len(diversified.Pipelines) != 2 {
		t.Fatalf("diversified.Pipelines = %#v, want both layouts in the breakdown", diversified.Pipelines)
	}
	if diversified.ScoredLayouts != 1 {
		t.Fatalf("diversified.ScoredLayouts = %d, want only the filtered layout counted", diversified.ScoredLayouts)
	}
}

// The caveat counts layouts over the population the score folded, and an
// unlabeled case is folded into nothing. Counting it warned that a single-layout
// score spanned two layouts.
func TestInspectSetsCountsScoredLayoutsOverTheGoldBearingCasesOnly(t *testing.T) {
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "review-early-gold", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            []FindingGold{{ID: "g1", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "g1", Severity: "error", Action: "auto-fix"}},
		roundFindings:   findingsJSON(findingSpec{ID: "g1", Severity: "error", File: "main.go", Line: 1, Description: "g1", Action: "auto-fix"}),
	})
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "cheap-gates-first-unlabeled", fingerprint: "repo-b", capturedAt: 2, changedLines: 10,
		pipelineVersion: PipelineCheapGatesFirst,
	})

	all := mustSetSummary(t, store, "all")
	if all.Cases != 2 || len(all.Pipelines) != 2 {
		t.Fatalf("all summary = %d case(s) across %d layout(s), want both cases and both layouts present", all.Cases, len(all.Pipelines))
	}
	if all.ScoredLayouts != 1 {
		t.Fatalf("all.ScoredLayouts = %d, want 1: the unlabeled case contributes nothing to the self-score", all.ScoredLayouts)
	}
}

// The eval run caveat is the same rule on the other surface: an evaluation with
// no gold is not folded into the score, so its layout must not raise the
// not-comparable warning either.
func TestPipelineLayoutsInEvaluationsCountsTheScoredReplaysOnly(t *testing.T) {
	evaluations := []Evaluation{
		{PipelineVersion: PipelineReviewEarly, Status: "completed", HasFindingGold: true, GoldCount: 1, TruePositive: 1},
		{PipelineVersion: PipelineCheapGatesFirst, Status: "completed"},
	}
	if got := PipelineLayoutsInEvaluations(evaluations); got != 1 {
		t.Fatalf("PipelineLayoutsInEvaluations = %d, want 1: the unscored replay contributes nothing", got)
	}
	if summary := SummarizeEvaluations(evaluations); summary.Labeled != 1 {
		t.Fatalf("SummarizeEvaluations folded %d labeled replay(s), want the layout count to describe exactly those", summary.Labeled)
	}
	// A failed replay still carries its gold into the score as false negatives,
	// so its layout does belong in the count.
	withFailedGold := append(evaluations, Evaluation{PipelineVersion: PipelineCheapGatesFirst, Status: "failed", HasFindingGold: true, GoldCount: 1})
	if got := PipelineLayoutsInEvaluations(withFailedGold); got != 2 {
		t.Fatalf("PipelineLayoutsInEvaluations = %d, want 2: a failed replay with gold is scored", got)
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

	reports, err := ReportForPipeline(store, PipelineAny)
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

	reports, err := ReportForPipeline(store, PipelineAny)
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

// "the filter matched nothing" and "nothing exists" need different fixes, and
// all three surfaces (eval sets, eval report, eval run) say so the same way.
func TestFilteredEmptyResultsNameTheFilterRatherThanAnEmptyCorpus(t *testing.T) {
	store := openEvalStore(t)
	// Two gold cases in different strata against a holdout of one, so tune
	// holds the leftover: with every gold case pinned, tune would be empty for
	// a reason that has nothing to do with the filter and the arm under test
	// would never run.
	store.SetDiversifiedSize(1)
	for i, fingerprint := range []string{"repo-a", "repo-b"} {
		id := "review-early-gold-" + strconv.Itoa(i+1)
		writeSyntheticCase(t, store, syntheticCaseSpec{
			id: id, fingerprint: fingerprint, capturedAt: int64(i + 1), changedLines: 10,
			pipelineVersion: PipelineReviewEarly,
			gold:            []FindingGold{{ID: id, Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: id, Severity: "error", Action: "auto-fix"}},
			roundFindings:   findingsJSON(findingSpec{ID: id, Severity: "error", File: "main.go", Line: 1, Description: id, Action: "auto-fix"}),
		})
	}
	for _, name := range []string{"diversified", "tune"} {
		cases, err := store.ListCasesForPipeline(context.Background(), name, PipelineReviewEarly)
		if err != nil {
			t.Fatal(err)
		}
		if len(cases) == 0 {
			t.Fatalf("set %q is empty before the filter runs, so a filter miss cannot be told apart from it", name)
		}
	}

	summaries, err := InspectSetsForPipeline(context.Background(), store, PipelineCheapGatesFirst)
	if err != nil {
		t.Fatal(err)
	}
	warned := map[string]string{}
	for _, summary := range summaries {
		warned[summary.Name] = summary.Warning
	}
	for _, name := range []string{"diversified", "tune"} {
		if !strings.Contains(warned[name], "has no case tagged cheap-gates-first") {
			t.Fatalf("%s warning = %q, want it to name the filter rather than report an empty set", name, warned[name])
		}
	}

	reports, err := ReportForPipeline(store, PipelineCheapGatesFirst)
	if err != nil {
		t.Fatal(err)
	}
	rendered := RenderReportForPipeline(reports, PipelineCheapGatesFirst)
	if !strings.Contains(rendered, "no candidate replay tagged cheap-gates-first recorded yet") {
		t.Fatalf("report = %q, want the empty result to name the filter", rendered)
	}
	if unfiltered := RenderReportForPipeline(nil, PipelineAny); !strings.Contains(unfiltered, "no candidate replays recorded yet") {
		t.Fatalf("unfiltered report = %q, want the plain empty message", unfiltered)
	}
}

// writeUnlabeledReviewEarlyCorpus seeds n unlabeled cases, all under the same
// tag, so a filter for that same tag matches every one of them.
func writeUnlabeledReviewEarlyCorpus(t *testing.T, store *Store, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		writeSyntheticCase(t, store, syntheticCaseSpec{
			id: "unlabeled-" + strconv.Itoa(i), fingerprint: "repo-a", capturedAt: int64(i), changedLines: 10,
			pipelineVersion: PipelineReviewEarly,
		})
	}
}

// replayRefusal returns the sentence eval run refuses an empty set with, so a
// scenario can assert the two surfaces agree.
func replayRefusal(t *testing.T, store *Store, set string, version PipelineVersion) string {
	t.Helper()
	_, _, err := Replay(context.Background(), store, ReplayOptions{
		Set:       set,
		Candidate: Candidate{Agent: types.AgentClaude, Model: "test"},
		Repeats:   1,
		Pipeline:  version,
	})
	if err == nil {
		t.Fatalf("Replay(%q, %s) = nil error, want the empty set refused", set, version)
	}
	return err.Error()
}

// Every case in the corpus carries the tag being filtered for, so an empty set
// is empty for its own reason. Naming the tag there is factually wrong and
// sends the operator to change the filter when the fix is to hold out less.
func TestFilteredTuneEmptyForItsOwnReasonKeepsTheHoldoutWarning(t *testing.T) {
	store := openEvalStore(t)
	id := "review-early-gold"
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: id, fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		pipelineVersion: PipelineReviewEarly,
		gold:            []FindingGold{{ID: id, Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: id, Severity: "error", Action: "auto-fix"}},
		roundFindings:   findingsJSON(findingSpec{ID: id, Severity: "error", File: "main.go", Line: 1, Description: id, Action: "auto-fix"}),
	})

	summaries, err := InspectSetsForPipeline(context.Background(), store, PipelineReviewEarly)
	if err != nil {
		t.Fatal(err)
	}
	warning := ""
	for _, summary := range summaries {
		if summary.Name == "tune" {
			warning = summary.Warning
		}
	}
	if strings.Contains(warning, "has no case tagged") {
		t.Fatalf("tune warning = %q, want the holdout reason rather than the tag: every case IS review-early", warning)
	}
	if !strings.Contains(warning, "tune is empty") {
		t.Fatalf("tune warning = %q, want the matcher-threshold warning", warning)
	}
	if refusal := replayRefusal(t, store, "tune", PipelineReviewEarly); !strings.Contains(refusal, `case set "tune" is empty`) {
		t.Fatalf("Replay refusal = %q, want it to agree with the eval sets warning", refusal)
	}
}

// Same rule for a corpus that simply has no gold: the filter matched every
// case, so the fix is to label gold, not to pick another tag.
func TestFilteredDiversifiedEmptyForItsOwnReasonKeepsTheMissingGoldWarning(t *testing.T) {
	store := openEvalStore(t)
	writeUnlabeledReviewEarlyCorpus(t, store, 5)

	summaries, err := InspectSetsForPipeline(context.Background(), store, PipelineReviewEarly)
	if err != nil {
		t.Fatal(err)
	}
	warning := ""
	for _, summary := range summaries {
		if summary.Name == "diversified" {
			warning = summary.Warning
		}
	}
	if strings.Contains(warning, "has no case tagged") {
		t.Fatalf("diversified warning = %q, want the missing-gold reason rather than the tag: every case IS review-early", warning)
	}
	if !strings.Contains(warning, "no labeled gold") {
		t.Fatalf("diversified warning = %q, want the missing-gold message", warning)
	}
	if refusal := replayRefusal(t, store, "diversified", PipelineReviewEarly); !strings.Contains(refusal, `case set "diversified" is empty`) {
		t.Fatalf("Replay refusal = %q, want it to agree with the eval sets warning", refusal)
	}
}

// The other half of the same rule: a set that was already empty before the
// filter IS a filter miss when nothing in the corpus carries the requested tag,
// because then the tag really is why the operator sees nothing.
func TestEmptySetUnderATagNoCaseCarriesStillNamesTheFilter(t *testing.T) {
	store := openEvalStore(t)
	writeUnlabeledReviewEarlyCorpus(t, store, 5)

	summaries, err := InspectSetsForPipeline(context.Background(), store, PipelineCheapGatesFirst)
	if err != nil {
		t.Fatal(err)
	}
	for _, summary := range summaries {
		if summary.Name != "diversified" && summary.Name != "tune" {
			continue
		}
		if !strings.Contains(summary.Warning, "has no case tagged cheap-gates-first") {
			t.Fatalf("%s warning = %q, want it to name the filter no case carries", summary.Name, summary.Warning)
		}
	}
	if refusal := replayRefusal(t, store, "diversified", PipelineCheapGatesFirst); !strings.Contains(refusal, "has no case tagged cheap-gates-first") {
		t.Fatalf("Replay refusal = %q, want it to agree with the eval sets warning", refusal)
	}
}

// The diversified warning must still report missing gold when no filter is
// narrowing anything, so the filter-aware branch cannot swallow it.
func TestUnfilteredEmptyDiversifiedStillReportsMissingGold(t *testing.T) {
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{id: "unlabeled", fingerprint: "repo-a", capturedAt: 1, changedLines: 10, pipelineVersion: PipelineReviewEarly})

	diversified := mustSetSummary(t, store, "diversified")
	if !strings.Contains(diversified.Warning, "no labeled gold") {
		t.Fatalf("diversified warning = %q, want the missing-gold message", diversified.Warning)
	}
}

// CurrentPipelineVersion derives a step's order from its index in
// types.AllSteps(). That is only correct while types.StepName.Order() agrees
// with that index, and the two live in different packages.
func TestAllStepsIndexMatchesTheDeclaredStepOrder(t *testing.T) {
	for i, name := range types.AllSteps() {
		if got := name.Order(); got != i+1 {
			t.Fatalf("types.AllSteps()[%d] = %q with Order() %d, want %d: CurrentPipelineVersion reads the index as the order", i, name, got, i+1)
		}
	}
}

func TestRenderReportNamesThePipelineVersion(t *testing.T) {
	output := renderReportAny([]CandidateReport{{
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

	all, err := store.ListCases(context.Background(), "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("corpus has %d cases after double capture/relabel, want 1", len(all))
	}
}
