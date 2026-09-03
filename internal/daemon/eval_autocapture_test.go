package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/eval"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAutoCaptureEvalCaseCollectsAFinishedRun is the trigger's contract: a run
// that reached the end of its pipeline with a decided review lands in the local
// corpus with nobody running a command. Without this the eval sets stay empty
// forever no matter how many reviews the machine performs.
func TestAutoCaptureEvalCaseCollectsAFinishedRun(t *testing.T) {
	ctx := context.Background()
	p, database, runID := setupFinishedReviewRun(t, ctx)
	m := NewRunManager(database, p, nil)

	m.autoCaptureEvalCase(ctx, evalConfig(true, true), runID)

	if got := capturedCaseCount(t, p); got != 1 {
		t.Fatalf("collected cases = %d, want 1", got)
	}
}

// TestAutoCaptureEvalCaseHonorsTheOperatorsSwitches pins both halves of the
// off switch. Collection writes to local disk on every run, so a user who turns
// it off must get nothing at all - not a smaller corpus, and not an eval
// directory that quietly appears anyway.
func TestAutoCaptureEvalCaseHonorsTheOperatorsSwitches(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{
		{"auto capture disabled", evalConfig(true, false)},
		{"provenance disabled", evalConfig(false, true)},
		{"no config", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			p, database, runID := setupFinishedReviewRun(t, ctx)
			m := NewRunManager(database, p, nil)

			m.autoCaptureEvalCase(ctx, tc.cfg, runID)

			if _, err := os.Stat(p.EvalDir()); !os.IsNotExist(err) {
				t.Fatalf("disabled collection touched the eval directory: %v", err)
			}
		})
	}
}

// TestAutoCaptureEvalCaseSurvivesAnUncapturableRun proves collection stays
// subordinate to the pipeline: an unknown run is a real capture error, and it
// still has to return quietly rather than propagate out of the run goroutine,
// where the enclosing recover would mark a finished run as failed.
func TestAutoCaptureEvalCaseSurvivesAnUncapturableRun(t *testing.T) {
	ctx := context.Background()
	p, database, _ := setupFinishedReviewRun(t, ctx)
	m := NewRunManager(database, p, nil)

	m.autoCaptureEvalCase(ctx, evalConfig(true, true), "01ZZZZZZZZZZZZZZZZZZZZZZZZ")

	if got := capturedCaseCount(t, p); got != 0 {
		t.Fatalf("collected cases = %d, want 0", got)
	}
}

// Auto capture re-plans the whole diversified holdout before it enforces the
// cap, so the operator's configured size has to reach it. Planning at the
// package default would silently resize the official held-out set.
func TestAutoCaptureEvalCasePlansThePinsAtTheConfiguredDiversifiedSize(t *testing.T) {
	ctx := context.Background()
	p, database, firstRun := setupFinishedReviewRun(t, ctx)
	m := NewRunManager(database, p, nil)
	// One run per stratum, so a size of 1 keeps strictly fewer pins than the
	// package default of 32 would.
	secondRun := addFinishedReviewRun(t, ctx, p, database, "warning-repo", "warning")
	thirdRun := addFinishedReviewRun(t, ctx, p, database, "info-repo", "info")

	cfg := evalRetentionConfig(true, true, 2, 1)
	for _, runID := range []string{firstRun, secondRun, thirdRun} {
		m.autoCaptureEvalCase(ctx, cfg, runID)
	}

	if got := diversifiedPinCount(t, p); got != 1 {
		t.Fatalf("diversified pins = %d, want the configured size of 1 rather than the package default of %d", got, config.DefaultEvalDiversifiedSize)
	}
	if got := capturedCaseCount(t, p); got != 2 {
		t.Fatalf("corpus = %d case(s), want the configured max_cases of 2", got)
	}
}

func evalConfig(provenance, autoCapture bool) *config.Config {
	return evalRetentionConfig(provenance, autoCapture, config.DefaultEvalMaxCases, config.DefaultEvalDiversifiedSize)
}

func evalRetentionConfig(provenance, autoCapture bool, maxCases, diversifiedSize int) *config.Config {
	return &config.Config{Eval: config.Eval{
		CaptureProvenance: provenance,
		AutoCapture:       autoCapture,
		MaxCases:          maxCases,
		DiversifiedSize:   diversifiedSize,
	}}
}

// diversifiedPinCount reads the pin table straight off the registry file.
// Resolving the set through the store would re-plan the pins at the store's own
// default size, which is the very value this asks about.
func diversifiedPinCount(t *testing.T, p *paths.Paths) int {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(p.EvalDir(), "registry.sqlite")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM diversified_pins`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func capturedCaseCount(t *testing.T, p *paths.Paths) int {
	t.Helper()
	store, err := eval.Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	return len(cases)
}

// setupFinishedReviewRun builds the smallest real thing collection needs: a
// gate holding the reviewed commits, and a completed run whose review round
// carries eval provenance and a recorded gate decision.
func setupFinishedReviewRun(t *testing.T, ctx context.Context) (*paths.Paths, *db.DB, string) {
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
	return p, database, addFinishedReviewRun(t, ctx, p, database, "eval-repo", "error")
}

// addFinishedReviewRun is setupFinishedReviewRun's body, parameterized by
// repository name and finding severity so one root can hold several finished
// runs. The severity matters because it is one of the axes the diversified
// holdout stratifies on, so runs with different severities land in different
// strata and a size cap can actually bind.
func addFinishedReviewRun(t *testing.T, ctx context.Context, p *paths.Paths, database *db.DB, name, severity string) string {
	t.Helper()
	root := p.Root()

	gateDir := p.RepoDir(name)
	if err := git.InitBare(ctx, gateDir); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(root, "source-"+name)
	mustGitRun(t, ctx, root, "clone", gateDir, workDir)
	mustGitRun(t, ctx, workDir, "config", "user.email", "eval@example.test")
	mustGitRun(t, ctx, workDir, "config", "user.name", "Eval Test")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitRun(t, ctx, workDir, "add", ".")
	mustGitRun(t, ctx, workDir, "commit", "-m", "base")
	mustGitRun(t, ctx, workDir, "branch", "-M", "main")
	mustGitRun(t, ctx, workDir, "push", "origin", "main")
	baseSHA := mustGitRun(t, ctx, workDir, "rev-parse", "HEAD")
	mustGitRun(t, ctx, workDir, "checkout", "-b", "feature/eval")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n\nfunc Changed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitRun(t, ctx, workDir, "add", "main.go")
	mustGitRun(t, ctx, workDir, "commit", "-m", "change")
	mustGitRun(t, ctx, workDir, "push", "origin", "feature/eval")
	headSHA := mustGitRun(t, ctx, workDir, "rev-parse", "HEAD")

	repo, err := database.InsertRepoWithID(name, workDir, "https://example.test/org/"+name, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/eval", headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"real-bug","severity":"` + severity + `","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	round, err := database.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &findings, nil, headSHA, headSHA, baseSHA, []byte("{}\n"), []byte("{}\n"), 50)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["real-bug"]`
	if err := database.SetStepRoundSelection(round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	return run.ID
}

func mustGitRun(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(ctx, dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}
