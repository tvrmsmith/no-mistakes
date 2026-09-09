package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestMetricsStep_ExemptPathClearsTheBreach(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, echoMetricsReport(metricsStepFixtureReport), 30)
	sctx.Config.Metrics.ExemptPaths = []string{"a.go"}

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected an exempt breaching file to leave the gate clean")
	}
}

func TestMetricsStep_UnparseableOutputWithANonzeroExitParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := coveredMetricsContext(t, ag, dir, baseSHA, headSHA, "echo 'crap: boom' >&2; echo 'crap: boom'; exit 2", 30)

	step := &MetricsStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected a nonzero exit to park the gate")
	}
	if !outcome.AutoFixable {
		t.Error("expected the fallback verdict to be auto-fixable")
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

func TestMetricsStep_CommandReceivesTheCoverageRootAndTheChangedFileSet(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	// The probe writes outside the worktree so the observation itself cannot
	// dirty the tree the step is gating.
	probe := filepath.Join(t.TempDir(), "env.txt")
	command := "printf '%s|%s|%s|%s' \"$NO_MISTAKES_COVERAGE_ROOT\" \"$NO_MISTAKES_BASE_SHA\" \"$NO_MISTAKES_CHANGED_FILE_COUNT\" \"${NO_MISTAKES_COVERAGE_DIR:-unset}\" > " + probe + "\n" +
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
	if len(fields) != 4 {
		t.Fatalf("probe wrote %q, want 4 fields", raw)
	}
	if fields[0] != sctx.CoverageDir {
		t.Errorf("NO_MISTAKES_COVERAGE_ROOT = %q, want %q", fields[0], sctx.CoverageDir)
	}
	if fields[1] != baseSHA {
		t.Errorf("NO_MISTAKES_BASE_SHA = %q, want %q", fields[1], baseSHA)
	}
	if fields[2] == "" {
		t.Error("NO_MISTAKES_CHANGED_FILE_COUNT is empty, want the changed-file total")
	}
	if fields[3] != "unset" {
		t.Errorf("NO_MISTAKES_COVERAGE_DIR = %q, want it unset: that name is the Test step's per-unit write target", fields[3])
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
