package steps

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RepoCleanupTimeout bounds one invocation of commands.cleanup.
//
// The command releases resources a run left outside its own process tree, so it
// is legitimately slow: stopping a container stack, waiting for each container
// to exit, then removing containers and volumes takes minutes on a loaded host.
// It still has to be bounded, because both call sites are on paths that must
// finish - a pipeline step the operator is waiting on, and run teardown, which
// holds the worktree removal and everything queued behind it. Five minutes is
// well past a normal stack teardown and far short of a hang. Cancelling the
// context signals the whole process group (see shellenv.ConfigureShellCommand),
// so a wedged cleanup does not survive its own timeout.
const RepoCleanupTimeout = 5 * time.Minute

// RunRepoCleanupCommand runs the repository's trusted commands.cleanup in
// workDir and reports the output, exit code, and any launch failure or
// timeout. Callers decide what to log; nothing here fails a run.
//
// workDir is the run worktree, and it is the whole point of the cwd: a cleanup
// command typically resolves WHICH stack to tear down from the directory it
// runs in, so once the worktree is gone nothing can attribute those resources
// to this run any more. Every caller must therefore run it before removing the
// directory.
//
// env is the caller's environment overlay on the daemon process environment,
// nil for none. The push step passes stepEnvironment, so its invocation sees
// exactly what commands.test and commands.prepare see. Run teardown has no step
// context and passes nil, so that invocation inherits the daemon process
// environment alone. Neither adds a PATH entry.
func RunRepoCleanupCommand(ctx context.Context, workDir string, env []string, cmdStr string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, RepoCleanupTimeout)
	defer cancel()
	output, exitCode, err := runShellCommandWithProcessEnv(ctx, workDir, env, cmdStr)
	if err == nil && ctx.Err() != nil {
		return output, exitCode, fmt.Errorf("cleanup command killed before it finished: %w", ctx.Err())
	}
	return output, exitCode, err
}

// releaseExternalRunResources runs commands.cleanup from a pipeline step. It is
// best effort in every direction: a repository that configures no cleanup
// command, a command that exits non-zero, and a command that cannot launch all
// leave the step's own result untouched. The run-end invocation in the daemon
// is the backstop, so nothing is lost by declining to fail here.
func releaseExternalRunResources(sctx *pipeline.StepContext, logStep types.StepName) {
	cmdStr := strings.TrimSpace(sctx.Config.Commands.Cleanup)
	if cmdStr == "" {
		return
	}
	sctx.Log(fmt.Sprintf("releasing external run resources: %s", cmdStr))
	started := time.Now()
	output, exitCode, err := RunRepoCleanupCommand(sctx.Ctx, sctx.WorkDir, stepEnvironment(sctx), cmdStr)
	if output != "" {
		logCommandOutput(sctx, output, "Cleanup", logStep)
	}
	switch {
	case err != nil:
		sctx.Log(fmt.Sprintf("warning: cleanup command failed: %v", err))
	case exitCode != 0:
		sctx.Log(fmt.Sprintf("warning: cleanup command exited with code %d", exitCode))
	default:
		sctx.Log(fmt.Sprintf("external run resources released in %s", time.Since(started).Round(time.Millisecond)))
	}
}
