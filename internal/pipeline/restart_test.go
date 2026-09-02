package pipeline

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRestartBoundaryIsTheFirstValidationStep pins the boundary to the first
// step of the validation region. Issues #7/#8 move that region's head to
// Format; this assertion is what makes the retarget break loudly instead of
// leaving a boundary that silently sits in the middle of the region.
func TestRestartBoundaryIsTheFirstValidationStep(t *testing.T) {
	if RestartBoundary != types.StepReview {
		t.Fatalf("RestartBoundary = %q, want %q", RestartBoundary, types.StepReview)
	}
	if RestartBoundary.Order() >= types.StepTest.Order() {
		t.Fatalf("RestartBoundary order = %d, want less than Test's %d",
			RestartBoundary.Order(), types.StepTest.Order())
	}
}

// blockingFindingsJSON is one finding the executor treats as blocking without
// routing it to auto-fix or to the ask-user gate, so a test can park a step on
// NeedsApproval alone.
const blockingFindingsJSON = `{"findings":[{"id":"f1","severity":"error","description":"blocking","action":"no-op"}]}`

// TestExecutor_RestartTakesPrecedenceOverApprovalGate proves a step that both
// found blocking issues and asked for a restart restarts instead of parking.
// Its verdict already describes a tree the restart is about to replace, so
// parking would ask a human to rule on stale findings, and nothing would ever
// answer.
func TestExecutor_RestartTakesPrecedenceOverApprovalGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var order []types.StepName
	record := func(name types.StepName) { order = append(order, name) }
	documentCalls := 0
	steps := []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			record(types.StepReview)
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) {
			record(types.StepDocument)
			documentCalls++
			if documentCalls == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      blockingFindingsJSON,
					RestartFrom:   types.StepReview,
				}, nil
			}
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepLint, fn: func(*StepContext) (*StepOutcome, error) {
			record(types.StepLint)
			return &StepOutcome{}, nil
		}},
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)
	waitExecutorDone(t, done)

	want := []types.StepName{types.StepReview, types.StepDocument, types.StepReview, types.StepDocument, types.StepLint}
	if !slices.Equal(order, want) {
		t.Fatalf("execution order = %v, want %v", order, want)
	}
}

// TestExecutor_RestartPreventsReviewCertification proves a later step's restart
// revokes the certification the earlier review captured. Push accepts a
// certified ancestor, so a surviving first-pass approval would authorise a head
// nothing has reviewed.
func TestExecutor_RestartPreventsReviewCertification(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	reviewCalls := 0
	documentCalls := 0
	steps := []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			reviewCalls++
			if reviewCalls == 1 {
				return &StepOutcome{ReviewApprovedHeadSHA: "sha-review-1"}, nil
			}
			return &StepOutcome{ReviewApprovedHeadSHA: "sha-review-2"}, nil
		}},
		&adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) {
			documentCalls++
			if documentCalls == 1 {
				return &StepOutcome{RestartFrom: types.StepReview}, nil
			}
			return &StepOutcome{}, nil
		}},
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != "sha-review-2" {
		t.Fatalf("run.ReviewApprovedHeadSHA = %v, want sha-review-2", run.ReviewApprovedHeadSHA)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if stored.ReviewApprovedHeadSHA == nil || *stored.ReviewApprovedHeadSHA != "sha-review-2" {
		t.Fatalf("stored review_approved_head_sha = %v, want sha-review-2", stored.ReviewApprovedHeadSHA)
	}
}

// TestExecutor_RestartingReviewCertifiesNothing proves a review round that asks
// for a restart completes as an ordinary step. Certifying the head it is
// simultaneously sending back for revalidation would publish authority over a
// tree it has already declared unfinished.
func TestExecutor_RestartingReviewCertifiesNothing(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	intentCalls := 0
	reviewCalls := 0
	steps := []Step{
		&adaptiveCallStep{name: types.StepIntent, fn: func(*StepContext) (*StepOutcome, error) {
			intentCalls++
			if intentCalls == 2 {
				stored, err := database.GetRun(run.ID)
				if err != nil {
					t.Errorf("GetRun() error = %v", err)
				} else if stored.ReviewApprovedHeadSHA != nil {
					t.Errorf("review_approved_head_sha = %q during re-entry, want NULL", *stored.ReviewApprovedHeadSHA)
				}
			}
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			reviewCalls++
			if reviewCalls == 1 {
				return &StepOutcome{ReviewApprovedHeadSHA: "sha-a", RestartFrom: types.StepIntent}, nil
			}
			return &StepOutcome{ReviewApprovedHeadSHA: "sha-b"}, nil
		}},
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != "sha-b" {
		t.Fatalf("run.ReviewApprovedHeadSHA = %v, want sha-b", run.ReviewApprovedHeadSHA)
	}
}

// TestExecutor_PrepareRestartClearsStaleReviewCertification proves the restart
// itself revokes an approval it inherited, not just one the same run captured.
// A re-review that parks never reaches the certification write, so without this
// the stale column keeps authorising the push.
func TestExecutor_PrepareRestartClearsStaleReviewCertification(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, "stale-sha"); err != nil {
		t.Fatalf("UpdateRunReviewApprovedHeadSHA() error = %v", err)
	}
	seeded, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	run.ReviewApprovedHeadSHA = seeded.ReviewApprovedHeadSHA

	reviewCalls := 0
	ciCalls := 0
	steps := []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
			reviewCalls++
			if reviewCalls == 2 {
				if sctx.Run.ReviewApprovedHeadSHA != nil {
					t.Errorf("in-memory review approval = %q on re-entry, want nil", *sctx.Run.ReviewApprovedHeadSHA)
				}
				stored, err := database.GetRun(run.ID)
				if err != nil {
					t.Errorf("GetRun() error = %v", err)
				} else if stored.ReviewApprovedHeadSHA != nil {
					t.Errorf("review_approved_head_sha = %q on re-entry, want NULL", *stored.ReviewApprovedHeadSHA)
				}
			}
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepCI, fn: func(*StepContext) (*StepOutcome, error) {
			ciCalls++
			if ciCalls == 1 {
				return &StepOutcome{RestartFrom: types.StepReview}, nil
			}
			return &StepOutcome{}, nil
		}},
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if reviewCalls != 2 {
		t.Fatalf("review executions = %d, want 2", reviewCalls)
	}
}

// TestExecutor_RestartCarriesFindingsIntoReEntry proves the restarting step
// sees what it said last time. Without the carry the step re-derives its own
// findings from scratch on every re-entry, which is how a fix loop repeats work
// it already reported.
func TestExecutor_RestartCarriesFindingsIntoReEntry(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	reviewCalls := 0
	documentCalls := 0
	steps := []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
			reviewCalls++
			if reviewCalls == 2 && sctx.PreviousFindings != "" {
				t.Errorf("boundary step previous findings = %q, want empty", sctx.PreviousFindings)
			}
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepDocument, fn: func(sctx *StepContext) (*StepOutcome, error) {
			documentCalls++
			if documentCalls == 2 {
				if !strings.Contains(sctx.PreviousFindings, `"f1"`) {
					t.Errorf("previous findings = %q, want it to carry f1", sctx.PreviousFindings)
				}
				if sctx.Fixing {
					t.Error("re-entry after a restart is not a fix round, want Fixing false")
				}
				return &StepOutcome{}, nil
			}
			return &StepOutcome{Findings: blockingFindingsJSON, RestartFrom: types.StepReview}, nil
		}},
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if documentCalls != 2 {
		t.Fatalf("document executions = %d, want 2", documentCalls)
	}
}

// TestExecutor_RestartCountsOnTheRun proves every restart is visible on the
// run. Termination is uncapped by design, so the count is the only signal that
// separates a healthy repair from a thrashing one.
func TestExecutor_RestartCountsOnTheRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	documentCalls := 0
	steps := []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) {
			documentCalls++
			if documentCalls <= 2 {
				return &StepOutcome{RestartFrom: types.StepReview}, nil
			}
			return &StepOutcome{}, nil
		}},
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if run.RestartCount != 2 {
		t.Fatalf("run.RestartCount = %d, want 2", run.RestartCount)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if stored.RestartCount != 2 {
		t.Fatalf("stored restart_count = %d, want 2", stored.RestartCount)
	}
}

// TestExecutor_RestartDoesNotRefillAutoFixBudget is half the termination
// argument: ResetStepsFrom leaves step_rounds intact, so a step re-entered by a
// restart recounts the auto-fix rounds it already spent instead of starting
// over with a full budget.
func TestExecutor_RestartDoesNotRefillAutoFixBudget(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	documentCalls := 0
	steps := []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				AutoFixable: true,
				Findings:    `{"findings":[{"id":"f1","severity":"warning","description":"fixable","action":"auto-fix"}]}`,
			}, nil
		}},
		&adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) {
			documentCalls++
			if documentCalls == 1 {
				return &StepOutcome{RestartFrom: types.StepReview}, nil
			}
			return &StepOutcome{}, nil
		}},
	}

	cfg := &config.Config{AutoFix: config.AutoFix{Review: 1}}
	exec := NewExecutor(database, p, cfg, nil, steps, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("GetStepsByRun() error = %v", err)
	}
	autoFixRounds := 0
	for _, result := range results {
		if result.StepName != types.StepReview {
			continue
		}
		rounds, err := database.GetRoundsByStep(result.ID)
		if err != nil {
			t.Fatalf("GetRoundsByStep() error = %v", err)
		}
		for _, round := range rounds {
			if round.SelectionSource != nil && *round.SelectionSource == db.RoundSelectionSourceAutoFix {
				autoFixRounds++
			}
		}
	}
	if autoFixRounds != 1 {
		t.Fatalf("auto-fix rounds across the run = %d, want 1", autoFixRounds)
	}
}
