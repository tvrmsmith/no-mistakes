package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type findingHistoryDoc struct {
	Outcome             string              `toon:"outcome"`
	FindingHistory      []findingHistoryRow `toon:"finding_history"`
	FindingHistoryError string              `toon:"finding_history_error"`
}

func decodeFindingHistoryDoc(t *testing.T, out string) findingHistoryDoc {
	t.Helper()
	var doc findingHistoryDoc
	if err := toon.UnmarshalString(out, &doc); err != nil {
		t.Fatalf("decode TOON: %v\n%s", err, out)
	}
	return doc
}

func insertHistoryRound(t *testing.T, database *db.DB, stepID string, round int, trigger string, items []types.Finding) *db.StepRound {
	t.Helper()
	var raw *string
	if items != nil {
		s := findingsJSON(t, items, "summary")
		raw = &s
	}
	r, err := database.InsertStepRound(stepID, round, trigger, raw, nil, 10)
	if err != nil {
		t.Fatalf("insert round: %v", err)
	}
	return r
}

func finishedRunOnCurrentBranch(t *testing.T, status types.RunStatus) (*db.DB, *db.Run) {
	t.Helper()
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/mine")
	chdir(t, repoDir)
	r, err := database.InsertRun(repo.ID, "feature/mine", "abcdef1234567890", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := database.UpdateRunStatus(r.ID, status); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	return database, r
}

// A --yes run resolves every gate inside the call, so the finished status is
// the only place a caller can read what each round raised and what was fixed.
func TestAxiStatusFinishedRunListsEveryRoundsFindingsAndSelection(t *testing.T) {
	database, r := finishedRunOnCurrentBranch(t, types.RunCompleted)
	longDesc := strings.Repeat("the reviewer's full reasoning ", 40)

	review, err := database.InsertStepResult(r.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert review step: %v", err)
	}
	r1 := insertHistoryRound(t, database, review.ID, 1, "initial", []types.Finding{
		{ID: "review-1", Severity: "error", File: "a.go", Line: 12, Action: types.ActionAutoFix, Description: "nil deref"},
		{ID: "review-2", Severity: "warning", Action: types.ActionAskUser, Description: longDesc},
		{ID: "review-3", Severity: "info", Action: types.ActionNoOp, Description: "nit"},
	})
	selected := `["review-1","review-2"]`
	dispatched := findingsJSON(t, []types.Finding{
		{ID: "review-1", Severity: "error", Action: types.ActionAutoFix, Description: "nil deref"},
		{ID: "user-1", Severity: "warning", Action: types.ActionAutoFix, Source: types.FindingSourceUser, Description: "also rename Foo"},
	}, "")
	if err := database.SetStepRoundUserDecision(r1.ID, &selected, db.RoundSelectionSourceUser, &dispatched); err != nil {
		t.Fatalf("record decision: %v", err)
	}
	r2 := insertHistoryRound(t, database, review.ID, 2, "auto_fix", []types.Finding{
		{ID: "review-1", Severity: "warning", Description: "still ambiguous"},
	})
	if err := database.SetStepRoundDeclined(r2.ID); err != nil {
		t.Fatalf("record decline: %v", err)
	}

	lint, err := database.InsertStepResult(r.ID, types.StepLint)
	if err != nil {
		t.Fatalf("insert lint step: %v", err)
	}
	insertHistoryRound(t, database, lint.ID, 1, "initial", nil)

	out := axiStatusOutput(t, "")
	doc := decodeFindingHistoryDoc(t, out)
	want := []findingHistoryRow{
		{Step: "review", Round: 1, ID: "review-1", Severity: "error", Action: types.ActionAutoFix, Source: "agent", Selected: true, File: "a.go", Line: 12, Description: "nil deref"},
		{Step: "review", Round: 1, ID: "review-2", Severity: "warning", Action: types.ActionAskUser, Source: "agent", Selected: true, Description: longDesc},
		{Step: "review", Round: 1, ID: "review-3", Severity: "info", Action: types.ActionNoOp, Source: "agent", Selected: false, Description: "nit"},
		{Step: "review", Round: 1, ID: "user-1", Severity: "warning", Action: types.ActionAutoFix, Source: "user", Selected: true, Description: "also rename Foo"},
		{Step: "review", Round: 2, ID: "review-1", Severity: "warning", Action: types.ActionAskUser, Source: "agent", Selected: false, Description: "still ambiguous"},
	}
	if len(doc.FindingHistory) != len(want) {
		t.Fatalf("finding_history has %d rows, want %d:\n%s", len(doc.FindingHistory), len(want), out)
	}
	for i := range want {
		if doc.FindingHistory[i] != want[i] {
			t.Errorf("row %d = %+v\nwant   %+v", i, doc.FindingHistory[i], want[i])
		}
	}
	if strings.Contains(out, "truncated") {
		t.Fatalf("finding history must carry the full description, not the gate's cut:\n%s", out)
	}
}

// An empty history must still render its key, so a consumer can tell a run
// that found nothing from a binary that does not emit the history.
func TestAxiStatusFinishedRunWithNoFindingsRendersAnEmptyHistory(t *testing.T) {
	database, r := finishedRunOnCurrentBranch(t, types.RunCompleted)
	step, err := database.InsertStepResult(r.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	insertHistoryRound(t, database, step.ID, 1, "initial", nil)

	out := axiStatusOutput(t, "")
	if !strings.Contains(out, "\nfinding_history[0]:\n") {
		t.Fatalf("want an explicit empty finding_history:\n%s", out)
	}
}

func TestAxiStatusUnreadableRoundReportsAHistoryErrorNotAnEmptyHistory(t *testing.T) {
	database, r := finishedRunOnCurrentBranch(t, types.RunFailed)
	step, err := database.InsertStepResult(r.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	bad := "{not json"
	if _, err := database.InsertStepRound(step.ID, 1, "initial", &bad, nil, 10); err != nil {
		t.Fatalf("insert round: %v", err)
	}

	doc := decodeFindingHistoryDoc(t, axiStatusOutput(t, ""))
	if doc.FindingHistory != nil || !strings.Contains(doc.FindingHistoryError, "review round 1 findings") {
		t.Fatalf("want finding_history_error naming the round and no history, got %+v", doc)
	}
}

func TestAxiStatusActiveRunCarriesNoFindingHistory(t *testing.T) {
	_, _ = finishedRunOnCurrentBranch(t, types.RunRunning)
	out := axiStatusOutput(t, "")
	if strings.Contains(out, "finding_history") {
		t.Fatalf("an unfinished run must not claim a finding history:\n%s", out)
	}
}

func TestRenderDriveResultCarriesFindingHistoryOnEveryFinalResult(t *testing.T) {
	history := findingHistory{rows: []findingHistoryRow{
		{Step: "review", Round: 1, ID: "review-1", Severity: "warning", Action: types.ActionAskUser, Source: "agent", Description: "why"},
	}}
	prURL := "https://example.test/pr/1"
	cases := []struct {
		name    string
		status  types.RunStatus
		ciReady bool
		outcome string
	}{
		{"checks-passed", types.RunRunning, true, "checks-passed"},
		{"passed", types.RunCompleted, false, "passed"},
		{"failed", types.RunFailed, false, "failed"},
		{"cancelled", types.RunCancelled, false, "cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			run := &ipc.RunInfo{ID: "r1", Branch: "feature/x", Status: tc.status, PRURL: &prURL, CIReady: tc.ciReady}
			_ = renderDriveResult(cmd, run, tc.ciReady, history)
			doc := decodeFindingHistoryDoc(t, out.String())
			if doc.Outcome != tc.outcome || len(doc.FindingHistory) != 1 || doc.FindingHistory[0] != history.rows[0] {
				t.Fatalf("outcome %q history %+v:\n%s", doc.Outcome, doc.FindingHistory, out.String())
			}
		})
	}
}

func TestRenderDriveResultGateCarriesNoFindingHistory(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	gateFindings := findingsJSON(t, []types.Finding{{ID: "review-1", Action: types.ActionAskUser, Description: "x"}}, "s")
	run := &ipc.RunInfo{ID: "r1", Branch: "feature/x", Status: types.RunRunning, Steps: []ipc.StepResultInfo{
		{ID: "s1", StepName: types.StepReview, Status: types.StepStatusAwaitingApproval, FindingsJSON: &gateFindings},
	}}
	history := findingHistory{rows: []findingHistoryRow{{Step: "review", Round: 1, ID: "review-1"}}}
	if err := renderDriveResult(cmd, run, false, history); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out.String(), "finding_history") {
		t.Fatalf("a gate is not a final result:\n%s", out.String())
	}
}
