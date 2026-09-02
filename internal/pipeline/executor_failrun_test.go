package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestExecutor_FailRunMapsCancelCauseToStatus pins the terminal status a
// cancel cause produces via failRun. A pre-cancelled context is observed at
// the top of Execute's step loop before any step runs, so the cause reaches
// failRun directly.
func TestExecutor_FailRunMapsCancelCauseToStatus(t *testing.T) {
	tests := []struct {
		name       string
		cause      string
		wantStatus types.RunStatus
	}{
		{
			name:       "ci monitor interrupted by crash",
			cause:      types.RunCIMonitorInterruptedReason,
			wantStatus: types.RunCIMonitorInterrupted,
		},
		{
			name:       "ci monitor interrupted by drain",
			cause:      types.RunCIMonitorDrainedReason,
			wantStatus: types.RunCIMonitorInterrupted,
		},
		{
			name:       "plain shutdown is not a ci monitor interruption",
			cause:      "daemon shutting down",
			wantStatus: types.RunFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)

			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(errors.New(tt.cause))

			workDir := t.TempDir()
			err := exec.Execute(ctx, run, repo, workDir)
			if err == nil || err.Error() != tt.cause {
				t.Fatalf("Execute() error = %v, want %q", err, tt.cause)
			}

			got, dbErr := database.GetRun(run.ID)
			if dbErr != nil {
				t.Fatal(dbErr)
			}
			if got.Status != tt.wantStatus {
				t.Fatalf("run status = %q, want %q", got.Status, tt.wantStatus)
			}
			if got.Error == nil || *got.Error != tt.cause {
				t.Fatalf("run error = %v, want %q", got.Error, tt.cause)
			}
		})
	}
}
