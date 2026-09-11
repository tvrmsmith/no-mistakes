package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle/lifecycletest"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func runFromState(t *testing.T, p *paths.Paths, runID string) *db.Run {
	t.Helper()

	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatalf("run %s is missing from the state db", runID)
	}
	return run
}

// A commit the run never recorded is the window steps.commitRepair opens: the
// checkout is clean and one commit ahead of run.HeadSHA. The rule stays strict
// rather than accepting a descendant, because on the gate-parked path a
// descendant head is the adverse evidence it exists to catch, and a stop that
// preserved it would promise a resume the next start refuses.
func TestWorktreeMatchesRun_AHeadTheRunNeverRecordedIsAdverseEvidence(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	plan := lifecycletest.Plan(types.StepReview, types.StepTest)
	parked := lifecycletest.SeedResumableParkedRun(t, p, "/tmp/project", "feature", plan)
	ahead := lifecycletest.CommitInWorktree(t, parked.WorkDir, "repair.go", "package repaired\n")
	if ahead == parked.HeadSHA {
		t.Fatal("the fixture did not move the worktree head")
	}

	err := WorktreeMatchesRun(context.Background(), p, runFromState(t, p, parked.RunID))
	if err == nil {
		t.Fatal("WorktreeMatchesRun(head ahead of the run) = nil, want a refusal")
	}
	if errors.Is(err, pipeline.ErrRecoveryEvidenceUnavailable) {
		t.Fatalf("WorktreeMatchesRun() error = %v, want adverse evidence rather than an incomplete read", err)
	}

	decision, decideErr := Decide(p, plan, SameBinary)
	if decideErr != nil {
		t.Fatal(decideErr)
	}
	if len(decision.Blocking) != 1 || len(decision.Parked) != 0 {
		t.Fatalf("Decide(head ahead of the run) = %d blocking / %d preserved, want 1 / 0", len(decision.Blocking), len(decision.Parked))
	}
	if decision.ParkedNotice() != "" {
		t.Errorf("ParkedNotice() = %q, want empty: the next start refuses this run", decision.ParkedNotice())
	}
}

// A head that matches keeps its exemption, so the check above is measuring the
// commit rather than the fixture.
func TestWorktreeMatchesRun_TheRecordedHeadStillResumes(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	plan := lifecycletest.Plan(types.StepReview, types.StepTest)
	parked := lifecycletest.SeedResumableParkedRun(t, p, "/tmp/project", "feature", plan)

	if err := WorktreeMatchesRun(context.Background(), p, runFromState(t, p, parked.RunID)); err != nil {
		t.Fatalf("WorktreeMatchesRun(recorded head) = %v, want nil", err)
	}
}

// A head that cannot be read says nothing about the run, so it must not be
// reported as an established fact: recovery defers on this and keeps the
// worktree, where an adverse finding fails the run terminally.
func TestWorktreeMatchesRun_AnUnreadableHeadIsUnavailableEvidence(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	plan := lifecycletest.Plan(types.StepReview, types.StepTest)
	parked := lifecycletest.SeedResumableParkedRun(t, p, "/tmp/project", "feature", plan)
	if err := os.RemoveAll(filepath.Join(parked.WorkDir, ".git")); err != nil {
		t.Fatal(err)
	}

	err := WorktreeMatchesRun(context.Background(), p, runFromState(t, p, parked.RunID))
	if !errors.Is(err, pipeline.ErrRecoveryEvidenceUnavailable) {
		t.Fatalf("WorktreeMatchesRun(unreadable worktree) error = %v, want ErrRecoveryEvidenceUnavailable", err)
	}

	decision, decideErr := Decide(p, plan, SameBinary)
	if decideErr != nil {
		t.Fatal(decideErr)
	}
	if len(decision.Parked) != 0 {
		t.Fatalf("Decide(unreadable worktree) preserved %d runs, want 0", len(decision.Parked))
	}
}
