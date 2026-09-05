package lifecycletest

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// MonitoringRun identifies the state SeedResumableCIMonitorRun wrote.
type MonitoringRun struct {
	RepoID  string
	RunID   string
	Branch  string
	HeadSHA string
	WorkDir string
	PRURL   string
}

// SeedResumableCIMonitorRun writes the second shape a stop preserves: a run
// whose only active step is a live CI monitor polling an open PR, with a real
// worktree at the run's head and every earlier step row completed. The CI step
// must be the last step of plan, which is where the real pipeline puts it.
func SeedResumableCIMonitorRun(t *testing.T, p *paths.Paths, repoPath, branch, prURL string, plan []pipeline.Step) MonitoringRun {
	t.Helper()

	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	repo, err := database.InsertRepo(repoPath, "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, branch, "0000000000000000000000000000000000000000", "000")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return MonitorRunAtCI(t, p, database, repo.ID, run.ID, prURL, plan)
}

// MonitorRunAtCI turns an already-inserted run into the same live-CI-monitor
// state, for a caller that built its own repo and run rows.
func MonitorRunAtCI(t *testing.T, p *paths.Paths, database *db.DB, repoID, runID, prURL string, plan []pipeline.Step) MonitoringRun {
	t.Helper()

	run, err := database.GetRun(runID)
	if err != nil || run == nil {
		t.Fatalf("get run %s: %v", runID, err)
	}
	ciIndex := ciIndexOf(t, plan)

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
	if err := database.UpdateRunPRURL(run.ID, prURL); err != nil {
		t.Fatalf("record pr url: %v", err)
	}

	for i := 0; i <= ciIndex; i++ {
		row, err := database.InsertStepResult(run.ID, plan[i].Name())
		if err != nil {
			t.Fatalf("insert step row: %v", err)
		}
		if i < ciIndex {
			if err := database.CompleteStep(row.ID, 0, 10, ""); err != nil {
				t.Fatalf("complete step: %v", err)
			}
			continue
		}
		if err := database.StartStep(row.ID); err != nil {
			t.Fatalf("start ci step: %v", err)
		}
	}

	return MonitoringRun{RepoID: repoID, RunID: run.ID, Branch: run.Branch, HeadSHA: headSHA, WorkDir: workDir, PRURL: prURL}
}

func ciIndexOf(t *testing.T, plan []pipeline.Step) int {
	t.Helper()

	for i, step := range plan {
		if step.Name() == types.StepCI {
			if i != len(plan)-1 {
				t.Fatalf("ci step is at index %d of %d; the fixture needs it last", i, len(plan))
			}
			return i
		}
	}
	t.Fatalf("plan has no %s step", types.StepCI)
	return 0
}
