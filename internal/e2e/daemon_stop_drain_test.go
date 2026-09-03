//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The drain journeys exercise `daemon stop --drain` and `--drain-timeout` the
// way an operator does: a real daemon, a real in-flight run, the real CLI
// process, and the real exit code. Everything below the flag (the IPC request,
// the daemon-side wait, the report that comes back) only exists on this path,
// so a unit test of any one layer cannot show that the flag reaches the daemon
// and that its answer reaches the operator.
//
// Each test owns its own NM_HOME through the harness, so no shared daemon is
// ever stopped.

// slowReviewScenario delays the review agent so a run is provably still doing
// its own work when the drain arrives. The delay is the only thing the drain
// has to wait out; everything after it is instant.
func slowReviewScenario(t *testing.T, branch string, delayMS int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slow-review-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: ` + branch + `"
    delay_ms: ` + strconv.Itoa(delayMS) + `
    text: "looks good"
    structured:
      findings: []
      summary: "no blocking issues"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      tested: ["fakeagent: simulated review"]
      testing_summary: "not run during review"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      tested: ["fakeagent: simulated test run"]
      testing_summary: "simulated tests passed"
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write slow review scenario: %v", err)
	}
	return path
}

func runStatusFromDB(t *testing.T, h *Harness, runID string) types.RunStatus {
	t.Helper()
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatalf("open db after the daemon stopped: %v", err)
	}
	defer database.Close()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	if run == nil {
		t.Fatalf("run %s vanished from the database", runID)
	}
	return run.Status
}

// TestDaemonStopDrainLetsAnInFlightRunFinish is the flag's whole promise: the
// operator asks for a drain, the daemon keeps the in-flight run's work, and the
// command exits 0 reporting the run as finished rather than cut.
func TestDaemonStopDrainLetsAnInFlightRunFinish(t *testing.T) {
	const branch = "drain-finishes"
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: slowReviewScenario(t, branch, 8000)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "drain.txt", "drain me\n", "add drain fixture")
	h.PushToGate(branch)
	run := h.WaitForRunRunning(branch, 30*time.Second)

	start := time.Now()
	out, err := h.Run("daemon", "stop", "--drain", "--drain-timeout", "50s")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("daemon stop --drain should exit 0 when every run finished: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 run(s) finished before the daemon stopped") {
		t.Fatalf("drain report should name the run it waited for, got:\n%s", out)
	}
	if strings.Contains(out, "forcibly stopped at the drain deadline") {
		t.Fatalf("the drain cut a run it had time to wait for:\n%s", out)
	}

	if status := runStatusFromDB(t, h, run.ID); status != types.RunCompleted {
		t.Fatalf("run %s status = %s, want %s: the drain must let its work finish", run.ID, status, types.RunCompleted)
	}
	assertNoDaemonProcessesForRoot(t, h, "after daemon stop --drain")
	t.Logf("drain waited %s for run %s; report: %s", elapsed, run.ID, strings.TrimSpace(out))
}

// TestDaemonStopDrainTimeoutCutsARunLoose proves --drain-timeout is the bound
// the daemon actually applies. The agent hangs far longer than the timeout, so
// only the flag can end the wait, and the operator gets a nonzero exit naming
// the run the deadline stopped.
func TestDaemonStopDrainTimeoutCutsARunLoose(t *testing.T) {
	const branch = "drain-deadline"
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: slowReviewScenario(t, branch, 120000)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	h.CommitChange(branch, "deadline.txt", "cut me loose\n", "add deadline fixture")
	h.PushToGate(branch)
	run := h.WaitForRunRunning(branch, 30*time.Second)

	start := time.Now()
	out, err := h.Run("daemon", "stop", "--drain", "--drain-timeout", "3s")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("daemon stop --drain should exit nonzero when the deadline stopped a run, got:\n%s", out)
	}
	if !strings.Contains(out, "forcibly stopped at the drain deadline") {
		t.Fatalf("drain report should name the deadline cut, got:\n%s", out)
	}
	if !strings.Contains(out, run.ID) {
		t.Fatalf("drain report should name run %s, got:\n%s", run.ID, out)
	}
	if elapsed < 3*time.Second {
		t.Fatalf("stop returned in %s, before the 3s deadline it was given", elapsed)
	}
	if elapsed > 60*time.Second {
		t.Fatalf("stop took %s, so it waited on the agent rather than the 3s deadline", elapsed)
	}

	if status := runStatusFromDB(t, h, run.ID); status == types.RunRunning {
		t.Fatalf("run %s is still marked running after the drain cut it", run.ID)
	}
	assertNoDaemonProcessesForRoot(t, h, "after daemon stop --drain hit its deadline")
	t.Logf("drain gave up after %s; report: %s", elapsed, strings.TrimSpace(out))
}
