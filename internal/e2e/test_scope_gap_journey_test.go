//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These journeys replay issue #63 through the real CLI, daemon, and gate. The
// trusted runbook writes its command as a template, and the canned discovery
// agent reports an unselected unit that owns a changed root file and whose
// command cannot run. The observed run copied the template's placeholders
// verbatim, which discovery now rejects before anything runs (issue #59), so
// the canned command is a filled-in one that still fails. Scope fault 1
// expands into that unit, it cannot run, rediscovery returns the same layout,
// and scope fault 2 parks.

const (
	scopeGapRunbook = "For each changed .NET service run: dotnet test <dir>/<name>.csproj --settings <path>/coverlet.runsettings"
	// scopeGapDeadCommand passes discovery's command check and then fails,
	// the way a filled-in runbook command fails when its runner cannot build.
	scopeGapDeadCommand = "printf 'svc runner did not build'; exit 2"
	// scopeGapReviewToken appears only in prompts that carry the review
	// finding back, so the re-review after the fix restart answers clean.
	scopeGapReviewToken   = "SCOPEGAP-REVIEW-TOKEN"
	reviewFixPromptMarker = "Investigate previous review findings"
	reviewPromptMarker    = "Review the code changes and return structured findings"
	templateRuleMarker    = "Never copy a placeholder token into a unit command"
)

func scopeGapScenario(t *testing.T) string {
	t.Helper()
	layout := func(match string) string {
		return `  - match: "` + match + `"
    text: "layout"
    structured:
      units:
        - name: web
          path: "web"
          command: "` + InferredUnitCommand + `"
        - name: svc
          path: "."
          command: "` + scopeGapDeadCommand + `"
      selected: ["web"]
`
	}
	data := "actions:\n" +
		`  - match: "` + reviewFixPromptMarker + `"
    text: "fixed the web copy"
    edits:
      - path: "web/app.js"
        old: "draft"
        new: "final"
    structured:
      summary: "finalize web copy"
` + layout(rediscoveryPromptMarker) + layout(discoveryPromptMarker) +
		`  - match: "` + scopeGapReviewToken + `"
    text: "no issues found"
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
  - match: "` + reviewPromptMarker + `"
    text: "review found an issue"
    structured:
      findings:
        - id: "scope-gap-review-1"
          severity: warning
          file: "web/app.js"
          line: 1
          description: "web copy is still a draft ` + scopeGapReviewToken + `"
          action: auto-fix
      summary: "found one issue"
      risk_level: medium
      risk_rationale: "draft copy would ship"
      risk_scope: source-or-external
`
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return appendCleanDefault(t, path)
}

// startScopeGapRun pushes a branch that changes web/app.js and a root
// lockfile, then waits for the Test step to park on the second scope fault.
func startScopeGapRun(t *testing.T, branch string) *Harness {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scopeGapScenario(t)})
	globalConfig := filepath.Join(h.NMHome, "config.yaml")
	data, err := os.ReadFile(globalConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalConfig, []byte(strings.Replace(string(data), "  review: 0\n", "  review: 1\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange("main", ".no-mistakes.yaml", "test:\n  instructions: '"+scopeGapRunbook+"'\n", "pin the test runbook")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push trusted repo config: %v\n%s", err, out)
	}
	h.CommitChange(branch, "web/app.js", "const copy = \"draft\";\n", "add web copy")
	h.CommitChange(branch, "package-lock.json", "{}\n", "touch the root lockfile")
	h.PushToGate(branch)

	waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 180*time.Second)
	logStatus(t, h)
	rec := readTestStep(t, h, branch)
	t.Logf("first park findings: %s", rec.findingsJSON)
	if !strings.Contains(rec.findingsJSON, "under-selected twice in this run") {
		t.Fatalf("Test did not park on the second scope fault: %s", rec.findingsJSON)
	}
	invs := h.AgentInvocations()
	if n := countInvocations(invs, rediscoveryPromptMarker); n != 1 {
		t.Fatalf("rediscovery ran %d times before the park, want 1", n)
	}
	t.Logf("discovery prompt carries the template rule: %v", strings.Contains(findInvocationContaining(invs, discoveryPromptMarker), templateRuleMarker))
	return h
}

func testLog(t *testing.T, h *Harness, runID string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.NMHome, "logs", runID, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestScopeGapJourney_ApprovedParkSurvivesAReviewFixRestart(t *testing.T) {
	const branch = "scope-gap-approve"
	h := startScopeGapRun(t, branch)
	first := readTestStep(t, h, branch)
	discoveriesBefore := countInvocations(h.AgentInvocations(), discoveryPromptMarker)

	out, err := h.Run("axi", "respond", "--step", "test", "--action", "approve", "--reason", "svc is not touched by this change")
	t.Logf("=== axi respond approve ===\n%s", out)
	if err != nil {
		t.Fatal(err)
	}

	// Drive the run to its end: the review raises one auto-fix finding, its
	// fix round edits web/app.js (already in the changed set), and that
	// agent commit restarts validation from Format, so Test runs again on the
	// same changed-file set. A parked Review gate is approved; a Test step
	// parked again after the fix round is the bug.
	deadline := time.Now().Add(300 * time.Second)
	var finalStatus types.RunStatus
	for time.Now().Before(deadline) {
		run := h.RunInfo(first.runID)
		if run == nil {
			t.Fatal("run disappeared")
		}
		if run.Status == types.RunCompleted || run.Status == types.RunFailed || run.Status == types.RunCancelled {
			finalStatus = run.Status
			break
		}
		fixed := countInvocations(h.AgentInvocations(), reviewFixPromptMarker) > 0
		if step, ok := findStep(run.Steps, types.StepTest); ok && fixed && step.Status == types.StepStatusAwaitingApproval {
			logStatus(t, h)
			t.Logf("test.log:\n%s", testLog(t, h, first.runID))
			t.Fatalf("Test parked again after the review fix restart: %s", readTestStep(t, h, branch).findingsJSON)
		}
		if step, ok := findStep(run.Steps, types.StepReview); ok && (step.Status == types.StepStatusFixReview || step.Status == types.StepStatusAwaitingApproval) {
			h.Respond(run.ID, types.StepReview, types.ActionApprove)
		}
		time.Sleep(500 * time.Millisecond)
	}
	logStatus(t, h)
	run := h.RunInfo(first.runID)
	log := testLog(t, h, first.runID)
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "scope") || strings.Contains(line, "reusing") || strings.Contains(line, "discovering") || strings.Contains(line, "not running") {
			t.Logf("test.log: %s", line)
		}
	}
	t.Logf("run status = %s, restart_count = %d", finalStatus, run.RestartCount)
	if finalStatus != types.RunCompleted {
		t.Fatalf("run ended %s, want completed", finalStatus)
	}
	if run.RestartCount < 1 {
		t.Fatal("the review fix never restarted validation, so Test never re-ran")
	}
	if n := countInvocations(h.AgentInvocations(), reviewFixPromptMarker); n < 1 {
		t.Fatal("the review fix round never ran")
	}
	if n := countInvocations(h.AgentInvocations(), discoveryPromptMarker); n != discoveriesBefore {
		t.Fatalf("discovery passes went from %d to %d, want the cached layout reused", discoveriesBefore, n)
	}
	if !strings.Contains(log, "not running svc: an operator accepted this scope gap earlier in the run") {
		t.Fatal("the re-test did not rely on the accepted gap")
	}
	final := readTestStep(t, h, branch)
	t.Logf("re-test findings: %s", final.findingsJSON)
	if !strings.Contains(final.findingsJSON, "as an operator accepted at an earlier scope-fault park in this run: svc") {
		t.Fatal("the accepted gap is not visible on the re-test's outcome")
	}
	if !strings.Contains(first.findingsJSON, `"id":"test-scope-fault"`) {
		t.Fatalf("the park finding lacks the test-scope-fault ID: %s", first.findingsJSON)
	}
	if !strings.Contains(findInvocationContaining(h.AgentInvocations(), discoveryPromptMarker), templateRuleMarker) {
		t.Fatal("the discovery prompt lacks the runbook-template rule")
	}
}

func TestScopeGapJourney_FixOnTheScopeFaultForcesRediscovery(t *testing.T) {
	const branch = "scope-gap-rediscover"
	h := startScopeGapRun(t, branch)
	first := readTestStep(t, h, branch)
	discoveriesBefore := countInvocations(h.AgentInvocations(), discoveryPromptMarker)

	out, err := h.Run("axi", "respond", "--step", "test", "--action", "fix", "--findings", "test-scope-fault")
	t.Logf("=== axi respond fix --findings test-scope-fault ===\n%s", out)
	if err != nil {
		t.Fatal(err)
	}

	// The canned agent answers a fresh discovery with the same layout, so the
	// forced rediscovery reaches the same park again. What proves it happened
	// is the fresh discovery pass and a second first-fault expansion, which
	// only a reset fault count allows.
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		if countInvocations(h.AgentInvocations(), rediscoveryPromptMarker) >= 2 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusFixReview, 120*time.Second)
	logStatus(t, h)
	invs := h.AgentInvocations()
	log := testLog(t, h, first.runID)
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "scope") || strings.Contains(line, "rediscover") || strings.Contains(line, "discovering") || strings.Contains(line, "repair turn") {
			t.Logf("test.log: %s", line)
		}
	}
	if n := countInvocations(invs, discoveryPromptMarker); n <= discoveriesBefore {
		t.Fatalf("discovery passes = %d, want more than %d after the forced rediscovery", n, discoveriesBefore)
	}
	if n := countInvocations(invs, testFixPromptMarker); n != 0 {
		t.Fatalf("a Test repair turn ran %d times for a layout problem", n)
	}
	if !strings.Contains(log, "fix selection holds the test scope fault; forgetting this run's discovered test units") {
		t.Fatal("the fix round did not reset the discovery state")
	}
	if n := strings.Count(log, "test scope fault: original selection web"); n != 2 {
		t.Fatalf("first-fault expansions = %d, want 2 (the reset fault count allows a fresh one)", n)
	}
}
