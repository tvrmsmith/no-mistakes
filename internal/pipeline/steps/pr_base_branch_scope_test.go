package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// newIntegrationBranchRepo builds the shape that exposed issue #39: an
// integration branch ("develop") carrying a commit the default branch lacks,
// and a feature branch forked from it, pushed as a new branch so the run's
// BaseSHA is the zero ref. Only services/api/main.go belongs to the feature;
// services/web/main.go changed on the integration branch alone.
func newIntegrationBranchRepo(t *testing.T) (dir, developSHA, headSHA string) {
	t.Helper()
	dir, baseSHA := newUnitRepo(t)
	gitCmd(t, dir, "checkout", "-b", "develop", baseSHA)
	developSHA = changeUnitFile(t, dir, "services/web/main.go")
	gitCmd(t, dir, "checkout", "-B", "feature", "develop")
	headSHA = changeUnitFile(t, dir, "services/api/main.go")
	return dir, developSHA, headSHA
}

const zeroSHA = "0000000000000000000000000000000000000000"

func TestTestStep_ChangedFilesFollowTheConfiguredPRBaseBranch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command reads the variables through POSIX shell interpolation")
	}
	t.Parallel()
	dir, developSHA, headSHA := newIntegrationBranchRepo(t)
	outFile := filepath.Join(t.TempDir(), "env.out")

	units := []config.TestUnit{{
		Name:    "repo",
		Path:    ".",
		Command: `printf '%s\n' "$NO_MISTAKES_BASE_SHA" "$NO_MISTAKES_CHANGED_FILES" > ` + outFile,
	}}
	sctx := unitTestContext(t, nil, dir, zeroSHA, headSHA, units)
	sctx.Config.PR.BaseBranch = "develop"

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	want := []string{developSHA, "services/api/main.go"}
	if !slices.Equal(lines, want) {
		t.Fatalf("base SHA and changed files = %q, want %q", lines, want)
	}
}

func TestReviewStep_DiffFollowsTheConfiguredPRBaseBranch(t *testing.T) {
	t.Parallel()
	dir, developSHA, headSHA := newIntegrationBranchRepo(t)
	var prompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			prompt = opts.Prompt
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"reviewed_paths":["services/api/main.go"],"risk_level":"low","risk_rationale":"small","risk_scope":"source-or-external"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, zeroSHA, headSHA, config.Commands{})
	sctx.Config.PR.BaseBranch = "develop"

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"services/api/main.go"}; !slices.Equal(outcome.ReviewablePaths, want) {
		t.Fatalf("ReviewablePaths = %q, want %q", outcome.ReviewablePaths, want)
	}
	if outcome.NeedsApproval {
		t.Fatal("a review covering the branch's own file must approve")
	}
	if !strings.Contains(prompt, "branch changes between "+developSHA+" and "+headSHA) {
		t.Fatalf("review prompt does not scope the diff to the develop merge-base %s:\n%s", developSHA, prompt)
	}
}

func TestIntentBaseSHA_NewBranchFollowsTheConfiguredPRBaseBranch(t *testing.T) {
	t.Parallel()
	dir, developSHA, headSHA := newIntegrationBranchRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, zeroSHA, headSHA, config.Commands{})
	sctx.Config.PR.BaseBranch = "develop"

	base := intentBaseSHA(context.Background(), sctx, dir)
	if base != developSHA {
		t.Errorf("intent base = %s, want the develop merge-base %s", base, developSHA)
	}
	files, err := diffFilesForIntentMatching(context.Background(), dir, base, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"services/api/main.go"}; !slices.Equal(files, want) {
		t.Errorf("intent diff files = %q, want %q", files, want)
	}
}
