package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The fix-review diff is the one piece of gate context that is not persisted
// anywhere, so it cannot be reconstructed from get_run. It is therefore served
// on demand from the run's worktree instead of riding the event stream, where
// a large diff would exceed the frame limit and take the whole subscription
// down with it.

func stepDiffFixture(t *testing.T, contents string) (*RunManager, string) {
	t.Helper()
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	repo, err := database.InsertRepoWithID("testrepo", filepath.Join(root, "clone"), "https://example.test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}

	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, worktree, "init")
	runGit(t, worktree, "config", "user.email", "test@example.com")
	runGit(t, worktree, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(worktree, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, worktree, "add", "tracked.txt")
	runGit(t, worktree, "commit", "-m", "base")
	if err := os.WriteFile(filepath.Join(worktree, "tracked.txt"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewRunManager(database, p, nil)
	return m, run.ID
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestStepDiff_ReturnsTheWorktreeDiffOnDemand(t *testing.T) {
	m, runID := stepDiffFixture(t, "agent fix\n")

	diff, truncated, err := m.StepDiff(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("small diff reported as truncated")
	}
	if !strings.Contains(diff, "tracked.txt") || !strings.Contains(diff, "agent fix") {
		t.Fatalf("diff did not describe the change:\n%s", diff)
	}
}

// A diff larger than the response budget is cut rather than returned whole.
// An oversized response would blow the transport frame limit, which is exactly
// the failure this RPC exists to avoid.
func TestStepDiff_BoundsAnOversizedDiff(t *testing.T) {
	huge := strings.Repeat("a very long changed line that repeats\n", 60_000)
	m, runID := stepDiffFixture(t, huge)

	diff, truncated, err := m.StepDiff(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("oversized diff was not reported as truncated")
	}
	if len(diff) > maxStepDiffBytes {
		t.Fatalf("diff length = %d, want <= %d", len(diff), maxStepDiffBytes)
	}
	if !strings.Contains(diff, "tracked.txt") {
		t.Fatalf("truncated diff lost its leading context:\n%s", diff[:200])
	}
}

// stepDiffWorktree resolves the run's worktree directory.
func stepDiffWorktree(t *testing.T, m *RunManager, runID string) string {
	t.Helper()
	run, err := m.db.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	return m.paths.WorktreeDir(repo.ID, run.ID)
}

// parkStepAtGate puts a step at an approval gate with one round that started on
// startingHead, which is the shape StepDiff reads to decide whether the round
// committed anything.
func parkStepAtGate(t *testing.T, m *RunManager, runID string, name types.StepName, startingHead string) {
	t.Helper()
	result, err := m.db.InsertStepResult(runID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.db.StartStep(result.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[]}`
	if _, err := m.db.InsertStepRoundWithStartingHead(result.ID, 1, "initial", &findings, nil, startingHead, 10); err != nil {
		t.Fatal(err)
	}
	if err := m.db.UpdateStepStatusWithDuration(result.ID, types.StepStatusAwaitingApproval, 10); err != nil {
		t.Fatal(err)
	}
}

func headSHA(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// A validation step commits its own output at its exit, so by the time its
// gate is observable the worktree is clean and the working-tree diff is empty.
// The gate must still show what the step did: without the exit-commit fallback
// a reviewer reads an empty diff and has nothing to rule on.
func TestStepDiff_ShowsTheExitCommitWhenTheWorktreeIsClean(t *testing.T) {
	m, runID := stepDiffFixture(t, "agent fix\n")
	worktree := stepDiffWorktree(t, m, runID)
	startingHead := headSHA(t, worktree)
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-m", "step exit commit")
	parkStepAtGate(t, m, runID, types.StepDocument, startingHead)

	diff, truncated, err := m.StepDiff(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("small diff reported as truncated")
	}
	if !strings.Contains(diff, "tracked.txt") || !strings.Contains(diff, "agent fix") {
		t.Fatalf("clean worktree served no exit-commit diff:\n%s", diff)
	}
}

// The reported defect: a gate whose step committed nothing was served the
// PREVIOUS step's commit as "what the parked step changed". A configured test
// command that exits nonzero parks the Test step with an untouched worktree, so
// the honest answer is an empty diff.
func TestStepDiff_StepThatCommittedNothingGetsNoDiff(t *testing.T) {
	m, runID := stepDiffFixture(t, "agent fix\n")
	worktree := stepDiffWorktree(t, m, runID)

	// An earlier step's commit, already ruled on, sitting at HEAD~1..HEAD.
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-m", "an earlier step's commit")
	// The parked step then runs and changes nothing.
	parkStepAtGate(t, m, runID, types.StepTest, headSHA(t, worktree))

	diff, truncated, err := m.StepDiff(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("empty diff reported as truncated")
	}
	if diff != "" {
		t.Fatalf("diff = %q, want empty: the parked step committed nothing", diff)
	}
}

// A step that did not author the commits under it must not be handed the
// range as "what the parked step changed". The rebase step is the reachable
// case: it replays the branch onto upstream, aborts a later conflicting
// target, and parks with a clean worktree, so the range from its round's
// starting head is every upstream commit the successful part picked up.
func TestStepDiff_NonValidationStepGetsNoCommitRange(t *testing.T) {
	m, runID := stepDiffFixture(t, "upstream work\n")
	worktree := stepDiffWorktree(t, m, runID)
	startingHead := headSHA(t, worktree)
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-m", "commit the rebase picked up")
	parkStepAtGate(t, m, runID, types.StepRebase, startingHead)

	diff, truncated, err := m.StepDiff(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("empty diff reported as truncated")
	}
	if diff != "" {
		t.Fatalf("diff = %q, want empty: the rebase step did not author that commit", diff)
	}
}

func TestStepDiff_UnknownRunFailsClosed(t *testing.T) {
	m, _ := stepDiffFixture(t, "agent fix\n")
	if _, _, err := m.StepDiff(context.Background(), "01NOSUCHRUN"); err == nil {
		t.Fatal("expected an error for an unknown run")
	}
}

// The fix-review gate depends on this RPC, and where the run's worktree is is
// recorded on the run - so an unrelated fault in the global config must not take
// the diff down with it. An operator who mistypes YAML while a run is parked
// would otherwise lose the gate's diff for a reason that has nothing to do with
// the run.
func TestStepDiff_ServesTheDiffWhileTheGlobalConfigIsUnreadable(t *testing.T) {
	m, runID := stepDiffFixture(t, "agent fix\n")
	if err := os.WriteFile(m.paths.ConfigFile(), []byte("worktree_roots: [not, a, mapping\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, truncated, err := m.StepDiff(context.Background(), runID)
	if err != nil {
		t.Fatalf("step diff with an unreadable global config: %v", err)
	}
	if truncated || !strings.Contains(diff, "agent fix") {
		t.Fatalf("diff = %q (truncated=%v), want the worktree's change", diff, truncated)
	}
}
