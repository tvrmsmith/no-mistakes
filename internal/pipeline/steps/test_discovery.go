package steps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// testDiscoverySchema is the JSON schema for the discovery agent pass,
// modelled on testFindingsSchema: it asks only for the repository's unit
// layout and the units the change touches, never for a test verdict.
var testDiscoverySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"units": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"name": {"type": "string"},
					"path": {"type": "string"},
					"command": {"type": "string"}
				},
				"required": ["name", "path", "command"]
			}
		},
		"selected": {
			"type": "array",
			"items": {"type": "string"}
		}
	},
	"required": ["units", "selected"]
}`)

// changedFilesFingerprint returns a stable key for a changed-file set, so a
// cached discovery is reused only for the set it was derived from.
//
// The separator is NUL, the one byte a git path cannot contain, and the same
// byte changedPathList splits the diff payload on. A newline separator made
// {"a\nb"} and {"a", "b"} hash alike, so a run could reuse a layout and a
// selection derived from a different diff.
func changedFilesFingerprint(changed []string) string {
	sorted := append([]string{}, changed...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return hex.EncodeToString(sum[:])
}

// unitOwnsPath reports whether a repository-relative changed file belongs to a
// unit. A unit at "." owns every path.
func unitOwnsPath(unit config.TestUnit, path string) bool {
	unitPath := normalizeUnitPath(unit.Path)
	normalizedPath := toSlashPath(path)
	if unitPath == "." {
		return true
	}
	return normalizedPath == unitPath || strings.HasPrefix(normalizedPath, unitPath+"/")
}

// normalizeUnitPath is config.NormalizeUnitPath, the single owner of a unit
// path's canonical form. An agent-inferred path goes through the same function
// a configured one does, so validation, the resolved config, and the matching
// below all judge one string.
func normalizeUnitPath(path string) string {
	return config.NormalizeUnitPath(path)
}

// toSlashPath normalises path separators without importing path/filepath
// purely for this: changed-file paths and configured unit paths are both
// already repository-relative slash paths in practice, but a Windows daemon
// host can hand either function backslashes.
func toSlashPath(path string) string {
	return strings.ReplaceAll(path, "\\", "/")
}

// mostSpecificOwners returns the units a changed path belongs to at the
// narrowest specificity available: the owners with the longest unit path, so a
// nested layout assigns the path to the narrowest units that claim it. A "."
// unit is the shortest owner of every path, which is what a layout means by
// it, a catch-all for code no narrower unit owns. Two units may share one path
// (a suite and its contract tests, say), so every owner at that length is
// returned rather than the first one found.
func mostSpecificOwners(units []config.TestUnit, path string) []config.TestUnit {
	var owners []config.TestUnit
	bestLen := -1
	for _, unit := range units {
		if !unitOwnsPath(unit, path) {
			continue
		}
		unitPath := normalizeUnitPath(unit.Path)
		length := len(unitPath)
		if unitPath == "." {
			length = 0
		}
		switch {
		case length > bestLen:
			owners = []config.TestUnit{unit}
			bestLen = length
		case length == bestLen:
			owners = append(owners, unit)
		}
	}
	return owners
}

// selectUnitsForPaths returns the names of the units a changed-file set
// touches, in the order the units are declared. Each path contributes only its
// most specific owner, so a change under api/ selects api and leaves a "."
// unit for the root-level files no narrower unit claims.
func selectUnitsForPaths(units []config.TestUnit, changed []string) []string {
	owners := map[string]bool{}
	for _, path := range changed {
		for _, owner := range mostSpecificOwners(units, path) {
			owners[owner.Name] = true
		}
	}
	return unitNamesInDeclarationOrder(units, owners)
}

// underSelectedUnits returns the units a changed file belongs to that the
// selection left out. Under-selection is a scope fault, not a coverage
// finding: discovery claimed a scope the changed files contradict.
//
// A path an already-selected unit owns raises nothing, whatever its most
// specific owner is, so a selection of the narrow unit alone stands and a
// broader unit is added only for the paths nothing selected covers. The
// predicate is the one selectUnitsForPaths derives from, so the config and
// command sources still cannot disagree with themselves.
func underSelectedUnits(units []config.TestUnit, changed, selected []string) []config.TestUnit {
	selectedSet := map[string]bool{}
	for _, name := range selected {
		selectedSet[name] = true
	}
	missingSet := map[string]bool{}
	for _, path := range changed {
		if anySelectedUnitOwns(units, selectedSet, path) {
			continue
		}
		for _, owner := range mostSpecificOwners(units, path) {
			missingSet[owner.Name] = true
		}
	}
	var missing []config.TestUnit
	seen := map[string]bool{}
	for _, unit := range units {
		if seen[unit.Name] || !missingSet[unit.Name] {
			continue
		}
		missing = append(missing, unit)
		seen[unit.Name] = true
	}
	return missing
}

// anySelectedUnitOwns reports whether a unit already in the selection covers
// the path.
func anySelectedUnitOwns(units []config.TestUnit, selected map[string]bool, path string) bool {
	for _, unit := range units {
		if selected[unit.Name] && unitOwnsPath(unit, path) {
			return true
		}
	}
	return false
}

// unitNamesInDeclarationOrder renders a name set in the order the layout
// declares the units.
func unitNamesInDeclarationOrder(units []config.TestUnit, names map[string]bool) []string {
	var ordered []string
	seen := map[string]bool{}
	for _, unit := range units {
		if seen[unit.Name] || !names[unit.Name] {
			continue
		}
		ordered = append(ordered, unit.Name)
		seen[unit.Name] = true
	}
	return ordered
}

// discoveryResultError marks a discovery failure the Test step parks on: the
// configuration or the agent answered, and the answer is unusable. An
// invocation failure is deliberately not one of these. A hanging or erroring
// agent fails the run, the contract every other agent-invoking step already
// holds (see TestTestStep_HangingEvidenceAgentFailsRunAfterTimeout), because
// parking would hold a run open at a gate on an agent that never spoke.
type discoveryResultError struct{ err error }

func (e discoveryResultError) Error() string { return e.err.Error() }

func (e discoveryResultError) Unwrap() error { return e.err }

// parkOnDiscoveryResult wraps a failure the step should park on.
func parkOnDiscoveryResult(err error) error {
	if err == nil {
		return nil
	}
	return discoveryResultError{err: err}
}

// validateDiscovery rejects a layout the execution half cannot act on, and
// normalises the layout it accepts in place (names and commands are trimmed,
// unit paths take their canonical form, and the selection is deduplicated) so
// the caller's discovery is the one execution then runs. A discovery failure parks rather than passing, so every
// rejection here has to name what is wrong precisely enough for a maintainer
// to fix it.
func validateDiscovery(d *pipeline.TestDiscovery) error {
	if len(d.Units) == 0 {
		return errors.New("discovery returned no test units")
	}
	known := map[string]bool{}
	for i := range d.Units {
		d.Units[i].Name = strings.TrimSpace(d.Units[i].Name)
		d.Units[i].Path = normalizeUnitPath(d.Units[i].Path)
		d.Units[i].Command = strings.TrimSpace(d.Units[i].Command)
		if d.Units[i].Name == "" {
			return errors.New("discovery returned a unit with no name")
		}
		if d.Units[i].Command == "" {
			return fmt.Errorf("discovered unit %q has no test command", d.Units[i].Name)
		}
		// An absolute or repository-escaping path owns no repository-relative
		// changed file, so the unit is never selected and under-selection never
		// names it either: the run would report green having never run its
		// command. config.ValidateUnitPath is the rule a configured layout is
		// held to, so both halves judge one rule.
		if err := config.ValidateUnitPath(d.Units[i].Path); err != nil {
			return fmt.Errorf("discovered unit %q %w", d.Units[i].Name, err)
		}
		// Name is how execution addresses a unit, so a repeated name hides
		// every unit after the first: the selection resolves to the first,
		// under-selection sees the name as already selected, and the run
		// reports green having never tested the others. config.validateTestRaw
		// rejects the same collision in a configured layout.
		if known[d.Units[i].Name] {
			return fmt.Errorf("discovery returned duplicate unit name %q", d.Units[i].Name)
		}
		known[d.Units[i].Name] = true
	}
	// The selection is deduplicated here rather than tolerated downstream,
	// because execution logs one line per selected name before it runs
	// anything: a name listed twice would claim an audited scope of two units
	// while only one command ever runs.
	deduped := make([]string, 0, len(d.Selected))
	chosen := map[string]bool{}
	for _, name := range d.Selected {
		name = strings.TrimSpace(name)
		if !known[name] {
			return fmt.Errorf("discovery selected unknown unit %q", name)
		}
		if chosen[name] {
			continue
		}
		chosen[name] = true
		deduped = append(deduped, name)
	}
	d.Selected = deduped
	return nil
}

// discoverTestUnits derives the repository's unit layout and the units this
// change touches, reusing the run's cached result when the changed-file set
// has not moved.
//
// Precedence: an explicit test.units layout always wins (it is trusted
// maintainer configuration, and free to compute), then a configured
// commands.test collapses the whole repository into one unit, and only when
// neither is configured does the step pay an agent pass to infer the layout.
func discoverTestUnits(sctx *pipeline.StepContext, baseSHA string, changed []string) (pipeline.TestDiscovery, error) {
	if len(sctx.Config.Test.Units) > 0 {
		units := append([]config.TestUnit{}, sctx.Config.Test.Units...)
		d := pipeline.TestDiscovery{
			Units:    units,
			Selected: selectUnitsForPaths(units, changed),
			Source:   "config",
		}
		if err := validateDiscovery(&d); err != nil {
			return pipeline.TestDiscovery{}, parkOnDiscoveryResult(err)
		}
		return d, nil
	}

	if cmd := strings.TrimSpace(sctx.Config.Commands.Test); cmd != "" {
		d := pipeline.TestDiscovery{
			Units:    []config.TestUnit{{Name: "repository", Path: ".", Command: cmd}},
			Selected: []string{"repository"},
			Source:   "command",
		}
		if err := validateDiscovery(&d); err != nil {
			return pipeline.TestDiscovery{}, parkOnDiscoveryResult(err)
		}
		return d, nil
	}

	fingerprint := changedFilesFingerprint(changed)
	if cached, ok := sctx.Shared.TestDiscovery(fingerprint); ok {
		sctx.Log("reusing discovered test units from earlier in this run")
		return cached, nil
	}

	sctx.Log("discovering test units...")
	d, err := discoverTestUnitsViaAgent(sctx, baseSHA, changed)
	if err != nil {
		return pipeline.TestDiscovery{}, err
	}
	if err := validateDiscovery(&d); err != nil {
		return pipeline.TestDiscovery{}, parkOnDiscoveryResult(err)
	}
	sctx.Shared.SetTestDiscovery(fingerprint, d)
	return d, nil
}

// discoveryAgentUnit and discoveryAgentOutput mirror the discovery agent's
// structured output shape (testDiscoverySchema) for decoding.
type discoveryAgentUnit struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Command string `json:"command"`
}

type discoveryAgentOutput struct {
	Units    []discoveryAgentUnit `json:"units"`
	Selected []string             `json:"selected"`
}

func discoverTestUnitsViaAgent(sctx *pipeline.StepContext, baseSHA string, changed []string) (pipeline.TestDiscovery, error) {
	discoveryCtx, cancel, timeout := testAgentContext(sctx)
	defer cancel()

	changedList := "(no changed files reported)"
	if len(changed) > 0 {
		changedList = "- " + strings.Join(changed, "\n- ")
	}

	result, err := sctx.RunAgentContext(discoveryCtx, agent.RunOpts{
		Prompt: fmt.Sprintf(
			`Derive this repository's independently testable units and the command that tests each one.

A unit is a service, a directory of code with its own test command, or the repository itself. "path" is the repository-relative directory the unit owns; use "." for the whole repository. The command must cover the unit, integration, and service-isolation test tiers for that unit, and must NOT run end-to-end tests, which remote CI owns.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Changed files:
%s

Task:
- Examine the repository and identify every independently testable unit.
- For each unit, report its name, its path, and the command that tests it.
- Select the units the changed files above touch.
- Do not run any test now. Only report the layout and the selection.

Rules for the command you report:
- Each command must scope itself to the changed files under its unit. Local Test is targeted validation of this change; remote CI owns broad regression.
- A command must NOT be the complete repository test suite, even when the unit is the repository itself. Name the specific test targets, directories, packages, or selectors the changed files reach.
- The command runs with NO_MISTAKES_BASE_SHA set to the base commit and NO_MISTAKES_CHANGED_FILES set to the newline-separated changed paths, with NO_MISTAKES_CHANGED_FILE_COUNT carrying the true total. Read those variables in the command when that is how a unit's runner takes a target list.
- A command that walks the whole repository is wrong even if it passes, because it spends the run's budget on work remote CI repeats.`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			changedList,
		),
		CWD:        sctx.WorkDir,
		JSONSchema: testDiscoverySchema,
		OnChunk:    sctx.LogChunk,
	})
	if runErr := testAgentError(discoveryCtx, timeout, "agent discover test units", err); runErr != nil {
		return pipeline.TestDiscovery{}, runErr
	}

	var out discoveryAgentOutput
	if result.Output == nil {
		return pipeline.TestDiscovery{}, parkOnDiscoveryResult(errors.New("discovery returned no test units"))
	}
	if err := json.Unmarshal(result.Output, &out); err != nil {
		return pipeline.TestDiscovery{}, parkOnDiscoveryResult(fmt.Errorf("parse discovery output: %w", err))
	}

	units := make([]config.TestUnit, 0, len(out.Units))
	for _, u := range out.Units {
		units = append(units, config.TestUnit{Name: u.Name, Path: u.Path, Command: u.Command})
	}
	return pipeline.TestDiscovery{
		Units:    units,
		Selected: out.Selected,
		Source:   "agent",
	}, nil
}
