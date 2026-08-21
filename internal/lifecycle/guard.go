package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ParkedAtGate reports whether run is parked at an awaiting_approval or
// fix_review gate. Two independent facts must agree: the run carries the
// agent-parked marker, and one of its step rows is actually sitting at a
// gate. The marker alone is a single best-effort write, so a stale one must
// never by itself present a run as preserved across a stop; steps are the
// run's step rows, in any order.
func ParkedAtGate(run *db.Run, steps []*db.StepResult) bool {
	if run == nil || run.Status != types.RunRunning || run.AwaitingAgentSince == nil {
		return false
	}
	for _, step := range steps {
		if step == nil {
			continue
		}
		if step.Status == types.StepStatusAwaitingApproval || step.Status == types.StepStatusFixReview {
			return true
		}
	}
	return false
}

// StepPlanDrifted reports whether run's recorded step plan differs from want.
// Only a nil want disables the check, because a caller that computed an empty
// plan proved nothing: under it no recorded plan is compatible. An unrecorded
// plan (legacy row) can never be proven compatible and counts as drifted.
func StepPlanDrifted(run *db.Run, want []types.StepName) bool {
	if want == nil {
		return false
	}
	if run == nil || len(run.StepPlan) == 0 || len(run.StepPlan) != len(want) {
		return true
	}
	for i := range want {
		if run.StepPlan[i] != want[i] {
			return true
		}
	}
	return false
}

// exemptFromGuard is the single predicate every guard surface splits on: the
// run is genuinely parked at a gate, the plan that would resume it still
// matches the one it was started under, and recovery's own preconditions
// corroborate that the next start could actually pick it up. resumable is nil
// only where a caller has no state to corroborate against.
func exemptFromGuard(run *db.Run, steps []*db.StepResult, requiredStepPlan []types.StepName, resumable func(*db.Run) bool) bool {
	if !ParkedAtGate(run, steps) || StepPlanDrifted(run, requiredStepPlan) {
		return false
	}
	return resumable == nil || resumable(run)
}

// splitActiveRuns divides the active runs into the ones a stop/restart/update
// would actually disrupt and the exempt complement that survives it and
// resumes when the daemon starts again. Order is preserved from the input.
func splitActiveRuns(runs []*db.Run, stepsByRun map[string][]*db.StepResult, requiredStepPlan []types.StepName, resumable func(*db.Run) bool) (blocking, parked []*db.Run) {
	for _, run := range runs {
		if run != nil && exemptFromGuard(run, stepsByRun[run.ID], requiredStepPlan, resumable) {
			parked = append(parked, run)
			continue
		}
		blocking = append(blocking, run)
	}
	return blocking, parked
}

// GuardDecision is the shared answer every destructive lifecycle command needs:
// which active runs the action would disrupt, and the operator-facing notice
// for the ones it would preserve. Call sites own only their writers and
// message strings.
type GuardDecision struct {
	Blocking []*db.Run
	Parked   []*db.Run
	// binarySwap records that the binary resuming the preserved runs is not
	// the one that answered this guard, so the promise states the limit of the
	// drift check backing it instead of a certainty.
	binarySwap bool
}

// ResumingBinary names which executable will resume the preserved runs, which
// decides how strong a promise the guard may print.
type ResumingBinary int

const (
	// SameBinary is a stop or restart: the executable installed now is the one
	// that comes back, so its step plan is the plan that will resume the runs.
	SameBinary ResumingBinary = iota
	// ReplacementBinary is an update: the incoming layout does not exist yet
	// at guard time, so the promise carries that limit.
	ReplacementBinary
)

const binarySwapQualifier = " unless the version being installed changes the pipeline step layout, which could only be checked against the layout installed now"

// ParkedNotice is the single owner of the operator-facing promise that parked
// runs survive a stop/restart/update. Callers print it only on a path that
// actually stops the daemon; it is empty when no run is parked.
func (d GuardDecision) ParkedNotice() string {
	qualifier := ""
	if d.binarySwap {
		qualifier = binarySwapQualifier
	}
	return parkedRunNotice(d.Parked, qualifier)
}

// corroborationTimeout bounds the per-run git read the guard makes while
// corroborating a parked candidate, so an unresponsive filesystem cannot hang
// a stop or an update.
const corroborationTimeout = 5 * time.Second

// Decide splits the active runs once into the blocking set and the preserved
// complement. resumeSteps is the pipeline layout of the binary answering this
// guard: its ordered names are the plan a parked run must match, and it is the
// step list recovery would resume the run under, so every candidate is
// corroborated against ResumePreconditionsMet before it earns the exemption. A
// candidate that fails corroboration counts as blocking, because the guard must
// never promise preservation for a run the next start would terminally fail.
// Passing nil steps disables both checks entirely.
func Decide(p *paths.Paths, resumeSteps []pipeline.Step, resuming ResumingBinary) (GuardDecision, error) {
	database, err := openStateDB(p)
	if err != nil {
		return GuardDecision{}, err
	}
	if database == nil {
		return GuardDecision{binarySwap: resuming == ReplacementBinary}, nil
	}
	defer database.Close()

	runs, stepsByRun, err := activeRunsWithSteps(database)
	if err != nil {
		return GuardDecision{}, err
	}
	var resumable func(*db.Run) bool
	if resumeSteps != nil {
		resumable = func(run *db.Run) bool {
			ctx, cancel := context.WithTimeout(context.Background(), corroborationTimeout)
			defer cancel()
			return ResumePreconditionsMet(ctx, database, p, run, resumeSteps) == nil
		}
	}
	blocking, parked := splitActiveRuns(runs, stepsByRun, stepPlanOf(resumeSteps), resumable)
	return GuardDecision{
		Blocking:   blocking,
		Parked:     parked,
		binarySwap: resuming == ReplacementBinary,
	}, nil
}

// stepPlanOf renders the ordered plan a run must have recorded to be resumable
// under steps. A nil step list stays nil so StepPlanDrifted keeps treating it
// as "no plan to check against".
func stepPlanOf(steps []pipeline.Step) []types.StepName {
	if steps == nil {
		return nil
	}
	names := make([]types.StepName, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.Name())
	}
	return names
}

// ActiveRuns returns all pending/running pipeline runs from the local state DB.
// It is a plain inspection read with no guard semantics: nothing is classified
// as blocking or preserved, so lifecycle decisions must use Decide.
func ActiveRuns(p *paths.Paths) ([]*db.Run, error) {
	database, err := openStateDB(p)
	if err != nil || database == nil {
		return nil, err
	}
	defer database.Close()
	return database.GetActiveRuns()
}

// openStateDB opens the local state DB, returning a nil handle and no error
// when there is nothing to inspect yet.
func openStateDB(p *paths.Paths) (*db.DB, error) {
	if p == nil {
		return nil, nil
	}
	dbPath := p.DB()
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat database: %w", err)
	}
	return db.Open(dbPath)
}

func activeRunsWithSteps(database *db.DB) ([]*db.Run, map[string][]*db.StepResult, error) {
	runs, err := database.GetActiveRuns()
	if err != nil {
		return nil, nil, err
	}
	stepsByRun := make(map[string][]*db.StepResult, len(runs))
	for _, run := range runs {
		steps, err := database.GetStepsByRun(run.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("get steps for run %s: %w", run.ID, err)
		}
		stepsByRun[run.ID] = steps
	}
	return runs, stepsByRun, nil
}

// parkedRunNotice renders the promise with an optional qualifier clause, so a
// caller whose resuming binary may differ states the guarantee it actually
// has rather than a certainty.
func parkedRunNotice(parked []*db.Run, qualifier string) string {
	if len(parked) == 0 {
		return ""
	}
	runWord, _ := RunCountWords(len(parked))
	return fmt.Sprintf("%d parked pipeline %s will be preserved and resumed when the daemon starts again%s\n", len(parked), runWord, qualifier) +
		RunListWith("parked pipeline runs:", parked)
}

// RunCountWords agrees a run count's noun and verb. The parked exemption makes
// a count of exactly one the common case on every guard surface, so no caller
// hardcodes the plural.
func RunCountWords(n int) (runWord, verb string) {
	if n == 1 {
		return "run", "is"
	}
	return "runs", "are"
}

func RunList(runs []*db.Run) string {
	return RunListWith("active pipeline runs:", runs)
}

// RunListWith renders runs under an explicit caption, so a command printing
// two different sets (blocking and parked) never labels both "active".
func RunListWith(caption string, runs []*db.Run) string {
	if len(runs) == 0 {
		return ""
	}
	out := caption + "\n"
	for _, run := range runs {
		out += fmt.Sprintf("  %s  %s  %s  %s\n", run.ID, run.Status, run.Branch, ShortSHA(run.HeadSHA))
	}
	return out
}

func ShortSHA(sha string) string {
	if len(sha) <= 8 {
		return sha
	}
	return sha[:8]
}
