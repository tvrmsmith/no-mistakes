package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// maxStepDiffBytes bounds one fix-review diff response. The IPC transport
// frames a whole JSON document per line and caps a line at 1 MiB, so an
// unbounded diff would not merely be large - it would break the connection
// carrying it. Half the frame budget leaves ample room for the rest of the
// response and keeps a pathological change a partial view rather than a
// transport failure.
const maxStepDiffBytes = 512 * 1024

// StepDiff returns the diff of what the parked step changed, derived on demand
// from the run's worktree.
//
// Which diff that is depends on where the step left its work. A step parked
// over an unclean worktree - the certifying step refusing to commit over its
// own certification - is served the working-tree diff. A validation step that
// commits its own output at its exit instead leaves a clean worktree and an
// empty working-tree diff by the time its gate is observable; that gate is
// served the range from the head its round started on to the current head, the
// work the round actually committed. Without it the Test step's evidence
// round, whose new test files a human at the gate reads, would show nothing at
// all.
//
// The commit range is served only on proof that this round advanced the head,
// read from the parked round's recorded starting head. A gate whose step
// committed nothing - the Test step parking because a configured test command
// exited nonzero, or CI, PR, and push gates, none of which commit at all - gets
// the empty diff it earned. Guessing HEAD~1..HEAD there would present the
// PREVIOUS step's commit as what this step changed, which is worse than showing
// nothing.
//
// This diff is the only piece of gate context the pipeline never persists, so
// it is the one thing a subscriber cannot rebuild from get_run. Serving it
// here rather than attaching it to a step_completed event keeps the largest
// possible payload off the event stream, where a frame over the transport
// limit would kill the subscription and hide every event after it.
//
// It is read-only, fails closed on an unknown run or repo, and never persists
// source text.
func (m *RunManager) StepDiff(ctx context.Context, runID string) (string, bool, error) {
	run, err := m.db.GetRun(runID)
	if err != nil {
		return "", false, fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return "", false, fmt.Errorf("run not found: %s", runID)
	}
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil {
		return "", false, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return "", false, fmt.Errorf("repo not found for run %s", runID)
	}

	workDir := worktrees.RecordedDir(m.paths, run.WorktreePath(), repo.ID, run.ID)
	diff, err := git.DiffHead(ctx, workDir)
	if err != nil {
		return "", false, fmt.Errorf("diff worktree: %w", err)
	}
	if diff == "" {
		if start := m.parkedRoundStartingHead(runID); start != "" {
			head, headErr := git.HeadSHA(ctx, workDir)
			if headErr != nil {
				slog.Warn("could not resolve head for step diff", "run", runID, "error", headErr)
			} else if head != start {
				committed, commitErr := git.Diff(ctx, workDir, start, head)
				if commitErr != nil {
					slog.Warn("could not diff the parked step's exit commit", "run", runID, "from", start, "to", head, "error", commitErr)
				} else {
					diff = committed
				}
			}
		}
	}
	if len(diff) > maxStepDiffBytes {
		return diff[:maxStepDiffBytes], true, nil
	}
	return diff, false, nil
}

// parkedRoundStartingHead returns the head the run's parked step began its
// latest round on, or "" when no step is parked, when the round predates the
// column, or when the lookup fails. Every one of those answers means "no proof
// this step committed anything", so the caller keeps the empty diff rather than
// showing a commit some earlier step made.
func (m *RunManager) parkedRoundStartingHead(runID string) string {
	steps, err := m.db.GetStepsByRun(runID)
	if err != nil {
		slog.Warn("could not read steps for step diff", "run", runID, "error", err)
		return ""
	}
	for _, step := range steps {
		if step.Status != types.StepStatusAwaitingApproval && step.Status != types.StepStatusFixReview {
			continue
		}
		rounds, err := m.db.GetRoundsByStep(step.ID)
		if err != nil {
			slog.Warn("could not read rounds for step diff", "run", runID, "step", step.StepName, "error", err)
			return ""
		}
		if len(rounds) == 0 {
			return ""
		}
		if start := rounds[len(rounds)-1].StartingHeadSHA; start != nil {
			return *start
		}
		return ""
	}
	return ""
}
