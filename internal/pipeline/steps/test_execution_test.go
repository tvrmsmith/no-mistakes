package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func markerCommand(marker string) string {
	return "touch " + marker
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
		{Name: "web", Path: "services/web", Command: "true"},
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
			out := `{"units":[{"name":"api","path":"services/api","command":"` + markerCommand(apiMarker) + `"},{"name":"web","path":"services/web","command":"` + markerCommand(webMarker) + `"}],"selected":["api"]}`
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
			out := `{"units":[{"name":"api","path":"services/api","command":"` + markerCommand(apiMarker) + `"},{"name":"web","path":"services/web","command":"` + markerCommand(webMarker) + `"}],"selected":["api"]}`
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

func TestTestStep_RunsEachUnitExactlyOncePerAttempt(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	logFile := filepath.Join(t.TempDir(), "runs.log")

	f, err := os.OpenFile(filepath.Join(dir, "services", "api", "extra.go"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("package api\n")
	f.Close()
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add extra file")
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	units := []config.TestUnit{
		{Name: "api", Path: "services/api", Command: "echo run >> " + logFile},
	}
	sctx := unitTestContext(t, nil, dir, baseSHA, headSHA, units)

	step := &TestStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one run, got %d: %v", len(lines), lines)
	}
}

func TestTestStep_PassesBaseSHAAndChangedFilesToTheCommand(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	outFile := filepath.Join(t.TempDir(), "env.out")

	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	units := []config.TestUnit{
		{Name: "api", Path: "services/api", Command: `printf '%s\n' "$NO_MISTAKES_BASE_SHA" > ` + outFile + `; printf '%s\n' "$NO_MISTAKES_CHANGED_FILES" >> ` + outFile},
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
}

func TestTestStep_FailingUnitCommandParksAutoFixable(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	units := []config.TestUnit{
		{Name: "api", Path: "services/api", Command: "false"},
		{Name: "web", Path: "services/web", Command: "true"},
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
	sctx := newTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Test: "false"})
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
