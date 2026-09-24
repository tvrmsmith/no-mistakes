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
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
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

	base := intentBaseSHA(t.Context(), sctx, dir)
	if base != developSHA {
		t.Errorf("intent base = %s, want the develop merge-base %s", base, developSHA)
	}
	files, err := diffFilesForIntentMatching(t.Context(), dir, base, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"services/api/main.go"}; !slices.Equal(files, want) {
		t.Errorf("intent diff files = %q, want %q", files, want)
	}
}

func TestMetricsStep_ChangedFilesFollowTheConfiguredPRBaseBranch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command reads the variables through POSIX shell interpolation")
	}
	t.Parallel()
	dir, developSHA, headSHA := newIntegrationBranchRepo(t)
	outFile := filepath.Join(t.TempDir(), "env.out")
	command := `printf '%s\n' "$NO_MISTAKES_BASE_SHA" "$NO_MISTAKES_CHANGED_FILES" > ` + outFile + "\n" +
		echoMetricsReport(`{"metric":"crap","functions":[]}`)
	sctx := coveredMetricsContext(t, &mockAgent{name: "test"}, dir, zeroSHA, headSHA, command, 30)
	sctx.Config.PR.BaseBranch = "develop"

	if _, err := (&MetricsStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if want := []string{developSHA, "services/api/main.go"}; !slices.Equal(lines, want) {
		t.Errorf("base SHA and changed files = %q, want %q", lines, want)
	}
}

func TestLintStep_ExtraLinterBaseFollowsTheConfiguredPRBaseBranch(t *testing.T) {
	t.Parallel()
	dir, developSHA, headSHA := newIntegrationBranchRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, zeroSHA, headSHA, config.Commands{Lint: "exit 0"})
	sctx.Config.PR.BaseBranch = "develop"
	sctx.Config.Lint = config.Lint{ExtraLinters: []config.ExtraLinter{{
		Name:            "probe",
		Command:         `echo "base=$NO_MISTAKES_BASE_SHA"`,
		FindingsPattern: `^(?P<message>base=.*)$`,
	}}}

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings := outcomeFindings(t, outcome)
	if len(findings.Items) != 1 {
		t.Fatalf("expected the probe finding, got %s", outcome.Findings)
	}
	if got, want := findings.Items[0].Description, "probe: base="+developSHA; got != want {
		t.Errorf("extra linter saw %q, want %q", got, want)
	}
}

func TestAgentPromptsCarryTheConfiguredPRBaseBranchMergeBase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		output string
		fixing bool
		step   func() pipeline.Step
	}{
		{"format fix", `{"summary":"repair source"}`, true, func() pipeline.Step { return &FormatStep{} }},
		{"document", `{"findings":[],"summary":"docs current"}`, false, func() pipeline.Step { return &DocumentStep{} }},
		{"custom gate fix", `{"summary":"satisfy gate"}`, true, func() pipeline.Step {
			return &CustomGateStep{Gate: config.Gate{Name: "probe", After: types.StepTest, Command: "exit 0"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, developSHA, headSHA := newIntegrationBranchRepo(t)
			ag := &mockAgent{
				name: "test",
				runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(tc.output)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, zeroSHA, headSHA, config.Commands{})
			sctx.Config.PR.BaseBranch = "develop"
			sctx.Fixing = tc.fixing

			if _, err := tc.step().Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if len(ag.calls) == 0 {
				t.Fatal("the step never prompted the agent")
			}
			if want := "- base commit: " + developSHA; !strings.Contains(ag.calls[0].Prompt, want) {
				t.Errorf("prompt does not carry %q:\n%s", want, ag.calls[0].Prompt)
			}
		})
	}
}

// Intent runs before rebase, and the daemon fetches only the default branch at
// run start, so on a gate that has never fetched the PR base branch intent
// must fetch it itself rather than diff against the empty tree.
func TestIntentBaseSHA_NewBranchFetchesAnUnfetchedPRBaseBranch(t *testing.T) {
	t.Parallel()
	dir, developSHA, headSHA := newIntegrationBranchRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "clone", "--bare", "--quiet", dir, ".")
	gitCmd(t, dir, "remote", "add", "origin", remote)
	gitCmd(t, dir, "branch", "-D", "develop")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, zeroSHA, headSHA, config.Commands{})
	sctx.Config.PR.BaseBranch = "develop"

	if base := intentBaseSHA(t.Context(), sctx, dir); base != developSHA {
		t.Errorf("intent base = %s, want the develop merge-base %s fetched from origin", base, developSHA)
	}
}
