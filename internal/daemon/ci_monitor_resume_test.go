package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// monitorPRURL is the PR every CI-monitor fixture in this file watches.
const monitorPRURL = "https://github.com/user/project/pull/7"

// mockCIMonitorStep blocks on its first execution the way a live CI monitor
// waits on a forge, and completes on the next one, so a run preserved by a
// clean stop can be driven to the end after it is re-entered.
type mockCIMonitorStep struct {
	name    types.StepName
	started chan struct{}
	resumed chan struct{}

	mu       sync.Mutex
	attempts int
	// resumedPRURL is the PR the re-entered monitor saw, which is the whole
	// point of resuming rather than restarting.
	resumedPRURL string
}

func newMockCIMonitorStep() *mockCIMonitorStep {
	return &mockCIMonitorStep{
		name:    types.StepCI,
		started: make(chan struct{}),
		resumed: make(chan struct{}),
	}
}

func (s *mockCIMonitorStep) Name() types.StepName { return s.name }

func (s *mockCIMonitorStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.mu.Lock()
	s.attempts++
	first := s.attempts == 1
	// A resumed monitor can legitimately drive further executions, so only the
	// second attempt closes the channel and later ones keep counting.
	signalResume := s.attempts == 2
	if !first && sctx.Run != nil && sctx.Run.PRURL != nil {
		s.resumedPRURL = *sctx.Run.PRURL
	}
	s.mu.Unlock()

	if first {
		close(s.started)
		<-sctx.Ctx.Done()
		return nil, sctx.Ctx.Err()
	}
	if signalResume {
		close(s.resumed)
	}
	return &pipeline.StepOutcome{}, nil
}

func (s *mockCIMonitorStep) resumedPR() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumedPRURL
}

// waitForRunStatus waits for one specific status. waitForRunTerminalState only
// knows the three ordinary endings, and ci_monitor_interrupted is the whole
// point of these rejections.
func waitForRunStatus(t *testing.T, d *db.DB, runID string, want types.RunStatus) *db.Run {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	var last types.RunStatus
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil {
			if run.Status == want {
				return run
			}
			last = run.Status
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s status = %s, want %s", runID, last, want)
	return nil
}

// startCIMonitorRunCore pushes to a gate repo and returns once the run's only
// active step is a CI monitor watching an open PR, the shape a clean stop
// preserves. firstStarted is the channel that closes when the monitor's first
// execution begins; both single-stop and multi-stop mocks share this body.
func startCIMonitorRunCore(t *testing.T, p *paths.Paths, d *db.DB, repoID string, firstStarted <-chan struct{}) (*db.Repo, string) {
	t.Helper()

	repo, headSHA := setupTestGitRepo(t, p, d, repoID)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repoID),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult); err != nil {
		t.Fatalf("push received: %v", err)
	}

	select {
	case <-firstStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("ci step never started")
	}
	// The real CI step writes the PR URL before it starts watching checks.
	if err := d.UpdateRunPRURL(pushResult.RunID, monitorPRURL); err != nil {
		t.Fatal(err)
	}
	return repo, pushResult.RunID
}

// startCIMonitorRun is startCIMonitorRunCore for the single-stop mock.
func startCIMonitorRun(t *testing.T, p *paths.Paths, d *db.DB, repoID string, step *mockCIMonitorStep) (*db.Repo, string) {
	t.Helper()
	return startCIMonitorRunCore(t, p, d, repoID, step.started)
}

// mockMultiStopCIMonitorStep is a CI monitor that blocks on each of its first
// attempts-1 executions and completes on the attempts-th, so a run can be
// preserved and resumed more than once before it finishes. Each execution's
// start is exposed as its own channel so a test can wait for a specific
// attempt instead of only the first.
type mockMultiStopCIMonitorStep struct {
	name     types.StepName
	attempts int
	started  []chan struct{} // started[i] closes when attempt i+1 begins

	mu     sync.Mutex
	seen   int
	prURLs []string
}

func newMockMultiStopCIMonitorStep(attempts int) *mockMultiStopCIMonitorStep {
	started := make([]chan struct{}, attempts)
	for i := range started {
		started[i] = make(chan struct{})
	}
	return &mockMultiStopCIMonitorStep{name: types.StepCI, attempts: attempts, started: started}
}

func (s *mockMultiStopCIMonitorStep) Name() types.StepName { return s.name }

func (s *mockMultiStopCIMonitorStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.mu.Lock()
	s.seen++
	attempt := s.seen
	var prURL string
	if sctx.Run != nil && sctx.Run.PRURL != nil {
		prURL = *sctx.Run.PRURL
	}
	s.prURLs = append(s.prURLs, prURL)
	s.mu.Unlock()

	close(s.started[attempt-1])

	if attempt < s.attempts {
		<-sctx.Ctx.Done()
		return nil, sctx.Ctx.Err()
	}
	return &pipeline.StepOutcome{}, nil
}

// waitForAttempt blocks until the monitor's nth execution (1-indexed) has
// started, or fails the test after timeout.
func (s *mockMultiStopCIMonitorStep) waitForAttempt(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	select {
	case <-s.started[n-1]:
	case <-time.After(timeout):
		t.Fatalf("ci monitor never reached attempt %d", n)
	}
}

// prURLAt returns the PR URL the monitor saw on its nth execution (1-indexed).
func (s *mockMultiStopCIMonitorStep) prURLAt(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n-1 < 0 || n-1 >= len(s.prURLs) {
		return ""
	}
	return s.prURLs[n-1]
}

// startMultiStopCIMonitorRun is startCIMonitorRunCore for the multi-stop mock.
func startMultiStopCIMonitorRun(t *testing.T, p *paths.Paths, d *db.DB, repoID string, step *mockMultiStopCIMonitorStep) (*db.Repo, string) {
	t.Helper()
	return startCIMonitorRunCore(t, p, d, repoID, step.started[0])
}

// TestNextDaemonStartResumesACIMonitorPreservedByCleanStop is the headline of
// the resumable-CI-monitor work: a run watching CI for an open PR survives a
// clean stop as a running row with its worktree, and the next start re-enters
// the monitor for the same PR instead of reporting the run interrupted.
func TestNextDaemonStartResumesACIMonitorPreservedByCleanStop(t *testing.T) {
	monitor := newMockCIMonitorStep()
	steps := func() []pipeline.Step { return []pipeline.Step{monitor} }
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, runID := startCIMonitorRun(t, p, d, "ci-resume-repo", monitor)

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	preserved, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Status != types.RunRunning {
		t.Fatalf("run status after clean stop = %s (error %v), want %s", preserved.Status, preserved.Error, types.RunRunning)
	}
	if preserved.Error != nil {
		t.Fatalf("run error after clean stop = %q, want nil", *preserved.Error)
	}
	if row := findStepRow(t, d, runID, types.StepCI); row == nil || row.Status != types.StepStatusRunning {
		t.Fatalf("ci step row = %v, want %s", row, types.StepStatusRunning)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil {
		t.Fatalf("clean stop removed the preserved worktree: %v", err)
	}

	restartTestDaemonInstance(t, p, d, steps)

	select {
	case <-monitor.resumed:
	case <-time.After(15 * time.Second):
		t.Fatal("the next daemon start never re-entered the ci monitor")
	}
	if got := monitor.resumedPR(); got != monitorPRURL {
		t.Fatalf("resumed monitor watched PR %q, want %q", got, monitorPRURL)
	}

	completed := waitForRunTerminalState(t, d, runID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("resumed run status = %s (error %v), want %s", completed.Status, completed.Error, types.RunCompleted)
	}
}

// TestCIMonitorWithAMissingWorktreeIsFailedWithThatConcreteReason proves a
// preserved monitor that genuinely cannot resume is told apart from the
// blanket crash stamp, and lands under ci_monitor_interrupted rather than
// failed: the PR is still open, and only that status spares the worktree.
func TestCIMonitorWithAMissingWorktreeIsFailedWithThatConcreteReason(t *testing.T) {
	monitor := newMockCIMonitorStep()
	steps := func() []pipeline.Step { return []pipeline.Step{monitor} }
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, runID := startCIMonitorRun(t, p, d, "ci-reject-worktree-repo", monitor)

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}
	if err := os.RemoveAll(p.WorktreeDir(repo.ID, runID)); err != nil {
		t.Fatal(err)
	}

	restartTestDaemonInstance(t, p, d, steps)

	run := waitForRunStatus(t, d, runID, types.RunCIMonitorInterrupted)
	if run.Error == nil {
		t.Fatal("rejected ci monitor recorded no error")
	}
	if *run.Error == "daemon crashed during execution" {
		t.Fatalf("run error = %q, want the concrete reason the monitor could not resume", *run.Error)
	}
	if !strings.Contains(*run.Error, "worktree") {
		t.Fatalf("run error = %q, want it to name the worktree", *run.Error)
	}
	if strings.Contains(*run.Error, "parked") {
		t.Fatalf("run error = %q, must not claim a CI monitor was parked", *run.Error)
	}
}

// TestRejectedCIMonitorKeepsItsWorktree is why the rejection records
// ci_monitor_interrupted rather than failed. The worktree can hold an unpushed
// CI auto-fix commit, and skipWorktreeCleanup spares it only under that
// status, so a rejection recorded the ordinary way would let the startup
// orphan sweep delete the work.
func TestRejectedCIMonitorKeepsItsWorktree(t *testing.T) {
	monitor := newMockCIMonitorStep()
	steps := func() []pipeline.Step { return []pipeline.Step{monitor} }
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, runID := startCIMonitorRun(t, p, d, "ci-reject-keeps-worktree-repo", monitor)

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	// Move the worktree head off the run's head, the adverse fact recovery
	// refuses on while the directory itself is still there to lose.
	workDir := p.WorktreeDir(repo.ID, runID)
	if err := os.WriteFile(filepath.Join(workDir, "autofix.txt"), []byte("unpushed ci repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", ".")
	// -c overrides keep the commit off the developer's signing key, which the
	// test has no reason to reach for and cannot always reach.
	gitCmd(t, workDir, "-c", "commit.gpgsign=false", "-c", "user.name=Test", "-c", "user.email=test@example.com",
		"commit", "-m", "ci auto-fix")

	restartTestDaemonInstance(t, p, d, steps)

	waitForRunStatus(t, d, runID, types.RunCIMonitorInterrupted)

	// The orphan sweep runs after recovery, so give it time to make the
	// mistake this status exists to prevent.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(workDir); err != nil {
			t.Fatalf("a rejected ci monitor lost its worktree: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(workDir, "autofix.txt")); err != nil {
		t.Fatalf("the worktree's unpushed repair is gone: %v", err)
	}
}

// TestOfflineStartDefersACIMonitorInsteadOfFailingIt proves the deferral
// default covers the new resume point too: the trusted default branch cannot
// be fetched, which says nothing about the run, so the start leaves the row
// running with its worktree instead of ending it.
func TestOfflineStartDefersACIMonitorInsteadOfFailingIt(t *testing.T) {
	monitor := newMockCIMonitorStep()
	steps := func() []pipeline.Step { return []pipeline.Step{monitor} }
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, runID := startCIMonitorRun(t, p, d, "ci-offline-repo", monitor)

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	gateDir := p.RepoDir(repo.ID)
	gitCmd(t, gateDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "off-network.git"))

	restartTestDaemonInstance(t, p, d, steps)

	// The row must stay untouched for the whole start, not merely at the
	// instant recovery finished.
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != types.RunRunning {
			t.Fatalf("offline start set run status to %s (error %v), want it left %s", run.Status, run.Error, types.RunRunning)
		}
		if run.Error != nil {
			t.Fatalf("offline start recorded error %q, want none", *run.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil {
		t.Fatalf("offline start removed the preserved worktree: %v", err)
	}
	if row := findStepRow(t, d, runID, types.StepCI); row == nil || row.Status != types.StepStatusRunning {
		t.Fatalf("ci step row = %v, want %s", row, types.StepStatusRunning)
	}
}

// TestCrashedMidReviewRunIsStillUnresumableWithoutAnyGitRead pins the cheap
// short-circuit. A run whose review row is running is neither parked nor a CI
// monitor, so recovery must reject it from the step rows alone: reaching the
// repo and git reads would let a read that fails there defer it forever, when
// the blanket sweep should stamp it instead.
func TestCrashedMidReviewRunIsStillUnresumableWithoutAnyGitRead(t *testing.T) {
	monitor := newMockCIMonitorStep()
	steps := func() []pipeline.Step { return []pipeline.Step{monitor} }
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, headSHA := setupTestGitRepo(t, p, d, "ci-midreview-repo")
	crashed, err := d.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(crashed.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	row, err := d.InsertStepResult(crashed.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatus(row.ID, types.StepStatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	// Every read past the short-circuit fails, so a run that got that far
	// would be deferred and left running forever.
	hideTable(t, p, "repos")

	manager := NewRunManager(d, p, steps)
	t.Cleanup(manager.Shutdown)
	plans, deferred, err := manager.recoverableParkedRuns(t.Context())
	if err != nil {
		t.Fatalf("recovery could not list active runs: %v", err)
	}
	if len(plans) != 0 {
		t.Fatalf("recovery planned %d resumes, want none", len(plans))
	}
	for _, id := range deferred {
		if id == crashed.ID {
			t.Fatal("a crashed mid-review run was deferred, want it left to the blanket sweep")
		}
	}
	// Left for the blanket sweep, which is the pass that stamps it: recovery
	// records nothing itself for a run that was never preserved.
	after, err := d.GetRun(crashed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != types.RunRunning || after.Error != nil {
		t.Fatalf("recovery ended the crashed run itself as %s (error %v), want it left to the sweep", after.Status, after.Error)
	}
}

// TestLivePushStillSupersedesACIMonitoringRun pins that branch contention is
// unchanged by preservation. A new push to a branch whose run is monitoring CI
// is the ordinary rerun workflow, so it supersedes that run rather than being
// refused the way a gate-parked run refuses it.
func TestLivePushStillSupersedesACIMonitoringRun(t *testing.T) {
	monitor := newMockCIMonitorStep()
	var launched atomic.Int32
	steps := func() []pipeline.Step {
		if launched.Add(1) == 1 {
			return []pipeline.Step{monitor}
		}
		return []pipeline.Step{&mockPassStep{name: types.StepCI}}
	}
	instance := startTestDaemonInstance(t, steps)
	p, d := instance.paths, instance.db

	repo, monitorID := startCIMonitorRun(t, p, d, "ci-supersede-repo", monitor)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	run, err := d.GetRun(monitorID)
	if err != nil {
		t.Fatal(err)
	}
	var pushResult ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/" + run.Branch,
		Old:  run.HeadSHA,
		New:  run.HeadSHA,
	}, &pushResult); err != nil {
		t.Fatalf("push over a ci-monitoring run was refused: %v", err)
	}
	if pushResult.RunID == "" || pushResult.RunID == monitorID {
		t.Fatalf("push started run %q, want a new run superseding %s", pushResult.RunID, monitorID)
	}

	superseded := waitForRunTerminalState(t, d, monitorID)
	if superseded.Status == types.RunRunning {
		t.Fatalf("superseded run status = %s, want it ended", superseded.Status)
	}
}

// TestASecondCleanStopPreservesAnAlreadyResumedCIMonitor proves resume is not
// a one-shot: a CI monitor preserved by a clean stop, resumed once, and still
// watching survives a SECOND clean stop through the recovered-run teardown
// path (Executor.Resume) rather than the freshly started one that only the
// first stop exercises, and a third start resumes it through to completion.
func TestASecondCleanStopPreservesAnAlreadyResumedCIMonitor(t *testing.T) {
	monitor := newMockMultiStopCIMonitorStep(3)
	steps := func() []pipeline.Step { return []pipeline.Step{monitor} }
	first := startTestDaemonInstance(t, steps)
	p, d := first.paths, first.db

	repo, runID := startMultiStopCIMonitorRun(t, p, d, "ci-resume-twice-repo", monitor)

	if err := first.stopAndWait(t); err != nil {
		t.Fatalf("first daemon exited with error: %v", err)
	}

	second := restartTestDaemonInstance(t, p, d, steps)
	monitor.waitForAttempt(t, 2, 15*time.Second)
	if got := monitor.prURLAt(2); got != monitorPRURL {
		t.Fatalf("resumed monitor's second attempt watched PR %q, want %q", got, monitorPRURL)
	}

	if err := second.stopAndWait(t); err != nil {
		t.Fatalf("second daemon exited with error: %v", err)
	}

	preserved, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Status != types.RunRunning {
		t.Fatalf("run status after second clean stop = %s (error %v), want %s", preserved.Status, preserved.Error, types.RunRunning)
	}
	if preserved.Error != nil {
		t.Fatalf("run error after second clean stop = %q, want nil", *preserved.Error)
	}
	if row := findStepRow(t, d, runID, types.StepCI); row == nil || row.Status != types.StepStatusRunning {
		t.Fatalf("ci step row after second clean stop = %v, want %s", row, types.StepStatusRunning)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil {
		t.Fatalf("second clean stop removed the preserved worktree: %v", err)
	}

	restartTestDaemonInstance(t, p, d, steps)
	monitor.waitForAttempt(t, 3, 15*time.Second)
	if got := monitor.prURLAt(3); got != monitorPRURL {
		t.Fatalf("resumed monitor's third attempt watched PR %q, want %q", got, monitorPRURL)
	}

	completed := waitForRunTerminalState(t, d, runID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("twice-resumed run status = %s (error %v), want %s", completed.Status, completed.Error, types.RunCompleted)
	}
}
