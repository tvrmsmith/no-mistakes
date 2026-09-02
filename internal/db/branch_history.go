package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// BranchStepHistory is what a previous run on the same branch recorded for one
// step. The run's terminal status travels with the rounds because it is the
// only evidence of whether the user finished deciding: a finding left
// unselected in a completed run was accepted as-is, while the same finding in a
// failed or cancelled run was simply never reached.
type BranchStepHistory struct {
	RunID     string
	RunStatus types.RunStatus
	Rounds    []*StepRound
}

// CountEarlierBranchStepRounds returns how many rounds of the named step every
// EARLIER run of the same repo and branch recorded, so a step that ramps with
// round count measures the branch rather than the run. A run that fails
// mid-review (an agent timeout, a network blip, a daemon crash) takes its
// step_results row's identity with it, so the next push starts a fresh row and
// a per-run count restarts at one however many rounds the branch has already
// absorbed.
//
// Every earlier run counts, including one that recorded no findings: the cost
// this feeds is the review effort already spent on the branch, which a round
// that came back empty paid in full.
//
// excludeRunID is the caller's own run, whose rounds the caller already holds.
func (d *DB) CountEarlierBranchStepRounds(repoID, branch string, step types.StepName, excludeRunID string) (int, error) {
	var count int
	err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM step_rounds sro
		 JOIN step_results sr ON sr.id = sro.step_result_id
		 JOIN runs r ON r.id = sr.run_id
		 WHERE r.repo_id = ? AND r.branch = ? AND r.id <> ? AND sr.step_name = ?`,
		repoID, branch, excludeRunID, string(step),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count earlier branch step rounds: %w", err)
	}
	return count, nil
}

// PreviousBranchStepHistory returns the named step's rounds from the most
// recent earlier run of the same repo and branch, so a step can see what a
// previous push already reported and what was done about it. Only a run that
// actually recorded findings qualifies: a run whose step never produced any
// carries no reusable history, and letting it win would hide the last run that
// did. Returns nil when the branch has no such history.
//
// excludeRunID is the caller's own run, which must never count as its own
// history.
func (d *DB) PreviousBranchStepHistory(repoID, branch string, step types.StepName, excludeRunID string) (*BranchStepHistory, error) {
	var stepResultID, runID, runStatus string
	err := d.sql.QueryRow(
		`SELECT sr.id, r.id, r.status FROM step_results sr
		 JOIN runs r ON r.id = sr.run_id
		 WHERE r.repo_id = ? AND r.branch = ? AND r.id <> ? AND sr.step_name = ?
		   AND EXISTS (
		     SELECT 1 FROM step_rounds sro
		     WHERE sro.step_result_id = sr.id AND TRIM(COALESCE(sro.findings_json, '')) <> ''
		   )
		 ORDER BY r.created_at DESC, r.id DESC
		 LIMIT 1`,
		repoID, branch, excludeRunID, string(step),
	).Scan(&stepResultID, &runID, &runStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get previous branch step: %w", err)
	}

	rounds, err := d.GetRoundsByStep(stepResultID)
	if err != nil {
		return nil, err
	}
	if len(rounds) == 0 {
		return nil, nil
	}
	return &BranchStepHistory{RunID: runID, RunStatus: types.RunStatus(runStatus), Rounds: rounds}, nil
}
