package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	_ "modernc.org/sqlite"
)

const testCIPRURL = "https://github.com/test/repo/pull/7"

// seedCIMonitorRun writes the rows a run leaves behind while its CI step polls
// an open PR: every earlier step completed and the ci row running. It returns
// the rows in plan order.
func seedCIMonitorRun(t *testing.T, database *db.DB, run *db.Run, plan []Step) []*db.StepResult {
	t.Helper()
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	rows := make([]*db.StepResult, 0, len(plan))
	for _, step := range plan {
		row, err := database.InsertStepResult(run.ID, step.Name())
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	for index, row := range rows {
		if index == len(rows)-1 {
			if err := database.StartStep(row.ID); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := database.CompleteStepWithStatus(row.ID, types.StepStatusCompleted, 0, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	return rows
}

func mustGetRun(t *testing.T, database *db.DB, runID string) *db.Run {
	t.Helper()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func ciMonitorPlan() []Step {
	return []Step{newPassStep(types.StepReview), newPassStep(types.StepCI)}
}

func TestRecoveredResumePoint_LiveCIMonitorIsAResumePoint(t *testing.T) {
	database, p, run, _ := setupTest(t)
	plan := ciMonitorPlan()
	rows := seedCIMonitorRun(t, database, run, plan)
	if err := database.UpdateRunPRURL(run.ID, testCIPRURL); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, plan, nil)
	point, err := exec.recoveredResumePoint(mustGetRun(t, database, run.ID))
	if err != nil {
		t.Fatalf("recoveredResumePoint() error = %v, want nil", err)
	}
	if point.gate != nil {
		t.Errorf("resume point gate = %v, want nil", point.gate)
	}
	if point.ciMonitor == nil {
		t.Fatal("resume point ciMonitor = nil, want the live ci monitor")
	}
	if point.ciMonitor.index != 1 {
		t.Errorf("ciMonitor index = %d, want 1", point.ciMonitor.index)
	}
	if point.ciMonitor.stepResult.ID != rows[1].ID {
		t.Errorf("ciMonitor step result = %s, want the ci row %s", point.ciMonitor.stepResult.ID, rows[1].ID)
	}
}

func TestRecoveredResumePoint_CIMonitorWithoutAPRURLIsNotAResumePoint(t *testing.T) {
	database, p, run, _ := setupTest(t)
	plan := ciMonitorPlan()
	seedCIMonitorRun(t, database, run, plan)

	exec := NewExecutor(database, p, nil, nil, plan, nil)
	_, err := exec.recoveredResumePoint(mustGetRun(t, database, run.ID))
	if err == nil {
		t.Fatal("recoveredResumePoint() = nil error for a ci row with no PR URL, want an error")
	}
	if errors.Is(err, ErrRecoveryEvidenceUnavailable) {
		t.Errorf("recoveredResumePoint() error = %v, want adverse evidence rather than an unavailable read", err)
	}
}

func TestRecoveredResumePoint_CIMonitorHoldingAnAgentPIDIsNotAResumePoint(t *testing.T) {
	database, p, run, _ := setupTest(t)
	plan := ciMonitorPlan()
	rows := seedCIMonitorRun(t, database, run, plan)
	if err := database.UpdateRunPRURL(run.ID, testCIPRURL); err != nil {
		t.Fatal(err)
	}
	pid := 4242
	if err := database.SetStepAgentActivity(rows[1].ID, "repairing ci", &pid); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, plan, nil)
	_, err := exec.recoveredResumePoint(mustGetRun(t, database, run.ID))
	if err == nil {
		t.Fatal("recoveredResumePoint() = nil error for a ci row holding an agent pid, want an error")
	}
	if errors.Is(err, ErrRecoveryEvidenceUnavailable) {
		t.Errorf("recoveredResumePoint() error = %v, want adverse evidence rather than an unavailable read", err)
	}
}

func TestRecoveredResumePoint_ARunningNonCIStepIsNotAResumePoint(t *testing.T) {
	database, p, run, _ := setupTest(t)
	plan := []Step{newPassStep(types.StepReview), newPassStep(types.StepCI)}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	reviewRow, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepResult(run.ID, types.StepCI); err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(reviewRow.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(run.ID, testCIPRURL); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, plan, nil)
	_, err = exec.recoveredResumePoint(mustGetRun(t, database, run.ID))
	if err == nil {
		t.Fatal("recoveredResumePoint() = nil error for a running review step, want an error")
	}
	if errors.Is(err, ErrRecoveryEvidenceUnavailable) {
		t.Errorf("recoveredResumePoint() error = %v, want adverse evidence rather than an unavailable read", err)
	}
}

func TestRecoveredResumePoint_StepReadFailureStaysAnUnavailableEvidenceError(t *testing.T) {
	database, p, run, _ := setupTest(t)
	plan := ciMonitorPlan()
	seedCIMonitorRun(t, database, run, plan)
	if err := database.UpdateRunPRURL(run.ID, testCIPRURL); err != nil {
		t.Fatal(err)
	}
	recovered := mustGetRun(t, database, run.ID)
	// Closing the database is the cheapest way to make the step read fail
	// without reaching past the executor's own db handle.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, plan, nil)
	_, err := exec.recoveredResumePoint(recovered)
	if !errors.Is(err, ErrRecoveryEvidenceUnavailable) {
		t.Errorf("recoveredResumePoint() error = %v, want ErrRecoveryEvidenceUnavailable", err)
	}
}

func TestRecoveredResumePoint_GateStillResolvesUnchanged(t *testing.T) {
	database, p, run, _ := setupTest(t)
	plan := []Step{newPassStep(types.StepReview)}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	row, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(row.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs a fix","action":"ask-user"}],"summary":"one issue"}`
	if err := database.SetStepFindings(row.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(row.ID, 1, "initial", &findings, nil, "1111111111111111111111111111111111111111", 25); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(row.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, plan, nil)
	point, err := exec.recoveredResumePoint(mustGetRun(t, database, run.ID))
	if err != nil {
		t.Fatalf("recoveredResumePoint() error = %v, want nil", err)
	}
	if point.ciMonitor != nil {
		t.Errorf("resume point ciMonitor = %v, want nil for a parked gate", point.ciMonitor)
	}
	if point.gate == nil {
		t.Fatal("resume point gate = nil, want the parked review gate")
	}
	if point.gate.stepResult.ID != row.ID {
		t.Errorf("gate step result = %s, want %s", point.gate.stepResult.ID, row.ID)
	}
}

func TestValidateRecoveredRun_AcceptsACIMonitorWithNoAwaitingAgentMarker(t *testing.T) {
	database, _, run, _ := setupTest(t)
	plan := ciMonitorPlan()
	seedCIMonitorRun(t, database, run, plan)
	if err := database.UpdateRunPRURL(run.ID, testCIPRURL); err != nil {
		t.Fatal(err)
	}

	recovered := mustGetRun(t, database, run.ID)
	if recovered.AwaitingAgentSince != nil {
		t.Fatalf("AwaitingAgentSince = %v, want nil for a CI monitor", recovered.AwaitingAgentSince)
	}
	if err := ValidateRecoveredRun(database, recovered, plan); err != nil {
		t.Errorf("ValidateRecoveredRun() error = %v, want nil", err)
	}
}

// recordingCIStep captures what each Execute call saw, so a resumed monitor
// can be proven to poll the run's own PR exactly once.
type recordingCIStep struct {
	mu      sync.Mutex
	prURLs  []string
	outcome *StepOutcome
	fn      func(sctx *StepContext) (*StepOutcome, error)
}

func (r *recordingCIStep) Name() types.StepName { return types.StepCI }

func (r *recordingCIStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	r.mu.Lock()
	prURL := ""
	if sctx.Run != nil && sctx.Run.PRURL != nil {
		prURL = *sctx.Run.PRURL
	}
	r.prURLs = append(r.prURLs, prURL)
	r.mu.Unlock()
	if r.fn != nil {
		return r.fn(sctx)
	}
	return r.outcome, nil
}

func (r *recordingCIStep) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prURLs...)
}

func TestExecutor_ResumeReentersCIMonitoringForTheSamePR(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	ciStep := &recordingCIStep{outcome: &StepOutcome{ExitCode: 0}}
	plan := []Step{newPassStep(types.StepReview), ciStep}
	seedCIMonitorRun(t, database, run, plan)
	if err := database.UpdateRunPRURL(run.ID, testCIPRURL); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, plan, nil)
	if err := exec.Resume(context.Background(), mustGetRun(t, database, run.ID), repo, workDir); err != nil {
		t.Fatalf("Resume() error = %v, want nil", err)
	}

	seen := ciStep.seen()
	if len(seen) != 1 {
		t.Fatalf("ci step Execute calls = %d, want 1", len(seen))
	}
	if seen[0] != testCIPRURL {
		t.Errorf("ci step saw PR URL %q, want %q", seen[0], testCIPRURL)
	}

	final := mustGetRun(t, database, run.ID)
	if final.Status != types.RunCompleted {
		t.Errorf("resumed run status = %s, want %s", final.Status, types.RunCompleted)
	}
}

func TestExecutor_ResumeOfACIMonitorDoesNotDereferenceTheParkMarker(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	plan := []Step{newPassStep(types.StepReview), &recordingCIStep{outcome: &StepOutcome{ExitCode: 0}}}
	seedCIMonitorRun(t, database, run, plan)
	if err := database.UpdateRunPRURL(run.ID, testCIPRURL); err != nil {
		t.Fatal(err)
	}
	recovered := mustGetRun(t, database, run.ID)
	if recovered.AwaitingAgentSince != nil {
		t.Fatalf("AwaitingAgentSince = %v, want nil", recovered.AwaitingAgentSince)
	}

	exec := NewExecutor(database, p, nil, nil, plan, nil)
	if err := exec.Resume(context.Background(), recovered, repo, workDir); err != nil {
		t.Fatalf("Resume() error = %v, want nil", err)
	}
}

// backdateStepStart rewrites a step row's started_at so a test can stand in
// for a monitor that had already been polling for a while before the restart.
// db exposes no setter for it, and a second handle on the same WAL file is
// cheaper than a production seam that exists only for tests.
func backdateStepStart(t *testing.T, p *paths.Paths, stepID string, elapsed time.Duration) {
	t.Helper()
	raw, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	started := time.Now().Add(-elapsed).Unix()
	if _, err := raw.Exec(`UPDATE step_results SET started_at = ? WHERE id = ?`, started, stepID); err != nil {
		t.Fatal(err)
	}
}

func TestExecutor_ResumedCIMonitorKeepsItsEarlierElapsedTime(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	plan := []Step{newPassStep(types.StepReview), &recordingCIStep{outcome: &StepOutcome{ExitCode: 0}}}
	rows := seedCIMonitorRun(t, database, run, plan)
	if err := database.UpdateRunPRURL(run.ID, testCIPRURL); err != nil {
		t.Fatal(err)
	}
	const alreadyMonitored = 10 * time.Minute
	backdateStepStart(t, p, rows[1].ID, alreadyMonitored)

	exec := NewExecutor(database, p, nil, nil, plan, nil)
	if err := exec.Resume(context.Background(), mustGetRun(t, database, run.ID), repo, workDir); err != nil {
		t.Fatalf("Resume() error = %v, want nil", err)
	}

	ciRow, err := database.GetStepResult(rows[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if ciRow.DurationMS == nil {
		t.Fatal("resumed ci step duration = nil, want the earlier elapsed time folded in")
	}
	if *ciRow.DurationMS < alreadyMonitored.Milliseconds() {
		t.Errorf("resumed ci step duration = %dms, want at least the %dms it had already monitored", *ciRow.DurationMS, alreadyMonitored.Milliseconds())
	}
}

// runCancelledCIStep drives a single CI step to the point where it is blocked
// on the run context, optionally leaving the row in some other state first,
// then cancels with cause and reports what Execute returned.
func runCancelledCIStep(t *testing.T, cause error, before func(sctx *StepContext)) (*db.DB, *db.StepResult, error) {
	t.Helper()
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	entered := make(chan struct{})
	ciStep := &recordingCIStep{
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if before != nil {
				before(sctx)
			}
			close(entered)
			<-sctx.Ctx.Done()
			return nil, context.Cause(sctx.Ctx)
		},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{ciStep}, nil)

	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		cancel(errors.New("test cleanup"))
		t.Fatal("ci step never started")
	}
	cancel(cause)

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	rows, getErr := database.GetStepsByRun(run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	return database, rows[0], err
}

func TestExecutor_CleanShutdownPreservesALiveCIMonitorForResume(t *testing.T) {
	_, ciRow, err := runCancelledCIStep(t, ErrDaemonShutdown, nil)
	if !errors.Is(err, ErrParkPreserved) {
		t.Fatalf("Execute() error = %v, want ErrParkPreserved", err)
	}
	if ciRow.Status != types.StepStatusRunning {
		t.Errorf("ci step status = %s, want %s", ciRow.Status, types.StepStatusRunning)
	}
	if ciRow.Error != nil {
		t.Errorf("ci step error = %q, want nil", *ciRow.Error)
	}
}

func TestExecutor_CleanShutdownDoesNotPreserveACIStepMidRepair(t *testing.T) {
	_, ciRow, err := runCancelledCIStep(t, ErrDaemonShutdown, func(sctx *StepContext) {
		if dbErr := sctx.DB.UpdateStepStatus(sctx.StepResultID, types.StepStatusFixing); dbErr != nil {
			t.Error(dbErr)
		}
	})
	if errors.Is(err, ErrParkPreserved) {
		t.Fatalf("Execute() error = %v, want NOT ErrParkPreserved for a row mid-repair", err)
	}
	if ciRow.Status != types.StepStatusFailed {
		t.Errorf("ci step status = %s, want %s", ciRow.Status, types.StepStatusFailed)
	}
}

func TestExecutor_CleanShutdownDoesNotPreserveACIStepHoldingAnAgentPID(t *testing.T) {
	_, ciRow, err := runCancelledCIStep(t, ErrDaemonShutdown, func(sctx *StepContext) {
		pid := 4242
		if dbErr := sctx.DB.SetStepAgentActivity(sctx.StepResultID, "repairing ci", &pid); dbErr != nil {
			t.Error(dbErr)
		}
	})
	if errors.Is(err, ErrParkPreserved) {
		t.Fatalf("Execute() error = %v, want NOT ErrParkPreserved for a row holding an agent pid", err)
	}
	if ciRow.Status != types.StepStatusFailed {
		t.Errorf("ci step status = %s, want %s", ciRow.Status, types.StepStatusFailed)
	}
}

func TestExecutor_CancellationThatIsNotACleanShutdownStillFailsTheCIStep(t *testing.T) {
	_, ciRow, err := runCancelledCIStep(t, errors.New(types.RunCancelReasonSuperseded), nil)
	if errors.Is(err, ErrParkPreserved) {
		t.Fatalf("Execute() error = %v, want NOT ErrParkPreserved for a supersede", err)
	}
	if ciRow.Status != types.StepStatusFailed {
		t.Errorf("ci step status = %s, want %s", ciRow.Status, types.StepStatusFailed)
	}
}
