// Package lifecycletest builds the state a destructive-lifecycle guard reads:
// a parked run that daemon startup recovery could genuinely resume. The guard
// corroborates every parked candidate against recovery's own preconditions, so
// a fixture that only sets the awaiting-agent marker no longer describes a
// preserved run, and every guard surface needs the same complete fixture.
package lifecycletest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ParkedRun identifies the state SeedResumableParkedRun wrote.
type ParkedRun struct {
	RepoID  string
	RunID   string
	Branch  string
	HeadSHA string
	WorkDir string
}

// SeedResumableParkedRun writes a run parked at a review gate under plan, with
// a real worktree whose HEAD is the run's head, pre-gate rows completed, the
// gate row complete, and post-gate rows pending.
func SeedResumableParkedRun(t *testing.T, p *paths.Paths, repoPath, branch string, plan []pipeline.Step) ParkedRun {
	t.Helper()

	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	repo, err := database.InsertRepo(repoPath, "git@github.com:user/"+filepath.Base(repoPath)+".git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, branch, "0000000000000000000000000000000000000000", "000")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return ParkRunAtGate(t, p, database, repo.ID, run.ID, plan)
}

// ParkRunAtGate turns an already-inserted run into the same resumable parked
// state, for a caller that built its own repo and run rows.
func ParkRunAtGate(t *testing.T, p *paths.Paths, database *db.DB, repoID, runID string, plan []pipeline.Step) ParkedRun {
	t.Helper()

	run, err := database.GetRun(runID)
	if err != nil || run == nil {
		t.Fatalf("get run %s: %v", runID, err)
	}

	workDir := p.WorktreeDir(repoID, run.ID)
	headSHA := initWorktree(t, workDir)
	if err := database.UpdateRunHeadSHA(run.ID, headSHA); err != nil {
		t.Fatalf("record run head: %v", err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := database.SetRunStepPlan(run.ID, stepNames(plan)); err != nil {
		t.Fatalf("record step plan: %v", err)
	}

	gateIndex := gateIndexOf(plan)
	findings := "[]"
	for i, step := range plan {
		row, err := database.InsertStepResult(run.ID, step.Name())
		if err != nil {
			t.Fatalf("insert step row: %v", err)
		}
		switch {
		case i < gateIndex:
			if err := database.CompleteStep(row.ID, 0, 10, ""); err != nil {
				t.Fatalf("complete step: %v", err)
			}
		case i == gateIndex:
			if err := database.StartStep(row.ID); err != nil {
				t.Fatalf("start gate step: %v", err)
			}
			if _, err := database.InsertStepRound(row.ID, 1, "initial", &findings, nil, 10); err != nil {
				t.Fatalf("insert gate round: %v", err)
			}
			if err := database.ParkStepForApproval(run.ID, row.ID, types.StepStatusAwaitingApproval, 10, &findings); err != nil {
				t.Fatalf("park gate step: %v", err)
			}
		}
	}

	return ParkedRun{RepoID: repoID, RunID: run.ID, Branch: run.Branch, HeadSHA: headSHA, WorkDir: workDir}
}

// SetGateAgentPID stamps the parked gate row with a live agent PID, the
// best-effort write recovery reads as an incomplete gate and refuses to resume.
func SetGateAgentPID(t *testing.T, p *paths.Paths, runID string, pid int) {
	t.Helper()

	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	rows, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	for _, row := range rows {
		if row.Status != types.StepStatusAwaitingApproval && row.Status != types.StepStatusFixReview {
			continue
		}
		if err := database.SetStepAgentActivity(row.ID, "agent working", &pid); err != nil {
			t.Fatalf("set gate agent pid: %v", err)
		}
		return
	}
	t.Fatalf("run %s has no gate step row", runID)
}

// Plan builds a step list from bare names, for a test that needs a layout the
// installed binary does not have.
func Plan(names ...types.StepName) []pipeline.Step {
	plan := make([]pipeline.Step, 0, len(names))
	for _, name := range names {
		plan = append(plan, namedStep(name))
	}
	return plan
}

type namedStep types.StepName

func (s namedStep) Name() types.StepName { return types.StepName(s) }

func (s namedStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return &pipeline.StepOutcome{}, nil
}

func gateIndexOf(plan []pipeline.Step) int {
	for i, step := range plan {
		if step.Name() == types.StepReview {
			return i
		}
	}
	return 0
}

func stepNames(plan []pipeline.Step) []types.StepName {
	names := make([]types.StepName, 0, len(plan))
	for _, step := range plan {
		names = append(names, step.Name())
	}
	return names
}

// initWorktree creates a one-commit git repository and returns its head.
func initWorktree(t *testing.T, dir string) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("parked\n"), 0o644); err != nil {
		t.Fatalf("write worktree file: %v", err)
	}
	gitCmd(t, dir, "init", "-b", "main")
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "parked run head")
	return gitCmd(t, dir, "rev-parse", "HEAD")
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()

	full := append([]string{
		"-c", "user.name=no-mistakes test",
		"-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	// Ambient GIT_CONFIG_* injection from an agent harness must not reach a
	// fixture repository.
	cmd.Env = append(os.Environ(), "GIT_CONFIG_COUNT=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return trimNewline(string(out))
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
