//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestDaemonStopPreservesGateParkedRunForTheNextStart is the reported bug as an
// operator experiences it at the CLI: a run parked at the review gate, a plain
// `no-mistakes daemon stop` (no --force), and then `no-mistakes daemon start`.
//
// Before the fix the stop cancelled the parked run and failed it with "daemon
// shutting down", so `axi status` after the restart showed a dead run and the
// operator's answer had nowhere to land. The preserved run must instead still be
// running and still parked at the same gate after the restart, and `axi respond`
// must carry it to completion, which is the whole point of preserving it.
func TestDaemonStopPreservesGateParkedRunForTheNextStart(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})

	h.CommitChange("init-stop-park", "seed.txt", "seed\n", "seed for stop preservation")
	initWorktree := h.AddWorktree("init-stop-park")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	h.CommitChange("feature/stop-park", "feature.txt", "change\n", "add feature change")
	fw := h.AddWorktree("feature/stop-park")

	if out, err := h.RunInDir(fw, "axi", "run", "--intent", axiIntent); err != nil {
		t.Fatalf("axi run (expected to stop at gate, exit 0): %v\n%s", err, out)
	}
	parked := waitForStepStatus(t, h, "feature/stop-park", types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	if parked == nil {
		t.Fatal("expected feature/stop-park run to be awaiting approval")
	}
	parkedID := parked.ID

	before := daemonPIDForRoot(t, h)

	// A plain stop, with no --force: the parked run must not make the guard
	// refuse, and the operator must be told the run survives.
	stopOut, err := h.RunInDir(fw, "daemon", "stop")
	if err != nil {
		t.Fatalf("nm daemon stop with a parked run: %v\n%s", err, stopOut)
	}
	if !strings.Contains(stopOut, "daemon stopped") {
		t.Fatalf("daemon stop did not report a stop:\n%s", stopOut)
	}
	if !strings.Contains(stopOut, "will be preserved and resumed when the daemon starts again") {
		t.Fatalf("daemon stop did not promise preservation of the parked run:\n%s", stopOut)
	}
	if alive, err := e2edaemon.ProcessAlive(before); err != nil {
		t.Fatalf("probe stopped daemon pid %d: %v", before, err)
	} else if alive {
		t.Fatalf("daemon stop returned while daemon pid %d was still running", before)
	}
	t.Logf("=== nm daemon stop (run %s parked at review) ===\n%s", parkedID, strings.TrimSpace(stopOut))

	startOut, err := h.RunInDir(fw, "daemon", "start")
	if err != nil {
		t.Fatalf("nm daemon start after a preserving stop: %v\n%s", err, startOut)
	}
	t.Logf("=== nm daemon start ===\n%s", strings.TrimSpace(startOut))

	// The next start resumes the preserved run: it is the same run, still
	// running, and it re-presents the same gate rather than being failed.
	resumed := waitForStepStatus(t, h, "feature/stop-park", types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	if resumed == nil {
		t.Fatal("expected the preserved run to be awaiting approval again after the restart")
	}
	if resumed.ID != parkedID {
		t.Fatalf("run after restart = %s, want the preserved run %s", resumed.ID, parkedID)
	}
	if resumed.Status != types.RunRunning {
		t.Fatalf("preserved run status after restart = %s, want %s", resumed.Status, types.RunRunning)
	}
	if !resumed.AwaitingAgent {
		t.Error("preserved run reports AwaitingAgent = false after the restart, want true")
	}

	statusOut, err := h.RunInDir(fw, "axi", "status")
	if err != nil {
		t.Fatalf("axi status after restart: %v\n%s", err, statusOut)
	}
	if !strings.Contains(statusOut, "awaiting_agent: parked ") {
		t.Fatalf("axi status after restart does not show the preserved run parked:\n%s", statusOut)
	}
	t.Logf("=== nm axi status (after restart) ===\n%s", strings.TrimSpace(statusOut))

	// Preservation is only worth anything if the operator can still answer the
	// gate and finish the run.
	respondOut, err := h.RunInDir(fw, "axi", "respond", "--action", "approve")
	if err != nil {
		t.Fatalf("axi respond approve after restart: %v\n%s", err, respondOut)
	}
	completed := h.WaitForRun("feature/stop-park", 90*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("resumed run status = %s, want completed", completed.Status)
	}
	if completed.ID != parkedID {
		t.Fatalf("completed run = %s, want the preserved run %s", completed.ID, parkedID)
	}
	t.Logf("=== nm axi respond --action approve (after restart) ===\n%s", strings.TrimSpace(respondOut))
	t.Logf("preserved run %s completed after the stop/start cycle", parkedID)
}
