package steps

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// recordCwdCommand is a cleanup command that appends the directory it ran in to
// marker. Where it runs is the load-bearing part of the contract: a real
// cleanup command resolves which stack to tear down from its working
// directory, so the pipeline must invoke it inside the run worktree.
func recordCwdCommand(marker string, exitCode int) string {
	if runtime.GOOS == "windows" {
		return "cd>>" + marker + " & exit /b " + strconv.Itoa(exitCode)
	}
	return "pwd -P >> " + marker + "; exit " + strconv.Itoa(exitCode)
}

func readCleanupMarker(t *testing.T, marker string) string {
	t.Helper()
	contents, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("cleanup command did not run: %v", err)
	}
	return strings.TrimSpace(string(contents))
}

func assertRanIn(t *testing.T, recorded, workDir string) {
	t.Helper()
	got, err := filepath.EvalSymlinks(strings.TrimSpace(recorded))
	if err != nil {
		t.Fatalf("resolve recorded cleanup directory %q: %v", recorded, err)
	}
	want, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("cleanup ran in %q, want the run worktree %q", got, want)
	}
}

// setupPushableWorktree builds the minimum a successful PushStep needs: an
// upstream bare remote, a feature branch, a gate mirror, and a recorded review
// approval for the head being pushed. It returns the step context, the run
// worktree, the upstream repository, and the branch's base commit.
func setupPushableWorktree(t *testing.T, cmds config.Commands) (*pipeline.StepContext, string, string, string) {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, head := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, head, cmds)
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	setupGateMirror(t, sctx)
	sctx.Run.HeadSHA = head
	recordReviewApproval(t, sctx, head)
	return sctx, dir, upstream, baseSHA
}

// TestPushStep_ReleasesExternalRunResourcesOnEntry pins the efficiency half of
// the cleanup hook. Nothing from push onward needs a local dev stack, and the
// ci step that follows can babysit a PR for hours, so the stack is released
// here rather than at run teardown.
func TestPushStep_ReleasesExternalRunResourcesOnEntry(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleanup.log")
	sctx, dir, upstream, _ := setupPushableWorktree(t, config.Commands{Cleanup: recordCwdCommand(marker, 0)})

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push step failed: %v", err)
	}

	assertRanIn(t, readCleanupMarker(t, marker), dir)
	if remote, local := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"), gitCmd(t, dir, "rev-parse", "HEAD"); remote != local {
		t.Fatalf("remote head = %s, want the pushed head %s", remote, local)
	}
}

// TestPushStep_FailingCleanupDoesNotFailThePush keeps the hook best-effort. A
// teardown script that exits non-zero (nothing to stop, a container already
// removed) must not cost the operator a validated push.
func TestPushStep_FailingCleanupDoesNotFailThePush(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleanup.log")
	sctx, dir, upstream, _ := setupPushableWorktree(t, config.Commands{Cleanup: recordCwdCommand(marker, 3)})

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("a failing cleanup command must not fail the push step: %v", err)
	}

	assertRanIn(t, readCleanupMarker(t, marker), dir)
	if remote, local := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"), gitCmd(t, dir, "rev-parse", "HEAD"); remote != local {
		t.Fatalf("remote head = %s, want the pushed head %s", remote, local)
	}
}

// TestPushStep_RunsCleanupBeforeItsOwnEntryGuards proves the call sits at
// entry: the head-continuity guard refuses this push, and the stack is still
// released. A cleanup that only ran on the success path would strand it.
func TestPushStep_RunsCleanupBeforeItsOwnEntryGuards(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleanup.log")
	sctx, dir, _, baseSHA := setupPushableWorktree(t, config.Commands{Cleanup: recordCwdCommand(marker, 0)})

	// Replace the reviewed head with a divergent commit and no pipeline commit
	// after it, which the push step refuses before touching the remote.
	gitCmd(t, dir, "reset", "--hard", baseSHA)
	if err := os.WriteFile(filepath.Join(dir, "unreviewed.txt"), []byte("out of band\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "out-of-band replacement")
	sctx.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")

	if _, err := (&PushStep{}).Execute(sctx); err == nil {
		t.Fatal("expected the push step to refuse a divergent head")
	}

	assertRanIn(t, readCleanupMarker(t, marker), dir)
}

// TestReleaseExternalRunResources_NoCommandConfiguredIsANoOp keeps the common
// case free: a repository that configures no cleanup command logs nothing and
// launches no shell.
func TestReleaseExternalRunResources_NoCommandConfiguredIsANoOp(t *testing.T) {
	dir, baseSHA, head := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, head, config.Commands{})
	var logged []string
	sctx.Log = func(line string) { logged = append(logged, line) }

	releaseExternalRunResources(sctx, (&PushStep{}).Name())

	if len(logged) != 0 {
		t.Fatalf("unconfigured cleanup logged %v", logged)
	}
}
