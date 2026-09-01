package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// WorktreeMatchesRun reports whether the run's worktree is still on disk at the
// exact commit the run parked on. Daemon recovery refuses a parked run that
// fails this check, so the lifecycle guard must read the same rule from the
// same owner rather than restate it.
//
// A read that could not be completed is reported as
// pipeline.ErrRecoveryEvidenceUnavailable rather than as adverse evidence: a
// stat that fails for anything but absence, or a git call that cannot run right
// now, says nothing about the run and must not cost it its worktree.
func WorktreeMatchesRun(ctx context.Context, p *paths.Paths, run *db.Run) error {
	if p == nil || run == nil {
		return errors.New("worktree is missing")
	}
	// A run may be placed under a configured worktree root rather than the
	// default one, so the recorded path is authoritative when it exists.
	workDir := worktrees.RecordedDir(p, run.WorktreePath(), run.RepoID, run.ID)
	info, err := os.Stat(workDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("worktree is missing")
	case err != nil:
		return fmt.Errorf("%w: stat worktree: %w", pipeline.ErrRecoveryEvidenceUnavailable, err)
	case !info.IsDir():
		return errors.New("worktree is missing")
	}
	headSHA, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		return fmt.Errorf("%w: read worktree head: %w", pipeline.ErrRecoveryEvidenceUnavailable, err)
	}
	if headSHA != run.HeadSHA {
		return errors.New("worktree head does not match run head")
	}
	return nil
}

// ResumePreconditionsMet answers the one question both the destructive
// lifecycle guard and daemon startup recovery ask about a parked run: could the
// next daemon start pick it up as it stands? Recovery calls it before resuming
// and the guard calls it before promising preservation, so an operator is never
// told a run survives a stop that the next start would terminally fail.
func ResumePreconditionsMet(ctx context.Context, database *db.DB, p *paths.Paths, run *db.Run, resumeSteps []pipeline.Step) error {
	if err := WorktreeMatchesRun(ctx, p, run); err != nil {
		return err
	}
	return pipeline.ValidateRecoveredRun(database, run, resumeSteps)
}
