//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These journeys drive the real CLI, daemon, and gate with a canned agent
// through a Test step whose unit command either fails its tests or cannot run
// any test at all (issues #37 and #38).

const (
	discoveryPromptMarker   = "Derive this repository's independently testable units"
	rediscoveryPromptMarker = "The command previously inferred for unit"
	evidencePromptMarker    = "You are validating a code change by driving the product itself."
	testFixPromptMarker     = "Fix the failing tests in this repository."
)

// writeRawTestCommand installs an executable script in BinDir that runs body
// verbatim, with no coverage artifacts added.
func writeRawTestCommand(t *testing.T, h *Harness, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.BinDir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// deadRunnerScenario writes a scenario whose first discovery infers
// firstCommand and whose rediscovery (when reached) infers secondCommand.
func deadRunnerScenario(t *testing.T, firstCommand, secondCommand string) string {
	t.Helper()
	unit := func(match, command string) string {
		return `  - match: "` + match + `"
    text: "layout"
    structured:
      units:
        - name: repository
          path: "."
          command: "` + command + `"
      selected: ["repository"]
`
	}
	data := "actions:\n" + unit(rediscoveryPromptMarker, secondCommand) + unit(discoveryPromptMarker, firstCommand)
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	// The scenario file replaces the built-in default wholesale, so every other
	// phase needs its own clean catch-all.
	return appendCleanDefault(t, path)
}

func appendCleanDefault(t *testing.T, path string) string {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = f.WriteString(`  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "synthetic clean response"
      risk_scope: source-or-external
      tested: ["fakeagent: simulated test run"]
      testing_summary: "simulated tests passed"
      artifacts: []
      verdict: go
      scenarios:
        - name: "fakeagent: simulated scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated"
          reason: ""
`)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func countInvocations(invs []Invocation, needle string) int {
	n := 0
	for _, i := range invs {
		if strings.Contains(i.Prompt, needle) {
			n++
		}
	}
	return n
}

type testStepRecord struct {
	exitCode     *int
	findings     []types.Finding
	findingsJSON string
	override     *string
	runID        string
}

func readTestStep(t *testing.T, h *Harness, branch string) testStepRecord {
	t.Helper()
	database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run := h.ActiveRun(branch)
	if run == nil {
		runs := h.Runs()
		for i := range runs {
			if runs[i].Branch == branch {
				run = &runs[i]
				break
			}
		}
	}
	if run == nil {
		t.Fatalf("no run for %s", branch)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName != types.StepTest {
			continue
		}
		rec := testStepRecord{exitCode: step.ExitCode, override: step.OverrideReason, runID: run.ID}
		if step.FindingsJSON != nil {
			rec.findingsJSON = *step.FindingsJSON
			var f struct {
				Items []types.Finding `json:"findings"`
			}
			if err := json.Unmarshal([]byte(*step.FindingsJSON), &f); err != nil {
				t.Fatalf("parse findings: %v\n%s", err, *step.FindingsJSON)
			}
			rec.findings = f.Items
		}
		return rec
	}
	t.Fatal("no test step row")
	return testStepRecord{}
}

func runTestJourney(t *testing.T, h *Harness, branch, config string) {
	t.Helper()
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if config != "" {
		h.CommitChange(branch, ".no-mistakes.yaml", config, "configure test command")
	}
	h.CommitChange(branch, "feature.txt", "synthetic feature\n", "add feature")
	out, err := h.Run("axi", "run", "--intent", "Validate the synthetic feature", "--skip", "pr,ci")
	t.Logf("=== axi run ===\n%s", out)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
}

func logStatus(t *testing.T, h *Harness) {
	t.Helper()
	status, _ := h.Run("axi", "status")
	t.Logf("=== axi status ===\n%s", status)
}

// #37: a configured unit whose tests ran and failed carries an auto-fix
// action, so the executor spends auto_fix.test rounds before parking.
func TestDeadRunnerJourney_FailingTestsSpendAutoFixRounds(t *testing.T) {
	// The harness zeroes every auto-fix budget globally, so the repo config
	// grants Test rounds back.
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	h.WriteTestCommand("nm-failing-tests", "echo 'TestCheckout failed'; exit 3")
	runTestJourney(t, h, "failing", "commands:\n  test: nm-failing-tests\n  lint: 'exit 0'\nauto_fix:\n  test: 2\n")
	// An auto-fix round ends at the fix_review gate; before #37 the step
	// parked at awaiting_approval with the budget unspent.
	waitForStepStatus(t, h, "failing", types.StepTest, types.StepStatusFixReview, 180*time.Second)
	logStatus(t, h)
	fixes := countInvocations(h.AgentInvocations(), testFixPromptMarker)
	t.Logf("test fix-round agent invocations: %d", fixes)
	if fixes < 1 {
		t.Fatalf("no auto_fix.test round was spent")
	}
	rec := readTestStep(t, h, "failing")
	t.Logf("test step findings: %s", rec.findingsJSON)
	if len(rec.findings) == 0 || rec.findings[0].Action != types.ActionAutoFix || !strings.Contains(rec.findings[0].Description, "tests failed with exit code 3") {
		t.Fatalf("want auto-fix failing-tests finding, got %s", rec.findingsJSON)
	}
}

// #38: a configured command that exits non-zero without running a test parks
// for the maintainer before the evidence pass, and approving it still records
// an override. The adversarial case writes a report claiming zero tests.
func TestDeadRunnerJourney_ConfiguredDeadCommandParks(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		exit               int
	}{
		{name: "no-report", body: "echo 'sh: nm-runner: not found' >&2; exit 7", reason: "wrote no test report", exit: 7},
		{name: "zero-tests", body: `printf '<testsuite tests="0" skipped="0"></testsuite>\n' > "$NO_MISTAKES_COVERAGE_DIR/report.xml"; exit 1`, reason: "reported 0 executed tests", exit: 1},
		{name: "unparseable", body: `printf 'not xml at all' > "$NO_MISTAKES_COVERAGE_DIR/report.xml"; exit 2`, reason: "wrote no test report", exit: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude"})
			writeRawTestCommand(t, h, "nm-dead-configured", tc.body)
			runTestJourney(t, h, tc.name, "commands:\n  test: nm-dead-configured\n  lint: 'exit 0'\n")
			waitForStepStatus(t, h, tc.name, types.StepTest, types.StepStatusAwaitingApproval, 90*time.Second)
			logStatus(t, h)
			invs := h.AgentInvocations()
			if n := countInvocations(invs, evidencePromptMarker); n != 0 {
				t.Fatalf("evidence pass ran %d times before the dead-runner park", n)
			}
			if n := countInvocations(invs, testFixPromptMarker); n != 0 {
				t.Fatalf("an auto-fix round was spent on a dead runner (%d)", n)
			}
			rec := readTestStep(t, h, tc.name)
			t.Logf("test step findings: %s", rec.findingsJSON)
			if rec.exitCode == nil || *rec.exitCode != tc.exit {
				t.Fatalf("exit code = %v, want %d", rec.exitCode, tc.exit)
			}
			f := rec.findings[0]
			if f.Action != types.ActionAskUser || f.Category != types.FindingCategoryTestCommand || !strings.Contains(f.Description, "could not run any test") || !strings.Contains(f.Description, tc.reason) {
				t.Fatalf("unexpected finding %+v", f)
			}
			out, err := h.Run("axi", "respond", "--step", "test", "--action", "approve", "--reason", "runner is broken on this host")
			t.Logf("=== axi respond approve ===\n%s", out)
			if err != nil {
				t.Fatal(err)
			}
			h.WaitForRun(tc.name, 90*time.Second)
			rec = readTestStep(t, h, tc.name)
			if rec.override == nil {
				t.Fatalf("approval over a dead configured command recorded no override")
			}
			t.Logf("override_reason recorded: %q", *rec.override)
			if !strings.Contains(out, "passed-with-override") {
				t.Fatalf("outcome is not passed-with-override:\n%s", out)
			}
		})
	}
}

// #38: an agent-inferred command that cannot run triggers one rediscovery in
// the same attempt, which carries the dead command's details, and a working
// replacement lets the run proceed through the evidence pass.
func TestDeadRunnerJourney_InferredDeadCommandRediscoversOnce(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: deadRunnerScenario(t, "nm-dead-inferred", InferredUnitCommand)})
	writeRawTestCommand(t, h, "nm-dead-inferred", "echo 'syntax error near unexpected token' >&2; exit 2")
	runTestJourney(t, h, "rediscover", "")
	run := h.WaitForRun("rediscover", 180*time.Second)
	logStatus(t, h)
	invs := h.AgentInvocations()
	if n := countInvocations(invs, rediscoveryPromptMarker); n != 1 {
		t.Fatalf("rediscovery ran %d times, want 1", n)
	}
	prompt := findInvocationContaining(invs, rediscoveryPromptMarker)
	idx := strings.Index(prompt, "The command previously inferred")
	t.Logf("=== rediscovery prompt failure section ===\n%s", prompt[idx:])
	for _, want := range []string{"nm-dead-inferred", "exited 2", "wrote no test report", "syntax error near unexpected token"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("rediscovery prompt lacks %q", want)
		}
	}
	if countInvocations(invs, evidencePromptMarker) == 0 {
		t.Fatal("evidence pass never ran after the working replacement")
	}
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", run.Status)
	}
	database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	state, err := database.GetRunTestDiscovery(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("runs.test_discovery = %s", state)
	if !strings.Contains(state, `"runner_faults":1`) {
		t.Fatalf("runner_faults not persisted on the run row: %s", state)
	}
}

// #38 adversarial: a replacement that also cannot run parks for the
// maintainer, and the one-per-run bound survives a daemon restart.
func TestDeadRunnerJourney_ReplacementAlsoDeadParksAndBoundSurvivesRestart(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: deadRunnerScenario(t, "nm-dead-inferred", "nm-dead-replacement")})
	writeRawTestCommand(t, h, "nm-dead-inferred", "echo 'command not found' >&2; exit 127")
	writeRawTestCommand(t, h, "nm-dead-replacement", "echo 'still broken' >&2; exit 5")
	runTestJourney(t, h, "replacement", "")
	waitForStepStatus(t, h, "replacement", types.StepTest, types.StepStatusAwaitingApproval, 120*time.Second)
	logStatus(t, h)
	rec := readTestStep(t, h, "replacement")
	t.Logf("test step findings: %s", rec.findingsJSON)
	if f := rec.findings[0]; f.Action != types.ActionAskUser || !strings.Contains(f.Description, "it replaced an inferred command that could not run any test either: nm-dead-inferred") {
		t.Fatalf("unexpected finding %+v", f)
	}
	if n := countInvocations(h.AgentInvocations(), rediscoveryPromptMarker); n != 1 {
		t.Fatalf("rediscovery ran %d times, want 1", n)
	}

	out, err := h.Run("daemon", "stop")
	t.Logf("=== daemon stop ===\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
	out, err = h.Run("daemon", "start")
	t.Logf("=== daemon start ===\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
	resumed := waitForStepStatus(t, h, "replacement", types.StepTest, types.StepStatusAwaitingApproval, 60*time.Second)
	if resumed.ID != rec.runID {
		t.Fatalf("resumed run %s, want %s", resumed.ID, rec.runID)
	}
	out, err = h.Run("axi", "respond", "--step", "test", "--action", "fix", "--findings", "test-1")
	t.Logf("=== axi respond fix (after restart) ===\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		rec = readTestStep(t, h, "replacement")
		if strings.Contains(rec.findingsJSON, "already replaced a dead inferred command once in this run") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	logStatus(t, h)
	t.Logf("test step findings after restart+fix: %s", rec.findingsJSON)
	if !strings.Contains(rec.findingsJSON, "already replaced a dead inferred command once in this run") {
		t.Fatalf("restart refilled the rediscovery bound: %s", rec.findingsJSON)
	}
	if n := countInvocations(h.AgentInvocations(), rediscoveryPromptMarker); n != 1 {
		t.Fatalf("rediscovery ran %d times across the restart, want 1", n)
	}
}

// #38 adversarial: a rediscovery that keeps the identical command treats the
// failure as failing tests, and later attempts keep that verdict.
func TestDeadRunnerJourney_KeptCommandIsFailingTests(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: deadRunnerScenario(t, "nm-dead-inferred", "nm-dead-inferred")})
	writeRawTestCommand(t, h, "nm-dead-inferred", "echo 'feature.go:3: undefined: Checkout' >&2; exit 2")
	// The harness zeroes every auto-fix budget globally; this journey needs
	// Test rounds to observe the kept verdict on a later attempt.
	runTestJourney(t, h, "kept", "auto_fix:\n  test: 2\n")
	waitForStepStatus(t, h, "kept", types.StepTest, types.StepStatusFixReview, 180*time.Second)
	logStatus(t, h)
	invs := h.AgentInvocations()
	if n := countInvocations(invs, rediscoveryPromptMarker); n != 1 {
		t.Fatalf("rediscovery ran %d times, want 1", n)
	}
	fixes := countInvocations(invs, testFixPromptMarker)
	t.Logf("test fix-round agent invocations: %d", fixes)
	if fixes < 1 {
		t.Fatal("the kept command's failure spent no auto_fix.test round")
	}
	rec := readTestStep(t, h, "kept")
	t.Logf("test step findings after the fix round: %s", rec.findingsJSON)
	f := rec.findings[0]
	if f.Action != types.ActionAutoFix || !strings.Contains(f.Description, "tests failed with exit code 2") {
		t.Fatalf("kept command not treated as failing tests on the fix round's attempt: %+v", f)
	}
	logData, err := os.ReadFile(filepath.Join(h.NMHome, "logs", rec.runID, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(logData), "\n") {
		if strings.Contains(line, "kept") || strings.Contains(line, "rediscovering") || strings.Contains(line, "unit repository:") {
			t.Logf("test.log: %s", line)
		}
	}
	if !strings.Contains(string(logData), "test unit discovery kept this command earlier in the run") {
		t.Fatal("the fix round's attempt did not reuse the persisted kept verdict")
	}
}
