package steps

import (
	"slices"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAllStepsMatchesTypesAllSteps pins the sequence the daemon executes
// (steps.AllSteps) against the sequence types.AllSteps orders and persists. A
// step in only one list would otherwise get Order() == 0, which
// db.ResetStepsFrom turns into a reset from the top of the pipeline.
func TestAllStepsMatchesTypesAllSteps(t *testing.T) {
	t.Setenv("NM_DEMO", "")

	executed := make([]types.StepName, 0, len(AllSteps()))
	for _, s := range AllSteps() {
		executed = append(executed, s.Name())
	}
	ordered := types.AllSteps()

	if !slices.Equal(executed, ordered) {
		t.Fatalf("steps.AllSteps() = %v, types.AllSteps() = %v; the executed sequence and the persisted order must match, so add or move the step in both lists (internal/pipeline/steps/common.go AllSteps and internal/types/types.go allSteps)", executed, ordered)
	}
}
