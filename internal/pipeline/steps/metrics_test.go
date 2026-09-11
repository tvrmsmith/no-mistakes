package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestMetricsStep_NoConfiguredCommandPassesWithoutRunningAnything(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when no metrics command is configured")
	}
	if len(ag.calls) != 0 {
		t.Errorf("expected no agent calls when no metrics command is configured, got %d", len(ag.calls))
	}
	if got := lastCommitMessage(t, dir); got == "" {
		t.Fatal("lastCommitMessage returned empty")
	} else if strings.Contains(got, "metrics") {
		t.Fatalf("expected no metrics commit, got %q", got)
	}
}

func TestMetricsStep_ConfiguredCommandWithNoCoverageParksForTheMaintainer(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Metrics: "touch ran.txt"})
	sctx.Config.Metrics = config.Metrics{Threshold: 30}

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	assertNoCoverageMaintainerPark(t, outcome, ag)
	if _, err := os.Stat(filepath.Join(dir, "ran.txt")); !os.IsNotExist(err) {
		t.Fatal("the metrics command must not run without coverage to measure against")
	}
}

func TestMetricsStep_EmptyCoverageDirectoryAlsoParksForTheMaintainer(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Metrics: "touch ran.txt"})
	sctx.Config.Metrics = config.Metrics{Threshold: 30}
	sctx.CoverageDir = t.TempDir()

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	assertNoCoverageMaintainerPark(t, outcome, ag)
	if _, err := os.Stat(filepath.Join(dir, "ran.txt")); !os.IsNotExist(err) {
		t.Fatal("the metrics command must not run against an empty coverage directory")
	}
}

// assertNoCoverageMaintainerPark pins the shared shape of both no-coverage
// parks: the maintainer decides, no agent round is spent, and the finding says
// what is missing.
func assertNoCoverageMaintainerPark(t *testing.T, outcome *pipeline.StepOutcome, ag *mockAgent) {
	t.Helper()
	if !outcome.NeedsApproval {
		t.Error("expected the metrics gate to park when the Test step produced no coverage")
	}
	if outcome.AutoFixable {
		t.Error("expected the no-coverage park to be the maintainer's, not an agent fix round's")
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("len(findings.Items) = %d, want 1", len(findings.Items))
	}
	item := findings.Items[0]
	if item.Severity != types.FindingSeverityError {
		t.Errorf("Severity = %q, want error", item.Severity)
	}
	if item.Action != types.ActionAskUser {
		t.Errorf("Action = %q, want ask_user", item.Action)
	}
	if !strings.Contains(item.Description, "coverage") {
		t.Errorf("Description = %q, want it to name the missing coverage", item.Description)
	}
	if len(ag.calls) != 0 {
		t.Errorf("expected no agent calls before the maintainer park, got %d", len(ag.calls))
	}
}

// metricsStepFixtureReport is the report every metrics step scenario's command
// echoes. Hairball breaches a threshold of 30, Fine does not.
const metricsStepFixtureReport = `{"metric":"crap","functions":[{"file":"a.go","function":"Hairball","line":42,"score":42.5,"complexity":7,"coverage":0},{"file":"b.go","function":"Fine","line":3,"score":12,"complexity":4,"coverage":0.9}]}`

// coveredMetricsContext builds a step context whose Test step is deemed to have
// produced coverage, which is what lets the metrics command run at all.
func coveredMetricsContext(t *testing.T, ag *mockAgent, dir, baseSHA, headSHA, command string, threshold float64) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Metrics: command})
	sctx.Config.Metrics = config.Metrics{Threshold: threshold}
	sctx.CoverageDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(sctx.CoverageDir, "lcov.info"), []byte("TN:\nend_of_record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return sctx
}

// echoMetricsReport renders a shell command that prints the given report.
func echoMetricsReport(report string) string {
	return "cat <<'REPORT'\n" + report + "\nREPORT"
}

func TestMetricsStep_CleanVerdictPasses(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(metricsStepFixtureReport), 100)

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when every measured function is under the threshold")
	}
	if outcome.AutoFixable {
		t.Error("expected no auto-fix on a clean verdict")
	}
	// An unparsed pass satisfies every assertion above, and it emits the
	// metrics-output-unparseable warning. An empty findings list is what
	// separates the two, so this test reds when the report stops parsing.
	if outcome.Findings != "" {
		t.Errorf("Findings = %q, want none: a measured clean verdict carries no advisory", outcome.Findings)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after the metrics gate, got %q", status)
	}
	if len(ag.calls) != 0 {
		t.Errorf("expected no agent calls on a clean verdict, got %d", len(ag.calls))
	}
}

func TestMetricsStep_BreachParksAutoFixable(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(metricsStepFixtureReport), 30)

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected the metrics gate to park on a breach")
	}
	if !outcome.AutoFixable {
		t.Error("expected a breach to be auto-fixable")
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("len(findings.Items) = %d, want 1", len(findings.Items))
	}
	item := findings.Items[0]
	if item.Severity != types.FindingSeverityError {
		t.Errorf("Severity = %q, want error", item.Severity)
	}
	if item.Action != types.ActionAutoFix {
		t.Errorf("Action = %q, want auto_fix", item.Action)
	}
	if item.File != "a.go" {
		t.Errorf("File = %q, want a.go", item.File)
	}
	if item.Line != 42 {
		t.Errorf("Line = %d, want 42", item.Line)
	}
	for _, want := range []string{"42.5", "30", "Hairball"} {
		if !strings.Contains(item.Description, want) {
			t.Errorf("Description = %q, want it to contain %q", item.Description, want)
		}
	}
}

func TestMetricsStep_AbsoluteReportedPathIsPublishedRepositoryRelative(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	report := fmt.Sprintf(
		`{"metric":"crap","functions":[{"file":%q,"function":"Hairball","line":42,"score":42.5}]}`,
		filepath.Join(dir, "a.go"),
	)
	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(report), 30)
	sctx.Config.Test.Evidence.StoreInRepo = true

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("len(findings.Items) = %d, want 1", len(findings.Items))
	}
	if got := findings.Items[0].File; got != "a.go" {
		t.Errorf("Finding.File = %q, want a.go", got)
	}
	// metrics.json reaches the orphan evidence branch unredacted, so an absolute
	// path here publishes the daemon host's worktree layout.
	evidence := readMetricsEvidence(t, sctx)
	if len(evidence.Breaches) != 1 {
		t.Fatalf("len(breaches) = %d, want 1", len(evidence.Breaches))
	}
	if got := evidence.Breaches[0].File; got != "a.go" {
		t.Errorf("evidence breach file = %q, want a.go", got)
	}
}

// A worktree reached through a symlink is named one way by the daemon and
// another by an analyser that resolved it, and the strip is a literal prefix
// match. Without the resolved root the daemon host's absolute path rides into
// Finding.File and into metrics.json, which reaches the evidence branch
// unredacted.
func TestMetricsStep_SymlinkedWorktreeStillPublishesRepositoryRelativePaths(t *testing.T) {
	t.Parallel()
	real, baseSHA, headSHA := setupGitRepo(t)
	link := filepath.Join(t.TempDir(), "worktree-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if resolved == link {
		t.Skip("the temp dir is not reached through a symlink on this host")
	}
	gitCmd(t, link, "checkout", "--detach", headSHA)

	report := fmt.Sprintf(
		`{"metric":"crap","functions":[{"file":%q,"function":"Hairball","line":42,"score":42.5}]}`,
		filepath.Join(resolved, "a.go"),
	)
	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, link, baseSHA, headSHA, echoMetricsReport(report), 30)
	sctx.Config.Metrics.ExemptPaths = []string{"vendor/**"}
	sctx.Config.Test.Evidence.StoreInRepo = true

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("len(findings.Items) = %d, want 1", len(findings.Items))
	}
	if got := findings.Items[0].File; got != "a.go" {
		t.Errorf("Finding.File = %q, want a.go: the resolved worktree root was not stripped", got)
	}
	evidence := readMetricsEvidence(t, sctx)
	if len(evidence.Breaches) != 1 {
		t.Fatalf("len(breaches) = %d, want 1", len(evidence.Breaches))
	}
	if got := evidence.Breaches[0].File; got != "a.go" {
		t.Errorf("evidence breach file = %q, want a.go", got)
	}
}

// A command that never ran names no function, so metricsFixPrompt would open
// by asserting a breach nobody measured. That decision belongs to a human.
func TestMetricsStep_MissingBinaryParksForTheMaintainer(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, "no-mistakes-crap-binary-that-does-not-exist", 30)

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a metrics command that could not run to park")
	}
	if outcome.AutoFixable {
		t.Error("AutoFixable = true, want false: no agent round can repair a command that did not run")
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("len(findings.Items) = %d, want 1", len(findings.Items))
	}
	item := findings.Items[0]
	if item.ID != metricsUnparseableFindingID {
		t.Errorf("ID = %q, want %q", item.ID, metricsUnparseableFindingID)
	}
	if item.Action != types.ActionAskUser {
		t.Errorf("Action = %q, want ask_user", item.Action)
	}
	if len(ag.calls) != 0 {
		t.Errorf("expected no agent calls, got %d", len(ag.calls))
	}
}

// metricsBreachVerdict builds a breached verdict carrying count functions, so
// the rendering rules can be read without a shell command in the way.
func metricsBreachVerdict(count int) metricsVerdict {
	verdict := metricsVerdict{Threshold: 30, Metric: "crap", FromJSON: true, Measured: count}
	for i := 0; i < count; i++ {
		verdict.Breaches = append(verdict.Breaches, metricsFunction{
			File:     "a" + strconv.Itoa(i) + ".go",
			Function: "F" + strconv.Itoa(i),
			Line:     i + 1,
			Score:    55,
		})
	}
	return verdict
}

// TestMetricsBreachFindings_FoldsTheRemainderIntoOneItem pins the cap a
// repository adopting the gate hits immediately: a findings list hundreds long
// is unreadable in the gate prompt and the PR body alike, so the tail folds into
// one item that still names how many functions it stands for.
func TestMetricsBreachFindings_FoldsTheRemainderIntoOneItem(t *testing.T) {
	t.Parallel()
	items := metricsBreachFindings(metricsBreachVerdict(maxMetricsBreachFindings + 2))

	if len(items) != maxMetricsBreachFindings+1 {
		t.Fatalf("len(items) = %d, want %d", len(items), maxMetricsBreachFindings+1)
	}
	remainder := items[len(items)-1]
	if !strings.Contains(remainder.Description, "2 more function(s)") {
		t.Errorf("remainder description = %q, want it to name the 2 folded functions", remainder.Description)
	}
	seen := map[string]bool{}
	for _, item := range items {
		if item.ID == "" {
			t.Fatalf("item %+v has no ID, so --findings cannot select it", item)
		}
		if seen[item.ID] {
			t.Fatalf("ID %q is reused, so selecting it decides more than one function", item.ID)
		}
		seen[item.ID] = true
	}
}

func TestMetricsBreachFindings_UnderTheCapReportsEveryFunction(t *testing.T) {
	t.Parallel()
	items := metricsBreachFindings(metricsBreachVerdict(maxMetricsBreachFindings))

	if len(items) != maxMetricsBreachFindings {
		t.Fatalf("len(items) = %d, want %d with no remainder item", len(items), maxMetricsBreachFindings)
	}
}

// The pair of numbers is what tells the maintainer and the fix agent which
// lever to pull, so the percent conversion has to be right and a value the
// contract does not allow must not be silently scaled into nonsense.
func TestMetricsBreachDescription_RendersComplexityAndCoverage(t *testing.T) {
	t.Parallel()
	verdict := metricsVerdict{Threshold: 30, Metric: "crap", FromJSON: true}
	complexity := 7
	for _, tc := range []struct {
		name     string
		coverage float64
		want     string
	}{
		{name: "fraction", coverage: 0.9, want: "coverage 90%"},
		{name: "zero", coverage: 0, want: "coverage 0%"},
		{name: "out of range", coverage: 85, want: "coverage 85 (outside the documented [0,1] fraction)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coverage := tc.coverage
			got := metricsBreachDescription(verdict, metricsFunction{
				File: "a.go", Function: "Hairball", Line: 42, Score: 42.5,
				Complexity: &complexity, Coverage: &coverage,
			})
			if !strings.Contains(got, "complexity 7") {
				t.Errorf("description = %q, want it to carry complexity 7", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("description = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestMetricsStep_ExemptPathClearsTheBreach(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(metricsStepFixtureReport), 30)
	sctx.Config.Metrics.ExemptPaths = []string{"a.go"}
	sctx.Config.Test.Evidence.StoreInRepo = true

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected an exempt breaching file to leave the gate clean")
	}
	// A verdict that never parsed also leaves the gate clean, so the counts are
	// what prove the glob was consulted: the fixture reports two functions, one
	// of them the breaching a.go the exemption waives.
	evidence := readMetricsEvidence(t, sctx)
	if evidence.Measured != 2 {
		t.Errorf("measured = %d, want 2", evidence.Measured)
	}
	if evidence.Exempted != 1 {
		t.Errorf("exempted = %d, want 1: the a.go glob did not waive the breach", evidence.Exempted)
	}
	if evidence.Breached {
		t.Error("breached = true, want false once the only breaching file is exempt")
	}
	if !evidence.FromJSON {
		t.Error("from_json = false, want true: the verdict came from the exit code, not the report")
	}
}

func TestMetricsStep_UnparseableOutputWithANonzeroExitParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, "echo 'crap: boom on stderr' >&2; echo 'crap: boom on stdout'; exit 2", 30)

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected a nonzero exit to park the gate")
	}
	if outcome.AutoFixable {
		t.Error("expected the fallback verdict to park for the maintainer, not an agent round")
	}
	if outcome.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", outcome.ExitCode)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("len(findings.Items) = %d, want 1", len(findings.Items))
	}
	item := findings.Items[0]
	if item.Severity != types.FindingSeverityWarning {
		t.Errorf("Severity = %q, want warning", item.Severity)
	}
	if !strings.Contains(item.Description, "exited 2") {
		t.Errorf("Description = %q, want it to name exit code 2", item.Description)
	}
	if !strings.Contains(item.Description, "did not parse") {
		t.Errorf("Description = %q, want it to say the output did not parse", item.Description)
	}
	// Only stdout is parsed, but a diagnosis needs the other half: the reason a
	// command failed is usually the text it wrote to stderr.
	if !strings.Contains(findings.Summary, "boom on stderr") {
		t.Errorf("Summary = %q, want the stderr text to survive the stream split", findings.Summary)
	}
	if !strings.Contains(findings.Summary, "boom on stdout") {
		t.Errorf("Summary = %q, want the stdout text to survive the stream split", findings.Summary)
	}
}

func TestMetricsStep_UnparseableOutputWithAZeroExitPasses(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, "echo 'nothing to report'", 30)

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected a zero exit to pass the gate even with nothing to parse")
	}
}

// TestMetricsStep_PassingUnparseableOutputCarriesAWarningFinding pins the
// pass-side counterpart of the breach path's honesty finding. Nothing was
// measured, so the operator has to read that on the durable record and in the
// PR body rather than only in the step log.
func TestMetricsStep_PassingUnparseableOutputCarriesAWarningFinding(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, "echo 'nothing to report'", 30)

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("the warning must not park the gate: the exit code was clean")
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("len(findings.Items) = %d, want 1: %s", len(findings.Items), outcome.Findings)
	}
	item := findings.Items[0]
	if item.Severity != types.FindingSeverityWarning {
		t.Errorf("Severity = %q, want warning", item.Severity)
	}
	if item.Action != types.ActionNoOp {
		t.Errorf("Action = %q, want no-op", item.Action)
	}
	if !strings.Contains(item.Description, "did not parse") {
		t.Errorf("Description = %q, want it to say the output did not parse", item.Description)
	}
}

// TestMetricsStep_ParsedReportWithANonzeroExitParksForTheMaintainer pins the
// fail-closed half of the verdict at the step level: a report that reads clean
// followed by a crash still blocks, and the gate says the report may be
// partial. It names no breaching function, so there is nothing for an agent to
// bring under the threshold and the decision is the maintainer's.
func TestMetricsStep_ParsedReportWithANonzeroExitParksForTheMaintainer(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	command := echoMetricsReport(`{"metric":"crap","functions":[{"file":"a.go","function":"Fine","line":3,"score":12}]}`) + "\nexit 3"
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, command, 30)

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a nonzero exit to park even with a clean-looking report")
	}
	if outcome.AutoFixable {
		t.Error("AutoFixable = true, want false: the report named no breaching function")
	}
	if outcome.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", outcome.ExitCode)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("len(findings.Items) = %d, want 1: %s", len(findings.Items), outcome.Findings)
	}
	item := findings.Items[0]
	if item.Severity != types.FindingSeverityWarning {
		t.Errorf("Severity = %q, want warning", item.Severity)
	}
	if item.Action != types.ActionAskUser {
		t.Errorf("Action = %q, want ask_user", item.Action)
	}
	for _, want := range []string{"exited 3", "may be partial", "no function breached"} {
		if !strings.Contains(item.Description, want) {
			t.Errorf("Description = %q, want it to contain %q", item.Description, want)
		}
	}
}

func TestMetricsStep_CommandReceivesTheCoverageRootAndTheChangedFileSet(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	// The probe writes outside the worktree so the observation itself cannot
	// dirty the tree the step is gating.
	probe := filepath.Join(t.TempDir(), "env.txt")
	command := "printf '%s|%s|%s|%s|%s' \"$NO_MISTAKES_COVERAGE_ROOT\" \"$NO_MISTAKES_BASE_SHA\" \"$NO_MISTAKES_CHANGED_FILE_COUNT\" \"$NO_MISTAKES_CHANGED_FILES\" \"${NO_MISTAKES_COVERAGE_DIR:-unset}\" > " + probe + "\n" +
		echoMetricsReport(`{"metric":"crap","functions":[]}`)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, command, 30)

	step := &MetricsStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(string(raw), "|")
	if len(fields) != 5 {
		t.Fatalf("probe wrote %q, want 5 fields", raw)
	}
	if fields[0] != sctx.CoverageDir {
		t.Errorf("NO_MISTAKES_COVERAGE_ROOT = %q, want %q", fields[0], sctx.CoverageDir)
	}
	if fields[1] != baseSHA {
		t.Errorf("NO_MISTAKES_BASE_SHA = %q, want %q", fields[1], baseSHA)
	}
	// The exact list and the exact count, because a regression to an empty list
	// still writes a non-empty "0" into the count.
	wantChanged := gitCmd(t, dir, "diff", "--name-only", baseSHA, headSHA)
	if wantChanged == "" {
		t.Fatal("the fixture's base..head diff is empty, so this test proves nothing")
	}
	if fields[3] != wantChanged {
		t.Errorf("NO_MISTAKES_CHANGED_FILES = %q, want %q", fields[3], wantChanged)
	}
	if want := strconv.Itoa(len(strings.Split(wantChanged, "\n"))); fields[2] != want {
		t.Errorf("NO_MISTAKES_CHANGED_FILE_COUNT = %q, want %q", fields[2], want)
	}
	if fields[4] != "unset" {
		t.Errorf("NO_MISTAKES_COVERAGE_DIR = %q, want it unset: that name is the Test step's per-unit write target", fields[4])
	}
}

// TestMetricsStep_GreenAttemptReportsTheDroppedChangedFileList proves the
// omission reaches the durable record and not only the run log. A metrics
// command that scopes its analysis to NO_MISTAKES_CHANGED_FILES measured less
// than the change, so a green verdict still has to say so.
func TestMetricsStep_GreenAttemptReportsTheDroppedChangedFileList(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)

	// One path per file whose names together exceed the cap, so
	// changedFilesEnvValue empties the value rather than truncating it.
	long := strings.Repeat("n", 200)
	for i := 0; i < 600; i++ {
		name := filepath.Join(dir, long+"-"+strconv.Itoa(i)+".go")
		if err := os.WriteFile(name, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "many files")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(`{"metric":"crap","functions":[]}`), 30)

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("the omission must not park a clean verdict")
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("len(findings.Items) = %d, want 1: %s", len(findings.Items), outcome.Findings)
	}
	item := findings.Items[0]
	if item.Severity != types.FindingSeverityWarning {
		t.Errorf("Severity = %q, want warning", item.Severity)
	}
	if !strings.Contains(item.Description, envTestChangedFiles) {
		t.Errorf("Description = %q, want it to name the dropped changed-file list", item.Description)
	}
}

// TestMetricsStep_StderrDoesNotCorruptTheReport pins the read source. A command
// that writes progress to stderr while its report is still going out on stdout
// had that line spliced into the middle of the report by the shared capture
// buffer, and the corrupted report failed OPEN onto the exit code.
func TestMetricsStep_StderrDoesNotCorruptTheReport(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	command := `printf '%s' '{"metric":"crap","functions":[{"file":"a.go","function":"Hairball","line":42,"score":42.5}'` + "\n" +
		`printf 'measuring a.go\n' >&2` + "\n" +
		`printf '%s\n' ']}'`

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, command, 30)

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected the breach in the report to park the gate")
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("len(findings.Items) = %d, want the one breaching function: %s", len(findings.Items), outcome.Findings)
	}
	if item := findings.Items[0]; !strings.Contains(item.Description, "Hairball") {
		t.Errorf("Description = %q, want the parsed per-function breach rather than an exit-code fallback", item.Description)
	}
}

// TestMetricsStep_FixModeMeasuresTheUntrackedRepairFile pins the ordering. A
// metrics repair commonly writes a new test file, and reading the changed-file
// set before the fix round left that file out of the set the command measures,
// so the re-run scored the same tree the round was meant to change.
func TestMetricsStep_FixModeMeasuresTheUntrackedRepairFile(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "hairball_test.go"), []byte("package main\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"add tests for Hairball"}`)}, nil
		},
	}
	probe := filepath.Join(t.TempDir(), "changed.txt")
	command := "printf '%s' \"$NO_MISTAKES_CHANGED_FILES\" > " + probe + "\n" +
		echoMetricsReport(`{"metric":"crap","functions":[]}`)
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, command, 30)
	sctx.Fixing = true

	step := &MetricsStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "hairball_test.go") {
		t.Errorf("NO_MISTAKES_CHANGED_FILES = %q, want the repair's new file in it", raw)
	}
}

// TestMetricsBreachFindings_IDsAreStableAcrossAReorder pins what a breach ID
// identifies. The list is sorted by score, so a positional ID names a different
// function as scores move between rounds and an operator's `--findings`
// selection then silently decides the wrong one.
func TestMetricsBreachFindings_IDsAreStableAcrossAReorder(t *testing.T) {
	t.Parallel()
	hairball := metricsFunction{File: "internal/a.go", Function: "Hairball", Line: 42, Score: 55}
	tangle := metricsFunction{File: "internal/b.go", Function: "Tangle", Line: 7, Score: 44}

	idsFor := func(breaches ...metricsFunction) map[string]string {
		items := metricsBreachFindings(metricsVerdict{
			Threshold: 30, Metric: "crap", FromJSON: true, Breaches: breaches,
		})
		byFunction := map[string]string{}
		for i, item := range items {
			byFunction[breaches[i].Function] = item.ID
		}
		return byFunction
	}

	first := idsFor(hairball, tangle)
	// The next round improves Hairball's coverage, so Tangle sorts first.
	second := idsFor(tangle, hairball)

	for _, name := range []string{"Hairball", "Tangle"} {
		if first[name] == "" {
			t.Fatalf("%s got no ID", name)
		}
		if first[name] != second[name] {
			t.Errorf("%s ID = %q then %q, want one stable ID across the reorder", name, first[name], second[name])
		}
	}
	if first["Hairball"] == first["Tangle"] {
		t.Errorf("both functions share the ID %q, so selecting it decides both", first["Hairball"])
	}
}

// Every item this step emits has to be selectable on its own, which means an
// explicit ID: a blank one is reachable only through the positional ID
// types.NormalizeFindings assigns, and that moves with the list around it.
func TestMetricsBreachFindings_EveryItemCarriesAnID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		verdict metricsVerdict
	}{
		{name: "unparseable output", verdict: metricsVerdict{Threshold: 30, ExitCode: 2}},
		{name: "partial report", verdict: func() metricsVerdict {
			verdict := metricsBreachVerdict(2)
			verdict.ExitCode = 3
			return verdict
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := metricsBreachFindings(tc.verdict)
			if len(items) == 0 {
				t.Fatal("no items")
			}
			for _, item := range items {
				if item.ID == "" {
					t.Errorf("item %+v has no ID, so --findings cannot select it", item)
				}
			}
		})
	}
}

func TestMetricsStep_FixModeCommitsTheAgentRepairAndRestartsFromFormat(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "metrics-repair.txt"), []byte("repaired"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"add tests for Hairball"}`)}, nil
		},
	}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(`{"metric":"crap","functions":[]}`), 30)
	sctx.Fixing = true

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	if outcome.RestartFrom != types.StepFormat {
		t.Errorf("RestartFrom = %q, want %q: an agent-authored commit re-enters validation", outcome.RestartFrom, types.StepFormat)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after the fix commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(metrics): add tests for Hairball" {
		t.Fatalf("last commit message = %q", got)
	}
}

// TestMetricsStep_FixPromptRequiresTheAgentToNameTestsOrComplexity pins the one
// rule the metrics fix prompt exists to carry. Coverage enters the CRAP formula
// cubed, so adding tests to a hairball is the cheap remedy the formula
// over-rewards and the maintainer has to be able to see which lever was pulled.
func TestMetricsStep_FixPromptRequiresTheAgentToNameTestsOrComplexity(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"summary":"reduce complexity of Hairball"}`)}, nil
		},
	}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(`{"metric":"crap","functions":[]}`), 30)
	sctx.Fixing = true

	step := &MetricsStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	const rule = "State in your summary whether you added tests or reduced complexity"
	if !strings.Contains(ag.calls[0].Prompt, rule) {
		t.Errorf("fix prompt does not carry the lever rule %q, prompt was:\n%s", rule, ag.calls[0].Prompt)
	}
}

func TestMetricsStep_FixModeCarriesPreviousFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"summary":"add tests for Hairball"}`)}, nil
		},
	}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(`{"metric":"crap","functions":[]}`), 30)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"severity":"error","description":"crap Hairball scores 42.5 UNIQUE-METRICS-MARKER"}],"summary":""}`

	step := &MetricsStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "UNIQUE-METRICS-MARKER") {
		t.Error("expected the fix prompt to carry the previous metrics findings")
	}
}

// metricsEvidenceFile is the published shape metrics.json decodes into. The
// test decodes into its own struct rather than the step's, so a silent rename
// of a json tag fails here.
type metricsEvidenceFile struct {
	Metric    string  `json:"metric"`
	Threshold float64 `json:"threshold"`
	Measured  int     `json:"measured"`
	Exempted  int     `json:"exempted"`
	Breached  bool    `json:"breached"`
	FromJSON  bool    `json:"from_json"`
	ExitCode  int     `json:"exit_code"`
	Summary   string  `json:"summary"`
	Breaches  []struct {
		File     string  `json:"file"`
		Function string  `json:"function"`
		Line     int     `json:"line"`
		Score    float64 `json:"score"`
	} `json:"breaches"`
}

// readMetricsEvidence decodes the metrics.json the step published. The caller
// must have enabled evidence storage on the context.
func readMetricsEvidence(t *testing.T, sctx *pipeline.StepContext) metricsEvidenceFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testEvidenceDir(sctx), "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got metricsEvidenceFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestMetricsStep_WritesTheVerdictToTheEvidenceDirWhenEvidenceIsEnabled(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(metricsStepFixtureReport), 30)
	sctx.Config.Test.Evidence.StoreInRepo = true

	step := &MetricsStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(testEvidenceDir(sctx), "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got metricsEvidenceFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Metric != "crap" {
		t.Errorf("metric = %q, want crap", got.Metric)
	}
	if got.Threshold != 30 {
		t.Errorf("threshold = %v, want 30", got.Threshold)
	}
	if got.Measured != 2 {
		t.Errorf("measured = %d, want 2", got.Measured)
	}
	if !got.Breached {
		t.Error("breached = false, want true")
	}
	if len(got.Breaches) != 1 {
		t.Fatalf("len(breaches) = %d, want 1", len(got.Breaches))
	}
	if got.Breaches[0].Function != "Hairball" {
		t.Errorf("breaches[0].function = %q, want Hairball", got.Breaches[0].Function)
	}
}

func TestMetricsStep_WritesNoEvidenceWhenEvidenceIsDisabled(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(metricsStepFixtureReport), 30)
	sctx.Config.Test.Evidence.StoreInRepo = false

	step := &MetricsStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(testEvidenceDir(sctx), "metrics.json")); !os.IsNotExist(err) {
		t.Fatal("expected no metrics.json when evidence storage is off")
	}
}

// TestMetricsStep_DoesNotPublishRawCoverage pins that coverage stays where the
// Test step wrote it. evidence.Publish copies the whole evidence directory, so
// anything the metrics step put there reaches the evidence branch.
func TestMetricsStep_DoesNotPublishRawCoverage(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(metricsStepFixtureReport), 30)
	sctx.Config.Test.Evidence.StoreInRepo = true

	step := &MetricsStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(testEvidenceDir(sctx))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 1 || names[0] != "metrics.json" {
		t.Fatalf("evidence dir holds %v, want only metrics.json: raw coverage must never be published", names)
	}
}

// TestMetricsStep_CleanVerdictEvidenceRendersAnEmptyBreachList pins the wire
// shape of an unbreached verdict. metricsVerdict.Breaches is nil when nothing
// breaches, and a nil slice marshals to null, so the exported struct normalises
// it to an empty slice and a consumer can iterate metrics.json without a nil
// check.
func TestMetricsStep_CleanVerdictEvidenceRendersAnEmptyBreachList(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(metricsStepFixtureReport), 100)
	sctx.Config.Test.Evidence.StoreInRepo = true

	step := &MetricsStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(testEvidenceDir(sctx), "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got metricsEvidenceFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Breached {
		t.Error("breached = true, want false")
	}
	if got.Measured != 2 {
		t.Errorf("measured = %d, want 2", got.Measured)
	}
	// The file is written indented so a maintainer can read it on the evidence
	// branch, so compact it first and assert the shape whitespace-independently.
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(compact.String(), `"breaches":[]`) {
		t.Errorf("metrics.json does not render an empty breach list, got:\n%s", raw)
	}
	if strings.Contains(compact.String(), `"breaches":null`) {
		t.Errorf("metrics.json renders a null breach list, got:\n%s", raw)
	}
}
