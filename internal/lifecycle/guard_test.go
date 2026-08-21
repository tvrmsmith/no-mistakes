package lifecycle

import (
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle/lifecycletest"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func gateStep(status types.StepStatus) []*db.StepResult {
	return []*db.StepResult{
		{StepName: types.StepIntent, Status: types.StepStatusCompleted},
		{StepName: types.StepReview, Status: status},
	}
}

func TestParkedAtGate(t *testing.T) {
	awaitingSince := int64(100)

	pending := &db.Run{ID: "pending", Status: types.RunPending}
	runningNotParked := &db.Run{ID: "running-not-parked", Status: types.RunRunning}
	runningParked := &db.Run{ID: "running-parked", Status: types.RunRunning, AwaitingAgentSince: &awaitingSince}

	if ParkedAtGate(pending, gateStep(types.StepStatusAwaitingApproval)) {
		t.Errorf("ParkedAtGate(pending) = true, want false")
	}
	if ParkedAtGate(runningNotParked, gateStep(types.StepStatusAwaitingApproval)) {
		t.Errorf("ParkedAtGate(running, nil AwaitingAgentSince) = true, want false")
	}
	if !ParkedAtGate(runningParked, gateStep(types.StepStatusAwaitingApproval)) {
		t.Errorf("ParkedAtGate(running, awaiting_approval step) = false, want true")
	}
	if !ParkedAtGate(runningParked, gateStep(types.StepStatusFixReview)) {
		t.Errorf("ParkedAtGate(running, fix_review step) = false, want true")
	}
	if ParkedAtGate(nil, nil) {
		t.Errorf("ParkedAtGate(nil) = true, want false")
	}
}

// TestParkedAtGateRequiresAGateStepRow proves the run marker alone cannot
// present a run as preserved: the awaiting-agent write is best-effort, so a
// stale marker over a run whose steps are all running must stay blocking.
func TestParkedAtGateRequiresAGateStepRow(t *testing.T) {
	awaitingSince := int64(100)
	run := &db.Run{ID: "stale-marker", Status: types.RunRunning, AwaitingAgentSince: &awaitingSince}

	if ParkedAtGate(run, gateStep(types.StepStatusRunning)) {
		t.Error("ParkedAtGate(stale marker, no gate step) = true, want false")
	}
	if ParkedAtGate(run, nil) {
		t.Error("ParkedAtGate(stale marker, no step rows) = true, want false")
	}
}

// TestStepPlanDrifted covers the signal update uses to decide whether the
// binary it installs could actually resume a preserved run.
func TestStepPlanDrifted(t *testing.T) {
	current := []types.StepName{types.StepReview, types.StepTest}
	same := &db.Run{ID: "same", StepPlan: []types.StepName{types.StepReview, types.StepTest}}
	reordered := &db.Run{ID: "reordered", StepPlan: []types.StepName{types.StepTest, types.StepReview}}
	shorter := &db.Run{ID: "shorter", StepPlan: []types.StepName{types.StepReview}}
	legacy := &db.Run{ID: "legacy"}

	if StepPlanDrifted(same, current) {
		t.Error("StepPlanDrifted(matching plan) = true, want false")
	}
	if !StepPlanDrifted(reordered, current) {
		t.Error("StepPlanDrifted(reordered plan) = false, want true")
	}
	if !StepPlanDrifted(shorter, current) {
		t.Error("StepPlanDrifted(shorter plan) = false, want true")
	}
	if !StepPlanDrifted(legacy, current) {
		t.Error("StepPlanDrifted(unrecorded plan) = false, want true")
	}
	if StepPlanDrifted(legacy, nil) {
		t.Error("StepPlanDrifted(no required plan) = true, want false")
	}
	// An empty-but-non-nil plan proves nothing about any run, so it must not
	// quietly turn the check off and exempt every parked run.
	if !StepPlanDrifted(same, []types.StepName{}) {
		t.Error("StepPlanDrifted(empty required plan) = false, want true")
	}
	if !StepPlanDrifted(legacy, []types.StepName{}) {
		t.Error("StepPlanDrifted(empty required plan, unrecorded run plan) = false, want true")
	}
}

func TestParkedRunNotice(t *testing.T) {
	awaitingSince := int64(100)
	one := &db.Run{ID: "run-1", Status: types.RunRunning, Branch: "feature", HeadSHA: "abcdef1234567890", AwaitingAgentSince: &awaitingSince}
	two := &db.Run{ID: "run-2", Status: types.RunRunning, Branch: "other", HeadSHA: "1234567890abcdef", AwaitingAgentSince: &awaitingSince}

	if got := (GuardDecision{}).ParkedNotice(); got != "" {
		t.Errorf("ParkedNotice(no parked runs) = %q, want empty", got)
	}

	single := GuardDecision{Parked: []*db.Run{one}}.ParkedNotice()
	if !strings.Contains(single, "1 parked pipeline run will be preserved and resumed when the daemon starts again") {
		t.Errorf("single-run notice = %q, want singular preservation sentence", single)
	}
	if !strings.Contains(single, "parked pipeline runs:") {
		t.Errorf("notice = %q, want its own list caption", single)
	}
	if strings.Contains(single, "active pipeline runs:") {
		t.Errorf("notice = %q, must not reuse the active-run caption", single)
	}
	if !strings.Contains(single, "run-1") || !strings.Contains(single, "abcdef12") {
		t.Errorf("notice = %q, want the parked run listed", single)
	}

	plural := GuardDecision{Parked: []*db.Run{one, two}}.ParkedNotice()
	if !strings.Contains(plural, "2 parked pipeline runs will be preserved") {
		t.Errorf("multi-run notice = %q, want plural preservation sentence", plural)
	}
}

func TestSplitActiveRuns(t *testing.T) {
	awaitingSince := int64(100)

	pending := &db.Run{ID: "pending", Status: types.RunPending}
	runningNotParked := &db.Run{ID: "running-not-parked", Status: types.RunRunning}
	runningParked := &db.Run{ID: "running-parked", Status: types.RunRunning, AwaitingAgentSince: &awaitingSince}

	runs := []*db.Run{pending, runningNotParked, runningParked}
	stepsByRun := map[string][]*db.StepResult{
		runningParked.ID: gateStep(types.StepStatusAwaitingApproval),
	}

	blocking, parked := splitActiveRuns(runs, stepsByRun, nil, nil)
	if len(blocking) != 2 || blocking[0] != pending || blocking[1] != runningNotParked {
		t.Errorf("blocking = %v, want [pending, runningNotParked]", blocking)
	}
	if len(parked) != 1 || parked[0] != runningParked {
		t.Errorf("parked = %v, want [runningParked]", parked)
	}

	// The same predicate the live guard splits on: a required plan the run
	// cannot prove it matches moves it back into the blocking set.
	requiredPlan := []types.StepName{types.StepReview}
	blocking, parked = splitActiveRuns(runs, stepsByRun, requiredPlan, nil)
	if len(parked) != 0 {
		t.Errorf("parked(required plan) = %v, want none exempt", parked)
	}
	if len(blocking) != 3 {
		t.Errorf("blocking(required plan) = %v, want all three", blocking)
	}
}

// TestDecideExemptsOnlyResumableParkedRuns walks the real state DB: a parked
// run is exempt for the binary that recorded its plan, and blocking for one
// whose pipeline layout has drifted.
func TestDecideExemptsOnlyResumableParkedRuns(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	plan := lifecycletest.Plan(types.StepReview, types.StepTest)
	lifecycletest.SeedResumableParkedRun(t, p, "/tmp/project", "feature", plan)

	same, err := Decide(p, plan, ReplacementBinary)
	if err != nil {
		t.Fatal(err)
	}
	if len(same.Blocking) != 0 || len(same.Parked) != 1 {
		t.Fatalf("Decide(same plan) = %d blocking / %d parked, want 0 / 1", len(same.Blocking), len(same.Parked))
	}
	if !strings.Contains(same.ParkedNotice(), "will be preserved and resumed") {
		t.Errorf("Decide(same plan).ParkedNotice() = %q, want the preservation promise", same.ParkedNotice())
	}
	// An update swaps the binary that resumes these runs, so the promise it
	// prints must state the limit of the check that backs it rather than a
	// certainty.
	if !strings.Contains(same.ParkedNotice(), "unless the version being installed changes the pipeline step layout") {
		t.Errorf("Decide(ReplacementBinary).ParkedNotice() = %q, want the binary-swap qualifier", same.ParkedNotice())
	}

	// A stop or restart brings the same binary back, so its promise carries no
	// such qualifier even though it checks the same plan.
	sameBinary, err := Decide(p, plan, SameBinary)
	if err != nil {
		t.Fatal(err)
	}
	if len(sameBinary.Parked) != 1 {
		t.Fatalf("Decide(SameBinary) = %d parked, want 1", len(sameBinary.Parked))
	}
	if strings.Contains(sameBinary.ParkedNotice(), "unless the version being installed") {
		t.Errorf("Decide(SameBinary).ParkedNotice() = %q, want no binary-swap qualifier", sameBinary.ParkedNotice())
	}

	drifted, err := Decide(p, lifecycletest.Plan(types.StepReview, types.StepTest, types.StepPush), SameBinary)
	if err != nil {
		t.Fatal(err)
	}
	if len(drifted.Blocking) != 1 || len(drifted.Parked) != 0 {
		t.Fatalf("Decide(drifted plan) = %d blocking / %d parked, want 1 / 0", len(drifted.Blocking), len(drifted.Parked))
	}
	if drifted.ParkedNotice() != "" {
		t.Errorf("Decide(drifted plan).ParkedNotice() = %q, want empty", drifted.ParkedNotice())
	}
}

// TestDecideKeepsAStaleMarkerRunBlocking walks the same real state DB for the
// case the marker alone would get wrong: the awaiting-agent write survived but
// no step row is at a gate, so the run is mid-step and a stop would disrupt it.
func TestDecideKeepsAStaleMarkerRunBlocking(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepo("/tmp/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	plan := lifecycletest.Plan(types.StepReview)
	run, err := database.InsertRun(repo.ID, "feature", "aaa111", "000")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunStepPlan(run.ID, []types.StepName{types.StepReview}); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(step.ID, types.StepStatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	decision, err := Decide(p, plan, SameBinary)
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.Blocking) != 1 || len(decision.Parked) != 0 {
		t.Fatalf("Decide(stale marker) = %d blocking / %d parked, want 1 / 0", len(decision.Blocking), len(decision.Parked))
	}
	if decision.ParkedNotice() != "" {
		t.Errorf("Decide(stale marker).ParkedNotice() = %q, want no preservation promise", decision.ParkedNotice())
	}
}

// TestDecideCorroboratesParkedRunsAgainstRecovery proves the exemption is
// backed by recovery's own preconditions rather than a second reading of the
// same rows: a parked run whose worktree is gone, and one whose gate row still
// carries an agent PID, are both refused by startup recovery, so neither may
// be counted as preserved here.
func TestDecideCorroboratesParkedRunsAgainstRecovery(t *testing.T) {
	plan := lifecycletest.Plan(types.StepReview, types.StepTest)

	t.Run("missing worktree", func(t *testing.T) {
		p := paths.WithRoot(t.TempDir())
		parked := lifecycletest.SeedResumableParkedRun(t, p, "/tmp/project", "feature", plan)
		if err := os.RemoveAll(parked.WorkDir); err != nil {
			t.Fatal(err)
		}

		decision, err := Decide(p, plan, SameBinary)
		if err != nil {
			t.Fatal(err)
		}
		if len(decision.Blocking) != 1 || len(decision.Parked) != 0 {
			t.Fatalf("Decide(no worktree) = %d blocking / %d parked, want 1 / 0", len(decision.Blocking), len(decision.Parked))
		}
		if decision.ParkedNotice() != "" {
			t.Errorf("Decide(no worktree).ParkedNotice() = %q, want no promise", decision.ParkedNotice())
		}
	})

	t.Run("gate row holds an agent pid", func(t *testing.T) {
		p := paths.WithRoot(t.TempDir())
		parked := lifecycletest.SeedResumableParkedRun(t, p, "/tmp/project", "feature", plan)
		lifecycletest.SetGateAgentPID(t, p, parked.RunID, 4242)

		decision, err := Decide(p, plan, SameBinary)
		if err != nil {
			t.Fatal(err)
		}
		if len(decision.Blocking) != 1 || len(decision.Parked) != 0 {
			t.Fatalf("Decide(stale agent pid) = %d blocking / %d parked, want 1 / 0", len(decision.Blocking), len(decision.Parked))
		}
		if decision.ParkedNotice() != "" {
			t.Errorf("Decide(stale agent pid).ParkedNotice() = %q, want no promise", decision.ParkedNotice())
		}
	})
}
