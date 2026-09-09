//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// staleMonitorFixture drives one full pipeline run that ends with pipeline fix
// commits pushed to the gate while the operator worktree stays at the head it
// submitted. That is exactly the shape a dead-monitor recovery starts from: the
// gate is ahead of the clone, so `rerun` would stamp a head the clone does not
// have. It returns the operator worktree and the pushed pipeline head.
func staleMonitorFixture(t *testing.T, branch string) (*Harness, string, string) {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-stale", "seed.txt", "seed\n", "seed stale-monitor init")
	initWorktree := h.AddWorktree("init-stale")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	operator := h.AddWorktree(branch)
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature before the monitor dies")
	if err != nil || !strings.Contains(gateOut, "sync-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	if fixOut, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil {
		t.Fatalf("review fix: %v\n%s", err, fixOut)
	}
	doneOut, err := h.RunInDir(operator, "axi", "respond", "--action", "approve")
	if err != nil {
		t.Fatalf("approve fix review: %v\n%s", err, doneOut)
	}
	pushedHead := h.UpstreamBranchSHA(branch)
	if pushedHead == submitted {
		t.Fatal("pipeline did not create and push a fix commit")
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != submitted {
		t.Fatalf("operator branch moved on its own: %s, want %s", got, submitted)
	}
	return h, operator, pushedHead
}

// TestAxiStaleMonitorSyncBeforeRerunReattaches executes the prescribed
// dead-monitor recovery in the order the guidance now states: take the
// pipeline's commits with the offered sync action FIRST, then `rerun`, then
// `axi run` to answer the recovered run's gates. Each step is asserted against
// the real binary rather than reasoned about.
func TestAxiStaleMonitorSyncBeforeRerunReattaches(t *testing.T) {
	branch := "feature/stale-sync-first"
	h, operator, pushedHead := staleMonitorFixture(t, branch)

	// The refusal an agent actually hits first must itself offer the sync.
	noIntentOut, noIntentErr := h.RunInDir(operator, "axi", "run")
	if noIntentErr == nil {
		t.Fatalf("axi run without --intent should fail:\n%s", noIntentOut)
	}
	for _, want := range []string{
		"--intent is required to start a run",
		"branch_sync:",
		"state: behind",
		"code: sync",
		"command: no-mistakes axi sync",
	} {
		if !strings.Contains(noIntentOut, want) {
			t.Errorf("intent-required refusal missing %q:\n%s", want, noIntentOut)
		}
	}

	syncOut, err := h.RunInDir(operator, "axi", "sync")
	if err != nil {
		t.Fatalf("guarded sync before rerun: %v\n%s", err, syncOut)
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != pushedHead {
		t.Fatalf("operator HEAD after sync = %s, want pushed head %s", got, pushedHead)
	}

	rerunOut, err := h.RunInDir(operator, "rerun")
	if err != nil {
		t.Fatalf("rerun after sync: %v\n%s", err, rerunOut)
	}
	reran := h.ActiveRun(branch)
	if reran == nil {
		t.Fatalf("rerun left no active run:\n%s", rerunOut)
	}
	if reran.HeadSHA != pushedHead {
		t.Fatalf("reran run head = %s, want the synchronized head %s", reran.HeadSHA, pushedHead)
	}

	// The reattach: no --intent, no push, same run driven to its gate.
	runsBefore := len(h.Runs())
	reattachOut, err := h.RunInDir(operator, "axi", "run")
	if err != nil {
		t.Fatalf("reattach after sync+rerun: %v\n%s", err, reattachOut)
	}
	if !strings.Contains(reattachOut, reran.ID) {
		t.Errorf("reattach drove a different run than %s:\n%s", reran.ID, reattachOut)
	}
	for _, forbidden := range []string{"--intent is required", "non-fast-forward", "fetch first"} {
		if strings.Contains(reattachOut, forbidden) {
			t.Errorf("reattach output contains %q:\n%s", forbidden, reattachOut)
		}
	}
	if got := len(h.Runs()); got != runsBefore {
		t.Fatalf("reattach changed run count from %d to %d", runsBefore, got)
	}
	if !strings.Contains(reattachOut, "gate:") && !strings.Contains(reattachOut, "outcome:") {
		t.Fatalf("reattach neither parked at a gate nor produced an outcome:\n%s", reattachOut)
	}
}

// TestAxiStaleMonitorRerunBeforeSyncIsRefused covers the WRONG order, which two
// earlier revisions shipped as the prescribed fallback. Rerun's clean-head rule
// now ends it before it can do damage: the clone is clean and behind the gate
// head rerun would select, so rerun refuses and creates no run at all. An
// earlier revision of this test asserted the outcome that rule replaced, where
// rerun's own pending run took ownership of the branch, the sync the agent was
// told to reach for was then refused as `pipeline_owned`, and the clone was
// left stranded behind the gate with no clean exit. Refusing up front is what
// keeps the prescribed order reachable, so this asserts that too.
func TestAxiStaleMonitorRerunBeforeSyncIsRefused(t *testing.T) {
	branch := "feature/stale-rerun-first"
	h, operator, pushedHead := staleMonitorFixture(t, branch)
	behindHead := strings.TrimSpace(h.WorktreeRefSHA(branch))

	rerunOut, err := h.RunInDir(operator, "rerun")
	if err == nil {
		t.Fatalf("rerun before sync should be refused:\n%s", rerunOut)
	}
	// The refusal has to be the clean-head mismatch and not any other error
	// that would also leave err non-nil, so it must name both heads and point
	// at the command that resolves custody.
	for _, want := range []string{"refusing rerun", pushedHead, behindHead, "no-mistakes axi status"} {
		if !strings.Contains(rerunOut, want) {
			t.Fatalf("rerun refusal did not name %q:\n%s", want, rerunOut)
		}
	}
	if run := h.ActiveRun(branch); run != nil {
		t.Fatalf("refused rerun still created active run %s:\n%s", run.ID, rerunOut)
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != behindHead {
		t.Fatalf("refused rerun moved the worktree to %s", got)
	}

	// The prescribed order is still open from here, which is the whole point of
	// refusing rather than stranding: the sync the guidance names first works.
	if syncOut, syncErr := h.RunInDir(operator, "axi", "sync"); syncErr != nil {
		t.Fatalf("sync after the refused rerun: %v\n%s", syncErr, syncOut)
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != pushedHead {
		t.Fatalf("operator HEAD after sync = %s, want pushed head %s", got, pushedHead)
	}
}
