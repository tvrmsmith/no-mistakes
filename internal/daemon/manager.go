package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/eval"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/procreap"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// StepFactory creates pipeline steps for a run. Defaults to steps.AllSteps.
type StepFactory func() []pipeline.Step

var recoveredConfigFetchTimeout = 10 * time.Second

var fetchRecoveredRemoteBranch = git.FetchRemoteBranch

// RunManager tracks active pipeline executors and manages run lifecycle.
type RunManager struct {
	mu           sync.Mutex
	executors    map[string]*pipeline.Executor      // runID → executor
	cancels      map[string]context.CancelCauseFunc // runID → cancel function with cause
	dones        map[string]chan struct{}           // runID → closed when goroutine exits
	wg           sync.WaitGroup                     // tracks background run goroutines
	shuttingDown atomic.Bool                        // prevents new runs during shutdown
	db           *db.DB
	paths        *paths.Paths
	steps        StepFactory

	// persistSkippedSteps is the run-scope write whose failure aborts a run
	// start. It is a field so a test can fault it: the outcome of that branch
	// (no worktree, no executor, a recorded reason) is the guarantee that
	// stops a resumed run from pushing a branch the operator excluded.
	persistSkippedSteps func(runID string, skipSteps []types.StepName) error

	branchLocks sync.Map // repoID+"/"+branch → *sync.Mutex

	// evalCaptureMu serializes automatic eval collection. Concurrent runs
	// finishing together would otherwise write the same per-repository object
	// pool and the same registry file at once.
	evalCaptureMu sync.Mutex

	// subMu guards the subscriber set and the per-run state revisions. It is
	// a plain Mutex, not an RWMutex, because revision assignment and fan-out
	// must be one atomic step: if two concurrent state events could be
	// enqueued out of revision order, a consumer's monotonic guard would
	// permanently discard the older one's payload. The critical section
	// contains no blocking operation and no I/O, so hold time is
	// O(subscribers) memory writes.
	subMu          sync.Mutex
	subscribers    map[string][]*eventMailbox // runID → subscriber mailboxes
	stateRevs      map[string]int64           // runID → monotonic state revision
	completedRuns  map[string]bool            // runIDs whose goroutines have finished
	completedOrder []string                   // insertion order for FIFO eviction
}

// maxSubscribersPerRun bounds the global mailbox footprint: queued bytes can
// never exceed activeRuns × maxSubscribersPerRun × mailboxMaxBytes. Refusing
// past the cap is an ordinary error, never unbounded growth.
const maxSubscribersPerRun = 32

// NewRunManager creates a RunManager. Pass nil for stepFactory to use default steps.
func NewRunManager(database *db.DB, p *paths.Paths, stepFactory StepFactory) *RunManager {
	if stepFactory == nil {
		stepFactory = func() []pipeline.Step { return steps.AllSteps() }
	}
	return &RunManager{
		executors:     make(map[string]*pipeline.Executor),
		cancels:       make(map[string]context.CancelCauseFunc),
		dones:         make(map[string]chan struct{}),
		db:            database,
		paths:         p,
		steps:         stepFactory,
		subscribers:   make(map[string][]*eventMailbox),
		stateRevs:     make(map[string]int64),
		completedRuns: make(map[string]bool),
	}
}

type recoveredRunPlan struct {
	run     *db.Run
	repo    *db.Repo
	workDir string
	gateDir string
	cfg     *config.Config
	agent   agent.Agent
	steps   []pipeline.Step
	forge   *forgecontext.Context
}

// errRunUnresumable marks a recovery failure as adverse evidence a completed
// read actually returned: a missing worktree, a head that no longer matches,
// a worktree outside its gate repository, an incomplete gate row (including
// one still holding an agent PID), a drifted step plan, a session provider the
// configuration no longer offers, a trusted config that parsed as invalid.
// Only those are recorded through rejectUnresumableRun.
//
// Deferral is the DEFAULT for everything else, and the direction of that
// default is the whole point. A clean stop promises the operator a parked run
// is preserved, and a parked run's worktree can hold unpushed pipeline
// commits; terminally failing such a run makes cleanupOrphanWorktrees delete
// that worktree. So an error reaches the destructive side only by an explicit
// positive classification at the point the adverse fact is established, and a
// read added to this path later - a database row, a git object, a config file
// - waits for a later start instead of costing a preserved run its work.
var errRunUnresumable = errors.New("run cannot be resumed")

// unresumable classifies cause as adverse evidence. It leaves the message
// untouched, because rejectUnresumableRun records it verbatim as the run's
// error and startRun surfaces the same text to the operator.
func unresumable(cause error) error {
	return &unresumableError{cause: cause}
}

type unresumableError struct{ cause error }

func (e *unresumableError) Error() string   { return e.cause.Error() }
func (e *unresumableError) Unwrap() []error { return []error{errRunUnresumable, e.cause} }

// resumeRejected is the single reader of that classification: recovery
// terminally fails a run only when it answers true.
func resumeRejected(err error) bool {
	return errors.Is(err, errRunUnresumable)
}

// recoverableParkedRuns returns the runs to resume now, plus the IDs of the
// runs deferred to a later start. Deferred IDs are NOT resumed and must still
// be excluded from the blanket stale-run sweep, which would otherwise stamp
// them failed and forfeit their worktrees.
//
// The active-run listing is the read every other recovery read depends on, so
// the deferral default applies to it hardest: a listing that failed is not a
// picture with no parked runs in it. It returns an error instead, and the
// caller defers the whole pass rather than sweeping every preserved run away
// on an empty picture.
func (m *RunManager) recoverableParkedRuns(ctx context.Context) ([]recoveredRunPlan, []string, error) {
	runs, err := m.db.GetActiveRuns()
	if err != nil {
		return nil, nil, fmt.Errorf("list active runs for recovery: %w", err)
	}
	plans := make([]recoveredRunPlan, 0, len(runs))
	var deferred []string
	contention := branchContentionOf(runs, m.gateStepRowsOf)
	for _, run := range runs {
		if contention.superseded[run.ID] {
			m.rejectUnresumableRun(run, errors.New("conflicting active run for branch"))
			continue
		}
		if contenders, ok := contention.unresolved[run.ID]; ok {
			slog.Warn("branch has more than one parked run; resuming none of them until an operator resolves it",
				"run_id", run.ID, "branch", run.Branch, "contending_runs", contenders)
			m.registerDeferredRun(run)
			deferred = append(deferred, run.ID)
			continue
		}
		plan, err := m.prepareRecoveredRun(ctx, run)
		if err != nil {
			if resumeRejected(err) {
				m.rejectUnresumableRun(run, err)
				continue
			}
			slog.Warn("parked run left for a later daemon start",
				"run_id", run.ID, "reason", boundedRecoveryReason(err))
			m.registerDeferredRun(run)
			deferred = append(deferred, run.ID)
			continue
		}
		plans = append(plans, *plan)
	}
	return plans, deferred, nil
}

// gateStepRowsOf reads the step rows a parked claim is corroborated against.
type gateStepReader func(runID string) ([]*db.StepResult, error)

func (m *RunManager) gateStepRowsOf(runID string) ([]*db.StepResult, error) {
	return m.db.GetStepsByRun(runID)
}

// preservedBranchRuns is the single owner of the branch-contention preference,
// shared by startup recovery and the live push path so the two cannot drift
// apart. Two active runs cannot share a branch, but the choice between them is
// not symmetric: a parked run was promised preservation and its worktree can
// hold unpushed pipeline commits, while the run competing with it is a push
// that can simply be pushed again.
//
// A claim to be parked takes two facts, exactly as it does everywhere else in
// this package: the awaiting-agent marker is a best-effort write, so it is
// corroborated against a real gate step row through lifecycle.ParkedAtGate and
// a stuck marker alone never wins a branch. A step read that fails leaves the
// claim unproven rather than refuted, so that run stays a candidate and the
// caller resolves the ambiguity conservatively.
func preservedBranchRuns(runs []*db.Run, stepsOf gateStepReader) []*db.Run {
	var keep []*db.Run
	for _, run := range runs {
		stepRows, err := stepsOf(run.ID)
		if err != nil {
			slog.Warn("could not read steps while resolving branch contention; treating the run as possibly parked",
				"run_id", run.ID, "error", err)
			if run.AwaitingAgentSince != nil {
				keep = append(keep, run)
			}
			continue
		}
		if lifecycle.ParkedAtGate(run, stepRows) {
			keep = append(keep, run)
		}
	}
	return keep
}

// branchContention is how startup divides the active runs of contended
// branches. superseded holds the runs a start ends to clear the branch;
// unresolved maps a run whose branch no single candidate wins to the IDs
// contending for it, for the log line that asks an operator to resolve it.
type branchContention struct {
	superseded map[string]bool
	unresolved map[string][]string
}

// branchContentionOf applies the preservation preference at startup: every run
// of a contended branch other than the single preserved one is superseded.
// With no preserved run the contention is unresolved state and every run on
// the branch goes.
//
// With more than one candidate there is no run to prefer. Superseding any of
// them could destroy a preserved worktree, and resuming them all is worse
// still: two runs would drive push and PR at the same remote branch from two
// worktrees at two head SHAs. The whole group is therefore unresolved, and its
// runs are deferred rather than resumed.
func branchContentionOf(runs []*db.Run, stepsOf gateStepReader) branchContention {
	byBranch := make(map[string][]*db.Run, len(runs))
	for _, run := range runs {
		key := run.RepoID + "\x00" + run.Branch
		byBranch[key] = append(byBranch[key], run)
	}
	contention := branchContention{
		superseded: make(map[string]bool),
		unresolved: make(map[string][]string),
	}
	for _, group := range byBranch {
		if len(group) < 2 {
			continue
		}
		keep := preservedBranchRuns(group, stepsOf)
		if len(keep) > 1 {
			ids := make([]string, 0, len(keep))
			for _, run := range keep {
				ids = append(ids, run.ID)
			}
			for _, run := range group {
				contention.unresolved[run.ID] = ids
			}
			continue
		}
		for _, run := range group {
			if len(keep) == 1 && run.ID == keep[0].ID {
				continue
			}
			contention.superseded[run.ID] = true
		}
	}
	return contention
}

// registerDeferredRun gives a deferred run an owner in the manager. Without an
// entry the row is unreachable: `axi abort --run` would report a successful
// no-op while the row stayed running forever, and a superseding push could not
// resolve it. Cancelling it ends the row with the cause - recorded cancelled or
// failed by the same reason mapping every other cancellation path uses - except
// for a clean shutdown, which preserves a deferred run exactly as it preserves
// a parked one.
func (m *RunManager) registerDeferredRun(run *db.Run) {
	runID := run.ID
	done := make(chan struct{})
	var once sync.Once
	cancel := func(cause error) {
		if errors.Is(cause, pipeline.ErrDaemonShutdown) {
			return
		}
		once.Do(func() {
			reason := types.RunCancelReasonAbortedByUser
			if cause != nil {
				reason = cause.Error()
			}
			if _, err := m.db.FailActiveRunWithReason(runID, reason); err != nil {
				slog.Error("failed to terminate a deferred run", "run_id", runID, "error", err)
			}
			m.mu.Lock()
			delete(m.cancels, runID)
			delete(m.dones, runID)
			m.mu.Unlock()
			m.closeSubscribers(runID)
			close(done)
		})
	}
	m.mu.Lock()
	m.cancels[runID] = cancel
	m.dones[runID] = done
	m.mu.Unlock()
}

// boundedRecoveryReason caps a logged recovery reason so a verbose git or
// network failure cannot flood the bounded daemon log.
func boundedRecoveryReason(err error) string {
	const maxReasonLen = 200
	reason := err.Error()
	if len(reason) > maxReasonLen {
		return reason[:maxReasonLen] + "..."
	}
	return reason
}

// rejectUnresumableRun records why an active run could not be resumed. A run
// that was parked at a gate was promised preservation by the stop that left
// it, so it must not inherit the blanket "daemon crashed during execution"
// stamp the generic recovery pass applies: it records the concrete reason
// instead. Every other active row is left to that pass.
func (m *RunManager) rejectUnresumableRun(run *db.Run, reason error) {
	slog.Warn("active run cannot be safely resumed", "run_id", run.ID, "error", reason)
	parked := run.AwaitingAgentSince != nil
	stepRows, err := m.db.GetStepsByRun(run.ID)
	if err != nil {
		// The step rows exist to stop a stale marker from over-claiming
		// preservation; recording a concrete rejection reason over-claims
		// nothing, so a read failure falls back to the marker rather than
		// dropping the run into the blanket crash stamp.
		slog.Warn("could not read steps while rejecting an unresumable run; falling back to its parked marker",
			"run_id", run.ID, "error", err, "records_reason", parked)
	} else {
		parked = lifecycle.ParkedAtGate(run, stepRows)
	}
	if !parked {
		return
	}
	errMsg := fmt.Sprintf("run was parked at a gate but could not be resumed: %s", reason)
	failed, err := m.db.FailActiveRunWithReason(run.ID, errMsg)
	if err != nil {
		slog.Error("failed to record why a parked run could not be resumed", "run_id", run.ID, "error", err)
		return
	}
	if !failed {
		slog.Warn("parked run was no longer active when its rejection reason was recorded", "run_id", run.ID)
	}
}

func (m *RunManager) setRunSkippedSteps(runID string, skipSteps []types.StepName) error {
	if m.persistSkippedSteps != nil {
		return m.persistSkippedSteps(runID, skipSteps)
	}
	return m.db.SetRunSkippedSteps(runID, skipSteps)
}

// finishRunGoroutine releases everything a run goroutine owns. parked is the
// one variation: a clean shutdown that caught the run at a gate leaves the
// worktree in place, because prepareRecoveredRun requires it on the next
// daemon start (its HEAD must match run.HeadSHA).
func (m *RunManager) finishRunGoroutine(repoID, runID string, cfg *config.Config, cancel context.CancelCauseFunc, ag agent.Agent, gateDir, workDir, reason string, parked bool) {
	cancel(nil)
	if ag != nil {
		_ = ag.Close()
	}
	m.closeSubscribers(runID)
	if !parked {
		// removeRunWorktree does the identity-based reap before the directory
		// goes: a descendant that left the process group can only be named by
		// the worktree its cwd resolves under.
		m.removeRunWorktree(repoID, runID, gateDir, workDir, reason)
		// A preserved run resumes and still owns its evidence; only a run that
		// is really finished gives its directory up.
		m.cleanupRunEvidence(cfg, runID)
	}
	m.mu.Lock()
	delete(m.executors, runID)
	delete(m.cancels, runID)
	delete(m.dones, runID)
	m.mu.Unlock()
}

// parkPreserved reports whether an execution error is the clean-shutdown park
// signal, which is not a failure: the run keeps its running status, its gate
// and its worktree for the next daemon start.
func (m *RunManager) parkPreserved(runID string, err error) bool {
	if !errors.Is(err, pipeline.ErrParkPreserved) {
		return false
	}
	slog.Info("pipeline parked for shutdown", "run_id", runID)
	return true
}

// prepareRecoveredRun builds everything a parked run needs to resume, or says
// why it cannot. Every failure here defers by default; only the calls below
// that establish an adverse fact wrap it in unresumable, so a read added here
// later waits for a later start instead of costing the run its worktree.
func (m *RunManager) prepareRecoveredRun(ctx context.Context, run *db.Run) (*recoveredRunPlan, error) {
	if run == nil || run.Status != types.RunRunning || run.AwaitingAgentSince == nil || run.Branch == "" {
		return nil, unresumable(fmt.Errorf("run is not a parked running run"))
	}
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, unresumable(fmt.Errorf("run repository is missing"))
	}
	workDir := worktrees.RecordedDir(m.paths, run.WorktreePath(), repo.ID, run.ID)
	execSteps := m.steps()
	// The destructive lifecycle guard promises an operator that a parked run
	// survives a stop, so it corroborates its candidates against this same
	// owner: the two must decide resumability by one rule, not two.
	// That owner already separates the two: a read that did not complete comes
	// back as pipeline.ErrRecoveryEvidenceUnavailable, so everything else it
	// reports is an established adverse fact.
	if err := lifecycle.ResumePreconditionsMet(ctx, m.db, m.paths, run, execSteps); err != nil {
		if errors.Is(err, pipeline.ErrRecoveryEvidenceUnavailable) {
			return nil, err
		}
		return nil, unresumable(err)
	}
	gateDir := m.paths.RepoDir(repo.ID)
	commonDir, err := git.Run(ctx, workDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve worktree common git dir: %w", err)
	}
	if !samePath(resolveGitPath(workDir, commonDir), gateDir) {
		return nil, unresumable(fmt.Errorf("worktree does not belong to its gate repository"))
	}

	cfg, err := m.loadRecoveredConfig(ctx, run, repo, workDir)
	if err != nil {
		return nil, err
	}
	forgeCtx, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
	if err != nil {
		return nil, fmt.Errorf("resolve forge profile: %w", err)
	}
	ag, err := newPipelineAgent(ctx, cfg, m.paths.EvidenceRoot(cfg.Test.Evidence.LocalRoot), exec.LookPath, forgeEnvironment(forgeCtx))
	if err != nil {
		// The agent launcher is resolved from PATH at this moment; one that is
		// absent right now says nothing adverse about the run.
		return nil, fmt.Errorf("resolve pipeline agent: %w", err)
	}
	if cfg.SessionReuse {
		if err := validateRecoveredSessionProviders(m.db, run.ID, ag); err != nil {
			_ = ag.Close()
			return nil, err
		}
	}
	return &recoveredRunPlan{
		run:     run,
		repo:    repo,
		workDir: workDir,
		gateDir: gateDir,
		cfg:     cfg,
		agent:   ag,
		steps:   execSteps,
		forge:   forgeCtx,
	}, nil
}

func validateRecoveredSessionProviders(database *db.DB, runID string, ag agent.Agent) error {
	sessions, err := database.GetRunAgentSessions(runID)
	if err != nil {
		return fmt.Errorf("get run sessions: %w", err)
	}
	for _, session := range sessions {
		if session.Role != string(pipeline.SessionRoleReviewer) && session.Role != string(pipeline.SessionRoleFixer) {
			return unresumable(fmt.Errorf("recovered run has unknown session role %q", session.Role))
		}
		if session.Agent == "" || session.SessionID == "" {
			return unresumable(fmt.Errorf("recovered run has incomplete session metadata"))
		}
		if session.Role == string(pipeline.SessionRoleFixer) && !agent.SupportsSessionProvider(ag, session.Agent) {
			return unresumable(fmt.Errorf("session provider %q is no longer configured", session.Agent))
		}
	}
	return nil
}

func (m *RunManager) loadRecoveredConfig(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) (*config.Config, error) {
	// A config read that fails right now is a fact about this filesystem
	// moment, not about the run, so both reads defer rather than reject.
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	repoCfg, err := config.LoadRepo(workDir)
	if err != nil {
		return nil, fmt.Errorf("load repo config: %w", err)
	}
	var trustedSHA string
	var reachErr error
	if repo.DefaultBranch != "" {
		fetchCtx, cancel := context.WithTimeout(ctx, recoveredConfigFetchTimeout)
		defer cancel()
		if err := fetchRecoveredRemoteBranch(fetchCtx, workDir, "origin", repo.DefaultBranch); err != nil {
			reachErr = fmt.Errorf("fetch default branch %q: %w", repo.DefaultBranch, err)
			slog.Warn("failed to fetch default branch while recovering run; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else if sha, err := git.ResolveRef(ctx, workDir, "refs/remotes/origin/"+repo.DefaultBranch); err != nil {
			reachErr = fmt.Errorf("resolve default branch %q: %w", repo.DefaultBranch, err)
			slog.Warn("failed to resolve default branch while recovering run; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else {
			trustedSHA = sha
		}
	}
	// SECURITY: a trusted-config fetch failure must abort, not silently disable
	// the disable_project_settings opt-out (see assertGateTrustedConfigReadable).
	// It aborts THIS start only: an unreachable branch is a network fact about
	// the moment, and so is a git read that did not complete, so both defer and
	// only a config that was read and did not parse rejects.
	if err := assertGateTrustedConfigReadable(ctx, workDir, repo.DefaultBranch, trustedSHA); err != nil {
		if reachErr != nil && !resumeRejected(err) {
			return nil, reachErr
		}
		return nil, err
	}
	trustedRepoCfg, allowRepoCommands := resolveTrustedRepoConfig(ctx, workDir, globalCfg, repo, trustedSHA, run.ID)
	effectiveRepoCfg := config.EffectiveRepoConfig(repoCfg, trustedRepoCfg, allowRepoCommands)
	cfg := config.Merge(globalCfg, effectiveRepoCfg)
	if err := m.paths.ValidateEvidenceRoot(cfg.Test.Evidence.LocalRoot); err != nil {
		return nil, err
	}
	cfg.TrustedConfigSHA = trustedSHA
	if globalCfg.Eval.CaptureProvenance {
		if err := cfg.EnableEvalProvenance(globalCfg, effectiveRepoCfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func newPipelineAgent(ctx context.Context, cfg *config.Config, evidenceRoot string, lookPath func(string) (string, error), environment runenv.Overlay) (agent.Agent, error) {
	if steps.IsDemoMode() {
		return agent.NewNoop(), nil
	}
	if err := cfg.ResolveAgent(ctx, lookPath); err != nil {
		return nil, err
	}
	agents := cfg.Agents
	if len(agents) == 0 {
		agents = []types.AgentName{cfg.Agent}
	}
	created := make([]agent.Agent, 0, len(agents))
	for _, name := range agents {
		next, err := agent.NewWithOptions(name, cfg.AgentPathFor(name), cfg.AgentArgsFor(name), agent.Options{
			ACPRegistryOverrides:   cfg.ACPRegistryOverrides,
			DisableProjectSettings: cfg.DisableProjectSettings,
			Profile:                cfg.AgentProfileFor(name),
			Environment:            environment,
		})
		if err != nil {
			for _, existing := range created {
				_ = existing.Close()
			}
			return nil, fmt.Errorf("create agent %s: %w", name, err)
		}
		created = append(created, agent.WithSteering(next, evidenceRoot))
	}
	ag := agent.NewFallback(created)
	// Fail closed ONLY under the trusted opt-out (see startRun): refuse an
	// unverified harness when the repo disabled project settings; otherwise run
	// every adapter as before.
	if cfg.DisableProjectSettings {
		if err := agent.EnsureGateNeutralized(ag); err != nil {
			_ = ag.Close()
			return nil, err
		}
	}
	return ag, nil
}

func forgeEnvironment(ctx *forgecontext.Context) runenv.Overlay {
	if ctx == nil {
		return runenv.Overlay{}
	}
	return ctx.Environment
}

func resolveGitPath(workDir, value string) string {
	value = strings.TrimSpace(value)
	if !filepath.IsAbs(value) {
		value = filepath.Join(workDir, value)
	}
	return filepath.Clean(value)
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return a == b
}

func (m *RunManager) resumeRecoveredRuns(plans []recoveredRunPlan) {
	for _, plan := range plans {
		m.resumeRecoveredRun(plan)
	}
}

func (m *RunManager) resumeRecoveredRun(plan recoveredRunPlan) {
	if m.shuttingDown.Load() {
		_ = plan.agent.Close()
		return
	}
	runCtx, cancel := context.WithCancelCause(context.Background())
	executor := pipeline.NewExecutor(m.db, m.paths, plan.cfg, plan.agent, plan.steps, m.broadcast)
	executor.SetOnPRMerged(func(_ context.Context, runID string) {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.relabelEvalRun(context.Background(), plan.cfg, runID)
		}()
	})
	executor.SetForgeContext(plan.forge)
	// Restore the run's own skip set: without it a resumed run would run the
	// very steps its start excluded with --skip.
	executor.SetSkippedSteps(plan.run.SkippedSteps)
	done := make(chan struct{})
	m.mu.Lock()
	m.executors[plan.run.ID] = executor
	m.cancels[plan.run.ID] = cancel
	m.dones[plan.run.ID] = done
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		startedAt := time.Now()
		parked := false
		deferredToLaterStart := false
		defer m.wg.Done()
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				errMsg := fmt.Sprintf("internal panic: %v", recovered)
				plan.run.Status = types.RunFailed
				plan.run.Error = &errMsg
				if err := m.db.UpdateRunErrorStatus(plan.run.ID, errMsg, types.RunFailed); err != nil {
					slog.Error("failed to update recovered run after panic", "run_id", plan.run.ID, "error", err)
				}
			}
			m.finishRunGoroutine(plan.repo.ID, plan.run.ID, plan.cfg, cancel, plan.agent, plan.gateDir, plan.workDir, "resumed_run_finished", parked || deferredToLaterStart)
			if deferredToLaterStart {
				m.registerDeferredRun(plan.run)
			}
		}()

		if err := executor.Resume(runCtx, plan.run, plan.repo, plan.workDir); err != nil {
			if parked = m.parkPreserved(plan.run.ID, err); parked {
				return
			}
			// Resume re-reads the gate evidence at entry, so the same
			// classifier prepareRecoveredRun defers on can arrive here too. It
			// must mean the same thing on both sides of that seam: a read that
			// did not complete never costs a preserved run its worktree.
			if errors.Is(err, pipeline.ErrRecoveryEvidenceUnavailable) && plan.run.Status == types.RunRunning {
				deferredToLaterStart = true
				slog.Warn("resumed run left for a later daemon start",
					"run_id", plan.run.ID, "reason", boundedRecoveryReason(err))
				return
			}
			if plan.run.Status == types.RunRunning {
				errMsg := err.Error()
				plan.run.Status = types.RunFailed
				plan.run.Error = &errMsg
				if dbErr := m.db.UpdateRunErrorStatus(plan.run.ID, errMsg, types.RunFailed); dbErr != nil {
					slog.Error("failed to mark recovered run failed", "run_id", plan.run.ID, "error", dbErr)
				}
			}
			slog.Error("recovered pipeline failed", "run_id", plan.run.ID, "error", err)
		}
		fields := telemetry.Fields{
			"action":      "finished",
			"trigger":     "recovery",
			"agent":       string(plan.cfg.Agent),
			"branch_role": telemetryBranchRole(plan.run.Branch, plan.repo.DefaultBranch),
			"status":      string(plan.run.Status),
			"duration_ms": time.Since(startedAt).Milliseconds(),
			"step_count":  len(plan.steps),
			"pr_created":  plan.run.PRURL != nil && *plan.run.PRURL != "",
		}
		if failedStep := telemetryFailedStepName(m.db, plan.run.ID); failedStep != "" {
			fields["failed_step"] = failedStep
		}
		addRunPerformanceSummary(m.db, plan.run.ID, fields)
		telemetry.Track("run", fields)
	}()
}

func agentListsEqual(a, b []types.AgentName) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Subscribe registers a subscriber mailbox for a run.
//
// The returned subscription always opens with a stream-gap frame, so a
// subscriber's first action is always one authoritative read. That makes
// attach and reconnect converge without each consumer needing its own
// subscribe-then-reconcile ordering rule. A run that has already completed
// yields that one gap and then finishes.
func (m *RunManager) Subscribe(runID string) (*Subscription, error) {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	mb := newEventMailbox(runID, m.stateRevs[runID])
	if m.completedRuns[runID] {
		mb.close()
		return &Subscription{mb: mb, unsub: func() {}}, nil
	}
	if len(m.subscribers[runID]) >= maxSubscribersPerRun {
		return nil, fmt.Errorf("run %s already has the maximum of %d event subscribers", runID, maxSubscribersPerRun)
	}
	m.subscribers[runID] = append(m.subscribers[runID], mb)

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			m.subMu.Lock()
			subs := m.subscribers[runID]
			for i, s := range subs {
				if s == mb {
					m.subscribers[runID] = append(subs[:i], subs[i+1:]...)
					break
				}
			}
			if len(m.subscribers[runID]) == 0 {
				delete(m.subscribers, runID)
			}
			m.subMu.Unlock()
			mb.release()
		})
	}
	return &Subscription{mb: mb, unsub: unsub}, nil
}

// Subscription is one subscriber's view of a run's event stream. It owns no
// goroutine: the caller drives it with Next.
type Subscription struct {
	mb    *eventMailbox
	unsub func()
}

// Next blocks until the next frame is available and returns it. ok is false
// once the stream is finished or ctx is done.
func (s *Subscription) Next(ctx context.Context) (ipc.Event, bool) { return s.mb.next(ctx) }

// Close unsubscribes and releases every retained payload. It is idempotent.
func (s *Subscription) Close() { s.unsub() }

// StateRev returns the current monotonic state revision for a run.
//
// A caller serving an authoritative snapshot must sample this BEFORE reading
// the database. Every producer writes state and only then broadcasts, so a
// revision sampled first is never newer than the snapshot that follows it:
// every event at or below it is already reflected in that read, and every
// event above it still reaches the subscriber and still applies on top.
func (m *RunManager) StateRev(runID string) int64 {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	return m.stateRevs[runID]
}

// broadcast stamps a state revision and publishes an event to every subscriber
// of the event's run. It performs no blocking channel operation and no I/O, so
// the executor can never be stalled by a slow or dead subscriber.
func (m *RunManager) broadcast(event ipc.Event) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	if ipc.ClassOf(event.Type) == ipc.ClassState {
		m.stateRevs[event.RunID]++
		event.StateRev = m.stateRevs[event.RunID]
	}
	for _, mb := range m.subscribers[event.RunID] {
		mb.publish(event)
	}
}

// sweepRunWorktreeProcesses terminates whatever is still standing in a
// finished run's worktree. Cancelling the run context tears down each step's
// process group, but a descendant that called setsid(2) is no longer in any
// group the pipeline can name - only the worktree it is standing in still
// identifies it (see internal/procreap).
//
// The run's own worktree is what the sweep is pointed at, so a placement
// outside the default tree needs no configuration lookup and cannot be hidden
// by a worktree_roots edit made while the run was executing. The ordering rule
// and its rationale live on procreap.SweepRunWorktree, which every removal site
// in every package goes through.
func (m *RunManager) sweepRunWorktreeProcesses(repoID, runID, wtDir string) {
	procreap.SweepRunWorktree(m.paths.WorktreesDir(), repoID, runID, wtDir, "run_cleanup")
}

// cleanupRunEvidence tidies up after one finished run, then bounds the whole
// evidence directory.
//
// The per-run half is deliberately os.Remove and not os.RemoveAll: it succeeds
// only when the directory is empty, so a run that produced no artifact leaves
// nothing behind while a run that did keeps every file. The test step creates
// the directory before the agent decides whether it has evidence to write, so
// without this nearly every run left a permanent empty directory - that alone
// was the overwhelming majority of the accumulation this reaper exists to stop.
//
// The sweep that follows keeps a long-lived daemon converging on the retention
// budget instead of waiting for a restart. Both halves are best effort: losing
// a cleanup pass costs disk, while failing a finished run over it would cost
// the user their result.
func (m *RunManager) cleanupRunEvidence(cfg *config.Config, runID string) {
	configured := ""
	policy := evidenceReapPolicy{
		Retention: config.DefaultEvidenceRetention,
		MaxRuns:   config.DefaultEvidenceMaxRuns,
	}
	if cfg != nil {
		configured = cfg.Test.Evidence.LocalRoot
		policy = evidenceReapPolicy{
			Retention: cfg.Test.Evidence.Retention,
			MaxRuns:   cfg.Test.Evidence.MaxRuns,
		}
	}
	root := m.paths.EvidenceRoot(configured)
	if err := os.Remove(filepath.Join(root, runID)); err != nil && !os.IsNotExist(err) {
		slog.Debug("run evidence kept", "run_id", runID, "reason", err)
	}
	reapEvidence(m.db, root, policy, time.Now())
}

// removeRunWorktree tears one run's worktree down: it sweeps whatever is still
// standing in the directory and only then removes it.
//
// Every removal of a run worktree this package performs goes through here, and
// none calls git.WorktreeRemove directly, because the ordering is easy to forget
// at one site and invisible when forgotten - a run whose setup failed, whose
// execution returned, or which was resumed after a crash all reach this point by
// different routes. reason distinguishes the routes in the log.
func (m *RunManager) removeRunWorktree(repoID, runID, gateDir, wtDir, reason string) {
	m.sweepRunWorktreeProcesses(repoID, runID, wtDir)
	if err := git.WorktreeRemove(context.Background(), gateDir, wtDir); err != nil {
		slog.Warn("failed to remove run worktree", "reason", reason, "run_id", runID, "path", wtDir, "error", err)
	}
}

// closeSubscribers soft-closes every subscriber for a run and marks the run
// completed so future Subscribe calls return a gapped, immediately-finished
// subscription. Soft close still drains queued frames and any pending gap, so
// a coalesced terminal transition cannot be discarded by completion.
func (m *RunManager) closeSubscribers(runID string) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	for _, mb := range m.subscribers[runID] {
		mb.close()
	}
	delete(m.subscribers, runID)
	m.completedRuns[runID] = true
	m.completedOrder = append(m.completedOrder, runID)
	if len(m.completedOrder) > 1000 {
		half := len(m.completedOrder) / 2
		for _, id := range m.completedOrder[:half] {
			delete(m.completedRuns, id)
			delete(m.stateRevs, id)
		}
		m.completedOrder = m.completedOrder[half:]
	}
}

// repoIDFromGatePath extracts the repo ID from a gate bare repo path.
// Gate paths look like: <root>/repos/<id>.git
func repoIDFromGatePath(gatePath string) (string, error) {
	base := filepath.Base(gatePath)
	if !strings.HasSuffix(base, ".git") {
		return "", fmt.Errorf("invalid gate path: %s", gatePath)
	}
	return strings.TrimSuffix(base, ".git"), nil
}

// branchFromRef extracts the branch name from a full git ref.
// "refs/heads/main" → "main", "main" → "main"
func branchFromRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// loadTrustedRepoConfig reads .no-mistakes.yaml from the trusted
// default-branch commit (trustedSHA - the exact SHA startRun just fetched and
// resolved) in the worktree and parses it. Reading at a pinned SHA, rather
// than the origin/<defaultBranch> remote-tracking ref, closes the stale-ref
// hole: the gate worktree shares refs with the bare repo, so without a fresh
// fetch + resolve the ref could point at a commit a previous run left behind.
//
// trustedSHA is empty when the default branch is unknown, the fetch failed,
// or the ref did not resolve. The caller must first reject those cases with
// assertGateTrustedConfigReadable; returning nil here remains defensive and
// ensures EffectiveRepoConfig never uses pushed gate-control fields.
func loadTrustedRepoConfig(ctx context.Context, wtDir, trustedSHA, runID string) *config.RepoConfig {
	if trustedSHA == "" {
		// No trusted SHA means no freshly-fetched default-branch commit to
		// read from. Return nil so EffectiveRepoConfig forces empty
		// commands/agent - the secure default - instead of falling back to a
		// potentially stale origin/<defaultBranch> ref.
		return nil
	}
	content, err := git.ShowFile(ctx, wtDir, trustedSHA, ".no-mistakes.yaml")
	if err != nil {
		// Path absent on the default branch is the common "repo has no
		// trusted commands" case; log at debug so it isn't noisy. Other
		// errors are surfaced at warn so a genuinely broken read isn't
		// silent. Either way trusted is nil → fail closed.
		slog.Debug("trusted repo config: not present on default branch", "run_id", runID, "sha", trustedSHA, "error", err)
		return nil
	}
	trusted, err := config.LoadRepoFromBytes([]byte(content))
	if err != nil {
		slog.Warn("trusted repo config: parse failed; commands/agent from pushed branch will be disabled", "run_id", runID, "sha", trustedSHA, "error", err)
		return nil
	}
	return trusted
}

// resolveTrustedRepoConfig is the single owner of the trusted repo config a run
// executes under: it reads the trusted default-branch copy, then hands over to
// the maintainer's working-path copy when there is one.
//
// SECURITY: allow_repo_commands is read from the default-branch copy BEFORE the
// working-path copy is adopted, and is returned separately rather than read
// back off the resolved config, so a local override can never widen the push
// trust boundary (config.ResolveWorkingPathTrusted keeps the field on the
// default-branch side). Both callers (run start and crash recovery) go through
// here so that ordering has one home.
func resolveTrustedRepoConfig(ctx context.Context, gitDir string, globalCfg *config.GlobalConfig, repo *db.Repo, trustedSHA, runID string) (*config.RepoConfig, bool) {
	trusted := loadTrustedRepoConfig(ctx, gitDir, trustedSHA, runID)
	allowRepoCommands := trusted != nil && trusted.AllowRepoCommands
	return applyWorkingPathTrustedConfig(ctx, globalCfg, repo, trusted, runID), allowRepoCommands
}

// applyWorkingPathTrustedConfig promotes the maintainer's own checkout copy of
// .no-mistakes.yaml to be the run's trusted config, when the global config
// opted in with trust_working_path_config. It returns trusted unchanged when
// the opt-in is off, the working path is unknown, or the file is absent.
//
// The working path is the registered primary checkout on the daemon host, not
// the ephemeral gate worktree, so nothing a contributor pushed can reach it.
// See config.ResolveWorkingPathTrusted for why a present file replaces the
// default-branch copy outright and which two boundaries it still cannot weaken.
func applyWorkingPathTrustedConfig(ctx context.Context, globalCfg *config.GlobalConfig, repo *db.Repo, trusted *config.RepoConfig, runID string) *config.RepoConfig {
	if globalCfg == nil || !globalCfg.TrustWorkingPathConfig || repo == nil || repo.WorkingPath == "" {
		return trusted
	}
	path := filepath.Join(repo.WorkingPath, ".no-mistakes.yaml")
	if _, err := os.Stat(path); err != nil {
		// An absent file is the ordinary case and says nothing. Any other stat
		// error means the opted-in file may well be there and unreadable, which
		// silently drops the maintainer's own commands, so it gets the same
		// loud treatment as a parse failure.
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("working-path repo config: could not be inspected; falling back to the default-branch copy", "run_id", runID, "path", path, "error", err)
		}
		return trusted
	}
	workingCfg, err := config.LoadRepo(repo.WorkingPath)
	if err != nil {
		// Keep the default-branch copy, which is still a safe config, but never
		// fall back to it SILENTLY: the maintainer asked for this file to steer
		// the gate, so running different commands than the ones they edited
		// must be visible in the log rather than a quiet substitution.
		slog.Warn("working-path repo config: parse failed; falling back to the default-branch copy", "run_id", runID, "path", path, "error", err)
		return trusted
	}
	// A tracked file is a footgun: checking out a contributor's branch in the
	// primary checkout would put their commands into a trusted position, which
	// is exactly what the default-branch rule prevents. Untracked files cannot
	// arrive over a push. Warn rather than refuse - the maintainer opted in.
	if _, err := git.Run(ctx, repo.WorkingPath, "ls-files", "--error-unmatch", ".no-mistakes.yaml"); err == nil {
		slog.Warn("working-path repo config is tracked by git: a branch checkout in the primary worktree can change trusted gate commands; keep it untracked (.git/info/exclude)", "run_id", runID, "path", path)
	}
	slog.Info("working-path repo config applied over the default-branch copy", "run_id", runID, "path", path)
	return config.ResolveWorkingPathTrusted(trusted, workingCfg)
}

// assertGateTrustedConfigReadable fails a run LOUD when the trusted
// default-branch copy of .no-mistakes.yaml could not be READ at all. This is the
// security correction for disable_project_settings: that field is a boundary
// honored only from the trusted copy, so an unreadable trusted config must NOT
// be silently treated as "not opted out" - no-mistakes cannot know whether the
// repo relies on the boundary, so it refuses to run rather than risk launching a
// gate agent with the project instructions loaded.
//
// It distinguishes "could not read the trusted config at all" (abort) from
// "read the trusted tree fine, there is simply no .no-mistakes.yaml on the
// default branch" (the common ordinary-repo case, which is NOT opted out and
// must proceed). Abort cases:
//   - no known default branch to read a trusted copy from,
//   - the default branch could not be fetched/resolved to a pinned SHA,
//   - the pinned commit or tree is not readable (missing object / partial fetch),
//   - the trusted .no-mistakes.yaml is present but unreadable or unparseable.
func assertGateTrustedConfigReadable(ctx context.Context, wtDir, defaultBranch, trustedSHA string) error {
	if defaultBranch == "" {
		return fmt.Errorf("cannot evaluate disable_project_settings: repository has no known default branch to read trusted config from")
	}
	if trustedSHA == "" {
		return fmt.Errorf("cannot evaluate disable_project_settings: failed to fetch or resolve trusted default branch %q (refusing to run without reading the trusted config)", defaultBranch)
	}
	if _, err := git.Run(ctx, wtDir, "rev-parse", "-q", "--verify", trustedSHA+"^{commit}"); err != nil {
		return fmt.Errorf("cannot evaluate disable_project_settings: trusted default-branch commit %s is not readable: %w", trustedSHA, err)
	}
	entry, err := git.Run(ctx, wtDir, "ls-tree", trustedSHA, "--", ".no-mistakes.yaml")
	if err != nil {
		return fmt.Errorf("cannot evaluate disable_project_settings: trusted default-branch tree at %s is not readable: %w", trustedSHA, err)
	}
	if entry == "" {
		return nil
	}
	content, err := git.ShowFile(ctx, wtDir, trustedSHA, ".no-mistakes.yaml")
	if err != nil {
		return fmt.Errorf("cannot evaluate disable_project_settings: trusted .no-mistakes.yaml at %s is present but not readable: %w", trustedSHA, err)
	}
	if _, err := config.LoadRepoFromBytes([]byte(content)); err != nil {
		// The only established fact here: the file was read and is invalid.
		// Every abort above is a read that did not complete, so a recovering
		// run defers on it and keeps its worktree.
		return unresumable(fmt.Errorf("cannot evaluate disable_project_settings: trusted .no-mistakes.yaml at %s is present but unparseable: %w", trustedSHA, err))
	}
	return nil
}

// HandlePushReceived processes a push notification from the post-receive hook.
// It creates a run, sets up a worktree, and launches pipeline execution in the background.
func (m *RunManager) HandlePushReceived(ctx context.Context, params *ipc.PushReceivedParams) (string, error) {
	// Ref deletion (git push remote :branch) sends new SHA as all-zeros.
	// Nothing to validate - skip pipeline.
	if git.IsZeroSHA(params.New) {
		return "", fmt.Errorf("ref deletion push, no pipeline to run")
	}

	repoID, err := repoIDFromGatePath(params.Gate)
	if err != nil {
		return "", err
	}

	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repo for gate %s", params.Gate)
	}

	branch := branchFromRef(params.Ref)
	return m.startRun(ctx, repo, branch, params.New, params.Old, "push", params.SkipSteps, params.Intent)
}

// HandleRerun creates a new run for the latest recoverable head on a branch:
// normally the gate branch, or the latest terminal run's verified unpublished
// head while custody remains outstanding. An explicit intent overrides the
// selected run. Otherwise an authoritative intent is inherited byte-for-byte;
// runs without one infer intent afresh.
func (m *RunManager) HandleRerun(ctx context.Context, repoID, branch, previousRunID string, skipSteps []types.StepName, intent string) (string, error) {
	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repo %s", repoID)
	}

	gateDir := m.paths.RepoDir(repo.ID)
	gateHead, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve gate head: %w", err)
	}

	runs, err := m.db.GetRunsByRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get runs: %w", err)
	}

	var latestForBranch *db.Run
	var matchingHead *db.Run
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		if latestForBranch == nil {
			latestForBranch = run
		}
		if run.HeadSHA == gateHead {
			matchingHead = run
			break
		}
	}
	if latestForBranch == nil {
		return "", fmt.Errorf("no previous run for branch %s", branch)
	}
	headSHA, err := resolveRerunHead(ctx, gateDir, branch, latestForBranch)
	if err != nil {
		return "", err
	}
	selectedRun := latestForBranch
	if previousRunID != "" {
		selectedRun, err = m.db.GetRun(previousRunID)
		if err != nil {
			return "", fmt.Errorf("get selected run: %w", err)
		}
		if selectedRun == nil || selectedRun.RepoID != repoID || selectedRun.Branch != branch {
			return "", fmt.Errorf("selected run %s does not belong to repo %s branch %s", previousRunID, repoID, branch)
		}
	}

	baseSHA := latestForBranch.BaseSHA
	if matchingHead != nil && headSHA == gateHead {
		baseSHA = matchingHead.BaseSHA
	}

	intentSource := db.RunIntentSourceAgent
	if strings.TrimSpace(intent) == "" {
		intentSource = ""
		if selectedRun.Intent != nil && selectedRun.IntentSource != nil &&
			db.IsAuthoritativeRunIntentSource(*selectedRun.IntentSource) {
			// Do not normalize or regenerate this value. The selected run's
			// persisted bytes are the canonical acceptance criteria for the
			// replacement run.
			intent = *selectedRun.Intent
			intentSource = db.RunIntentSourceRerun
		}
	}

	return m.startRunWithIntentSource(ctx, repo, branch, headSHA, baseSHA, "rerun", skipSteps, intent, intentSource)
}

func resolveRerunHead(ctx context.Context, gateDir, branch string, latest *db.Run) (string, error) {
	gateHead, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve gate head: %w", err)
	}
	if latest == nil || !latest.Status.Terminal() || latest.CustodyReturnedAt != nil || latest.HeadSHA == gateHead {
		return gateHead, nil
	}
	published := ""
	if latest.LastPushedSHA != nil {
		published = *latest.LastPushedSHA
	} else if latest.SubmittedHeadSHA != nil {
		published = *latest.SubmittedHeadSHA
	}
	if published == latest.HeadSHA || latest.TerminalHeadVerifiedAt == nil {
		return gateHead, nil
	}
	recoveryRef := custody.RecoveryRef(latest.ID)
	refTarget, refExists, refErr := git.ExactRefTarget(ctx, gateDir, recoveryRef)
	if refErr != nil {
		return "", fmt.Errorf("inspect terminal recovery ref for run %s: %w", latest.ID, refErr)
	}
	if refExists {
		preserved, preserveErr := git.Run(ctx, gateDir, "rev-parse", recoveryRef+"^{commit}")
		if preserveErr != nil {
			return "", fmt.Errorf("refusing rerun: terminal recovery ref for run %s points at non-commit object %s; inspect with `no-mistakes axi status` and reconcile custody first", latest.ID, refTarget)
		}
		if preserved != latest.HeadSHA {
			return "", fmt.Errorf("refusing rerun: terminal recovery ref for run %s points at %s, not recorded unpublished head %s; inspect with `no-mistakes axi status` and reconcile custody first", latest.ID, preserved, latest.HeadSHA)
		}
		return preserved, nil
	}
	if preserved, objectErr := git.Run(ctx, gateDir, "rev-parse", latest.HeadSHA+"^{commit}"); objectErr == nil && preserved == latest.HeadSHA {
		if anchorErr := custody.PreserveRecoveryHead(ctx, gateDir, latest.ID, preserved); anchorErr != nil {
			return "", fmt.Errorf("preserve terminal head %s before rerun: %w", preserved, anchorErr)
		}
		return preserved, nil
	}
	return "", fmt.Errorf("refusing rerun from stale gate head %s: terminal run %s recorded unpublished head %s, but that head is unavailable; inspect with `no-mistakes axi status` and reconcile custody first", gateHead, latest.ID, latest.HeadSHA)
}

// fetchRunDefaultBranch fetches the trusted branch from the refreshed
// registration when it differs from the gate worktree's inherited origin. It
// updates only the run worktree's existing origin tracking ref and never
// rewrites clone or gate remote configuration. When the values agree after
// redaction, origin remains authoritative so embedded credentials retained in
// the gate can still authenticate without ever entering the database.
func fetchRunDefaultBranch(ctx context.Context, workDir string, repo *db.Repo) error {
	originURL, err := git.GetRemoteURL(ctx, workDir, "origin")
	if !repo.URLsVerified || (err == nil && safeurl.Redact(originURL) == repo.UpstreamURL) {
		return git.FetchRemoteBranch(ctx, workDir, "origin", repo.DefaultBranch)
	}
	return git.FetchRemoteBranchToRef(ctx, workDir, repo.UpstreamURL, repo.DefaultBranch, "refs/remotes/origin/"+repo.DefaultBranch)
}

// startRun creates a run, sets up a worktree, and launches pipeline execution.
// A non-empty intent is stamped onto the run as agent-supplied, so the intent
// step uses it instead of inferring from transcripts.
func (m *RunManager) startRun(ctx context.Context, repo *db.Repo, branch, headSHA, baseSHA, trigger string, skipSteps []types.StepName, intent string) (string, error) {
	return m.startRunWithIntentSource(ctx, repo, branch, headSHA, baseSHA, trigger, skipSteps, intent, db.RunIntentSourceAgent)
}

// startRunWithIntentSource is the common run-creation path. source is empty
// when no intent is supplied, RunIntentSourceAgent for a new explicit
// override, and RunIntentSourceRerun for inherited explicit intent.
func (m *RunManager) startRunWithIntentSource(ctx context.Context, repo *db.Repo, branch, headSHA, baseSHA, trigger string, skipSteps []types.StepName, intent, source string) (string, error) {
	branchRole := telemetryBranchRole(branch, repo.DefaultBranch)
	trackStartFailure := func(stage string) {
		telemetry.Track("run", telemetry.Fields{
			"action":      "start_failed",
			"trigger":     trigger,
			"branch_role": branchRole,
			"stage":       stage,
		})
	}

	if m.shuttingDown.Load() {
		trackStartFailure("daemon_shutdown")
		return "", fmt.Errorf("daemon is shutting down")
	}

	// Serialize per repo+branch to prevent two concurrent pushes from both
	// passing cancelActiveRuns and creating duplicate pipelines.
	lockKey := repo.ID + "/" + branch
	lockVal, _ := m.branchLocks.LoadOrStore(lockKey, &sync.Mutex{})
	branchMu := lockVal.(*sync.Mutex)
	branchMu.Lock()
	defer branchMu.Unlock()

	// Best-effort only: a clone's remotes may change after init. Refresh the
	// registered URLs before constructing any run-owned Git operation, but keep
	// the exact prior repo value and continue when discovery, validation, or the
	// atomic database replacement fails. The reason is deliberately bounded and
	// URL-free so neither credentials nor sensitive remote material reach logs.
	if refreshed, _, refreshErr := gate.RefreshRepoURLs(ctx, m.db, repo); refreshErr != nil {
		slog.Warn("repository URL refresh skipped; continuing with existing registration", "repo_id", repo.ID, "reason", gate.ReasonForRefreshFailure(refreshErr))
	} else {
		repo = refreshed
	}

	// Cancel any active run for this repo+branch, unless one of them is parked
	// and must be preserved, in which case this newer run is the one that loses.
	if err := m.cancelActiveRuns(repo.ID, branch); err != nil {
		trackStartFailure("branch_run_preserved")
		return "", err
	}

	storedIntent := intent
	if source != db.RunIntentSourceRerun {
		storedIntent = strings.TrimSpace(storedIntent)
	}
	var runIntent *db.RunIntent
	if strings.TrimSpace(storedIntent) != "" {
		if source == "" {
			source = db.RunIntentSourceAgent
		}
		runIntent = &db.RunIntent{Summary: storedIntent, Source: source, Score: 1}
	}

	run, err := m.db.InsertRunWithIntent(repo.ID, branch, headSHA, baseSHA, runIntent)
	if err != nil {
		trackStartFailure("create_run")
		return "", fmt.Errorf("create run: %w", err)
	}
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("load config: %s", err))
		trackStartFailure("load_global_config")
		return "", fmt.Errorf("load global config: %w", err)
	}

	// Create worktree from the gate bare repo, where this repository's
	// worktree placement says it belongs (see internal/worktrees). This is the
	// only point at which configuration decides placement: the resolved
	// directory is recorded on the run before it exists on disk, and every
	// later consumer reads that record back, so an edit to worktree_roots from
	// here on is inert for this run.
	gateDir := m.paths.RepoDir(repo.ID)
	layout := worktrees.New(m.paths, globalCfg.WorktreeRoots)
	checkouts, err := registeredCheckouts(m.db)
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("list registered checkouts: %s", err))
		trackStartFailure("list_registered_checkouts")
		return "", fmt.Errorf("list registered checkouts: %w", err)
	}
	if err := layout.ValidateCheckout(repo.WorkingPath, checkouts...); err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("worktree placement: %s", err))
		trackStartFailure("invalid_worktree_placement")
		return "", fmt.Errorf("worktree placement: %w", err)
	}
	wtDir := layout.Dir(repo.ID, repo.WorkingPath, run.ID)
	if err := m.db.SetRunWorktreeDir(run.ID, wtDir); err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("record worktree placement: %s", err))
		trackStartFailure("record_worktree_placement")
		return "", fmt.Errorf("record worktree placement: %w", err)
	}
	if err := git.WorktreeAdd(ctx, gateDir, wtDir, headSHA); err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("create worktree: %s", err))
		trackStartFailure("create_worktree")
		return "", fmt.Errorf("create worktree: %w", err)
	}

	// The worktree exists from here on, so cleanup ownership is armed from here
	// on: every later setup failure returns through this defer, and the
	// background goroutine takes ownership only once it is running. Arming it any
	// later would leave the directory behind - in the operator's own worktree
	// root, unswept - for whichever failures happen in between.
	bgOwnsWorktree := false
	defer func() {
		if !bgOwnsWorktree {
			m.removeRunWorktree(repo.ID, run.ID, gateDir, wtDir, "run_setup_failed")
		}
	}()

	if err := git.CopyLocalUserIdentity(ctx, repo.WorkingPath, wtDir); err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("configure worktree git identity: %s", err))
		trackStartFailure("configure_worktree_identity")
		return "", fmt.Errorf("configure worktree git identity: %w", err)
	}
	// Fetch the trusted default branch and resolve it to an exact commit SHA
	// before any read. Reading the trusted config at this pinned SHA (rather
	// than the origin/<defaultBranch> remote-tracking ref) is what makes a
	// fetch failure fail closed: if the fetch errors or the ref does not
	// resolve, trustedSHA stays empty, loadTrustedRepoConfig returns nil, and
	// EffectiveRepoConfig drops the pushed branch's commands/agent. Without
	// the resolve, a stale origin/<defaultBranch> left in the shared bare
	// repo by a previous run could serve a trusted copy that the live default
	// branch has already removed - silently running stale shell.
	var trustedSHA string
	if repo.DefaultBranch != "" {
		fetchErr := fetchRunDefaultBranch(ctx, wtDir, repo)
		if fetchErr != nil {
			slog.Warn("failed to fetch default branch into worktree; trusted config disabled (commands/agent from pushed branch will be dropped)", "run_id", run.ID, "branch", repo.DefaultBranch, "error", fetchErr)
		} else if sha, err := git.ResolveRef(ctx, wtDir, "refs/remotes/origin/"+repo.DefaultBranch); err != nil {
			slog.Warn("failed to resolve fetched default-branch ref; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else {
			trustedSHA = sha
		}
	}

	// SECURITY: sign_commits is read from the global config only, never from
	// the pushed branch, so a contributor cannot turn the maintainer's commit
	// signing off. Disabling it here (rather than in the gate's shared config)
	// keeps the write strictly per-worktree and picks up a config change on
	// the next run without a re-init. A repo where per-worktree config is
	// unusable fails the run rather than writing the opt-out into the gate's
	// shared config, where it would outlive the setting.
	if !globalCfg.SignCommits {
		if err := git.DisableCommitSigning(ctx, wtDir); err != nil {
			m.db.UpdateRunError(run.ID, fmt.Sprintf("disable commit signing: %s", err))
			trackStartFailure("disable_commit_signing")
			return "", fmt.Errorf("disable commit signing: %w", err)
		}
	}
	repoCfg, err := config.LoadRepo(wtDir)
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("load config: %s", err))
		trackStartFailure("load_repo_config")
		return "", fmt.Errorf("load repo config: %w", err)
	}
	// SECURITY: load the code-executing selection fields (commands.* and
	// agent) from the trusted default-branch copy of .no-mistakes.yaml rather
	// than the pushed SHA. The worktree is checked out at headSHA (the
	// contributor's branch), so reading repoCfg above would honor a
	// contributor's commands/agent and let any pushed SHA run arbitrary shell
	// (sh -c) or pick the launched agent (incl. acp: targets) on the daemon
	// host with the maintainer's env (GH_TOKEN, SSH agent, ...).
	// EffectiveRepoConfig replaces commands + agent with the trusted
	// default-branch values unless the maintainer has explicitly opted in.
	//
	// allow_repo_commands is itself read ONLY from the trusted copy: a
	// contributor cannot self-enable it from the pushed branch. A readable
	// trusted tree with no config leaves the opt-in false and forces
	// commands/agent empty. An unreadable trusted tree aborts below.
	// SECURITY: a trusted-config fetch failure must abort, not silently disable
	// the disable_project_settings opt-out (see assertGateTrustedConfigReadable).
	if err := assertGateTrustedConfigReadable(ctx, wtDir, repo.DefaultBranch, trustedSHA); err != nil {
		m.db.UpdateRunError(run.ID, err.Error())
		trackStartFailure("trusted_config_unreadable")
		return "", err
	}
	trustedRepoCfg, allowRepoCommands := resolveTrustedRepoConfig(ctx, wtDir, globalCfg, repo, trustedSHA, run.ID)
	effectiveRepoCfg := config.EffectiveRepoConfig(repoCfg, trustedRepoCfg, allowRepoCommands)
	if allowRepoCommands {
		slog.Warn("allow_repo_commands is enabled on the default branch: honoring commands/agent from pushed branch", "run_id", run.ID, "branch", branch)
	} else if repoCfg.Commands != effectiveRepoCfg.Commands || repoCfg.Agent != effectiveRepoCfg.Agent || !agentListsEqual(repoCfg.Agents, effectiveRepoCfg.Agents) {
		// Surface the silent override so a maintainer who shipped a commands.*
		// or agent change on a feature branch understands why it did not run.
		// This is not an error: it is the secure default in action.
		slog.Info("repo commands/agent loaded from default branch, not pushed branch", "run_id", run.ID, "branch", branch, "default_branch", repo.DefaultBranch)
	}
	cfg := config.Merge(globalCfg, effectiveRepoCfg)
	if err := m.paths.ValidateEvidenceRoot(cfg.Test.Evidence.LocalRoot); err != nil {
		m.db.UpdateRunError(run.ID, err.Error())
		trackStartFailure("evidence_root")
		return "", err
	}
	cfg.TrustedConfigSHA = trustedSHA

	// Persist the EFFECTIVE skip set, the same union the executor runs with,
	// so a run preserved across a clean daemon stop resumes with the scope it
	// started under. Both sources reach delivery steps: --skip accepts push,
	// pr and ci, and the trusted repo config's skip_steps is the standing form
	// of the same selection. Persisting only the run argument leaves a resumed
	// run free to push a branch and open a PR the repository itself excluded,
	// because config skips are applied lazily inside the execution loop and
	// post-gate step rows are still pending at park. Resolving it here rather
	// than at the executor keeps one owner for what the run is scoped to. The
	// write is authoritative: the run does not start at all rather than start
	// with a scope that cannot survive a stop.
	effectiveSkips := cfg.SkippedSteps(skipSteps)
	if len(effectiveSkips) > 0 {
		run.SkippedSteps = effectiveSkips
		if err := m.setRunSkippedSteps(run.ID, effectiveSkips); err != nil {
			m.db.UpdateRunError(run.ID, fmt.Sprintf("persist run skip set: %s", err))
			trackStartFailure("persist_skip_set")
			return "", fmt.Errorf("persist run skip set: %w", err)
		}
	}

	if globalCfg.Eval.CaptureProvenance {
		if err := cfg.EnableEvalProvenance(globalCfg, effectiveRepoCfg); err != nil {
			m.db.UpdateRunError(run.ID, err.Error())
			trackStartFailure("eval_provenance")
			return "", err
		}
	}
	forgeCtx, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("resolve forge profile: %s", err))
		trackStartFailure("resolve_forge_profile")
		return "", fmt.Errorf("resolve forge profile: %w", err)
	}

	// Create agent. In demo mode, skip resolution and use a no-op agent.
	var ag agent.Agent
	if steps.IsDemoMode() {
		ag = agent.NewNoop()
	} else {
		if err := cfg.ResolveAgent(ctx, exec.LookPath); err != nil {
			m.db.UpdateRunError(run.ID, err.Error())
			trackStartFailure("resolve_agent")
			return "", err
		}
		agents := cfg.Agents
		if len(agents) == 0 {
			agents = []types.AgentName{cfg.Agent}
		}
		created := make([]agent.Agent, 0, len(agents))
		for _, name := range agents {
			next, agErr := agent.NewWithOptions(name, cfg.AgentPathFor(name), cfg.AgentArgsFor(name), agent.Options{
				ACPRegistryOverrides:   cfg.ACPRegistryOverrides,
				DisableProjectSettings: cfg.DisableProjectSettings,
				Profile:                cfg.AgentProfileFor(name),
				Environment:            forgeEnvironment(forgeCtx),
			})
			if agErr != nil {
				m.db.UpdateRunError(run.ID, fmt.Sprintf("create agent %s: %s", name, agErr))
				trackStartFailure("create_agent")
				return "", fmt.Errorf("create agent %s: %w", name, agErr)
			}
			// Steer every pipeline agent to keep writes inside the worktree and
			// avoid mutating system state (e.g. brew/Homebrew touching
			// /Applications), which triggers macOS App Management prompts.
			created = append(created, agent.WithSteering(next, m.paths.EvidenceRoot(cfg.Test.Evidence.LocalRoot)))
		}
		ag = agent.NewFallback(created)
		// Fail closed ONLY under the trusted opt-out: when the repo asked to
		// disable project settings, refuse any resolved harness that lacks a
		// verified suppression knob rather than launch it with the target repo's
		// project instructions loaded. When the repo did not opt out, every
		// adapter runs exactly as before (backward-compat).
		if cfg.DisableProjectSettings {
			if err := agent.EnsureGateNeutralized(ag); err != nil {
				m.db.UpdateRunError(run.ID, err.Error())
				trackStartFailure("gate_not_neutralized")
				return "", err
			}
		}
	}

	execSteps := m.steps()
	// Persist the step plan so a lifecycle guard can tell whether the binary
	// that would resume this run still runs the same layout. A write failure
	// only leaves the plan unknown, which the guard treats as unresumable.
	planNames := steps.StepNames(execSteps)
	run.StepPlan = planNames
	if err := m.db.SetRunStepPlan(run.ID, planNames); err != nil {
		slog.Warn("failed to persist run step plan", "run_id", run.ID, "error", err)
	}
	telemetry.Track("run", telemetry.Fields{
		"action":      "started",
		"trigger":     trigger,
		"agent":       string(cfg.Agent),
		"branch_role": branchRole,
		"step_count":  len(execSteps),
		"demo_mode":   steps.IsDemoMode(),
	})

	// Create executor with event broadcast.
	runCtx, cancel := context.WithCancelCause(context.Background())
	executor := pipeline.NewExecutor(m.db, m.paths, cfg, ag, execSteps, m.broadcast)
	executor.SetForgeContext(forgeCtx)
	executor.SetSkippedSteps(effectiveSkips)
	executor.SetOnPRMerged(func(_ context.Context, runID string) {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.relabelEvalRun(context.Background(), cfg, runID)
		}()
	})

	// Track executor.
	done := make(chan struct{})
	m.mu.Lock()
	m.executors[run.ID] = executor
	m.cancels[run.ID] = cancel
	m.dones[run.ID] = done
	m.mu.Unlock()

	// Background goroutine now owns worktree cleanup.
	bgOwnsWorktree = true

	// Launch pipeline in background.
	m.wg.Add(1)
	go func() {
		startedAt := time.Now()
		parked := false
		defer m.wg.Done()
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				errMsg := fmt.Sprintf("internal panic: %v", r)
				slog.Error("panic in pipeline goroutine", "run_id", run.ID, "panic", r)
				run.Status = types.RunFailed
				run.Error = &errMsg
				fields := telemetry.Fields{
					"action":      "finished",
					"trigger":     trigger,
					"agent":       string(cfg.Agent),
					"branch_role": branchRole,
					"status":      string(run.Status),
					"duration_ms": time.Since(startedAt).Milliseconds(),
					"step_count":  len(execSteps),
					"pr_created":  run.PRURL != nil && *run.PRURL != "",
				}
				if failedStep := telemetryFailedStepName(m.db, run.ID); failedStep != "" {
					fields["failed_step"] = failedStep
				}
				addRunPerformanceSummary(m.db, run.ID, fields)
				telemetry.Track("run", fields)
				verifiedHead, verified := preserveRunHead(m.db, wtDir, run)
				var dbErr error
				if verified {
					dbErr = m.db.UpdateRunErrorStatusWithVerifiedHead(run.ID, errMsg, types.RunFailed, verifiedHead)
				} else {
					dbErr = m.db.UpdateRunErrorStatus(run.ID, errMsg, types.RunFailed)
				}
				if dbErr != nil {
					slog.Error("failed to update run after panic", "run_id", run.ID, "error", dbErr)
				}
			}
			m.finishRunGoroutine(repo.ID, run.ID, cfg, cancel, ag, gateDir, wtDir, "run_finished", parked)
		}()

		if err := executor.Execute(runCtx, run, repo, wtDir); err != nil {
			if parked = m.parkPreserved(run.ID, err); parked {
				return
			}
			fields := telemetry.Fields{
				"action":      "finished",
				"trigger":     trigger,
				"agent":       string(cfg.Agent),
				"branch_role": branchRole,
				"status":      string(run.Status),
				"duration_ms": time.Since(startedAt).Milliseconds(),
				"step_count":  len(execSteps),
				"pr_created":  run.PRURL != nil && *run.PRURL != "",
			}
			if failedStep := telemetryFailedStepName(m.db, run.ID); failedStep != "" {
				fields["failed_step"] = failedStep
			}
			addRunPerformanceSummary(m.db, run.ID, fields)
			telemetry.Track("run", fields)
			slog.Error("pipeline failed", "run_id", run.ID, "error", err)
		} else {
			fields := telemetry.Fields{
				"action":      "finished",
				"trigger":     trigger,
				"agent":       string(cfg.Agent),
				"branch_role": branchRole,
				"status":      string(run.Status),
				"duration_ms": time.Since(startedAt).Milliseconds(),
				"step_count":  len(execSteps),
				"pr_created":  run.PRURL != nil && *run.PRURL != "",
			}
			addRunPerformanceSummary(m.db, run.ID, fields)
			telemetry.Track("run", fields)
			slog.Info("pipeline completed", "run_id", run.ID)
		}
		// Collection runs here, on the finished run, because a case is only
		// honest once the human gate decision it labels is recorded - which is
		// exactly what reaching this point means. It is last on purpose: the
		// pipeline's own outcome is already decided and reported above, so
		// nothing below can change it.
		m.autoCaptureEvalCase(runCtx, cfg, run.ID)
	}()

	return run.ID, nil
}

// evalAutoCaptureTimeout bounds one automatic collection pass. Collection is
// local Git and SQLite work whose slowest step is seeding a repository's object
// pool the first time it is seen, so this is generous rather than tight; the
// pass also stops early with the run context, which Shutdown cancels.
const evalAutoCaptureTimeout = 3 * time.Minute

// autoCaptureEvalCase freezes a finished run's review passes into the local
// eval corpus.
//
// Everything here is subordinate to the run: collection can be slow, can fail,
// can find nothing, and none of that may reach the pipeline. So it swallows its
// own panic rather than letting the run goroutine's recover mark a completed
// run as failed, it bounds its own time, and it reports failure only to the
// log. Runs are serialized against each other because they share one object
// pool and one registry file; the wait is harmless, since every run holding
// this lock has already finished its pipeline.
//
// A run with nothing to collect is the common case (no review step, a skipped
// gate, rounds recorded before provenance was on), so that outcome is DEBUG.
// Only a genuine capture fault is worth a warning.
func (m *RunManager) autoCaptureEvalCase(ctx context.Context, cfg *config.Config, runID string) {
	if cfg == nil || !cfg.Eval.AutoCapture || !cfg.Eval.CaptureProvenance {
		return
	}
	// A cancelled or aborted run has nothing worth freezing and every Git call
	// below would fail on the dead context anyway. Leaving early keeps that
	// ordinary outcome out of the warning log.
	if ctx.Err() != nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic while collecting eval case", "run_id", runID, "panic", r)
		}
	}()
	m.evalCaptureMu.Lock()
	defer m.evalCaptureMu.Unlock()

	if ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, evalAutoCaptureTimeout)
	defer cancel()

	result, err := eval.AutoCapture(ctx, m.paths, m.db, runID, cfg.Eval.MaxCases, cfg.Eval.DiversifiedSize)
	switch {
	case err != nil:
		slog.Warn("failed to collect eval case", "run_id", runID, "error", err)
	case result.Skipped:
		slog.Debug("run has no eval case to collect", "run_id", runID, "reason", result.Reason)
	default:
		if result.PinWarning != "" {
			slog.Warn("eval retention skipped: diversified pins unavailable", "run_id", runID, "reason", result.PinWarning)
		}
		slog.Info("collected eval case", "run_id", runID, "cases", result.Captured, "pruned", result.Pruned)
	}
}

func (m *RunManager) relabelEvalRun(ctx context.Context, cfg *config.Config, runID string) {
	// Best-effort and off the CI step's call stack: a merge must not stall
	// the pipeline for eval I/O. The caller holds m.wg for daemon drain.
	if m == nil || m.paths == nil || m.db == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic while relabeling eval case", "run_id", runID, "panic", r)
		}
	}()
	m.evalCaptureMu.Lock()
	defer m.evalCaptureMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, evalAutoCaptureTimeout)
	defer cancel()
	store, err := eval.Open(m.paths.EvalDir())
	if err != nil {
		slog.Warn("failed to open eval store for relabel", "run_id", runID, "error", err)
		return
	}
	defer store.Close()
	if cfg != nil {
		store.SetDiversifiedSize(cfg.Eval.DiversifiedSize)
	}
	if _, err := eval.RelabelRun(ctx, store, m.paths, m.db, runID); err != nil {
		slog.Warn("failed to relabel eval case after merge", "run_id", runID, "error", err)
	}
}

// addRunPerformanceSummary attaches the bounded per-run performance rollup
// to the terminal "run finished" event: low-cardinality counts only. The
// detailed per-invocation evidence (session keys, models, timings, tokens)
// stays in the local agent_invocations table and is never sent remotely.
func addRunPerformanceSummary(database *db.DB, runID string, fields telemetry.Fields) {
	summary, err := database.AgentInvocationSummaryForRun(runID)
	if err != nil {
		return
	}
	fields["agent_invocations"] = summary.Count
	fields["resumed_invocations"] = summary.Resumed
	fields["fallback_invocations"] = summary.Fallback
}

func telemetryBranchRole(branch, defaultBranch string) string {
	if branch == "" {
		return "unknown"
	}
	if defaultBranch != "" && branch == defaultBranch {
		return "default"
	}
	return "feature"
}

func telemetryFailedStepName(database *db.DB, runID string) string {
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return ""
	}
	for _, step := range steps {
		if step.Status == types.StepStatusFailed {
			return string(step.StepName)
		}
	}
	return ""
}

// HandleRespond routes a user approval action to the executor for the given run.
func (m *RunManager) HandleRespond(runID string, step types.StepName, action types.ApprovalAction, findingIDs []string) error {
	return m.HandleRespondWithOverrides(runID, step, action, findingIDs, nil, nil)
}

// HandleRespondWithOverrides is like HandleRespond but also forwards user
// instructions and user-authored findings to the executor.
func (m *RunManager) HandleRespondWithOverrides(runID string, step types.StepName, action types.ApprovalAction, findingIDs []string, instructions map[string]string, addedFindings []types.Finding) error {
	m.mu.Lock()
	exec, ok := m.executors[runID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("no active executor for run %s", runID)
	}

	return exec.RespondWithOverrides(step, action, findingIDs, instructions, addedFindings)
}

// Shutdown signals all active runs to stop. Called during daemon shutdown to
// prevent orphaned goroutines from continuing agent calls and git operations.
// The cause is pipeline.ErrDaemonShutdown, which a run parked at an approval
// gate treats as "leave me resumable" rather than "fail": it keeps its run
// row, its gate step and its worktree, and the next daemon start resumes it.
// A run cancelled mid-step still fails with "daemon shutting down".
func (m *RunManager) Shutdown() {
	m.shuttingDown.Store(true)

	m.mu.Lock()
	cancels := make(map[string]context.CancelCauseFunc, len(m.cancels))
	for id, cancel := range m.cancels {
		cancels[id] = cancel
	}
	m.mu.Unlock()

	for id, cancel := range cancels {
		cancel(pipeline.ErrDaemonShutdown)
		slog.Info("signalled run to stop for shutdown", "run_id", id)
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		slog.Warn("timed out waiting for runs to finish during shutdown")
	}
}

// HandleCancel stops an active run and propagates cancellation to the executor.
func (m *RunManager) HandleCancel(runID string) error {
	m.mu.Lock()
	cancel, ok := m.cancels[runID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("no active run %s", runID)
	}

	cancel(fmt.Errorf(types.RunCancelReasonAbortedByUser))
	return nil
}

// cancelActiveRuns cancels any in-progress runs for the given repo+branch
// and waits for their goroutines to finish before returning, preventing
// concurrent pushes to upstream.
// The cancellation cause is propagated to the executor via context.Cause,
// which uses it as the run's error message in the DB.
func (m *RunManager) cancelActiveRuns(repoID, branch string) error {
	runs, err := m.db.GetRunsByRepo(repoID)
	if err != nil {
		// A listing that failed is not a branch with nothing on it: proceeding
		// would start a second run over a run this read never saw, including a
		// parked one holding unpushed commits.
		slog.Error("failed to query active runs for cancellation", "repo", repoID, "branch", branch, "error", err)
		return fmt.Errorf("could not check branch %q for active runs: %w", branch, err)
	}

	var active []*db.Run
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		if run.Status != types.RunPending && run.Status != types.RunRunning {
			continue
		}
		active = append(active, run)
	}
	// The live path decides branch contention by the same owner startup uses:
	// a parked or deferred run holds unpushed pipeline commits it was promised
	// would survive, so the newer push loses instead of destroying it.
	if preserved := preservedBranchRuns(active, m.gateStepRowsOf); len(preserved) > 0 {
		return fmt.Errorf("run %s is parked at a gate on branch %q and would be destroyed by a new run; resolve or abort it first", preserved[0].ID, branch)
	}

	var toWait []chan struct{}
	for _, run := range active {
		m.mu.Lock()
		cancel, ok := m.cancels[run.ID]
		done := m.dones[run.ID]
		m.mu.Unlock()
		if !ok {
			continue
		}

		cancel(fmt.Errorf(types.RunCancelReasonSuperseded))
		slog.Info("cancelled active run", "run_id", run.ID, "repo_id", repoID, "branch", branch)
		if done != nil {
			toWait = append(toWait, done)
		}
	}

	timeout := time.After(30 * time.Second)
	for _, done := range toWait {
		select {
		case <-done:
		case <-timeout:
			slog.Warn("timed out waiting for cancelled runs to finish")
			return nil
		}
	}
	return nil
}
