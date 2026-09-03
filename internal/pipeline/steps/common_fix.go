package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type fixExecutionOptions struct {
	RequirePreviousFindings bool
	MissingFindingsError    string
	LogMessage              string
	Prompt                  string
	ErrorPrefix             string
	FallbackSummary         string
	AfterAgentRun           func(*agent.Result) error
	AgentContext            context.Context
	// SessionRole, when set, runs the fix turn in that durable review-loop
	// session (the review step's fixer role). Steps outside the review loop
	// leave it empty and stay session-isolated.
	SessionRole pipeline.SessionRole
	// Purpose labels the invocation for local performance telemetry.
	Purpose string
	// Workload records the bounded size of the change under fix for local
	// telemetry. Optional; nil leaves the invocation's workload unknown.
	Workload *agent.InvocationWorkload
}

type commitSummary struct {
	Summary string `json:"summary"`
}

var errRejectedCommitSummary = errors.New("rejected commit summary")

var commitSummarySchema = json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "maxLength": %d}
	},
	"required": ["summary"]
}`, config.MaxFixMessageSummaryBytes))

// hasBlockingFindings returns true if any finding has error or warning severity.
func hasBlockingFindings(items []Finding) bool {
	for _, f := range items {
		if f.Severity == types.FindingSeverityError || f.Severity == types.FindingSeverityWarning {
			return true
		}
	}
	return false
}

// assertPipelineHeadContinuity fails closed when the worktree HEAD is no longer
// equal to or a descendant of the head the pipeline itself last recorded
// (sctx.Run.HeadSHA). Every post-review step calls this guard at entry, and
// commitAgentFixes calls it around commits that advance the recorded head.
//
// The pipeline advances HEAD only through its own commits, each of which updates
// sctx.Run.HeadSHA in lockstep. If HEAD has diverged from that recorded head -
// e.g. a concurrent process reset the shared worktree to a different commit -
// then the reviewed change the pipeline approved is no longer in HEAD's history,
// and continuing would ship an unreviewed tree. The whole job of this tool is
// to not lose people's code, so we refuse rather than proceed.
//
// Anchor integrity: sctx.Run.HeadSHA is the correct, un-clobberable anchor. It
// is the *recorded* head the pipeline itself produced at its last commit - held
// in the single daemon process's in-memory Run struct (one shared pointer per
// run, never re-read from the DB mid-pipeline) and written only by no-mistakes
// commit code (commit_fix / rebase / ci_fix / push). An out-of-band `git reset`
// mutates the worktree HEAD on disk but cannot touch this field, so at the check
// point the anchor still holds the reviewed head even after a clobber. The guard
// deliberately compares the *recorded* head against the *live* worktree HEAD
// (git.HeadSHA); it never derives the anchor from the mutable worktree, which
// would be circular and defeatable. Because the guard runs at every post-review
// step entry and at the very top of commitAgentFixes - before any commit that
// would advance sctx.Run.HeadSHA - the next pipeline boundary after a clobber is
// caught while the anchor is still the pre-clobber reviewed head; the anchor can
// never be advanced into a clobbered lineage without first passing this check.
//
// This is what happened in run 01KXC3SD5NZYMERGDS68Z1C8ER: the review step
// committed a correct fix, a sibling worktree sharing the bare repo reset HEAD
// to a divergent commit that lacked it, and the document step committed on the
// clobber and shipped it. A forward-only agent commit (git rebase --continue,
// etc.) keeps the recorded head as an ancestor and is allowed; a divergent
// (sibling) reset or a backward reset both trip this guard. On any failure the
// step and the whole run abort (executor.failRun) before doing more work -
// nothing is committed or shipped.
func assertPipelineHeadContinuity(sctx *pipeline.StepContext, stepName types.StepName) error {
	recorded := strings.TrimSpace(sctx.Run.HeadSHA)
	if recorded == "" {
		return nil
	}
	currentHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head before %s step: %w", stepName, err)
	}
	if currentHead == recorded {
		return nil
	}
	// Fail closed: refuse unless the recorded head is genuinely an ancestor of the
	// live HEAD (a legitimate forward move). A non-ancestor result OR any git error
	// (e.g. an unknown recorded object) aborts rather than proceeds.
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", "--is-ancestor", recorded, currentHead); err != nil {
		return fmt.Errorf("refusing to run %s step: worktree HEAD %s is not a descendant of the pipeline's recorded head %s; "+
			"the reviewed change was rewritten out-of-band and would be lost - aborting to protect it",
			stepName, currentHead, recorded)
	}
	return nil
}

// commitPipelineCorrection creates a pipeline-authored correction commit with
// hook verification bypassed, and is the single owner of that bypass.
//
// A correction commit is machine-authored: the pipeline records the change its
// own agents or its own formatter produced, inside the throwaway run
// worktree. That worktree is freshly carved from the bare gate repo, so tracked
// hooks that depend on generated untracked runtime files cannot run there - the
// canonical case is a repository whose shared config sets core.hooksPath=.husky
// while a tracked .husky hook sources the generated .husky/_/husky.sh that no
// install step ever created in this worktree. The hook exits nonzero, the
// correction commit fails, and the whole run dies on setup state that says
// nothing about the change under review.
//
// --no-verify alone is not enough, because Git gates only pre-commit and
// commit-msg on it and always runs prepare-commit-msg (builtin/commit.c
// prepare_to_commit), so a repository carrying a legacy .husky
// prepare-commit-msg hook - commitizen and ticket-prefix setups are the common
// ones - still fails the exact commit this helper exists to complete. Pointing
// core.hooksPath at a freshly created empty directory for this one invocation
// covers the whole commit hook family; --no-verify is kept so the intent stays
// explicit at the call. The override lives only in this process argument list
// and the directory is removed afterwards, so nothing persists in the
// repository, the user's configuration, or the daemon's environment.
//
// Reach is deliberately narrow. Only commitAgentFixes (Review, Test, Document,
// Lint) and the Push step's leftover-worktree commit route here, because those
// are the two commits the pipeline authors from its own agents' and formatter's
// output.
// CI repair commits, the generic git runner, and every user-authored commit keep
// hook verification; the Review, Test, Document, Lint, Push, PR, and CI gates
// remain the authoritative quality checks for what these commits contain.
func commitPipelineCorrection(ctx context.Context, workDir, message string, logf func(string)) error {
	return commitPipelineCorrectionWithCleanup(ctx, workDir, message, logf, os.RemoveAll)
}

func commitPipelineCorrectionWithCleanup(
	ctx context.Context,
	workDir, message string,
	logf func(string),
	cleanup func(string) error,
) error {
	emptyHooksDir, err := os.MkdirTemp("", "no-mistakes-correction-hooks-")
	if err != nil {
		return fmt.Errorf("prepare hook-free commit environment: %w", err)
	}
	_, commitErr := git.Run(ctx, workDir, "-c", "core.hooksPath="+emptyHooksDir, "commit", "--no-verify", "-m", message)
	if cleanupErr := cleanup(emptyHooksDir); cleanupErr != nil {
		if logf != nil {
			logf(fmt.Sprintf("warning: failed to remove temporary hook-free commit directory %s: %v", emptyHooksDir, cleanupErr))
		} else {
			slog.Warn("failed to remove temporary hook-free commit directory", "path", emptyHooksDir, "error", cleanupErr)
		}
	}
	return commitErr
}

func commitAgentFixes(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string) error {
	ctx := sctx.Ctx
	if err := assertPipelineHeadContinuity(sctx, stepName); err != nil {
		return err
	}
	status, _ := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		sctx.Log("no agent changes to commit")
		return nil
	}
	if summary == "" {
		summary = fallbackSummary
	}
	if summary == "" {
		summary = "apply fixes"
	}
	commitMessage, err := sctx.Config.Commit.RenderFixMessage(stepName, summary)
	if err != nil {
		return fmt.Errorf("render %s fix commit message: %w", stepName, err)
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "add", "-A"); err != nil {
		return fmt.Errorf("stage %s changes: %w", stepName, err)
	}
	if err := commitPipelineCorrection(ctx, sctx.WorkDir, commitMessage, sctx.Log); err != nil {
		return fmt.Errorf("commit %s changes: %w", stepName, err)
	}
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head after %s commit: %w", stepName, err)
	}
	if err := assertPipelineHeadContinuity(sctx, stepName); err != nil {
		return err
	}
	ref := normalizedBranchRef(sctx.Run.Branch)
	if _, err := git.Run(ctx, sctx.WorkDir, "update-ref", ref, headSHA); err != nil {
		return fmt.Errorf("update local branch ref: %w", err)
	}
	startingHead := strings.TrimSpace(sctx.ReviewStartingHeadSHA)
	if startingHead == "" {
		startingHead = sctx.Run.HeadSHA
	}
	sctx.Run.HeadSHA = headSHA
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
		return err
	}
	if stepName == types.StepReview {
		pipeline.PersistUncertifiedPipelineRange(sctx, startingHead, headSHA)
	}
	sctx.Log(fmt.Sprintf("committed agent fixes: %s", commitMessage))
	return nil
}

// runValidationStep is the single exit path of every validation step: it runs
// the step body, commits an unclean worktree at that step's exit, attributes
// the commit, and decides whether the run must re-enter validation.
//
// Attribution is the whole point. A commit made in a round that invoked an
// agent is agent-authored, so nothing has judged it and validation must see it
// again. A commit a deterministic tool produced - a formatter rewriting
// whitespace - carries nothing new to judge and costs no revalidation.
//
// The one step that does not commit at its exit is the one that certifies: a
// step which records the review-approved head must not modify the tree it
// certifies, so it parks over the leftovers instead (residueGateOutcome).
func runValidationStep(
	sctx *pipeline.StepContext,
	name types.StepName,
	inner func(*pipeline.StepContext) (*pipeline.StepOutcome, error),
) (*pipeline.StepOutcome, error) {
	headBefore := sctx.Run.HeadSHA
	agentsBefore := sctx.AgentInvocations()

	outcome, err := inner(sctx)
	if err != nil {
		// The step failed and the run is about to fail with it. Committing the
		// residue of a failed round would publish work no gate ever accepted.
		return nil, err
	}
	if outcome == nil {
		outcome = &pipeline.StepOutcome{}
	}

	// A failure here is not "the tree is clean": a locked index or a transient
	// filesystem error would skip the exit commit, leave the agent's edits
	// uncommitted, and silently skip the attribution this helper exists for.
	dirty, err := git.HasUncommittedChanges(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("inspect worktree after %s: %w", name, err)
	}
	if dirty && outcome.ReviewApprovedHeadSHA != "" {
		// The step that certifies must not modify the tree it certifies, so it
		// parks over the residue instead of committing it. See
		// residueGateOutcome and pipeline.ApprovalResidueDiscarder.
		return residueGateOutcome(sctx, name, outcome)
	}
	if dirty {
		fallback := "commit " + string(name) + " changes"
		// A commit failure fails the run, exactly as every other
		// commitAgentFixes call site already does.
		if err := commitAgentFixes(sctx, name, outcome.FixSummary, fallback); err != nil {
			return nil, err
		}
	}

	// The step body's own commits count too. commitAgentFixes/executeFixMode is
	// where the deliberate agent edits actually land, so watching only the exit
	// residue would fire on stray build artifacts and miss every commit this
	// rule exists for.
	committed := sctx.Run.HeadSHA != headBefore
	agentAuthored := sctx.AgentInvocations() > agentsBefore
	if !committed {
		return outcome, nil
	}
	if agentAuthored {
		sctx.Log(fmt.Sprintf("%s committed an agent-authored change", name))
	} else {
		sctx.Log(fmt.Sprintf("%s committed a tool-authored change", name))
	}

	if pipeline.RestartBoundary.Order() >= name.Order() {
		// The boundary step cannot restart into itself, and it has nothing to
		// revoke either: the certifying step parks rather than committing at
		// its exit, so a commit that reaches here carried no certification of
		// its own and cannot have left one covering an ancestor.
		return outcome, nil
	}
	if !agentAuthored || outcome.RestartFrom != "" {
		// A step that already named its own restart boundary (CI) owns that
		// decision; never overwrite it.
		return outcome, nil
	}

	tree, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, fmt.Errorf("resolve tree after %s commit: %w", name, err)
	}
	tree = strings.TrimSpace(tree)
	if tree == "" {
		// Storing "" would make every later comparison for this step
		// short-circuit and disarm the guard for the rest of the run.
		return nil, fmt.Errorf("resolve tree after %s commit: git reported no tree for HEAD", name)
	}
	// Same grain as the CI step's lastFixedChecks guard: a step that commits
	// the tree its own most recent restart already produced is churning, so
	// stop asking. The comparison is per-process and remembers only that one
	// tree, so it narrows the loop rather than bounding it - runs.restart_count
	// and the soft-cap warning are what make a divergent loop visible.
	if tree == sctx.Shared.LastRestartTree(name) {
		sctx.Log(fmt.Sprintf("%s committed the same tree it already restarted on; continuing instead of restarting again", name))
		// Declining the restart does not make the commit judged. It still moved
		// the head past whatever review certified, and push accepts a certified
		// ancestor, so revoke that authority rather than ship the commit
		// unjudged.
		if err := revokeReviewAuthority(sctx); err != nil {
			return nil, err
		}
		return outcome, nil
	}
	sctx.Shared.SetLastRestartTree(name, tree)

	// prepareRestart owns revoking review authority and warning above the soft
	// cap; doing either here would write state the executor may then reject,
	// and would report a restart count that never existed.
	outcome.RestartFrom = pipeline.RestartBoundary
	sctx.Log(fmt.Sprintf("%s made an agent-authored commit; re-entering validation from %s", name, pipeline.RestartBoundary))
	return outcome, nil
}

// revokeReviewAuthority NULLs the run's review approval in the same statement
// that records the head. Split into two writes, a daemon crashing between them
// would recover a run whose approval covers an ancestor of its head and whose
// push guard therefore passes.
func revokeReviewAuthority(sctx *pipeline.StepContext) error {
	if err := sctx.DB.UpdateRunHeadSHAForRevalidation(sctx.Run.ID, sctx.Run.HeadSHA); err != nil {
		return err
	}
	sctx.Run.ReviewApprovedHeadSHA = nil
	return nil
}

// residueGateOutcome parks the certifying step over work it refused to commit.
//
// Committing at the exit of the step that records the review-approved head
// leaves that SHA a strict ancestor of the new head, and push accepts a
// certified ancestor, so the leftovers would ship with nothing having judged
// them. Dropping the certification instead dead-ends the run: the boundary step
// cannot restart into itself, so nothing would ever re-certify and push would
// fail three steps later on a NULL column.
//
// So nothing is committed and nothing is destroyed. The gate lists what was
// left behind, separating tracked-file modifications - the reviewer editing
// code outside its deliberate fix path, the louder case - from untracked
// non-ignored files. The two answers are discard (ApprovalResidueDiscarder,
// the existing certification stands) and fix, which commits the residue through
// the step's own fix round and re-reviews the new head. Every residue item is
// no-op, so an unattended --yes run discards and reports instead of bouncing
// through review with nobody watching.
func residueGateOutcome(sctx *pipeline.StepContext, name types.StepName, outcome *pipeline.StepOutcome) (*pipeline.StepOutcome, error) {
	modified, untracked, err := worktreeResidue(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("inspect worktree residue after %s: %w", name, err)
	}
	var findings Findings
	if outcome.Findings != "" {
		if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
			findings = Findings{}
		}
	}
	for i, file := range modified {
		findings.Items = append(findings.Items, Finding{
			ID:          fmt.Sprintf("residue-tracked-%d", i+1),
			Severity:    "warning",
			Action:      types.ActionNoOp,
			File:        file,
			Description: fmt.Sprintf("%s left tracked file %s modified and uncommitted; discarding restores it, fixing commits it and re-reviews the new head", name, file),
		})
	}
	for i, file := range untracked {
		findings.Items = append(findings.Items, Finding{
			ID:          fmt.Sprintf("residue-untracked-%d", i+1),
			Severity:    "info",
			Action:      types.ActionNoOp,
			File:        file,
			Description: fmt.Sprintf("%s left untracked file %s in the worktree; discarding removes it, fixing commits it and re-reviews the new head", name, file),
		})
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		return nil, fmt.Errorf("render %s residue findings: %w", name, err)
	}
	sctx.Log(fmt.Sprintf("%s certified %s but left %d modified tracked and %d untracked file(s) uncommitted; parking instead of committing over its own certification",
		name, sctx.Run.HeadSHA, len(modified), len(untracked)))
	outcome.NeedsApproval = true
	outcome.Findings = string(findingsJSON)
	return outcome, nil
}

// worktreeResidue splits an unclean worktree into tracked files that differ
// from HEAD and untracked non-ignored files. Gitignored build output is in
// neither list: it is not residue anyone needs to rule on, and discard leaves
// it alone.
func worktreeResidue(ctx context.Context, workDir string) (modified, untracked []string, err error) {
	out, err := git.Run(ctx, workDir, "diff", "--name-only", "HEAD")
	if err != nil {
		return nil, nil, fmt.Errorf("list modified tracked files: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			modified = append(modified, line)
		}
	}
	untracked, err = git.UntrackedFiles(ctx, workDir)
	if err != nil {
		return nil, nil, fmt.Errorf("list untracked files: %w", err)
	}
	return modified, untracked, nil
}

// discardValidationResidue restores tracked files and removes untracked
// non-ignored ones, leaving gitignored output in place. It is what approving a
// residue gate means, and it is a no-op on a clean worktree so approving any
// other gate the certifying step raises changes nothing.
func discardValidationResidue(sctx *pipeline.StepContext, name types.StepName) error {
	dirty, err := git.HasUncommittedChanges(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("inspect worktree before discarding %s residue: %w", name, err)
	}
	if !dirty {
		return nil
	}
	modified, untracked, err := worktreeResidue(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "reset", "--hard", "HEAD"); err != nil {
		return fmt.Errorf("restore tracked files after %s: %w", name, err)
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "clean", "-fd"); err != nil {
		return fmt.Errorf("remove untracked files after %s: %w", name, err)
	}
	sctx.Log(fmt.Sprintf("discarded %s residue: restored %d tracked and removed %d untracked file(s); the existing certification stands",
		name, len(modified), len(untracked)))
	return nil
}

func extractCommitSummary(result *agent.Result) (string, error) {
	var summary commitSummary
	if result.Output == nil {
		return "", fmt.Errorf("agent returned no structured summary")
	}
	if !utf8.Valid(result.Output) {
		return "", fmt.Errorf("%w: agent output must contain valid UTF-8", errRejectedCommitSummary)
	}
	if err := json.Unmarshal(result.Output, &summary); err != nil {
		return "", fmt.Errorf("parse commit summary: %w", err)
	}
	if len(summary.Summary) > config.MaxFixMessageSummaryBytes {
		return "", fmt.Errorf("%w: commit summary must not exceed %d bytes", errRejectedCommitSummary, config.MaxFixMessageSummaryBytes)
	}
	cleaned := strings.Join(strings.Fields(summary.Summary), " ")
	cleaned = strings.Trim(cleaned, " \t\r\n\"'.;:,-")
	return cleaned, nil
}

// executeFixMode runs the fix agent and commits any resulting changes. It
// returns the agent's one-line fix summary (empty when the agent returned
// nothing parseable), which the caller should place on StepOutcome.FixSummary
// so the executor can persist it on the round record.
func executeFixMode(sctx *pipeline.StepContext, stepName types.StepName, opts fixExecutionOptions) (string, error) {
	if !sctx.Fixing {
		return "", nil
	}
	if opts.RequirePreviousFindings && sctx.PreviousFindings == "" {
		return "", errors.New(opts.MissingFindingsError)
	}
	if opts.LogMessage != "" {
		sctx.Log(opts.LogMessage)
	}
	purpose := opts.Purpose
	if purpose == "" {
		purpose = string(stepName) + "-fix"
	}
	runOpts := agent.RunOpts{
		Prompt:     opts.Prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    purpose,
		Workload:   opts.Workload,
	}
	agentCtx := sctx.Ctx
	if opts.AgentContext != nil {
		agentCtx = opts.AgentContext
	}
	result, err := sctx.RunAgentSessionContext(agentCtx, opts.SessionRole, runOpts)
	if err != nil {
		return "", fmt.Errorf("%s: %w", opts.ErrorPrefix, err)
	}
	if opts.AfterAgentRun != nil {
		if err := opts.AfterAgentRun(result); err != nil {
			return "", err
		}
	}
	summary, err := extractCommitSummary(result)
	if err != nil {
		if errors.Is(err, errRejectedCommitSummary) {
			return "", fmt.Errorf("validate %s fix summary: %w", stepName, err)
		}
		sctx.Log(fmt.Sprintf("warning: could not parse fix summary: %v", err))
	}
	if err := commitAgentFixes(sctx, stepName, summary, opts.FallbackSummary); err != nil {
		return "", err
	}
	return summary, nil
}
