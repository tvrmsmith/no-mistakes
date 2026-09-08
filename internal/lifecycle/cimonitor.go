package lifecycle

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ciMonitorActiveStatuses is the wider set db.failActiveRuns' interrupted-CI
// lift already encodes in SQL.
var ciMonitorActiveStatuses = map[types.StepStatus]bool{
	types.StepStatusRunning:          true,
	types.StepStatusAwaitingApproval: true,
	types.StepStatusFixing:           true,
	types.StepStatusFixReview:        true,
}

// ResumableCIMonitor reports whether run's only active step is a live CI
// monitor that daemon startup recovery could re-enter as it stands: the run is
// running with a PR URL, and its single active step row is a running
// types.StepCI row holding no agent pid.
//
// The PR URL matters because the CI step row is already running while the step
// builds its host and before it bails out with "no PR URL found", and there is
// nothing for a re-entered monitor to poll without one.
//
// Only a running row is re-enterable, which is why this set is narrower than
// CIMonitorRun's. A fixing or fix_review row is an auto-fix agent that died
// partway through a repair, with half-written edits in the worktree that no
// round record explains, and an awaiting_approval row is the window
// CompleteRunAwaitingAgent opens while an answer the operator already gave is
// being applied.
//
// The agent pid is a separate fact from the status and cannot be folded into
// it. The CI step runs its auto-fix agent inline from inside Execute, and the
// executor only writes fixing for a step whose outcome is auto-fixable, which
// a CI outcome never is, so the row stays running for the whole repair while
// the agent's pid is recorded against it. Status alone therefore cannot tell a
// bare monitor from a live repair, and the drain reads this predicate to
// decide what it may walk away from.
func ResumableCIMonitor(run *db.Run, steps []*db.StepResult) bool {
	return ciMonitorShape(run, steps, func(step *db.StepResult) bool {
		return step.Status == types.StepStatusRunning && step.AgentPID == nil
	})
}

// CIMonitorRun reports the wider CI-monitor shape db.failActiveRuns' lift
// already encodes in SQL (the recoverInterruptedCIMonitors block in
// internal/db/run.go): a running run with a PR URL whose CI row is active in
// any of running, awaiting_approval, fixing, or fix_review, and no other row
// is active in any of those. Keep the two in step.
//
// It answers only "what terminal status does this run deserve", never "can it
// be resumed"; ResumableCIMonitor owns that.
// It deliberately ignores the agent pid ResumableCIMonitor refuses on, because
// the SQL lift it mirrors does too: a run whose repair agent died still
// deserves the interrupted-monitor status that spares its worktree, even
// though nothing may re-enter it.
func CIMonitorRun(run *db.Run, steps []*db.StepResult) bool {
	return ciMonitorShape(run, steps, func(step *db.StepResult) bool {
		return ciMonitorActiveStatuses[step.Status]
	})
}

// ciMonitorShape is the common test both predicates apply, differing only in
// which CI rows each accepts. Any other row in the wider active set
// disqualifies the run under both, because a second live step means the run is
// not sitting in its monitor.
func ciMonitorShape(run *db.Run, steps []*db.StepResult, ciRowQualifies func(*db.StepResult) bool) bool {
	if run == nil || run.Status != types.RunRunning {
		return false
	}
	if run.PRURL == nil || strings.TrimSpace(*run.PRURL) == "" {
		return false
	}
	ciActive := false
	for _, step := range steps {
		if step == nil || !ciMonitorActiveStatuses[step.Status] {
			continue
		}
		if step.StepName != types.StepCI {
			return false
		}
		if !ciRowQualifies(step) {
			return false
		}
		ciActive = true
	}
	return ciActive
}
