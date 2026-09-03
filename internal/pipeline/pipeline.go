package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var ErrFatalGateReconciliation = errors.New("fatal gate reconciliation")

// ErrDaemonShutdown is the cancellation cause a clean daemon stop uses. The
// message is unchanged so a run that is not gate-parked still records the
// same error it always did.
var ErrDaemonShutdown = errors.New("daemon shutting down")

// ErrParkPreserved is returned by Execute and Resume when a clean shutdown
// interrupted a run parked at an approval gate. The run row is left running
// and parked, the gate step row is untouched, and the caller must keep the
// worktree so startup recovery can resume the run.
var ErrParkPreserved = errors.New("run left parked for daemon shutdown")

// ErrRecoveryEvidenceUnavailable marks a recovery check that could not be
// completed because a read failed, as opposed to one that completed and found
// adverse evidence. A failed read says nothing about the run, so callers that
// would otherwise terminally fail a preserved run must wait instead.
var ErrRecoveryEvidenceUnavailable = errors.New("recovery evidence unavailable")

// evidenceUnavailable marks cause as a failed read rather than adverse
// evidence about the run.
func evidenceUnavailable(cause error) error {
	return fmt.Errorf("%w: %w", ErrRecoveryEvidenceUnavailable, cause)
}

// RestartBoundary is the step a restart re-enters validation at, the first
// step of the validation region.
//
// On today's unreordered pipeline Review holds both roles the region has: it
// is the boundary and it is the certifier that records the review-approved
// head, so the region is a single step. That is why the boundary step cannot
// restart into itself - it would re-enter the same step whose commit triggered
// the restart, and nothing further along would ever judge the result. Issues
// #7/#8 separate the two roles by adding Format and making it the boundary,
// leaving Review the certifier at the far end of a multi-step region.
const RestartBoundary types.StepName = types.StepReview

// CommitsOwnWorkAtExit reports whether a step routes its exit through the
// validation helper that commits an unclean worktree, which is what makes the
// step's own round the author of every commit between the head the round
// started on and the current head.
//
// Steps outside this set move the head for reasons of their own - the rebase
// step replays the branch onto upstream, the CI step commits an agent repair -
// so the same range there is not "what this step changed" and must not be
// presented as it.
func CommitsOwnWorkAtExit(name types.StepName) bool {
	switch name {
	case types.StepReview, types.StepTest, types.StepDocument, types.StepLint:
		return true
	default:
		return false
	}
}

// StepContext provides shared resources to pipeline steps during execution.
type StepContext struct {
	Ctx                   context.Context
	Run                   *db.Run
	Repo                  *db.Repo
	WorkDir               string
	GateDir               string
	Agent                 agent.Agent
	Config                *config.Config
	ForgeContext          *forgecontext.Context
	DB                    *db.DB
	Log                   func(string) // discrete log line (newline-terminated, user-visible + file)
	LogChunk              func(string) // raw streaming chunk (user-visible + file)
	LogFile               func(string) // file-only log callback (not shown to user)
	Fixing                bool         // true when re-executing after a "fix" action
	SkipFixExecution      bool         // replay an already-completed fix round's review turn only
	ReviewStartingHeadSHA string
	PreviousFindings      string // JSON findings from the previous execution (set during fix loop)
	// StepResultID is the DB row ID of the current step's step_results record.
	// Steps use it to query their own round history for multi-round prompts.
	StepResultID string
	// EvidenceDir is where this run's test-evidence artifacts belong, always
	// outside the worktree. The executor resolves it once from the app root
	// (honoring test.evidence.local_root) so every consumer - the test step's
	// prompt and the PR step's publisher - names the same directory. Empty only
	// in embeddings that never gather evidence.
	EvidenceDir string
	Env         []string // extra environment variables for subprocesses (used in tests)
	// UserIntent is a short, possibly-empty summary of what the change author
	// was trying to accomplish. It's surfaced in step prompts so agents have
	// context beyond the diff. Its authority depends on IntentSource: an
	// explicit `--intent` is the author's own goal statement, while an
	// inferred summary comes from a local agent transcript.
	UserIntent string
	// IntentSource records the provenance of UserIntent so steps can weigh
	// its authority. db.RunIntentSourceAgent ("agent") means the driving
	// agent supplied it explicitly via `axi run --intent`; db.RunIntentSourceRerun
	// ("rerun") means that authoritative intent was inherited. Both are
	// authoritative acceptance criteria; an agent name ("claude", "codex", ...)
	// means it was inferred from a transcript (a hint). Empty when no intent exists.
	IntentSource string
	// UncertifiedFromSHA/ToSHA/SourceRunID name a previous run's fixer
	// commits on this branch whose re-review did not complete. They are set
	// on a later run's initial review (Fixing==false) so that review still
	// receives fix-round provenance. Empty when no such range applies.
	UncertifiedFromSHA     string
	UncertifiedToSHA       string
	UncertifiedSourceRunID string
	// UncertifiedPriorRounds are review rounds from the source run that left
	// the uncertified range. Nil when none apply.
	UncertifiedPriorRounds []*db.StepRound
	// PriorBranchDecisions are rounds from EARLIER runs on this branch that
	// recorded a human decision about their findings. Unlike the uncertified
	// range above, nothing clears them when a review completes: a decision the
	// user made about this branch keeps standing until the branch's run history
	// ages out of the loader's bound. Nil when none apply. Advisory prompt
	// context only.
	PriorBranchDecisions          []*db.BranchDecisionRound
	PriorBranchDecisionsTruncated bool
	// Sessions manages the run's durable review-fixer session. The session
	// machinery remains role-generic for legacy recovery; nil runs every
	// invocation cold.
	Sessions *RunSessions
	// Shared carries in-memory run-scoped results one step hands to a later
	// step in the same run (e.g. the combined document+lint pass).
	Shared             *RunShared
	CIReadinessChanged func(ready, declaredNoCI bool)
	// OnPRMerged is a best-effort hook after a merged PR state is persisted.
	// Eval uses it to relabel auto-fix/shipped-unfixed gold; nil is a no-op.
	OnPRMerged func(ctx context.Context, runID string)
	// agentInvocations counts the agent turns run through this context. It is
	// read and written with sync/atomic free functions on the field address
	// rather than an atomic.Int64 so StepContext stays copyable and go vet's
	// copylocks check stays quiet.
	//
	// Copyability is the constraint that bounds this counter's reach: a copied
	// StepContext counts independently, so a turn run through a copy is
	// invisible to the original and its commit would be attributed to a
	// deterministic tool. The two copy sites (reconcileApprovalGate and the CI
	// step's publishedBranchHead) run no agent, and CIStep is the only
	// ApprovalGateReconciler. A step that routes through runValidationStep must
	// therefore never implement ApprovalGateReconciler; TestValidationStepsAreNotApprovalGateReconcilers
	// in internal/pipeline/steps pins that.
	agentInvocations int64
}

// AgentInvocations returns how many agent turns have run through this step
// context. Commit attribution reads it as a snapshot taken around one round:
// a commit made in a round that ran an agent is agent-authored, otherwise a
// deterministic tool produced it.
func (sctx *StepContext) AgentInvocations() int64 {
	if sctx == nil {
		return 0
	}
	return atomic.LoadInt64(&sctx.agentInvocations)
}

// RunAgentSession executes one turn of a durable review-loop role session,
// running cold when sessions are unavailable. The invocation is bounded by
// RunAgent's deadline. Only the review step's fixer turns use this; every
// other agent invocation - including every review turn, which must stay
// independent of the session that prescribed the fixes under review - goes
// through RunAgent and stays session-isolated.
func (sctx *StepContext) RunAgentSession(role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(sctx.Ctx, opts, role)
}

// ResetAgentSession drops the run's durable session identity for a role, so the
// next turn of that role starts in a fresh session. It is a no-op when sessions
// are unavailable, where every turn is already cold.
func (sctx *StepContext) ResetAgentSession(role SessionRole) {
	if sctx.Sessions == nil {
		return
	}
	sctx.Sessions.Reset(role)
}

// StepOutcome is the result of executing a pipeline step.
type StepOutcome struct {
	NeedsApproval bool // whether the step pauses for user action
	AutoFixable   bool
	Findings      string // JSON findings for TUI display (optional)
	ExitCode      int    // process exit code (0 = success)
	PRURL         string // PR/MR URL if this step created or found one
	Skipped       bool   // mark the step as skipped without failing the run
	SkipRemaining bool   // skip all subsequent steps (e.g. empty diff after rebase)
	// RestartFrom asks the executor to re-run validation from this earlier step.
	// CI repairs use it when policy requires revalidation or continuity cannot be
	// proven, sending the new local head back through review before push.
	RestartFrom types.StepName
	// FixSummary, when non-empty, is the agent's one-line commit summary for
	// the fix attempt performed during this round. Steps populate it in fix
	// mode so the executor can persist it on the round record and later
	// rounds can reference what was previously attempted.
	FixSummary string
	// ReviewApprovedHeadSHA is set only by a successfully executed full review
	// round. The executor durably records it only when the review step actually
	// completes, never while that outcome is parked or after a failed round.
	ReviewApprovedHeadSHA string

	// DurationOverrideMS, when positive, replaces the wall-clock duration
	// reported for this step. Used by demo mode to show realistic durations
	// without actually waiting.
	DurationOverrideMS int64
}

// Step is the interface that each pipeline step implements.
type Step interface {
	// Name returns the step's identity in the fixed pipeline sequence.
	Name() types.StepName

	// Execute runs the step logic and returns an outcome.
	// A step that returns NeedsApproval=true will pause the pipeline
	// until the user responds with an approval action.
	Execute(sctx *StepContext) (*StepOutcome, error)
}

// ApprovalGateReconciler is implemented by a step whose parked approval gate
// can become obsolete when an external source of truth changes. The executor
// invokes it with a bounded context while also waiting for an approval. A true
// result completes the step through the normal success path; false or an error
// leaves the gate parked. Implementations must be read-only and fail closed.
type ApprovalGateReconciler interface {
	ReconcileApprovalGate(sctx *StepContext) (resolved bool, err error)
}

// ApprovalResidueDiscarder is implemented by the step that records the run's
// review-approved head (Review today, whatever certifies after issues #7/#8).
// The step that certifies must not modify the tree it certifies, so when it
// exits with an unclean worktree it parks instead of committing: nothing is
// destroyed and the existing certification is untouched. The gate's own
// findings are what enumerate the leftovers, one item per path. The gate diff
// shows only the tracked modifications among them, since it is a git diff
// against the worktree and untracked files never appear in one.
//
// That gate has two answers. Approving means "discard", and the executor calls
// this so the step removes exactly the leftovers the park recorded before the
// run continues under the certification that already stands. Choosing fix
// instead means "keep": the step's own fix round commits the residue and
// re-reviews the new head, so that answer needs no hook here.
//
// findingsJSON is the parked gate's own findings, and it is what says which
// paths the park recorded. Discard is scoped to those paths because a human or
// a driving agent may edit the worktree while the run is parked, and a blanket
// reset over whatever the tree holds at approval time would destroy work
// nobody ruled on. The guarantee is exactly that scope: a file outside the
// recorded list survives, and an edit made during the park to a file that IS
// on the list goes with the rest of that path's contents.
//
// It is fail-closed. A discard that cannot complete leaves an uncommitted tree
// a later step would commit unjudged, so the error fails the run.
type ApprovalResidueDiscarder interface {
	DiscardApprovalResidue(sctx *StepContext, findingsJSON string) error
}
