package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// newUnitRepo builds a git repo with a base commit containing services/api and
// services/web, so a later commit can change one unit's files without any
// noise from setupGitRepo's own feature.txt landing in the diff.
func newUnitRepo(t *testing.T) (dir, baseSHA string) {
	t.Helper()
	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	// git init inherits the developer's global config, so a machine that signs
	// every commit would make this fixture depend on a signing agent being
	// unlocked.
	gitCmd(t, dir, "config", "commit.gpgsign", "false")
	// newTestContext's StepContext reports "main" as the default branch, and
	// resolveBranchBaseSHA prefers a real merge-base against it over the
	// fallback base SHA. Naming the base branch "main" and moving to a
	// separate "feature" branch for later commits (mirroring setupGitRepo)
	// keeps that merge-base pointed at the true base rather than at HEAD.
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "services", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "services", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "services", "api", "main.go"), []byte("package api\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "services", "web", "main.go"), []byte("package web\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "-b", "feature")
	return dir, baseSHA
}

// changeUnitFile appends a line to path (repository-relative) and commits it,
// returning the new HEAD.
func changeUnitFile(t *testing.T, dir, path string) (headSHA string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(path))
	f, err := os.OpenFile(full, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("// changed\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "change "+path)
	return gitCmd(t, dir, "rev-parse", "HEAD")
}

// markerCommand writes a marker file. The Windows shard runs this package
// through cmd.exe, which has no touch and reports 9009 rather than the exit
// code a test asserts, so the command has to be one both shells accept.
func markerCommand(marker string) string {
	return `echo ran > "` + marker + `"`
}

// unitTestContext builds a StepContext with Shared wired, as the executor
// does, so discovery caching and scope-fault counting behave like production.
func unitTestContext(t *testing.T, ag agent.Agent, workDir, baseSHA, headSHA string, units []config.TestUnit) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContext(t, ag, workDir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Test.Units = units
	sctx.Shared = &pipeline.RunShared{}
	return sctx
}

func capturingLog(sctx *pipeline.StepContext) *[]string {
	var lines []string
	var mu sync.Mutex
	sctx.Log = func(s string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, s)
	}
	return &lines
}

func joinedLog(lines []string) string {
	return strings.Join(lines, "\n")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// jsonString renders s as a JSON string literal, quotes included, so a shell
// command carrying quotes or Windows backslashes can be embedded in a fixture
// agent answer.
func jsonString(t *testing.T, s string) string {
	t.Helper()
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestTestStep_RunsOnlyTheChangedUnitsCommand(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	markerDir := t.TempDir()
	apiMarker := filepath.Join(markerDir, "api.done")
	webMarker := filepath.Join(markerDir, "web.done")

	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	units := []config.TestUnit{
		{Name: "api", Path: "services/api", Command: markerCommand(apiMarker)},
		{Name: "web", Path: "services/web", Command: markerCommand(webMarker)},
	}
	sctx := unitTestContext(t, nil, dir, baseSHA, headSHA, units)

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome parked unexpectedly: %s", outcome.Findings)
	}
	if !fileExists(apiMarker) {
		t.Error("api marker not created, expected api unit to run")
	}
	if fileExists(webMarker) {
		t.Error("web marker created, expected web unit not to run")
	}
}

func TestTestStep_LogsTheSelectedUnitsAndEachCommand(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	markerDir := t.TempDir()
	apiMarker := filepath.Join(markerDir, "api.done")
	apiCmd := markerCommand(apiMarker)

	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	units := []config.TestUnit{
		{Name: "api", Path: "services/api", Command: apiCmd},
		{Name: "web", Path: "services/web", Command: "exit 0"},
	}
	sctx := unitTestContext(t, nil, dir, baseSHA, headSHA, units)
	lines := capturingLog(sctx)

	step := &TestStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	log := joinedLog(*lines)
	if !strings.Contains(log, "selected test units (config): api") {
		t.Errorf("log missing selection line, got:\n%s", log)
	}
	if !strings.Contains(log, "unit api: "+apiCmd) {
		t.Errorf("log missing unit command line, got:\n%s", log)
	}
}

// decodeFindings reads a step outcome's findings payload, the durable record a
// reviewer and the PR body read, as opposed to the run log.
func decodeFindings(t *testing.T, payload string) Findings {
	t.Helper()
	var findings Findings
	if err := json.Unmarshal([]byte(payload), &findings); err != nil {
		t.Fatalf("decode findings %q: %v", payload, err)
	}
	return findings
}

func TestTestStep_GreenOutcomeNamesTheUnitCommandsItRan(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	apiCmd := markerCommand(filepath.Join(t.TempDir(), "api.done"))

	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	units := []config.TestUnit{
		{Name: "api", Path: "services/api", Command: apiCmd},
		{Name: "web", Path: "services/web", Command: "exit 0"},
	}
	sctx := unitTestContext(t, nil, dir, baseSHA, headSHA, units)

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	tested := decodeFindings(t, outcome.Findings).Tested
	if len(tested) != 1 || tested[0] != apiCmd {
		t.Fatalf("Tested = %v, want [%s]", tested, apiCmd)
	}
}

func TestTestStep_DiscoveryAgentInvocationFailureFailsTheRun(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return nil, errors.New("transport exploded")
		},
	}
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err == nil {
		t.Fatalf("expected the run to fail, got outcome: %+v", outcome)
	}
	if outcome != nil {
		t.Fatalf("expected no outcome alongside the error, got: %+v", outcome)
	}
	if !strings.Contains(err.Error(), "transport exploded") {
		t.Errorf("error should carry the agent's own failure, got: %v", err)
	}
}

func TestTestStep_ConfiguredLayoutOwningNoChangedFileFallsBackToTheEvidenceAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	markerDir := t.TempDir()
	apiMarker := filepath.Join(markerDir, "api.done")

	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"items":[],"testing_summary":"exercised the change by hand"}`)}, nil
		},
	}
	// Both units sit outside the changed file's directory, so the selection is
	// empty and there is no unit command to prove anything.
	units := []config.TestUnit{
		{Name: "docs", Path: "docs", Command: markerCommand(apiMarker)},
		{Name: "web", Path: "services/web", Command: "exit 1"},
	}
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, units)
	lines := capturingLog(sctx)

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected no approval for a clean evidence pass, got: %s", outcome.Findings)
	}
	if fileExists(apiMarker) {
		t.Error("an unselected unit's command ran")
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1 evidence pass", len(ag.calls))
	}
	// No unit command produced a baseline, so the pass runs the tests itself.
	if !strings.Contains(ag.calls[0].Prompt, "run the smallest relevant tests yourself") {
		t.Errorf("evidence prompt missing the unbaselined opening, got:\n%s", ag.calls[0].Prompt)
	}
	if strings.Contains(ag.calls[0].Prompt, "already ran to completion and passed") {
		t.Errorf("evidence prompt claims a baseline that never ran, got:\n%s", ag.calls[0].Prompt)
	}
	if !strings.Contains(joinedLog(*lines), "no test units selected for the changed files") {
		t.Errorf("log missing the empty-selection line, got:\n%s", joinedLog(*lines))
	}
}

func TestTestStep_EvidencePassAfterUnitCommandsJudgesThemInsteadOfRerunning(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	var evidencePrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "Derive this repository's independently testable units") {
				return &agent.Result{Output: json.RawMessage(`{"units":[{"name":"api","path":"services/api","command":"exit 0"}],"selected":["api"]}`)}, nil
			}
			evidencePrompt = opts.Prompt
			return &agent.Result{Output: json.RawMessage(`{"items":[]}`)}, nil
		},
	}
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)

	step := &TestStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if evidencePrompt == "" {
		t.Fatal("the evidence pass did not run")
	}
	if strings.Contains(evidencePrompt, "run the smallest relevant tests yourself") {
		t.Errorf("evidence prompt still asks the agent to run the tests again:\n%s", evidencePrompt)
	}
	if !strings.Contains(evidencePrompt, "do NOT run them again") {
		t.Errorf("evidence prompt does not bind the agent to the results that already ran:\n%s", evidencePrompt)
	}
}

func TestTestStep_FixModeRunsOnlyTheChangedUnitsCommand(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	markerDir := t.TempDir()
	apiMarker := filepath.Join(markerDir, "api.done")
	webMarker := filepath.Join(markerDir, "web.done")

	// Fix mode diffs against the base alone, so the repair commit and the
	// original change are both in the changed set.
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix the api test"}`)}, nil
		},
	}
	units := []config.TestUnit{
		{Name: "api", Path: "services/api", Command: markerCommand(apiMarker)},
		{Name: "web", Path: "services/web", Command: markerCommand(webMarker)},
	}
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, units)
	sctx.Fixing = true

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.FixSummary != "fix the api test" {
		t.Errorf("FixSummary = %q, want the repair agent's summary", outcome.FixSummary)
	}
	if !fileExists(apiMarker) {
		t.Error("api marker not created, expected the changed unit to run in fix mode")
	}
	if fileExists(webMarker) {
		t.Error("web marker created, expected the untouched unit not to run in fix mode")
	}
}

func TestTestStep_DiscoveryFailureParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"units":[{"name":"api","path":"services/api","command":""}],"selected":["api"]}`)}, nil
		},
	}
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected NeedsApproval, discovery failure should park")
	}
	if outcome.AutoFixable {
		t.Fatal("expected AutoFixable false for a discovery failure")
	}
	if !strings.Contains(outcome.Findings, `discovered unit \"api\" has no test command`) {
		t.Errorf("findings missing discovery error, got: %s", outcome.Findings)
	}
}

func TestTestStep_UnderSelectionExpandsRunsTheMissingUnitAndLogsBoth(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	markerDir := t.TempDir()
	apiMarker := filepath.Join(markerDir, "api.done")
	webMarker := filepath.Join(markerDir, "web.done")

	f, err := os.OpenFile(filepath.Join(dir, "services", "web", "main.go"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("// changed\n")
	f.Close()
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "change web too")
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			out := `{"units":[{"name":"api","path":"services/api","command":` + jsonString(t, markerCommand(apiMarker)) + `},{"name":"web","path":"services/web","command":` + jsonString(t, markerCommand(webMarker)) + `}],"selected":["api"]}`
			return &agent.Result{Output: json.RawMessage(out)}, nil
		},
	}
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)
	lines := capturingLog(sctx)

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected the run not to park, got: %s", outcome.Findings)
	}
	if !fileExists(apiMarker) {
		t.Error("api marker not created")
	}
	if !fileExists(webMarker) {
		t.Error("web marker not created, expected under-selection to expand and run it")
	}

	log := joinedLog(*lines)
	if !strings.Contains(log, "test scope fault: original selection api") {
		t.Errorf("log missing original-selection line, got:\n%s", log)
	}
	if !strings.Contains(log, "expanding selection with web") {
		t.Errorf("log missing expansion line, got:\n%s", log)
	}
}

// runStore is an in-memory stand-in for the run row's discovery column, so a
// restart can be modelled without a database.
type runStore struct {
	mu   sync.Mutex
	rows map[string]string
}

func (r *runStore) GetRunTestDiscovery(id string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rows[id], nil
}

func (r *runStore) SetRunTestDiscovery(id, state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows == nil {
		r.rows = map[string]string{}
	}
	r.rows[id] = state
	return nil
}

func TestTestStep_RecoveredRunReusesTheDiscoveredLayout(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	markerDir := t.TempDir()
	apiMarker := filepath.Join(markerDir, "api.done")
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	store := &runStore{}
	var discoveryPasses int32

	newAgent := func() *mockAgent {
		return &mockAgent{
			name: "test",
			runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if strings.Contains(opts.Prompt, "Derive this repository's independently testable units") {
					atomic.AddInt32(&discoveryPasses, 1)
					out := `{"units":[{"name":"api","path":"services/api","command":` + jsonString(t, markerCommand(apiMarker)) + `}],"selected":["api"]}`
					return &agent.Result{Output: json.RawMessage(out)}, nil
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[]}`)}, nil
			},
		}
	}

	first := unitTestContext(t, newAgent(), dir, baseSHA, headSHA, nil)
	first.Shared = pipeline.NewRunShared(store, "run-1")
	if _, err := (&TestStep{}).Execute(first); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&discoveryPasses); got != 1 {
		t.Fatalf("first attempt discovery passes = %d, want 1", got)
	}
	os.Remove(apiMarker)

	// The daemon restarted; the executor restores the run's shared state rather
	// than starting empty.
	second := unitTestContext(t, newAgent(), dir, baseSHA, headSHA, nil)
	second.Shared = pipeline.RestoreRunShared(store, "run-1")
	outcome, err := (&TestStep{}).Execute(second)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("recovered run parked: %s", outcome.Findings)
	}
	if got := atomic.LoadInt32(&discoveryPasses); got != 1 {
		t.Fatalf("discovery passes after recovery = %d, want the layout reused", got)
	}
	if !fileExists(apiMarker) {
		t.Error("recovered run did not run the reused unit's command")
	}
}

func TestTestStep_SecondScopeFaultInOneRunParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	markerDir := t.TempDir()
	apiMarker := filepath.Join(markerDir, "api.done")
	webMarker := filepath.Join(markerDir, "web.done")

	f, err := os.OpenFile(filepath.Join(dir, "services", "web", "main.go"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("// changed\n")
	f.Close()
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "change web too")
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			out := `{"units":[{"name":"api","path":"services/api","command":` + jsonString(t, markerCommand(apiMarker)) + `},{"name":"web","path":"services/web","command":` + jsonString(t, markerCommand(webMarker)) + `}],"selected":["api"]}`
			return &agent.Result{Output: json.RawMessage(out)}, nil
		},
	}
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)
	sctx.Shared.NoteTestScopeFault()

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected the run to park on a second scope fault")
	}
	if fileExists(webMarker) {
		t.Error("web marker created, expected the run to park instead of running the missing unit")
	}
}

// TestTestStep_RunsEachUnitExactlyOncePerAttempt drives a selection that names
// the same unit twice, which is the reachable way a unit could run twice:
// discovery validates every selected name against the layout but does not
// de-duplicate the selection, so only the per-attempt ran set stops the second
// visit from re-running the command.
func TestTestStep_RunsEachUnitExactlyOncePerAttempt(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	logFile := filepath.Join(t.TempDir(), "runs.log")
	appendCommand := `echo run >> "` + logFile + `"`

	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			out := `{"units":[{"name":"api","path":"services/api","command":` + jsonString(t, appendCommand) + `},{"name":"web","path":"services/web","command":"exit 0"}],"selected":["api","api"]}`
			return &agent.Result{Output: json.RawMessage(out)}, nil
		},
	}
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)

	step := &TestStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(content), "\r\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one run, got %d: %v", len(lines), lines)
	}
}

func TestTestStep_PassesBaseSHAAndChangedFilesToTheCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command reads the variables through POSIX shell interpolation")
	}
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	outFile := filepath.Join(t.TempDir(), "env.out")

	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	units := []config.TestUnit{
		{Name: "api", Path: "services/api", Command: `printf '%s\n' "$NO_MISTAKES_BASE_SHA" > ` + outFile + `; printf '%s\n' "$NO_MISTAKES_CHANGED_FILES" >> ` + outFile + `; printf 'count=%s\n' "$NO_MISTAKES_CHANGED_FILE_COUNT" >> ` + outFile},
	}
	sctx := unitTestContext(t, nil, dir, baseSHA, headSHA, units)

	step := &TestStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(content)
	if !strings.Contains(got, baseSHA) {
		t.Errorf("output missing base SHA %q, got: %s", baseSHA, got)
	}
	if !strings.Contains(got, "services/api/main.go") {
		t.Errorf("output missing changed path, got: %s", got)
	}
	if !strings.Contains(got, "count=1") {
		t.Errorf("output missing the changed-file count, got: %s", got)
	}
}

func TestTestStep_FailingUnitCommandParksAutoFixable(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	units := []config.TestUnit{
		{Name: "api", Path: "services/api", Command: "exit 1"},
		{Name: "web", Path: "services/web", Command: "exit 0"},
	}
	sctx := unitTestContext(t, nil, dir, baseSHA, headSHA, units)

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected NeedsApproval on a failing unit command")
	}
	if !outcome.AutoFixable {
		t.Fatal("expected AutoFixable true on a failing unit command")
	}
	if outcome.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", outcome.ExitCode)
	}
	if !strings.Contains(outcome.Findings, "unit api: tests failed with exit code 1") {
		t.Errorf("findings missing unit-prefixed description, got: %s", outcome.Findings)
	}
}

func TestTestStep_ConfiguredCommandStillBehavesAsOneRepositoryUnit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Test: "exit 1"})
	sctx.Shared = &pipeline.RunShared{}
	lines := capturingLog(sctx)

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable {
		t.Fatalf("expected a parked, auto-fixable failure, got: %+v", outcome)
	}
	if outcome.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", outcome.ExitCode)
	}
	if !strings.Contains(outcome.Findings, `"description":"tests failed with exit code 1"`) {
		t.Errorf("findings should carry the bare description with no unit prefix, got: %s", outcome.Findings)
	}

	log := joinedLog(*lines)
	if !strings.Contains(log, "selected test units (command): repository") {
		t.Errorf("log missing command-source selection line, got:\n%s", log)
	}
}
