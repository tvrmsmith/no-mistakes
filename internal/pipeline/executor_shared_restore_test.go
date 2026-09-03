package pipeline

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// resumeAndReadSharedDiscovery parks a run at a review gate, resumes it, and
// returns what the step after the gate found in the run's shared state. Only
// Resume can prove the recovered wiring: a run that pays a second cold
// discovery agent pass is exactly what the restore exists to prevent.
func resumeAndReadSharedDiscovery(t *testing.T, seed string, fingerprint string) (TestDiscovery, bool) {
	t.Helper()
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"decision","action":"ask-user"}]}`
	const reviewedHead = "3333333333333333333333333333333333333333"
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, reviewedHead, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepResult(run.ID, types.StepTest); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunTestDiscovery(run.ID, seed); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	var (
		got TestDiscovery
		hit bool
	)
	after := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			got, hit = sctx.Shared.TestDiscovery(fingerprint)
			return &StepOutcome{}, nil
		},
	}
	steps := []Step{newApprovalStep(types.StepReview, findings), after}
	exec := NewExecutor(database, p, &config.Config{}, nil, steps, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered review never accepted approval")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovered executor timed out")
	}
	return got, hit
}

func TestExecutor_ResumedRunReusesTheRunsPersistedTestDiscovery(t *testing.T) {
	seed, err := json.Marshal(map[string]any{
		"fingerprint": "fp-1",
		"units":       []config.TestUnit{{Name: "api", Path: "services/api", Command: "go test ./services/api/..."}},
		"selected":    []string{"api"},
		"source":      "agent",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, hit := resumeAndReadSharedDiscovery(t, string(seed), "fp-1")
	if !hit {
		t.Fatal("a resumed run did not see the discovery its earlier process persisted")
	}
	if len(got.Units) != 1 || got.Units[0].Command != "go test ./services/api/..." {
		t.Fatalf("restored units = %+v", got.Units)
	}
	if len(got.Selected) != 1 || got.Selected[0] != "api" || got.Source != "agent" {
		t.Fatalf("restored selection = %v, source = %q", got.Selected, got.Source)
	}
}

func TestExecutor_ResumedRunDoesNotReuseADiscoveryFromAnotherChangedFileSet(t *testing.T) {
	seed, err := json.Marshal(map[string]any{
		"fingerprint": "fp-1",
		"units":       []config.TestUnit{{Name: "api", Path: "services/api", Command: "go test"}},
		"selected":    []string{"api"},
		"source":      "agent",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, hit := resumeAndReadSharedDiscovery(t, string(seed), "fp-2"); hit {
		t.Fatal("a resumed run reused a discovery derived from a different changed-file set")
	}
}
