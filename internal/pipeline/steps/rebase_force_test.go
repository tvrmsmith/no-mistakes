package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestRebaseStep_ForcePushSkipsOriginBranch(t *testing.T) {
	t.Parallel()
	// Simulate the scenario that caused the bug: user force-pushes a commit,
	// but origin/<branch> on the upstream remote has autofix commits from a
	// prior pipeline run. The rebase step should skip origin/<branch> entirely
	// and only rebase onto origin/main.

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	writeFile(t, filepath.Join(dir, "app.txt"), "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	gitCmd(t, dir, "push", "origin", "main")

	// Advance main with another commit
	writeFile(t, filepath.Join(dir, "app.txt"), "base\nmain-update\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main update")
	gitCmd(t, dir, "push", "origin", "main")

	// Create feature branch with user's original commit
	gitCmd(t, dir, "checkout", "-b", "feature")
	writeFile(t, filepath.Join(dir, "feature.txt"), "user-change\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "user commit")
	userCommitSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// Simulate a prior pipeline run that added autofix commits on top and pushed
	writeFile(t, filepath.Join(dir, "feature.txt"), "autofix-overwrote-user\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(review): autofix commit")
	autofixSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// User force-pushes back to their original commit
	gitCmd(t, dir, "reset", "--hard", userCommitSHA)

	// baseSHA = autofixSHA (what the gate had before force push)
	// headSHA = userCommitSHA (what the user force-pushed)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, autofixSHA, userCommitSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval for clean rebase")
	}

	// After rebase, HEAD should contain the user's feature.txt content, not autofix
	content, err := os.ReadFile(filepath.Join(dir, "feature.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ReplaceAll(string(content), "\r\n", "\n"); got != "user-change\n" {
		t.Fatalf("feature.txt = %q, want %q; force push was not respected", got, "user-change\n")
	}

	// Should be rebased onto origin/main
	mergeBase := gitCmd(t, dir, "merge-base", "HEAD", "origin/main")
	originMain := gitCmd(t, dir, "rev-parse", "origin/main")
	if mergeBase != originMain {
		t.Fatalf("merge-base = %s, want origin/main %s", mergeBase, originMain)
	}
}

func TestRebaseStep_ForcePushOnDefaultBranchSkipsRemoteSync(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)

	writeFile(t, filepath.Join(dir, "app.txt"), "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	gitCmd(t, dir, "push", "origin", "main")

	writeFile(t, filepath.Join(dir, "app.txt"), "base\nuser-change\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "user commit")
	userCommitSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	writeFile(t, filepath.Join(dir, "app.txt"), "base\nautofix\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(review): autofix commit")
	autofixSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "reset", "--hard", userCommitSHA)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, autofixSHA, userCommitSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/main"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval for clean default-branch force push")
	}

	content, err := os.ReadFile(filepath.Join(dir, "app.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ReplaceAll(string(content), "\r\n", "\n"); got != "base\nuser-change\n" {
		t.Fatalf("app.txt = %q, want %q; default branch force push was not respected", got, "base\nuser-change\n")
	}

	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != userCommitSHA {
		t.Fatalf("HEAD = %s, want %s", got, userCommitSHA)
	}
}

func TestRebaseStep_ForcePushOnDefaultBranchAllowsRewrittenRemoteHead(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)

	writeFile(t, filepath.Join(dir, "app.txt"), "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	gitCmd(t, dir, "push", "origin", "main")

	writeFile(t, filepath.Join(dir, "app.txt"), "base\nuser-change\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "user commit")
	userCommitSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	writeFile(t, filepath.Join(dir, "app.txt"), "base\nautofix\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(review): autofix commit")
	autofixSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "reset", "--hard", userCommitSHA)
	gitCmd(t, dir, "push", "--force", "origin", "main")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, autofixSHA, userCommitSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/main"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval when origin/main matches forced HEAD")
	}

	if got := gitCmd(t, dir, "rev-parse", "origin/main"); got != userCommitSHA {
		t.Fatalf("origin/main = %s, want %s", got, userCommitSHA)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != userCommitSHA {
		t.Fatalf("HEAD = %s, want %s", got, userCommitSHA)
	}
	if autofixSHA == userCommitSHA {
		t.Fatal("expected distinct rewritten and previous tips")
	}
}

func TestRebaseStep_ForcePushOnDefaultBranchStopsWhenRemoteAdvanced(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)

	writeFile(t, filepath.Join(dir, "app.txt"), "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	gitCmd(t, dir, "push", "origin", "main")

	writeFile(t, filepath.Join(dir, "user.txt"), "user-change\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "user commit")
	userCommitSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	writeFile(t, filepath.Join(dir, "autofix.txt"), "autofix\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(review): autofix commit")
	autofixSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "reset", "--hard", userCommitSHA)

	other := t.TempDir()
	gitCmd(t, other, "clone", upstream, ".")
	gitCmd(t, other, "config", "user.name", "test")
	gitCmd(t, other, "config", "user.email", "test@test.com")
	gitCmd(t, other, "checkout", "main")
	writeFile(t, filepath.Join(other, "remote.txt"), "remote update\n")
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "remote update")
	gitCmd(t, other, "push", "origin", "main")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, autofixSHA, userCommitSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/main"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval when remote default branch advanced after force push")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != userCommitSHA {
		t.Fatalf("HEAD = %s, want %s", got, userCommitSHA)
	}
	if _, err := os.Stat(filepath.Join(dir, "remote.txt")); !os.IsNotExist(err) {
		t.Fatal("expected remote update to remain unapplied")
	}
}

func TestRebaseStep_NormalPushSyncsOriginBranch(t *testing.T) {
	t.Parallel()
	// Verify that a normal (non-force) push still syncs with origin/<branch>.
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	writeFile(t, filepath.Join(dir, "app.txt"), "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	// Create feature branch with one commit
	gitCmd(t, dir, "checkout", "-b", "feature")
	writeFile(t, filepath.Join(dir, "feature.txt"), "v1\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature v1")
	gitCmd(t, dir, "push", "origin", "feature")

	// Simulate another commit on origin/feature (e.g. from a prior pipeline run)
	writeFile(t, filepath.Join(dir, "extra.txt"), "extra\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "extra commit on feature")
	gitCmd(t, dir, "push", "origin", "feature")
	originFeatureSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Go back to v1 - simulating a new push that's behind origin/feature
	// but is NOT a force push (baseSHA is ancestor of headSHA)
	gitCmd(t, dir, "reset", "--hard", "HEAD~1")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	_, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// Should have incorporated origin/feature (fast-forward or rebase)
	afterSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	// The extra.txt from origin/feature should be present
	if _, err := os.Stat(filepath.Join(dir, "extra.txt")); os.IsNotExist(err) {
		t.Fatalf("expected extra.txt from origin/feature to be present after normal push sync (HEAD=%s, origin/feature=%s)", afterSHA, originFeatureSHA)
	}
}

func TestIsForcePush_IgnoresMergeBaseLookupErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")

	writeFile(t, filepath.Join(dir, "app.txt"), "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")

	if isForcePush(context.Background(), dir, "", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef") {
		t.Fatal("expected missing base SHA lookup error to not be treated as force push")
	}
}

func TestIsForcePush_RerunAfterNormalRebaseIsNotForcePush(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)

	writeFile(t, filepath.Join(dir, "app.txt"), "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	writeFile(t, filepath.Join(dir, "feature.txt"), "v1\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature v1")
	gitCmd(t, dir, "push", "origin", "feature")

	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	writeFile(t, filepath.Join(dir, "app.txt"), "base\nmain update\n")
	gitCmd(t, dir, "checkout", "main")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main update")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "rebase", "origin/main")
	gitCmd(t, dir, "push", "origin", "feature", "--force-with-lease")

	if isForcePush(context.Background(), dir, "feature", baseSHA) {
		t.Fatal("expected rerun after normal rebase to not be treated as force push")
	}
}

func TestIsForcePush_RerunWithoutLocalRemoteRefIsNotForcePush(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	originRepo := t.TempDir()
	gitCmd(t, originRepo, "init")
	gitCmd(t, originRepo, "config", "user.name", "test")
	gitCmd(t, originRepo, "config", "user.email", "test@test.com")
	gitCmd(t, originRepo, "checkout", "-b", "main")
	gitCmd(t, originRepo, "remote", "add", "origin", upstream)

	writeFile(t, filepath.Join(originRepo, "app.txt"), "base\n")
	gitCmd(t, originRepo, "add", "-A")
	gitCmd(t, originRepo, "commit", "-m", "base commit")
	gitCmd(t, originRepo, "push", "origin", "main")

	gitCmd(t, originRepo, "checkout", "-b", "feature")
	writeFile(t, filepath.Join(originRepo, "feature.txt"), "v1\n")
	gitCmd(t, originRepo, "add", "-A")
	gitCmd(t, originRepo, "commit", "-m", "feature v1")
	gitCmd(t, originRepo, "push", "origin", "feature")
	baseSHA := gitCmd(t, originRepo, "rev-parse", "HEAD")

	gitCmd(t, originRepo, "checkout", "main")
	writeFile(t, filepath.Join(originRepo, "app.txt"), "base\nmain update\n")
	gitCmd(t, originRepo, "add", "-A")
	gitCmd(t, originRepo, "commit", "-m", "main update")
	gitCmd(t, originRepo, "push", "origin", "main")

	gitCmd(t, originRepo, "checkout", "feature")
	gitCmd(t, originRepo, "rebase", "origin/main")
	gitCmd(t, originRepo, "push", "origin", "feature", "--force-with-lease")

	worktree := t.TempDir()
	gitCmd(t, worktree, "init")
	gitCmd(t, worktree, "config", "user.name", "test")
	gitCmd(t, worktree, "config", "user.email", "test@test.com")
	gitCmd(t, worktree, "remote", "add", "origin", upstream)
	gitCmd(t, worktree, "fetch", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main")
	gitCmd(t, worktree, "fetch", "--no-tags", "origin", "+refs/heads/feature:refs/tmp/feature")
	gitCmd(t, worktree, "checkout", "--detach", "refs/tmp/feature")
	gitCmd(t, worktree, "update-ref", "-d", "refs/tmp/feature")

	if isForcePush(context.Background(), worktree, "feature", baseSHA) {
		t.Fatal("expected rerun without local origin/feature ref to not be treated as force push")
	}
}

func TestIsForcePush_StaleLocalRemoteRefUsesAuthoritativeRemoteTip(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	originRepo := t.TempDir()
	gitCmd(t, originRepo, "init")
	gitCmd(t, originRepo, "config", "user.name", "test")
	gitCmd(t, originRepo, "config", "user.email", "test@test.com")
	gitCmd(t, originRepo, "checkout", "-b", "main")
	gitCmd(t, originRepo, "remote", "add", "origin", upstream)

	writeFile(t, filepath.Join(originRepo, "app.txt"), "base\n")
	gitCmd(t, originRepo, "add", "-A")
	gitCmd(t, originRepo, "commit", "-m", "base commit")
	gitCmd(t, originRepo, "push", "origin", "main")

	gitCmd(t, originRepo, "checkout", "-b", "feature")
	writeFile(t, filepath.Join(originRepo, "feature.txt"), "ancestor\n")
	gitCmd(t, originRepo, "add", "-A")
	gitCmd(t, originRepo, "commit", "-m", "feature ancestor")
	ancestorSHA := gitCmd(t, originRepo, "rev-parse", "HEAD")
	gitCmd(t, originRepo, "push", "origin", "feature")

	worktree := t.TempDir()
	gitCmd(t, worktree, "init")
	gitCmd(t, worktree, "config", "user.name", "test")
	gitCmd(t, worktree, "config", "user.email", "test@test.com")
	gitCmd(t, worktree, "remote", "add", "origin", upstream)
	gitCmd(t, worktree, "fetch", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main")
	gitCmd(t, worktree, "fetch", "--no-tags", "origin", "+refs/heads/feature:refs/remotes/origin/feature")
	gitCmd(t, worktree, "checkout", "--detach", ancestorSHA)

	writeFile(t, filepath.Join(originRepo, "feature.txt"), "remote tip\n")
	gitCmd(t, originRepo, "add", "-A")
	gitCmd(t, originRepo, "commit", "-m", "remote tip")
	baseSHA := gitCmd(t, originRepo, "rev-parse", "HEAD")
	gitCmd(t, originRepo, "push", "origin", "feature")
	gitCmd(t, worktree, "fetch", "--no-tags", "origin", "+refs/heads/feature:refs/tmp/base")
	gitCmd(t, worktree, "update-ref", "-d", "refs/tmp/base")

	writeFile(t, filepath.Join(worktree, "feature.txt"), "rewritten tip\n")
	gitCmd(t, worktree, "add", "-A")
	gitCmd(t, worktree, "commit", "-m", "rewritten tip")

	if !isForcePush(context.Background(), worktree, "feature", baseSHA) {
		t.Fatal("expected stale local origin/feature ref to defer to authoritative remote tip")
	}
}

func TestIsForcePush_LsRemoteFailureIsNotForcePush(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))

	writeFile(t, filepath.Join(dir, "app.txt"), "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	writeFile(t, filepath.Join(dir, "app.txt"), "rewritten\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "rewritten commit")
	gitCmd(t, dir, "reset", "--hard", "HEAD~1")

	if isForcePush(context.Background(), dir, "main", baseSHA) {
		t.Fatal("expected ls-remote failure to not be treated as force push")
	}
}

func TestIsForcePush_MissingRemoteObjectIsNotForcePush(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	originRepo := t.TempDir()
	gitCmd(t, originRepo, "init")
	gitCmd(t, originRepo, "config", "user.name", "test")
	gitCmd(t, originRepo, "config", "user.email", "test@test.com")
	gitCmd(t, originRepo, "checkout", "-b", "main")
	gitCmd(t, originRepo, "remote", "add", "origin", upstream)

	writeFile(t, filepath.Join(originRepo, "app.txt"), "base\n")
	gitCmd(t, originRepo, "add", "-A")
	gitCmd(t, originRepo, "commit", "-m", "base commit")
	gitCmd(t, originRepo, "push", "origin", "main")

	gitCmd(t, originRepo, "checkout", "-b", "feature")
	writeFile(t, filepath.Join(originRepo, "feature.txt"), "remote feature\n")
	gitCmd(t, originRepo, "add", "-A")
	gitCmd(t, originRepo, "commit", "-m", "feature commit")
	gitCmd(t, originRepo, "push", "origin", "feature")

	worktree := t.TempDir()
	gitCmd(t, worktree, "init")
	gitCmd(t, worktree, "config", "user.name", "test")
	gitCmd(t, worktree, "config", "user.email", "test@test.com")
	gitCmd(t, worktree, "remote", "add", "origin", upstream)
	gitCmd(t, worktree, "fetch", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main")
	gitCmd(t, worktree, "checkout", "--detach", "origin/main")

	writeFile(t, filepath.Join(worktree, "local.txt"), "local only\n")
	gitCmd(t, worktree, "add", "-A")
	gitCmd(t, worktree, "commit", "-m", "local commit")
	baseSHA := gitCmd(t, worktree, "rev-parse", "HEAD")
	gitCmd(t, worktree, "checkout", "--detach", "origin/main")

	if isForcePush(context.Background(), worktree, "feature", baseSHA) {
		t.Fatal("expected missing remote tip object to not be treated as force push")
	}
}
