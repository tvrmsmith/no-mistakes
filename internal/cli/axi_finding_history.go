package cli

import (
	"encoding/json"
	"fmt"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// findingHistoryRow is one finding one gate round raised, with that round's
// decision about it. Description is never truncated: an unattended caller
// quotes ask-user findings verbatim, and the gate's inline limit would make it
// quote a cut.
type findingHistoryRow struct {
	Step        string `toon:"step"`
	Round       int    `toon:"round"`
	ID          string `toon:"id"`
	Severity    string `toon:"severity"`
	Action      string `toon:"action"`
	Source      string `toon:"source"`
	Selected    bool   `toon:"selected"`
	File        string `toon:"file"`
	Line        int    `toon:"line"`
	Description string `toon:"description"`
}

// findingHistory is every finding every round of a run raised, in step then
// round order. A read that did not complete is carried as err rather than as
// a shorter list, so a caller can never read a failed read as "nothing found".
type findingHistory struct {
	rows []findingHistoryRow
	err  error
}

// loadFindingHistory reads the round records of each step. The steps come
// from the caller's view so the IPC and database paths share one reader.
func loadFindingHistory(d *db.DB, steps []stepView) findingHistory {
	var rows []findingHistoryRow
	for _, s := range steps {
		if s.ID == "" {
			return findingHistory{err: fmt.Errorf("step %s has no step id", s.Name)}
		}
		rounds, err := d.GetRoundsByStep(s.ID)
		if err != nil {
			return findingHistory{err: fmt.Errorf("read %s rounds: %w", s.Name, err)}
		}
		stepRows, err := roundFindingRows(s.Name, rounds)
		if err != nil {
			return findingHistory{err: err}
		}
		rows = append(rows, stepRows...)
	}
	return findingHistory{rows: rows}
}

// roundFindingRows flattens one step's rounds. A round's selection names the
// findings the NEXT round's fix was dispatched for, whether auto-fix or a
// human (--yes included) chose them. Findings a human added at that gate are
// recorded only in the dispatched list, so they are appended from there as
// selected.
func roundFindingRows(step string, rounds []*db.StepRound) ([]findingHistoryRow, error) {
	var rows []findingHistoryRow
	for _, r := range rounds {
		selected, err := selectedFindingIDs(r.SelectedFindingIDs)
		if err != nil {
			return nil, fmt.Errorf("parse %s round %d selection: %w", step, r.Round, err)
		}
		raised, err := roundFindings(r.FindingsJSON)
		if err != nil {
			return nil, fmt.Errorf("parse %s round %d findings: %w", step, r.Round, err)
		}
		seen := make(map[string]bool, len(raised))
		for _, f := range raised {
			seen[f.ID] = true
			rows = append(rows, newFindingHistoryRow(step, r.Round, f, selected[f.ID]))
		}
		dispatched, err := roundFindings(r.UserFindingsJSON)
		if err != nil {
			return nil, fmt.Errorf("parse %s round %d dispatched findings: %w", step, r.Round, err)
		}
		for _, f := range dispatched {
			if f.Source == types.FindingSourceUser && !seen[f.ID] {
				rows = append(rows, newFindingHistoryRow(step, r.Round, f, true))
			}
		}
	}
	return rows, nil
}

func newFindingHistoryRow(step string, round int, f types.Finding, selected bool) findingHistoryRow {
	source := f.Source
	if source == "" {
		source = types.FindingSourceAgent
	}
	return findingHistoryRow{
		Step:        step,
		Round:       round,
		ID:          f.ID,
		Severity:    f.Severity,
		Action:      f.ActionOrDefault(),
		Source:      source,
		Selected:    selected,
		File:        f.File,
		Line:        f.Line,
		Description: f.Description,
	}
}

func roundFindings(raw *string) ([]types.Finding, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	parsed, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return nil, err
	}
	return parsed.Items, nil
}

func selectedFindingIDs(raw *string) (map[string]bool, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(*raw), &ids); err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set, nil
}

// field renders the history. The key is always present on a final result,
// and an empty history renders as `finding_history[0]:`, so a run that found
// nothing reads differently from a binary that predates the field. A failed
// read renders `finding_history_error` in its place.
func (h findingHistory) field() toon.Field {
	if h.err != nil {
		return toon.Field{Key: "finding_history_error", Value: h.err.Error()}
	}
	rows := h.rows
	if rows == nil {
		rows = []findingHistoryRow{}
	}
	return toon.Field{Key: "finding_history", Value: rows}
}
