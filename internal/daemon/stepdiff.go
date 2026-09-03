package daemon

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/git"
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
// own certification - is served the working-tree diff. Every validation step
// otherwise commits its own output at its exit, so by the time its gate is
// observable the worktree is clean and the working-tree diff is empty; those
// gates are served HEAD~1..HEAD, the commit the step just made. Without that
// fallback the Test step's evidence round, whose new test files a human at the
// gate reads, would show nothing at all.
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
		// The step committed its own output at its exit. Show that commit
		// rather than an empty gate. A root commit has no HEAD~1, in which case
		// there is genuinely nothing to show and the empty diff stands.
		if committed, commitErr := git.Diff(ctx, workDir, "HEAD~1", "HEAD"); commitErr == nil {
			diff = committed
		}
	}
	if len(diff) > maxStepDiffBytes {
		return diff[:maxStepDiffBytes], true, nil
	}
	return diff, false, nil
}
