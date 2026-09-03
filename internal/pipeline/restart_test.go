package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
// nothing has reviewed. The re-review deliberately certifies nothing, which is
// what a re-entry that parks or defers looks like, so the only way the column
// can hold a value at the end is a revocation that never happened.
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
			return &StepOutcome{}, nil
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
	if reviewCalls != 2 {
		t.Fatalf("review executions = %d, want 2", reviewCalls)
	}

	if run.ReviewApprovedHeadSHA != nil {
		t.Fatalf("run.ReviewApprovedHeadSHA = %q, want nil", *run.ReviewApprovedHeadSHA)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if stored.ReviewApprovedHeadSHA != nil {
		t.Fatalf("stored review_approved_head_sha = %q, want NULL", *stored.ReviewApprovedHeadSHA)
	}
}

// TestExecutor_RestartingReviewCertifiesNothing proves a review round that asks
// for a restart completes as an ordinary step. Certifying the head it is
// simultaneously sending back for revalidation would publish authority over a
// tree it has already declared unfinished.
//
// The restart deliberately names a boundary the pipeline rejects (review's own
// index), so the run fails before prepareRestart can revoke anything. That is
// what makes the assertion discriminating: the completion write is the only
// thing that could have put a value in the column, so a stored SHA here means
// the restarting round certified.
func TestExecutor_RestartingReviewCertifiesNothing(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{ReviewApprovedHeadSHA: "sha-a", RestartFrom: types.StepReview}, nil
		}},
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil {
		t.Fatal("Execute() error = nil, want the invalid restart boundary to fail the run")
	}
	if !strings.Contains(err.Error(), "invalid restart") {
		t.Fatalf("Execute() error = %v, want it to name the invalid restart boundary", err)
	}

	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if stored.ReviewApprovedHeadSHA != nil {
		t.Fatalf("stored review_approved_head_sha = %q, want NULL from a round that asked for a restart", *stored.ReviewApprovedHeadSHA)
	}
	if run.ReviewApprovedHeadSHA != nil {
		t.Fatalf("run.ReviewApprovedHeadSHA = %q, want nil", *run.ReviewApprovedHeadSHA)
	}
}

// TestExecutor_RestartCompletesNormallyWhenTheBoundaryIsValid keeps the happy
// path the discriminating test above gave up: a valid restart re-enters, and
// the re-review's own certification is the one that lands.
func TestExecutor_RestartCompletesNormallyWhenTheBoundaryIsValid(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	reviewCalls := 0
	steps := []Step{
		&adaptiveCallStep{name: types.StepIntent, fn: func(*StepContext) (*StepOutcome, error) {
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

// TestExecutor_RestartIntoASkippedBoundary covers the run whose operator
// skipped the boundary step. Rewinding there re-marks it skipped and walks
// straight back to the requesting step, repeating every agent pass in between
// while nothing revalidates, so the run must not rewind either way. Whether it
// dies there turns on publication: a run that still pushes would refuse three
// steps later on the certification the skipped review never wrote, naming
// nothing about the skip, while --skip review --skip push is the documented
// validate-without-publishing mode with no certification to protect. Document's
// and Lint's agent commits make the request routine, so failing that mode would
// break it outright.
func TestExecutor_RestartIntoASkippedBoundary(t *testing.T) {
	tests := []struct {
		name        string
		skips       []types.StepName
		wantOrder   []types.StepName
		wantFailure bool
	}{
		{
			name:        "push still runs",
			skips:       []types.StepName{types.StepReview},
			wantOrder:   []types.StepName{types.StepTest},
			wantFailure: true,
		},
		{
			name:      "push skipped too",
			skips:     []types.StepName{types.StepReview, types.StepPush},
			wantOrder: []types.StepName{types.StepTest, types.StepPR},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := t.TempDir()

			var order []types.StepName
			call := func(name types.StepName, outcome *StepOutcome) Step {
				return &adaptiveCallStep{name: name, fn: func(*StepContext) (*StepOutcome, error) {
					order = append(order, name)
					return outcome, nil
				}}
			}
			steps := []Step{
				call(types.StepReview, &StepOutcome{}),
				call(types.StepTest, &StepOutcome{RestartFrom: types.StepReview}),
				call(types.StepPush, &StepOutcome{}),
				call(types.StepPR, &StepOutcome{}),
			}

			exec := NewExecutor(database, p, nil, nil, steps, nil)
			exec.SetSkippedSteps(tt.skips)

			err := exec.Execute(context.Background(), run, repo, workDir)
			if tt.wantFailure {
				if err == nil {
					t.Fatal("Execute() error = nil, want the restart into a skipped boundary to fail the run")
				}
				for _, want := range []string{"review", "test", "restart"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("Execute() error = %v, want it to name %q", err, want)
					}
				}
			} else if err != nil {
				t.Fatalf("Execute() error = %v, want the declined restart to leave the run running", err)
			}
			if !slices.Equal(order, tt.wantOrder) {
				t.Fatalf("execution order = %v, want %v", order, tt.wantOrder)
			}
			after, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.RestartCount != 0 {
				t.Fatalf("restart count = %d, want the unhonoured restart to write nothing", after.RestartCount)
			}
		})
	}
}

// TestExecutor_PrepareRestartWriteFailureIsNotReportedAsAStepBug separates the
// two ways a restart can fail. A step naming a boundary the pipeline rejects is
// the step's fault; a database write that fails is not, and reporting both as
// "step X requested invalid restart" sends whoever reads the run at a step that
// did nothing wrong.
//
// Each of prepareRestart's three writes gets a case, because each one failing
// silently breaks the restart a different way: an unreset step_results replays
// the run from a step already marked completed, a swallowed
// UpdateRunHeadSHAForRevalidation leaves review_approved_head_sha covering a
// head the re-review never reached so push accepts a certified ancestor, and a
// lost restart_count hides a thrashing run from the soft-cap warning and from
// axi status.
func TestExecutor_PrepareRestartWriteFailureIsNotReportedAsAStepBug(t *testing.T) {
	tests := []struct {
		name    string
		on      string
		message string
	}{
		{
			name:    "step_results reset",
			on:      "UPDATE OF status ON step_results WHEN NEW.status = 'pending'",
			message: "step reset refused",
		},
		{
			name:    "review authority revocation",
			on:      "UPDATE OF head_sha ON runs",
			message: "revalidation write refused",
		},
		{
			name:    "restart count",
			on:      "UPDATE OF restart_count ON runs",
			message: "restart count write refused",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := t.TempDir()

			if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, run.HeadSHA); err != nil {
				t.Fatalf("UpdateRunReviewApprovedHeadSHA() error = %v", err)
			}
			seeded, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			run.ReviewApprovedHeadSHA = seeded.ReviewApprovedHeadSHA

			refusePrepareRestartWrite(t, p.DB(), tt.on, tt.message)

			reachedPush := false
			steps := []Step{
				&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
					return &StepOutcome{}, nil
				}},
				&adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) {
					return &StepOutcome{RestartFrom: types.StepReview}, nil
				}},
				&adaptiveCallStep{name: types.StepPush, fn: func(*StepContext) (*StepOutcome, error) {
					reachedPush = true
					return &StepOutcome{}, nil
				}},
			}

			exec := NewExecutor(database, p, nil, nil, steps, nil)
			err = exec.Execute(context.Background(), run, repo, workDir)
			if err == nil {
				t.Fatal("Execute() error = nil, want the failed write to fail the run")
			}
			if strings.Contains(err.Error(), "invalid restart") {
				t.Fatalf("Execute() error = %v, want a write failure reported as itself, not as a step bug", err)
			}
			if !strings.Contains(err.Error(), "restart from review requested by step document") {
				t.Fatalf("Execute() error = %v, want the restart failure wrapper naming the requesting step", err)
			}
			if !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("Execute() error = %v, want it to carry the underlying write failure", err)
			}
			if reachedPush {
				t.Fatal("the run walked on to push on a restart that never completed")
			}
		})
	}
}

// refusePrepareRestartWrite aborts one of prepareRestart's writes and leaves the
// other two working, so a case's failure can only have come from the write it
// named. The step_results clause is narrowed to NEW.status = 'pending' because
// ResetStepsFrom is the only writer that sets that value, while every ordinary
// step transition also updates the same column.
func refusePrepareRestartWrite(t *testing.T, dbPath, on, message string) {
	t.Helper()
	conn, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stmt := fmt.Sprintf("CREATE TRIGGER refuse_restart_write BEFORE %s BEGIN SELECT RAISE(ABORT, '%s'); END", on, message)
	if _, err := conn.Exec(stmt); err != nil {
		t.Fatal(err)
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
