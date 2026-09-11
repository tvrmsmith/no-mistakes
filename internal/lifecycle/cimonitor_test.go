package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle/lifecycletest"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func monitoringRun(prURL string) *db.Run {
	run := &db.Run{ID: "ci-run", Status: types.RunRunning}
	if prURL != "" {
		run.PRURL = &prURL
	}
	return run
}

func ciSteps(ciStatus types.StepStatus) []*db.StepResult {
	return []*db.StepResult{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepPush, Status: types.StepStatusCompleted},
		{StepName: types.StepCI, Status: ciStatus},
	}
}

func TestResumableCIMonitor_LiveMonitorWithAnOpenPRQualifies(t *testing.T) {
	if !ResumableCIMonitor(monitoringRun("https://github.com/o/r/pull/1"), ciSteps(types.StepStatusRunning)) {
		t.Error("ResumableCIMonitor(live monitor, open PR) = false, want true")
	}
}

func TestResumableCIMonitor_MonitorWithoutAPRURLDoesNot(t *testing.T) {
	if ResumableCIMonitor(monitoringRun(""), ciSteps(types.StepStatusRunning)) {
		t.Error("ResumableCIMonitor(no PR URL) = true, want false")
	}
	if ResumableCIMonitor(monitoringRun("   "), ciSteps(types.StepStatusRunning)) {
		t.Error("ResumableCIMonitor(blank PR URL) = true, want false")
	}
}

// The CI step runs its auto-fix agent inline and the executor never moves the
// row to fixing for it, so a live repair looks like a running monitor in every
// way except the pid it records. The drain reads this predicate to decide what
// it may walk away from, so missing the pid would let it abandon a working
// agent and report the run under none of waited, finished, or interrupted.
func TestResumableCIMonitor_ARunningCIRowHoldingAnAgentPIDDoesNot(t *testing.T) {
	run := monitoringRun("https://github.com/o/r/pull/1")
	steps := ciSteps(types.StepStatusRunning)
	pid := 4242
	steps[len(steps)-1].AgentPID = &pid
	if ResumableCIMonitor(run, steps) {
		t.Error("ResumableCIMonitor(running ci row holding an agent pid) = true, want false")
	}
	// The wider predicate deliberately ignores the pid, because the SQL lift it
	// mirrors does: the run still deserves the status that spares its worktree.
	if !CIMonitorRun(run, steps) {
		t.Error("CIMonitorRun(running ci row holding an agent pid) = false, want true")
	}
}

// Every case above builds a running run, so nothing pinned the guard that stops
// a terminal row from being read as resumable.
func TestResumableCIMonitor_ATerminalRunDoesNot(t *testing.T) {
	run := monitoringRun("https://github.com/o/r/pull/1")
	run.Status = types.RunCompleted
	if ResumableCIMonitor(run, ciSteps(types.StepStatusRunning)) {
		t.Error("ResumableCIMonitor(completed run) = true, want false")
	}
}

func TestResumableCIMonitor_ACIStepMidRepairDoesNot(t *testing.T) {
	run := monitoringRun("https://github.com/o/r/pull/1")
	for _, status := range []types.StepStatus{types.StepStatusFixing, types.StepStatusFixReview, types.StepStatusAwaitingApproval} {
		if ResumableCIMonitor(run, ciSteps(status)) {
			t.Errorf("ResumableCIMonitor(ci step %s) = true, want false", status)
		}
	}
}

func TestResumableCIMonitor_AnotherActiveStepDisqualifiesIt(t *testing.T) {
	steps := append(ciSteps(types.StepStatusRunning), &db.StepResult{StepName: types.StepTest, Status: types.StepStatusRunning})
	if ResumableCIMonitor(monitoringRun("https://github.com/o/r/pull/1"), steps) {
		t.Error("ResumableCIMonitor(second active step) = true, want false")
	}
}

func TestCIMonitorRun_MatchesTheWiderStatusSetIncludingFixing(t *testing.T) {
	run := monitoringRun("https://github.com/o/r/pull/1")
	for _, status := range []types.StepStatus{
		types.StepStatusRunning,
		types.StepStatusAwaitingApproval,
		types.StepStatusFixing,
		types.StepStatusFixReview,
	} {
		if !CIMonitorRun(run, ciSteps(status)) {
			t.Errorf("CIMonitorRun(ci step %s) = false, want true", status)
		}
	}
	if CIMonitorRun(run, ciSteps(types.StepStatusCompleted)) {
		t.Error("CIMonitorRun(completed ci step) = true, want false")
	}
	if CIMonitorRun(monitoringRun(""), ciSteps(types.StepStatusFixing)) {
		t.Error("CIMonitorRun(no PR URL) = true, want false")
	}
}

func TestCIMonitorRun_StillRequiresCIToBeTheOnlyActiveStep(t *testing.T) {
	steps := append(ciSteps(types.StepStatusFixing), &db.StepResult{StepName: types.StepReview, Status: types.StepStatusFixReview})
	if CIMonitorRun(monitoringRun("https://github.com/o/r/pull/1"), steps) {
		t.Error("CIMonitorRun(review also active) = true, want false")
	}
}

// TestExemptFromGuard_ALiveCIMonitorIsPreserved walks the real state DB: a
// clean stop now preserves a CI monitor as well as a gate park, so the guard
// must count it exempt instead of blocking.
func TestExemptFromGuard_ALiveCIMonitorIsPreserved(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	plan := lifecycletest.Plan(types.StepReview, types.StepPush, types.StepCI)
	lifecycletest.SeedResumableCIMonitorRun(t, p, "/tmp/project", "feature", "https://github.com/o/r/pull/7", plan)

	decision, err := Decide(p, plan, SameBinary)
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.Blocking) != 0 || len(decision.Parked) != 1 {
		t.Fatalf("Decide(live ci monitor) = %d blocking / %d preserved, want 0 / 1", len(decision.Blocking), len(decision.Parked))
	}
	if !strings.Contains(decision.ParkedNotice(), "will be preserved and resumed") {
		t.Errorf("ParkedNotice() = %q, want the preservation promise", decision.ParkedNotice())
	}
}

// TestExemptFromGuard_ACIMonitorWithUncommittedWorkIsNotPreserved is the case
// recovery's own preconditions cannot see. A CI auto-fix turn killed mid-edit
// leaves the row running with no pid, a PR URL, and a head still equal to
// run.HeadSHA, so the worktree exists and matches; only the uncommitted edits
// underneath give it away, and Executor.ciMonitorPreservable refuses on exactly
// those. Without the guard's own cleanliness read the operator is promised a
// resume the same stop then refuses.
func TestExemptFromGuard_ACIMonitorWithUncommittedWorkIsNotPreserved(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	plan := lifecycletest.Plan(types.StepReview, types.StepPush, types.StepCI)
	monitor := lifecycletest.SeedResumableCIMonitorRun(t, p, "/tmp/project", "feature", "https://github.com/o/r/pull/7", plan)
	if err := os.WriteFile(filepath.Join(monitor.WorkDir, "half-written.go"), []byte("package broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	decision, err := Decide(p, plan, SameBinary)
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.Blocking) != 1 || len(decision.Parked) != 0 {
		t.Fatalf("Decide(dirty ci monitor) = %d blocking / %d preserved, want 1 / 0", len(decision.Blocking), len(decision.Parked))
	}
	if decision.ParkedNotice() != "" {
		t.Errorf("ParkedNotice() = %q, want empty: the stop refuses this monitor", decision.ParkedNotice())
	}
}

// TestExemptFromGuard_AGateParkedRunWithUncommittedWorkIsStillPreserved keeps
// the cleanliness read on the CI branch alone. A gate park is expected to hold
// pipeline work in its worktree, and the stop preserves it regardless.
func TestExemptFromGuard_AGateParkedRunWithUncommittedWorkIsStillPreserved(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	plan := lifecycletest.Plan(types.StepReview, types.StepTest)
	parked := lifecycletest.SeedResumableParkedRun(t, p, "/tmp/project", "feature", plan)
	if err := os.WriteFile(filepath.Join(parked.WorkDir, "pending.go"), []byte("package pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	decision, err := Decide(p, plan, SameBinary)
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.Blocking) != 0 || len(decision.Parked) != 1 {
		t.Fatalf("Decide(dirty gate park) = %d blocking / %d preserved, want 0 / 1", len(decision.Blocking), len(decision.Parked))
	}
}

// TestExemptFromGuard_ACIMonitorWithADriftedStepPlanIsNotPreserved proves the
// plan-drift corroboration applies to the new exemption exactly as it does to
// a gate park.
func TestExemptFromGuard_ACIMonitorWithADriftedStepPlanIsNotPreserved(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	plan := lifecycletest.Plan(types.StepReview, types.StepPush, types.StepCI)
	lifecycletest.SeedResumableCIMonitorRun(t, p, "/tmp/project", "feature", "https://github.com/o/r/pull/7", plan)

	drifted, err := Decide(p, lifecycletest.Plan(types.StepReview, types.StepTest, types.StepPush, types.StepCI), SameBinary)
	if err != nil {
		t.Fatal(err)
	}
	if len(drifted.Blocking) != 1 || len(drifted.Parked) != 0 {
		t.Fatalf("Decide(drifted plan) = %d blocking / %d preserved, want 1 / 0", len(drifted.Blocking), len(drifted.Parked))
	}
	if drifted.ParkedNotice() != "" {
		t.Errorf("ParkedNotice() = %q, want empty", drifted.ParkedNotice())
	}
}

// TestPreservedRunNotice_DoesNotCallACIMonitorParked pins the wording: the
// preserved set now holds runs of two shapes, and only one of them is parked.
func TestPreservedRunNotice_DoesNotCallACIMonitorParked(t *testing.T) {
	prURL := "https://github.com/o/r/pull/7"
	monitor := &db.Run{ID: "run-ci", Status: types.RunRunning, Branch: "feature", HeadSHA: "abcdef1234567890", PRURL: &prURL}

	notice := GuardDecision{Parked: []*db.Run{monitor}}.ParkedNotice()
	if strings.Contains(notice, "parked") {
		t.Errorf("notice = %q, must not claim a CI monitor is parked", notice)
	}
	if !strings.Contains(notice, "1 pipeline run will be preserved and resumed when the daemon starts again") {
		t.Errorf("notice = %q, want the singular preservation sentence", notice)
	}
	if !strings.Contains(notice, "run-ci") || !strings.Contains(notice, "abcdef12") {
		t.Errorf("notice = %q, want the preserved run listed", notice)
	}
	if strings.Contains(notice, "active pipeline runs:") {
		t.Errorf("notice = %q, must not reuse the active-run caption", notice)
	}

	two := GuardDecision{Parked: []*db.Run{monitor, monitor}}.ParkedNotice()
	if !strings.Contains(two, "2 pipeline runs will be preserved") {
		t.Errorf("multi-run notice = %q, want plural agreement", two)
	}
}
