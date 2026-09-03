package cli

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/eval"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestEvalSetsIsLocalOnlyAndEmitsNoTelemetry(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, t.TempDir())

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	out, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v", err)
	}
	if !strings.Contains(out, "eval case sets") || !strings.Contains(out, "local-only") {
		t.Fatalf("output = %q, want the eval case sets dashboard with its local-only footer", out)
	}
	if strings.Contains(out, "verdict") || strings.Contains(out, "park") || strings.Contains(out, ", pass ") {
		t.Fatalf("eval sets still uses park/pass accuracy language: %q", out)
	}
	if recorder.count("command") != 0 || recorder.count("pageview") != 0 {
		t.Fatalf("eval emitted remote telemetry: %#v", recorder.events)
	}
}

func TestEvalCaptureAndSetsSpeakInFindingGoldTerms(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	selected := `["real-bug"]`
	if err := fixture.db.SetStepRoundSelection(fixture.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("eval", "capture", fixture.run.ID)
	if err != nil {
		t.Fatalf("eval capture: %v\n%s", err, out)
	}
	if !strings.Contains(out, "captured 1 local review case") {
		t.Fatalf("capture output = %q", out)
	}

	out, err = executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v\n%s", err, out)
	}
	if !strings.Contains(out, "TP      1") || !strings.Contains(out, "FP      0") || !strings.Contains(out, "0 unlabeled / pending") {
		t.Fatalf("sets output = %q, want finding-level gold, not park/pass", out)
	}
	if !strings.Contains(out, "Diversified holdout") || !strings.Contains(out, "Self-score") || !strings.Contains(out, "1/1 true issues") {
		t.Fatalf("sets output = %q, want the diversified headline with its instant self-score", out)
	}
	if strings.Contains(out, "verdict") || strings.Contains(out, "park") || strings.Contains(out, ", pass ") {
		t.Fatalf("sets output still uses park/pass accuracy language: %q", out)
	}

	out, err = executeCmd("eval", "report")
	if err != nil {
		t.Fatalf("eval report: %v\n%s", err, out)
	}
	if !strings.Contains(out, "LOCAL-ONLY EVAL REPORT") || !strings.Contains(out, "no candidate replays recorded yet") {
		t.Fatalf("report output = %q", out)
	}
}

func TestEvalMissIngestLabelsFalseNegativeGold(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	findings := `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	if err := fixture.db.UpdateStepStatus(fixture.step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("eval", "miss", "ingest", fixture.run.ID, "--finding", `{"id":"silent-wrong-set","file":"pkg/compute.go","line":12,"severity":"error","description":"returns the wrong set for a valid input"}`)
	if err != nil {
		t.Fatalf("eval miss ingest: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ingested 1 false-negative gold finding") {
		t.Fatalf("ingest output = %q", out)
	}

	out, err = executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v\n%s", err, out)
	}
	if !strings.Contains(out, "FN      1") || !strings.Contains(out, "TP      0") {
		t.Fatalf("sets output = %q, want ingested false-negative gold", out)
	}

	out, err = executeCmd("eval", "capture", fixture.run.ID)
	if err != nil {
		t.Fatalf("recapture after ingest: %v\n%s", err, out)
	}
	out, err = executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets after recapture: %v\n%s", err, out)
	}
	if !strings.Contains(out, "FN      1") || !strings.Contains(out, "TP      0") {
		t.Fatalf("sets after recapture = %q, want ingested false-negative gold to persist", out)
	}

	out, err = executeCmd("eval", "miss", "ingest", fixture.run.ID, "--finding", `{"id":"silent-wrong-set","file":"pkg/compute.go","line":12,"severity":"error","description":"returns the wrong set for a valid input"}`)
	if err != nil {
		t.Fatalf("duplicate eval miss ingest: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ingested 0 false-negative gold finding") {
		t.Fatalf("duplicate ingest output = %q", out)
	}
}

// Every read-or-converging eval subcommand must be idempotent at the CLI: the
// second identical invocation prints the same output and leaves the same
// state. Both halves are asserted - stdout equality alone let a command that
// created a new file under the app root pass as idempotent. (eval run is
// additive by design and is covered separately; eval miss ingest's duplicate
// no-op is covered above.)
func TestEvalCaptureSetsReportAndRelabelAreIdempotentAtTheCLI(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	selected := `["real-bug"]`
	if err := fixture.db.SetStepRoundSelection(fixture.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateRunPRState(fixture.run.ID, "merged"); err != nil {
		t.Fatal(err)
	}

	for _, command := range [][]string{
		{"eval", "capture", fixture.run.ID},
		{"eval", "sets"},
		{"eval", "sets", "--refresh-diversified"},
		{"eval", "relabel", fixture.run.ID},
		{"eval", "relabel"},
		{"eval", "report"},
	} {
		first, err := executeCmd(command...)
		if err != nil {
			t.Fatalf("%v (first): %v\n%s", command, err, first)
		}
		treeBefore := nmHomeTree(t, root)
		second, err := executeCmd(command...)
		if err != nil {
			t.Fatalf("%v (second): %v\n%s", command, err, second)
		}
		if first != second {
			t.Fatalf("%v is not idempotent:\nfirst: %s\nsecond: %s", command, first, second)
		}
		if treeAfter := nmHomeTree(t, root); !slices.Equal(treeBefore, treeAfter) {
			t.Fatalf("%v changed the app root's shape:\nbefore: %v\nafter:  %v", command, treeBefore, treeAfter)
		}
	}
}

func TestEvalRunRendersProgressAndScoreDashboard(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	selected := `["real-bug"]`
	if err := fixture.db.SetStepRoundSelection(fixture.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	installFakeCLIReviewAgent(t, root, findings)

	if out, err := executeCmd("eval", "capture", fixture.run.ID); err != nil {
		t.Fatalf("eval capture: %v\n%s", err, out)
	}
	setsBefore, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("eval", "run", "--cases", "labeled", "--candidate", "claude,model=test", "--repeats", "2")
	if err != nil {
		t.Fatalf("eval run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "replaying 1 case(s) x 2 repeat(s) with claude,model=test on labeled") {
		t.Fatalf("run output = %q, want the replay plan header", out)
	}
	if !strings.Contains(out, "1/2") || !strings.Contains(out, "2/2") || !strings.Contains(out, "TP 1 · FN 0 · FP 0 · pending 0") {
		t.Fatalf("run output = %q, want one scored progress line per replay", out)
	}
	if !strings.Contains(out, " eval run ") || !strings.Contains(out, "Recall") || !strings.Contains(out, "2/2 true issues") {
		t.Fatalf("run output = %q, want the score summary dashboard aggregating both repeats", out)
	}
	if !strings.Contains(out, "local eval session") {
		t.Fatalf("run output = %q, want the trailing session line", out)
	}

	// Re-running the same eval run is additive (a fresh measurement session)
	// but must stay safe: the frozen corpus is untouched, and the identical
	// input lands in the same cohort so the report aggregates instead of
	// fragmenting into a new comparison group.
	if out, err = executeCmd("eval", "run", "--cases", "labeled", "--candidate", "claude,model=test", "--repeats", "2"); err != nil {
		t.Fatalf("second eval run: %v\n%s", err, out)
	}
	setsAfter, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatal(err)
	}
	if setsBefore != setsAfter {
		t.Fatalf("eval run mutated the case sets:\nbefore: %s\nafter: %s", setsBefore, setsAfter)
	}
	report, err := executeCmd("eval", "report")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(report, "cohort"); got != 1 {
		t.Fatalf("report = %q, want both identical runs aggregated into one cohort, got %d", report, got)
	}
	if !strings.Contains(report, "replays: 4") {
		t.Fatalf("report = %q, want all four replays counted in the single cohort", report)
	}
}

type evalCLIFixture struct {
	run   *db.Run
	round *db.StepRound
	step  *db.StepResult
	db    *db.DB
}

// setupEvalCLIFixture builds the minimal real gate, working clone, and
// recorded review round the eval CLI commands read from. The repository is
// named "repo" so its URL stays https://example.test/org/repo.
func setupEvalCLIFixture(t *testing.T, ctx context.Context, root, findings string) evalCLIFixture {
	t.Helper()
	return setupEvalCLIFixtureNamed(t, ctx, root, "repo", findings)
}

// installFakeCLIReviewAgent puts a scripted claude on PATH that replies with
// the given review findings, mirroring the fake agent the internal/eval
// replay tests use.
func installFakeCLIReviewAgent(t *testing.T, root, findingsJSON string) {
	t.Helper()
	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "claude")
	reply := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Skill","input":{"skill":"comprehensive-code-review"}}]}}
{"type":"assistant","message":{"usage":{"input_tokens":12,"output_tokens":3},"content":[{"type":"text","text":"review"}]}}
{"type":"result","subtype":"success","is_error":false,"structured_output":` + findingsJSON + `,"usage":{"input_tokens":12,"output_tokens":3}}
`
	var script string
	if runtime.GOOS == "windows" {
		fake += ".cmd"
		script = "@echo off\r\nmore >nul\r\necho " + strings.ReplaceAll(strings.TrimSpace(reply), "\n", "\r\necho ") + "\r\n"
	} else {
		script = "#!/bin/sh\n[ \"$NM_HOME\" = \"" + root + "\" ] && touch \"" + root + "/shared-home-used\"\ncat >/dev/null\ncat <<'EOF'\n" + reply + "EOF\n"
	}
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func mustCLIGit(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(ctx, dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// The eval-sets dashboard identifies a case's repository by name and lays the
// finding-level gold out as a confusion matrix. The repository name is
// resolved from the locally registered repositories, since a case stores only
// the fingerprint of its upstream URL.
func TestEvalSetsNamesTheRepositoryAndTablesTheConfusionMatrix(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	selected := `["real-bug"]`
	if err := fixture.db.SetStepRoundSelection(fixture.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if out, err := executeCmd("eval", "capture", fixture.run.ID); err != nil {
		t.Fatalf("eval capture: %v\n%s", err, out)
	}

	out, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v\n%s", err, out)
	}
	if !strings.Contains(out, "org/repo") {
		t.Fatalf("sets output = %q, want the repository name from its registered upstream URL", out)
	}
	for _, want := range []string{"Confusion matrix", "real issue", "not an issue", "review raised", "review missed", "TP", "FP", "FN", "TN"} {
		if !strings.Contains(out, want) {
			t.Fatalf("sets output = %q, want confusion-matrix table cell %q", out, want)
		}
	}
	if strings.Contains(out, "TN      0") {
		t.Fatalf("sets output = %q, want an uncounted true-negative cell, not a fabricated zero", out)
	}
	if !strings.Contains(out, "Diversified holdout") || !strings.Contains(out, "1/1 true issues") {
		t.Fatalf("sets output = %q, want the diversified headline and self-score preserved", out)
	}
}

// The composition table shows one kind of repository identity for every row
// and never runs past the dashboard box, however long the names are.
func TestEvalCompositionRepoColumnIsUniformAndFitsTheBox(t *testing.T) {
	rows := []eval.CompositionRow{
		{Repo: "a-very-long-organization-name/no-mistakes", Language: "go", Size: "large", Severity: "warning", FindingType: "warning/auto-fix", Cases: 2},
		{Repo: "group/very-long-subgroup/actual-repo", Language: "go", Size: "large", Severity: "error", FindingType: "error/ask-user", Cases: 1},
	}
	lines := compositionLines(rows)
	if len(lines) != len(rows) {
		t.Fatalf("compositionLines returned %d line(s), want %d", len(lines), len(rows))
	}
	for _, line := range lines {
		if width := lipgloss.Width(line); width > evalBoxWidth-4 {
			t.Fatalf("composition line %q is %d wide, want at most %d", line, width, evalBoxWidth-4)
		}
	}
	if strings.Contains(lines[0], "a-very-long-organization-name/no-mistakes") {
		t.Fatalf("line = %q, want the long identity shortened when it does not fit its column", lines[0])
	}
	if !strings.Contains(lines[0], "no-mistakes") || !strings.Contains(lines[1], "actual-repo") {
		t.Fatalf("lines = %q, want every repository's final path segment", lines)
	}
	if strings.Contains(lines[1], "very-long-subgroup") {
		t.Fatalf("line = %q, want a uniformly shortened repository name", lines[1])
	}
	if !strings.Contains(lines[0], "warning/auto-fix") || !strings.Contains(lines[1], "error/ask-user") {
		t.Fatalf("lines = %q, want the strata kept on every row", lines)
	}

	narrow := compositionLines([]eval.CompositionRow{{Repo: "owner/name", Language: "go", Size: "tiny", Severity: "none", FindingType: "none", Cases: 1}})
	if !strings.Contains(narrow[0], "owner/name") {
		t.Fatalf("line = %q, want the full owner/name identity when it fits", narrow[0])
	}
	if got := compositionLines(nil); len(got) != 0 {
		t.Fatalf("compositionLines(nil) = %q, want no lines", got)
	}
}

// The composition table must fit the dashboard box on BOTH of its variable
// axes. The repository column is only one of them: the strata are the other,
// and a finding type carrying a non-canonical severity or action can push the
// fixed strata past the room the box has, at which point clamping the
// repository column to its minimum is not enough and the box renderer silently
// cuts the finding type off the end of the row.
func TestEvalCompositionFitsTheBoxWhenTheStrataAreOversized(t *testing.T) {
	rows := []eval.CompositionRow{
		{Repo: "kunchenguid/no-mistakes", Language: "javascript", Size: "medium", Severity: "warning", FindingType: "blocking-correctness-defect/requires-human-review", Cases: 3},
		{Repo: "another-organization/service", Language: "typescript", Size: "large", Severity: "error", FindingType: "error/ask-user", Cases: 1},
	}
	lines := compositionLines(rows)
	if len(lines) != len(rows) {
		t.Fatalf("compositionLines returned %d line(s), want %d", len(lines), len(rows))
	}
	for _, line := range lines {
		if width := lipgloss.Width(line); width > evalBoxWidth-4 {
			t.Fatalf("composition line %q is %d wide, want at most %d", line, width, evalBoxWidth-4)
		}
	}

	// The rendered box is the surface the reader sees: nothing may be cut off
	// there either, and every row must still start with its case count and
	// repository identity.
	box := renderTitledBox(" eval case sets ", evalBoxWidth, lines)
	for _, line := range strings.Split(box, "\n") {
		if width := lipgloss.Width(line); width != evalBoxWidth {
			t.Fatalf("rendered box line %q is %d wide, want exactly %d", line, width, evalBoxWidth)
		}
	}
	if !strings.Contains(lines[0], "no-mista") || !strings.Contains(lines[1], "service") {
		t.Fatalf("lines = %q, want every row to keep a repository identity", lines)
	}
	if !strings.Contains(lines[0], "javascript") || !strings.Contains(lines[1], "typescript") {
		t.Fatalf("lines = %q, want the leading strata preserved when the trailing ones are shortened", lines)
	}
}

func TestEvalCompositionFitsTheBoxWhenCaseCountsExpand(t *testing.T) {
	rows := []eval.CompositionRow{
		{Repo: "owner/abcdefghijklmnopqrstuvwxyz", Language: "go", Size: "large", Severity: "warning", FindingType: "warning/auto-fix", Cases: 9999},
		{Repo: "owner/zyxwvutsrqponmlkjihgfedcba", Language: "go", Size: "large", Severity: "warning", FindingType: "warning/auto-fix", Cases: 10000},
	}
	box := renderTitledBox(" eval case sets ", evalBoxWidth, compositionLines(rows))
	for _, line := range strings.Split(box, "\n") {
		if width := lipgloss.Width(line); width != evalBoxWidth {
			t.Fatalf("rendered box line %q is %d wide, want exactly %d", line, width, evalBoxWidth)
		}
	}
	if got := strings.Count(box, "warning/auto-fix"); got != len(rows) {
		t.Fatalf("rendered box contains %d complete finding types, want %d:\n%s", got, len(rows), box)
	}
}

func TestEvalReportPipelineFlagRejectsAnUnknownLayout(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, t.TempDir())

	_, err := executeCmd("eval", "report", "--pipeline", "v2")
	if err == nil || !strings.Contains(err.Error(), `unknown pipeline version "v2"`) {
		t.Fatalf("eval report --pipeline v2 error = %v, want unknown pipeline version", err)
	}
}

func TestEvalSetsPipelineFlagRejectsAnUnknownLayout(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, t.TempDir())

	_, err := executeCmd("eval", "sets", "--pipeline", "v2")
	if err == nil || !strings.Contains(err.Error(), `unknown pipeline version "v2"`) {
		t.Fatalf("eval sets --pipeline v2 error = %v, want unknown pipeline version", err)
	}
}

// The pipeline flag is validated before eval run does any other work: the
// error returned must be the parse error, never a replay or empty-corpus
// error, so a typo costs nothing.
func TestEvalRunPipelineFlagRejectsAnUnknownLayout(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, t.TempDir())

	_, err := executeCmd("eval", "run", "--cases", "all", "--candidate", "codex,model=gpt-5.4", "--pipeline", "v2")
	if err == nil || !strings.Contains(err.Error(), `unknown pipeline version "v2"`) {
		t.Fatalf("eval run --pipeline v2 error = %v, want unknown pipeline version", err)
	}
}

// The --pipeline flag's usage string must read identically on every command
// that carries it, so an operator learns the flag once.
func TestEvalPipelineFlagUsageIsIdenticalOnEveryCommand(t *testing.T) {
	root := newEvalCmd()
	usage := map[string]string{}
	for _, name := range []string{"run", "sets", "report"} {
		sub, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatalf("find eval %s: %v", name, err)
		}
		flag := sub.Flags().Lookup("pipeline")
		if flag == nil {
			t.Fatalf("eval %s has no --pipeline flag", name)
		}
		usage[name] = flag.Usage
	}
	// Compared to each other rather than to a copy of the text: the wording is
	// free to change, the three commands disagreeing about it is the bug.
	if usage["sets"] != usage["run"] || usage["report"] != usage["run"] {
		t.Fatalf("--pipeline usage differs between commands: %#v", usage)
	}
	if usage["run"] == "" {
		t.Fatal("--pipeline carries no usage text on any command")
	}
}

// setupEvalCLIFixtureNamed is setupEvalCLIFixture parameterized by repository
// name, so two fixtures can coexist under one NM_HOME root: setupEvalCLIFixture
// always names its gate and worktree "eval-repo"/"source", which collides when
// a test needs two independent captured runs (one per pipeline layout) side by
// side.
func setupEvalCLIFixtureNamed(t *testing.T, ctx context.Context, root, name, findings string) evalCLIFixture {
	t.Helper()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	gateDir := p.RepoDir(name)
	if err := git.InitBare(ctx, gateDir); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(root, name)
	mustCLIGit(t, ctx, root, "clone", gateDir, workDir)
	mustCLIGit(t, ctx, workDir, "config", "user.email", "eval@example.test")
	mustCLIGit(t, ctx, workDir, "config", "user.name", "Eval Test")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustCLIGit(t, ctx, workDir, "add", ".")
	mustCLIGit(t, ctx, workDir, "commit", "-m", "base")
	mustCLIGit(t, ctx, workDir, "branch", "-M", "main")
	mustCLIGit(t, ctx, workDir, "push", "origin", "main")
	baseSHA := mustCLIGit(t, ctx, workDir, "rev-parse", "HEAD")
	mustCLIGit(t, ctx, workDir, "checkout", "-b", "feature/eval")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n\nfunc Changed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustCLIGit(t, ctx, workDir, "add", "main.go")
	mustCLIGit(t, ctx, workDir, "commit", "-m", "change")
	mustCLIGit(t, ctx, workDir, "push", "origin", "feature/eval")
	headSHA := mustCLIGit(t, ctx, workDir, "rev-parse", "HEAD")

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
	round, err := database.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &findings, nil, headSHA, headSHA, baseSHA, []byte("{}\n"), []byte("{}\n"), 50)
	if err != nil {
		t.Fatal(err)
	}
	return evalCLIFixture{run: run, round: round, step: step, db: database}
}

// forceStepOrder overrides a recorded step's order directly in the pipeline
// database, mirroring internal/eval's own test helper of the same name: it is
// the only way to make a captured run's OWN recorded step order say the cheap
// gates ran before review, since InsertStepResult always assigns the build's
// current order.
func forceStepOrder(t *testing.T, p *paths.Paths, stepID string, order int) {
	t.Helper()
	// The live *db.DB handle is still open on this WAL database, so this second
	// writer needs the same busy timeout db.Open uses or a concurrent
	// checkpoint returns SQLITE_BUSY immediately and fails the test spuriously.
	raw, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE step_results SET step_order = ? WHERE id = ?`, order, stepID); err != nil {
		t.Fatal(err)
	}
}

// setupEvalCLIFixtureTaggedCheapGatesFirst is setupEvalCLIFixtureNamed with
// extra cheap-gate step rows forced ahead of review, so the captured case's
// OWN recorded step order tags it cheap-gates-first instead of the default
// review-early.
func setupEvalCLIFixtureTaggedCheapGatesFirst(t *testing.T, ctx context.Context, root, name, findings string) evalCLIFixture {
	t.Helper()
	fixture := setupEvalCLIFixtureNamed(t, ctx, root, name, findings)
	p := paths.WithRoot(root)
	for _, stepName := range []types.StepName{"format", types.StepLint, types.StepTest} {
		step, err := fixture.db.InsertStepResult(fixture.run.ID, stepName)
		if err != nil {
			t.Fatal(err)
		}
		forceStepOrder(t, p, step.ID, 1)
	}
	forceStepOrder(t, p, fixture.step.ID, 10)
	return fixture
}

// TestEvalReportNarrowsToOnePipelineLayout and
// TestEvalReportGroupsBothPipelineLayoutsByDefault share one corpus holding a
// recorded evaluation under each pipeline layout.
func recordTwoPipelineLayoutEvaluations(t *testing.T, root string) {
	t.Helper()
	ctx := context.Background()

	reviewEarlyFindings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	reviewEarly := setupEvalCLIFixtureNamed(t, ctx, root, "alpha-service", reviewEarlyFindings)
	selected := `["real-bug"]`
	if err := reviewEarly.db.SetStepRoundSelection(reviewEarly.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	installFakeCLIReviewAgent(t, root, reviewEarlyFindings)
	if out, err := executeCmd("eval", "capture", reviewEarly.run.ID); err != nil {
		t.Fatalf("eval capture (review-early): %v\n%s", err, out)
	}
	if out, err := executeCmd("eval", "run", "--cases", "labeled", "--candidate", "claude,model=test", "--repeats", "1", "--pipeline", "review-early"); err != nil {
		t.Fatalf("eval run (review-early): %v\n%s", err, out)
	}

	cheapGatesFindings := `{"findings":[{"id":"other-bug","severity":"warning","file":"main.go","line":3,"description":"other bug","action":"ask-user","review_scope":"source"}],"risk_level":"low","risk_rationale":"minor","risk_scope":"source-or-external"}`
	cheapGatesFirst := setupEvalCLIFixtureTaggedCheapGatesFirst(t, ctx, root, "beta-service", cheapGatesFindings)
	otherSelected := `["other-bug"]`
	if err := cheapGatesFirst.db.SetStepRoundSelection(cheapGatesFirst.round.ID, &otherSelected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	installFakeCLIReviewAgent(t, root, cheapGatesFindings)
	if out, err := executeCmd("eval", "capture", cheapGatesFirst.run.ID); err != nil {
		t.Fatalf("eval capture (cheap-gates-first): %v\n%s", err, out)
	}
	if out, err := executeCmd("eval", "run", "--cases", "labeled", "--candidate", "claude,model=test", "--repeats", "1", "--pipeline", "cheap-gates-first"); err != nil {
		t.Fatalf("eval run (cheap-gates-first): %v\n%s", err, out)
	}
}

func TestEvalReportNarrowsToOnePipelineLayout(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())
	recordTwoPipelineLayoutEvaluations(t, root)

	out, err := executeCmd("eval", "report", "--pipeline", "review-early")
	if err != nil {
		t.Fatalf("eval report --pipeline review-early: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pipeline review-early") {
		t.Fatalf("report output = %q, want it to contain the review-early group", out)
	}
	if strings.Contains(out, "cheap-gates-first") {
		t.Fatalf("report output = %q, want cheap-gates-first excluded", out)
	}
}

func TestEvalReportGroupsBothPipelineLayoutsByDefault(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())
	recordTwoPipelineLayoutEvaluations(t, root)

	out, err := executeCmd("eval", "report")
	if err != nil {
		t.Fatalf("eval report: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pipeline review-early") || !strings.Contains(out, "pipeline cheap-gates-first") {
		t.Fatalf("report output = %q, want both pipeline layouts grouped separately", out)
	}
}

// TestEvalSetsDashboardShowsThePipelineBreakdown and
// TestEvalSetsDashboardOmitsThePipelineBreakdownForASingleLayout use distinct
// finding severities across the two fixtures so both cases land in the
// diversified holdout: diversifiedStratum keys on severity among other axes,
// so cases of one severity never collide with and evict the other.
func TestEvalSetsDashboardShowsThePipelineBreakdown(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())
	ctx := context.Background()

	reviewEarlyFindings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	reviewEarly := setupEvalCLIFixtureNamed(t, ctx, root, "alpha-service", reviewEarlyFindings)
	selected := `["real-bug"]`
	if err := reviewEarly.db.SetStepRoundSelection(reviewEarly.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if out, err := executeCmd("eval", "capture", reviewEarly.run.ID); err != nil {
		t.Fatalf("eval capture (review-early): %v\n%s", err, out)
	}

	cheapGatesFindings := `{"findings":[{"id":"other-bug","severity":"warning","file":"main.go","line":3,"description":"other bug","action":"ask-user","review_scope":"source"}],"risk_level":"low","risk_rationale":"minor","risk_scope":"source-or-external"}`
	cheapGatesFirst := setupEvalCLIFixtureTaggedCheapGatesFirst(t, ctx, root, "beta-service", cheapGatesFindings)
	otherSelected := `["other-bug"]`
	if err := cheapGatesFirst.db.SetStepRoundSelection(cheapGatesFirst.round.ID, &otherSelected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if out, err := executeCmd("eval", "capture", cheapGatesFirst.run.ID); err != nil {
		t.Fatalf("eval capture (cheap-gates-first): %v\n%s", err, out)
	}

	out, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Pipeline layouts") {
		t.Fatalf("sets output = %q, want the pipeline breakdown block", out)
	}
	// The row shape, not the bare version string: a repository name or a
	// warning mentioning a layout would satisfy a substring match on its own.
	for _, want := range []string{"1  review-early · 1 gold", "1  cheap-gates-first · 1 gold"} {
		if !strings.Contains(out, want) {
			t.Fatalf("sets output = %q, want the breakdown row %q", out, want)
		}
	}
}

// The --pipeline filter on eval sets and eval run has to change what those
// commands actually operate on, not just parse.
func TestEvalPipelineFilterNarrowsWhatSetsAndRunOperateOn(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())
	ctx := context.Background()

	reviewEarlyFindings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	reviewEarly := setupEvalCLIFixtureNamed(t, ctx, root, "alpha-service", reviewEarlyFindings)
	selected := `["real-bug"]`
	if err := reviewEarly.db.SetStepRoundSelection(reviewEarly.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if out, err := executeCmd("eval", "capture", reviewEarly.run.ID); err != nil {
		t.Fatalf("eval capture (review-early): %v\n%s", err, out)
	}

	cheapGatesFindings := `{"findings":[{"id":"other-bug","severity":"warning","file":"main.go","line":3,"description":"other bug","action":"ask-user","review_scope":"source"}],"risk_level":"low","risk_rationale":"minor","risk_scope":"source-or-external"}`
	cheapGatesFirst := setupEvalCLIFixtureTaggedCheapGatesFirst(t, ctx, root, "beta-service", cheapGatesFindings)
	otherSelected := `["other-bug"]`
	if err := cheapGatesFirst.db.SetStepRoundSelection(cheapGatesFirst.round.ID, &otherSelected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if out, err := executeCmd("eval", "capture", cheapGatesFirst.run.ID); err != nil {
		t.Fatalf("eval capture (cheap-gates-first): %v\n%s", err, out)
	}

	unfiltered, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v\n%s", err, unfiltered)
	}
	if want := strings.TrimRight(metricStatsLine("Cases", "2", ""), " "); !strings.Contains(unfiltered, want) {
		t.Fatalf("eval sets = %q, want the unfiltered diversified line %q", unfiltered, want)
	}

	filtered, err := executeCmd("eval", "sets", "--pipeline", "review-early")
	if err != nil {
		t.Fatalf("eval sets --pipeline review-early: %v\n%s", err, filtered)
	}
	if want := strings.TrimRight(metricStatsLine("Cases", "1", ""), " "); !strings.Contains(filtered, want) {
		t.Fatalf("eval sets --pipeline review-early = %q, want the diversified line narrowed to %q", filtered, want)
	}

	installFakeCLIReviewAgent(t, root, reviewEarlyFindings)
	run, err := executeCmd("eval", "run", "--cases", "labeled", "--candidate", "claude,model=test", "--repeats", "1", "--pipeline", "review-early")
	if err != nil {
		t.Fatalf("eval run --pipeline review-early: %v\n%s", err, run)
	}
	if !strings.Contains(run, "replaying 1 case(s)") {
		t.Fatalf("eval run --pipeline review-early = %q, want it to plan the one review-early case", run)
	}
	unfilteredRun, err := executeCmd("eval", "run", "--cases", "labeled", "--candidate", "claude,model=test", "--repeats", "1")
	if err != nil {
		t.Fatalf("eval run: %v\n%s", err, unfilteredRun)
	}
	if !strings.Contains(unfilteredRun, "replaying 2 case(s)") {
		t.Fatalf("eval run = %q, want both cases planned without a filter", unfilteredRun)
	}
}

func TestEvalSetsDashboardOmitsThePipelineBreakdownForASingleLayout(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	selected := `["real-bug"]`
	if err := fixture.db.SetStepRoundSelection(fixture.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if out, err := executeCmd("eval", "capture", fixture.run.ID); err != nil {
		t.Fatalf("eval capture: %v\n%s", err, out)
	}

	out, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v\n%s", err, out)
	}
	if strings.Contains(out, "Pipeline layouts") {
		t.Fatalf("sets output = %q, want no pipeline breakdown for a single layout", out)
	}
}

// TestEvalSetsSelfScoreWarnsWhenTheSetSpansTwoPipelineLayouts pins the caveat
// on the headline number. The self-score folds every case in the set into one
// figure, so a set holding both layouts reports a single score over two
// populations, which is the exact "review-quality regression that is really a
// scope change" misreading the tag exists to prevent. The count breakdown alone
// is passive; the number itself has to say it is not comparable.
func TestEvalSetsSelfScoreWarnsWhenTheSetSpansTwoPipelineLayouts(t *testing.T) {
	out := renderEvalSetsDashboard([]eval.SetSummary{{
		Name:      "diversified",
		Cases:     2,
		GoldCases: 2,
		SelfScore: eval.EvaluationSummary{Labeled: 2},
		Pipelines: []eval.PipelineCountRow{
			{PipelineVersion: eval.PipelineReviewEarly, Cases: 1, GoldCases: 1},
			{PipelineVersion: eval.PipelineCheapGatesFirst, Cases: 1, GoldCases: 1},
		},
		// Unfiltered, so the scored population is the whole set.
		ScoredLayouts: 2,
	}})
	if !strings.Contains(out, "not comparable") {
		t.Fatalf("sets output = %q, want the self-score to be marked not comparable across layouts", out)
	}
	if !strings.Contains(out, "2 pipeline layouts") {
		t.Fatalf("sets output = %q, want the caveat to name how many layouts the set spans", out)
	}
}

// renderBoxLine silently cuts a line wider than the box content width, so the
// cap detail has to fit at every pin count rather than only at the small ones
// a fixture happens to use. "cap none" is the long branch.
func TestEvalCapDetailFitsTheBox(t *testing.T) {
	for _, pins := range []int{0, 9, 32, 999, 9999} {
		for _, capValue := range []int{0, 32, 9999} {
			line := metricStatsLine("Cases", strconv.Itoa(pins), evalCapDetail(pins, capValue))
			if width := lipgloss.Width(line); width > evalCompositionContentWidth {
				t.Fatalf("cap line at pins=%d cap=%d is %d columns wide, want at most %d: %q", pins, capValue, width, evalCompositionContentWidth, line)
			}
		}
	}
}

// The dashboard must show the whole cap detail, not a prefix of it, and it must
// say the pin count is corpus-wide: under `eval sets --pipeline X` the Cases
// figure beside it counts one layout's share, so a bare "pins 999" next to
// "Cases 4" reads as pins having been lost. The literal is spelled out here
// rather than derived from evalCapDetail, which would pass on any wording.
func TestEvalSetsDashboardRendersTheCapDetailUntruncated(t *testing.T) {
	out := renderEvalSetsDashboard([]eval.SetSummary{{
		Name: "diversified", Cases: 4, GoldCases: 4, PinCount: 999, Cap: 0,
		SelfScore: eval.EvaluationSummary{Labeled: 4},
	}})
	want := strings.TrimRight(metricStatsLine("Cases", "4", "pins 999 corpus-wide · cap none (1 per stratum)"), " ")
	if !strings.Contains(out, want) {
		t.Fatalf("sets dashboard = %q, want the complete cap line %q rather than a truncated one", out, want)
	}

	capped := renderEvalSetsDashboard([]eval.SetSummary{{
		Name: "diversified", Cases: 4, GoldCases: 4, PinCount: 999, Cap: 32,
		SelfScore: eval.EvaluationSummary{Labeled: 4},
	}})
	if wantCapped := strings.TrimRight(metricStatsLine("Cases", "4", "pins 999 corpus-wide · cap 32"), " "); !strings.Contains(capped, wantCapped) {
		t.Fatalf("sets dashboard = %q, want the capped detail %q", capped, wantCapped)
	}
}

// The layout breakdown is computed over the WHOLE set while the Cases and
// Composition figures above it honor the filter, so it has to say which
// population it describes or its rows read as contradicting the count.
func TestEvalSetsDashboardLabelsThePipelineBreakdownAsWholeSet(t *testing.T) {
	out := renderEvalSetsDashboard([]eval.SetSummary{{
		Name: "diversified", Cases: 1, GoldCases: 1,
		SelfScore: eval.EvaluationSummary{Labeled: 1},
		Pipelines: []eval.PipelineCountRow{
			{PipelineVersion: eval.PipelineReviewEarly, Cases: 1, GoldCases: 1},
			{PipelineVersion: eval.PipelineCheapGatesFirst, Cases: 5, GoldCases: 5},
		},
		ScoredLayouts: 1,
	}})
	if !strings.Contains(out, "Pipeline layouts (whole set)") {
		t.Fatalf("sets dashboard = %q, want the layout breakdown labeled as covering the whole set", out)
	}
}

func TestEvalSetsSelfScoreCarriesNoCaveatForASingleLayout(t *testing.T) {
	out := renderEvalSetsDashboard([]eval.SetSummary{{
		Name:      "diversified",
		Cases:     2,
		GoldCases: 2,
		SelfScore: eval.EvaluationSummary{Labeled: 2},
		Pipelines: []eval.PipelineCountRow{
			{PipelineVersion: eval.PipelineReviewEarly, Cases: 2, GoldCases: 2},
		},
		ScoredLayouts: 1,
	}})
	if strings.Contains(out, "not comparable") {
		t.Fatalf("sets output = %q, want no comparability caveat for a single layout", out)
	}
}

// eval run is the surface an operator watches while paying for the replays, and
// its score box folds every replay into one number the same way the sets
// self-score does, so an unfiltered run over a mixed corpus has to carry the
// same caveat.
func TestEvalRunSummaryWarnsWhenTheReplaysSpanTwoPipelineLayouts(t *testing.T) {
	session := eval.Session{Candidate: "claude,model=test", Set: "all", Cohort: "cohort", Repeats: 1}
	evaluations := []eval.Evaluation{
		{CaseID: "a", Status: "completed", HasFindingGold: true, GoldCount: 1, TruePositive: 1, PipelineVersion: eval.PipelineReviewEarly},
		{CaseID: "b", Status: "completed", HasFindingGold: true, GoldCount: 1, TruePositive: 1, PipelineVersion: eval.PipelineCheapGatesFirst},
	}
	out := renderEvalRunSummary(session, evaluations, 2)
	if !strings.Contains(out, "not comparable") || !strings.Contains(out, "2 pipeline layouts") {
		t.Fatalf("run summary = %q, want the mixed-layout caveat naming how many layouts the session spans", out)
	}

	single := renderEvalRunSummary(session, evaluations[:1], 1)
	if strings.Contains(single, "not comparable") {
		t.Fatalf("run summary = %q, want no comparability caveat for a single layout", single)
	}
}

// Under `eval sets --pipeline review-early` over a mixed corpus the self-score
// covers one layout and is comparable, while the breakdown still lists both so
// an operator can see what the filter left out. The caveat follows the scored
// population, so it stays silent.
func TestEvalSetsDashboardKeepsTheCaveatSilentForAFilteredSingleLayoutScore(t *testing.T) {
	both := []eval.PipelineCountRow{
		{PipelineVersion: eval.PipelineReviewEarly, Cases: 20, GoldCases: 20},
		{PipelineVersion: eval.PipelineCheapGatesFirst, Cases: 10, GoldCases: 10},
	}
	filtered := eval.SetSummary{
		Name: "diversified", Cases: 20, GoldCases: 20, TruePositive: 20, Cap: 32, PinCount: 30,
		SelfScore:     eval.EvaluationSummary{Total: 20, Labeled: 20, TruePositive: 20},
		Pipelines:     both,
		ScoredLayouts: 1,
	}
	out := renderEvalSetsDashboard([]eval.SetSummary{filtered})
	if strings.Contains(out, "not comparable") {
		t.Fatalf("sets dashboard = %q, want no caveat when the filter left one layout in the score", out)
	}
	if !strings.Contains(out, string(eval.PipelineCheapGatesFirst)) {
		t.Fatalf("sets dashboard = %q, want the unfiltered breakdown to still list both layouts", out)
	}

	unfiltered := filtered
	unfiltered.ScoredLayouts = 2
	if !strings.Contains(renderEvalSetsDashboard([]eval.SetSummary{unfiltered}), "not comparable") {
		t.Fatal("sets dashboard printed no caveat for a score that really does span two layouts")
	}
}

// nmHomeTree lists every path under root, relative and sorted, so a test can
// assert that a command left the app root's shape untouched.
func nmHomeTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel != "." {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// The eval dashboards are display surfaces over the local corpus: they resolve
// repository names from the pipeline database only if one already exists, and
// must never bring that database into being. db.Open creates the file and runs
// every migration, so a display-only lookup routed through it turned `eval
// sets`, `eval report`, and `eval run` into commands that initialize pipeline
// state on a machine that has none.
func TestEvalDisplayCommandsDoNotCreateThePipelineDatabase(t *testing.T) {
	tests := []struct {
		command []string
		// eval run legitimately refuses an empty case set; the filesystem
		// effect under test is the same either way.
		allowError bool
	}{
		{command: []string{"eval", "sets"}},
		{command: []string{"eval", "report"}},
		{command: []string{"eval", "run", "--cases", "labeled", "--candidate", "claude,model=test", "--repeats", "1"}, allowError: true},
	}
	for _, tt := range tests {
		command := tt.command
		t.Run(strings.Join(command, "-"), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("NM_HOME", root)
			chdir(t, t.TempDir())

			out, err := executeCmd(command...)
			if err != nil && !tt.allowError {
				t.Fatalf("%v: %v\n%s", command, err, out)
			}

			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if _, statErr := os.Stat(p.DB() + suffix); !os.IsNotExist(statErr) {
					t.Fatalf("%v created pipeline database file %q (stat err %v); a display-only repository-name lookup must not create or migrate pipeline state",
						command, p.DB()+suffix, statErr)
				}
			}
		})
	}
}

// A pre-existing pipeline database still resolves repository names: opening it
// read-only removes the side effect, not the feature.
func TestEvalSetsStillNamesRepositoriesFromAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	chdir(t, t.TempDir())

	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	fixture := setupEvalCLIFixture(t, ctx, root, findings)
	selected := `["real-bug"]`
	if err := fixture.db.SetStepRoundSelection(fixture.round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if out, err := executeCmd("eval", "capture", fixture.run.ID); err != nil {
		t.Fatalf("eval capture: %v\n%s", err, out)
	}

	out, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v\n%s", err, out)
	}
	if !strings.Contains(out, "org/repo") {
		t.Fatalf("sets output = %q, want the repository name resolved from the existing pipeline database", out)
	}
}
