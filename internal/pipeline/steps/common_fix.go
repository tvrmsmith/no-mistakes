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
// A round that records a review-approved head does not commit at its exit. The
// step that certifies must not modify the tree it certifies, so it parks over
// the leftovers instead (residueGateOutcome).
func runValidationStep(
	sctx *pipeline.StepContext,
	name types.StepName,
	inner func(*pipeline.StepContext) (*pipeline.StepOutcome, error),
) (*pipeline.StepOutcome, error) {
	headBefore := sctx.Run.HeadSHA
	agentsBefore := sctx.AgentInvocations()
	sctx.Shared.ClearValidationResidue(name)

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
		// A round that records a review-approved head does not commit at its
		// exit; it parks over the residue instead. See residueGateOutcome and
		// pipeline.ApprovalResidueDiscarder.
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
		// The boundary step cannot restart into itself, and it has no stale
		// certification to revoke either. A Review fix round does reach here
		// having committed and certified, but review.go captures
		// reviewTargetSHA from sctx.Run.HeadSHA AFTER commitAgentFixes has
		// run, so the SHA it records is the head that commit produced and can
		// never be an ancestor of it. Moving that capture above the fix-mode
		// commit is what would break this.
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
	// stop restarting on it unasked. The comparison is per-process and
	// remembers only that one tree, so it narrows the loop rather than bounding
	// it - runs.restart_count and the soft-cap warning are what make a
	// divergent loop visible.
	if tree == sctx.Shared.LastRestartTree(name) {
		return churnGateOutcome(sctx, name, tree, outcome)
	}
	sctx.Shared.SetLastRestartTree(name, tree)

	// prepareRestart owns revoking review authority and warning above the soft
	// cap; doing either here would write state the executor may then reject,
	// and would report a restart count that never existed.
	outcome.RestartFrom = pipeline.RestartBoundary
	sctx.Log(fmt.Sprintf("%s made an agent-authored commit; re-entering validation from %s", name, pipeline.RestartBoundary))
	return outcome, nil
}

// churnGateOutcome parks a step that keeps producing the tree its own last
// restart already produced.
//
// Walking forward instead is the trap this replaces. The commit moved the head
// past whatever review certified, so continuing either ships an unjudged tree
// or, if the step revokes the certification on its way past, dead-ends at
// push - which fails with "run has no durably recorded review-approved head",
// naming nothing about churn and leaving the operator to guess. So the run
// holds here and says plainly which step is churning and on which tree.
//
// The gate has the same three answers every gate has, and they read as: ship
// it anyway (approve, the existing certification stands and the run walks on),
// try one more re-check (fix, the step runs again and a genuinely different
// tree restarts normally), or abort. The finding is ask-user rather than the
// residue gate's no-op so that an unattended --yes run answers fix and buys one
// more re-check instead of shipping the repeated tree on sight. It does not
// stop such a run: gateResolution (internal/cli/axi_drive.go) converges rather
// than fixing until clean, so it approves a gate raised inside a fix round, or
// one whose step it already answered with fix once. A churn park therefore
// survives at most one extra round unattended and is then approved.
//
// The round's auto-fix eligibility is withdrawn with it. The executor's
// auto-fix branch runs before the approval park, so a round left AutoFixable
// spends a fix round and discards this warning with it, and the operator never
// sees the gate. A step that already produced the same tree twice is by
// definition not something another automatic round should attempt.
func churnGateOutcome(sctx *pipeline.StepContext, name types.StepName, tree string, outcome *pipeline.StepOutcome) (*pipeline.StepOutcome, error) {
	sctx.Log(fmt.Sprintf("%s committed tree %s, the same tree its last restart produced; parking instead of restarting again", name, tree))
	findings, err := parseStepFindings(sctx, name, outcome.Findings)
	if err != nil {
		return nil, err
	}
	findings.Items = append(findings.Items, Finding{
		ID:       "restart-churn-1",
		Severity: "warning",
		Action:   types.ActionAskUser,
		Description: fmt.Sprintf("%s committed tree %s again, the same tree its previous restart produced, so re-entering validation would repeat a round that already changed nothing; approve to ship it anyway under the review that stands, fix to run %s once more, or abort",
			name, tree, name),
	})
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		return nil, fmt.Errorf("render %s churn findings: %w", name, err)
	}
	outcome.AutoFixable = false
	outcome.NeedsApproval = true
	outcome.Findings = string(findingsJSON)
	return outcome, nil
}

// parseStepFindings decodes a round's own verdict so a gate can append to it.
// The JSON is produced inside this package, so a decode failure is a bug here
// rather than untrusted input, and swallowing it would drop the round's real
// findings with no signal at all.
func parseStepFindings(sctx *pipeline.StepContext, name types.StepName, raw string) (Findings, error) {
	var findings Findings
	if raw == "" {
		return findings, nil
	}
	if err := json.Unmarshal([]byte(raw), &findings); err != nil {
		sctx.Log(fmt.Sprintf("%s produced findings that could not be parsed; failing rather than reporting a gate without them", name))
		return findings, fmt.Errorf("parse %s findings: %w", name, err)
	}
	return findings, nil
}

// residueGateOutcome parks a round that recorded a review-approved head over
// work it refused to commit. It fires on the recorded head rather than on the
// executor's later decision to publish it, because whether that round ends up
// certifying is not knowable here and committing on the wrong guess is the one
// mistake that cannot be walked back.
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
//
// The round's auto-fix eligibility is withdrawn here too, and not because an
// auto-fix round would reach a bad tree: it would commit the residue through
// executeFixMode and re-certify the head that results, which is what the
// gate's own fix answer does. It is withdrawn because that happens with nobody
// told. Making a certifying step that left files behind visible to a human is
// the whole purpose of this gate, and an unattended round that quietly tidies
// up and re-certifies is exactly the outcome it exists to prevent.
func residueGateOutcome(sctx *pipeline.StepContext, name types.StepName, outcome *pipeline.StepOutcome) (*pipeline.StepOutcome, error) {
	modified, untracked, err := worktreeResidue(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("inspect worktree residue after %s: %w", name, err)
	}
	findings, err := parseStepFindings(sctx, name, outcome.Findings)
	if err != nil {
		return nil, err
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
	sctx.Shared.SetValidationResidue(name, pipeline.ValidationResidue{Modified: modified, Untracked: untracked})
	sctx.Log(fmt.Sprintf("%s examined %s but left these files uncommitted; parking instead of committing over its own certification: modified %s, untracked %s",
		name, sctx.Run.HeadSHA, describePaths(modified), describePaths(untracked)))
	outcome.AutoFixable = false
	outcome.NeedsApproval = true
	outcome.Findings = string(findingsJSON)
	return outcome, nil
}

// worktreeResidue splits an unclean worktree into tracked files that differ
// from HEAD and untracked non-ignored files. Gitignored build output is in
// neither list: it is not residue anyone needs to rule on, and discard leaves
// it alone.
//
// The listing is NUL-separated because core.quotePath C-quotes any path with a
// non-ASCII, control, quote, or backslash byte, and that quoted token is both
// what an operator reads at the gate and what discard hands back to git as a
// pathspec. Splitting on newlines and trimming spaces also corrupts a path that
// legitimately holds either.
//
// --no-renames because rename detection collapses a staged rename into the new
// path alone, and discard restoring only that path leaves the staged deletion
// of the old one behind. assertResidueGone would still pass, since every path
// the park recorded really is gone, and a deletion nobody ruled on would ride
// into the next validation step's exit commit.
func worktreeResidue(ctx context.Context, workDir string) (modified, untracked []string, err error) {
	out, err := git.RunRaw(ctx, workDir, "diff", "--name-only", "-z", "--no-renames", "HEAD")
	if err != nil {
		return nil, nil, fmt.Errorf("list modified tracked files: %w", err)
	}
	for _, path := range strings.Split(string(out), "\x00") {
		if path != "" {
			modified = append(modified, path)
		}
	}
	untracked, err = git.UntrackedFiles(ctx, workDir)
	if err != nil {
		return nil, nil, fmt.Errorf("list untracked files: %w", err)
	}
	return modified, untracked, nil
}

// discardValidationResidue removes exactly the paths the residue park recorded:
// tracked ones are restored from HEAD, untracked ones are deleted, and
// gitignored output is left alone.
//
// Scoping to the recorded paths is the point. A run can sit parked for hours,
// and a human or a driving agent editing the worktree in that window has done
// work no gate asked to throw away; a reset --hard plus clean -fd over whatever
// the tree happens to hold at approval time would destroy it. So this touches
// the enumerated paths and nothing else, and a gate that recorded no residue
// leaves the worktree untouched.
//
// The list comes from what the step itself read out of git when it raised the
// gate, held on RunShared. It used to be re-derived from the parked findings,
// which handed the choice of files to destroy to the review agent, since a
// finding ID is whatever the agent wrote.
func discardValidationResidue(sctx *pipeline.StepContext, name types.StepName) error {
	residue, ok := sctx.Shared.ValidationResidue(name)
	if !ok {
		return nil
	}
	modified, untracked := residue.Modified, residue.Untracked
	if len(modified) == 0 && len(untracked) == 0 {
		sctx.Shared.ClearValidationResidue(name)
		return nil
	}
	if len(modified) > 0 {
		// git restore rather than git checkout HEAD --: the tracked list comes
		// from diff against HEAD, which includes a staged-added path HEAD does
		// not contain, and checkout aborts the whole invocation on that
		// unmatched pathspec without restoring anything. restore resets index
		// and worktree to HEAD, deleting such a path instead of failing.
		args := append([]string{"restore", "--source=HEAD", "--staged", "--worktree", "--"}, literalPathspecs(modified)...)
		if _, err := git.Run(sctx.Ctx, sctx.WorkDir, args...); err != nil {
			return fmt.Errorf("restore tracked files after %s: %w", name, err)
		}
	}
	if len(untracked) > 0 {
		// -ffd, not -f: git.UntrackedFiles lists with --untracked-files=all, so
		// an ordinary untracked directory arrives already expanded into its
		// individual files. The one entry that stays a directory is an embedded
		// repository, which needs -d to be treated as a directory at all and
		// the second -f to be removed rather than skipped; plain clean exits 0
		// having removed it neither way. The force flags stay harmless because
		// the pathspec is the recorded list, so nothing outside it is
		// reachable.
		args := append([]string{"clean", "-ffd", "--"}, literalPathspecs(untracked)...)
		if _, err := git.Run(sctx.Ctx, sctx.WorkDir, args...); err != nil {
			return fmt.Errorf("remove untracked files after %s: %w", name, err)
		}
	}
	if err := assertResidueGone(sctx, name, modified, untracked); err != nil {
		return err
	}
	sctx.Shared.ClearValidationResidue(name)
	sctx.Log(fmt.Sprintf("discarded %s residue; the existing certification stands: restored %s, removed %s",
		name, describePaths(modified), describePaths(untracked)))
	return nil
}

// literalPathspecs wraps each recorded path so git matches it as a name rather
// than a pattern. A residue path is a filename git already handed us, never a
// pattern anyone wrote, so the glob characters in it are part of the name: an
// unwrapped notes*.txt would make clean delete an unrecorded notes1.txt a human
// created while the run sat parked, and a path beginning with a colon would
// read as pathspec magic and never be discarded at all. git clean has no
// --pathspec-from-file, so the magic prefix is the portable way to say this to
// both commands.
func literalPathspecs(paths []string) []string {
	specs := make([]string, len(paths))
	for i, path := range paths {
		specs[i] = ":(literal)" + path
	}
	return specs
}

// assertResidueGone re-reads the worktree and fails when a path the park
// recorded is still there. Every git command above can report success while
// removing nothing, and a silent no-op here is the worst outcome the gate has:
// the executor completes the step, and the survivor rides into a later
// validation step's exit commit that no certification ever judged.
func assertResidueGone(sctx *pipeline.StepContext, name types.StepName, modified, untracked []string) error {
	stillModified, stillUntracked, err := worktreeResidue(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("re-read worktree after discarding %s residue: %w", name, err)
	}
	present := make(map[string]bool, len(stillModified)+len(stillUntracked))
	for _, path := range stillModified {
		present[path] = true
	}
	for _, path := range stillUntracked {
		present[path] = true
	}
	var survivors []string
	for _, path := range append(append([]string{}, modified...), untracked...) {
		if present[path] {
			survivors = append(survivors, path)
		}
	}
	if len(survivors) > 0 {
		return fmt.Errorf("discarding %s residue left %s in the worktree", name, describePaths(survivors))
	}
	return nil
}

// describePaths renders a residue path list for a log line. Naming the files is
// the whole value of the line: a count tells an operator that something was
// discarded without telling them what.
func describePaths(paths []string) string {
	if len(paths) == 0 {
		return "none"
	}
	return strings.Join(paths, ", ")
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
