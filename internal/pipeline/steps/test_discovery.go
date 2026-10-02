package steps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
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

	if cached, ok := sctx.Shared.TestDiscovery(changedFilesFingerprint(changed)); ok {
		sctx.Log("reusing discovered test units from earlier in this run")
		return cached, nil
	}

	sctx.Log("discovering test units...")
	return discoverAndCacheViaAgent(sctx, baseSHA, changed)
}

// rediscoverTestUnits asks the discovery agent again after an agent-inferred
// command could not run any test, showing it the dead command and its output.
// The agent may report a replacement or keep the same command when the output
// shows the runner is sound. It does not cache the answer: the caller adopts a
// replacement only once it knows the answer selects something to run.
func rediscoverTestUnits(sctx *pipeline.StepContext, baseSHA string, changed []string, dead deadTestRunner) (pipeline.TestDiscovery, error) {
	sctx.Log(fmt.Sprintf("test unit %q could not run any test, rediscovering test units...", dead.unit.Name))
	failure := fmt.Sprintf(`

The command previously inferred for unit %q could not run any test: it exited %d and %s.
Command:
%s
Output:
%s

Report a command that can actually run this repository's tests on this machine. If the output shows the command itself is sound and the failure is in the code under test (for example a compile error in a changed file), report that same command unchanged.`,
		dead.unit.Name, dead.exitCode, dead.reason, dead.unit.Command, dead.output)
	return discoverValidatedViaAgent(sctx, baseSHA, changed, failure)
}

func discoverAndCacheViaAgent(sctx *pipeline.StepContext, baseSHA string, changed []string) (pipeline.TestDiscovery, error) {
	d, err := discoverValidatedViaAgent(sctx, baseSHA, changed, "")
	if err != nil {
		return pipeline.TestDiscovery{}, err
	}
	sctx.Shared.SetTestDiscovery(changedFilesFingerprint(changed), d)
	return d, nil
}

// maxDiscoveryAnswers bounds how many layouts one discovery asks the agent for:
// its answer, and one re-ask naming the command that answer was rejected for.
const maxDiscoveryAnswers = 2

// discoverValidatedViaAgent asks the agent for a layout. A layout whose
// command check fails is re-asked once, naming the rejected command, before it
// parks: nothing has run by then, so the re-ask costs one agent turn and no
// side effects. Every other rejection parks on the first answer.
func discoverValidatedViaAgent(sctx *pipeline.StepContext, baseSHA string, changed []string, failureSection string) (pipeline.TestDiscovery, error) {
	section := failureSection
	for answer := 1; ; answer++ {
		d, err := discoverTestUnitsViaAgent(sctx, baseSHA, changed, section)
		if err != nil {
			return pipeline.TestDiscovery{}, err
		}
		if err := validateDiscovery(&d); err != nil {
			return pipeline.TestDiscovery{}, parkOnDiscoveryResult(err)
		}
		err = checkInferredCommands(sctx.Ctx, d.Units)
		if err == nil {
			return d, nil
		}
		var rejection discoveryResultError
		if !errors.As(err, &rejection) || answer == maxDiscoveryAnswers {
			return pipeline.TestDiscovery{}, err
		}
		sctx.Log(fmt.Sprintf("test unit discovery answer rejected, asking again: %v", err))
		section = failureSection + fmt.Sprintf(`

Your previous answer was rejected before anything ran: %v
Report the complete layout and selection again with that corrected.`, err)
	}
}

// checkInferredCommands checks every unit's command, selected or not, because
// under-selection can run an unselected unit's command with no agent turn in
// between. Only an agent-written layout gets the check: a configured command
// that does not parse already parks as a dead runner with sh's own error, and
// the placeholder pattern could misread a legitimate redirect in trusted
// configuration.
func checkInferredCommands(ctx context.Context, units []config.TestUnit) error {
	for _, unit := range units {
		if err := checkInferredCommand(ctx, unit); err != nil {
			return err
		}
	}
	return nil
}

// templatePlaceholder matches a <...> placeholder that is not glued to a
// preceding identifier or to a heredoc's <<, so <svc>/<name>.csproj,
// <path/to/project.csproj> and <crate::module> match and a generic type in a
// test filter such as Cache<Key> does not. A space may separate words but never
// precedes the closing >, and a later word needs a non-digit, so an input
// redirect followed by an output redirect (<in.txt >out.txt, <in.txt 2>err.txt)
// is not a placeholder. sh -n alone misses a placeholder like <svc>, which
// parses as a redirect. Callers go through templatePlaceholderIn, which also
// skips quoted text and unspaced redirect pairs.
var templatePlaceholder = regexp.MustCompile(`(?:^|[^A-Za-z0-9_<])(<[A-Za-z][A-Za-z0-9._/:-]*(?: [A-Za-z0-9._/:-]*[A-Za-z._/:-][A-Za-z0-9._/:-]*)*>)`)

// templatePlaceholderIn returns the first template placeholder in command.
// Quoted text is literal data (grep -q "<testsuite>", pytest -k 'not <lambda>'),
// so a placeholder inside quotes is left to the dead-runner path, as every
// placeholder was before this check. sh tokenizes <in.txt>out.txt and
// <Service>Tests.csproj identically, as two redirects, so only the inner token
// tells them apart: a dotted file name glued to the following word is a
// redirect pair, and a bare name such as <svc> stays a placeholder.
func templatePlaceholderIn(command string) (string, bool) {
	unquoted := blankQuotedSpans(command)
	for _, m := range templatePlaceholder.FindAllStringSubmatchIndex(unquoted, -1) {
		token, end := unquoted[m[2]:m[3]], m[3]
		gluedToNextWord := end < len(unquoted) && !unicode.IsSpace(rune(unquoted[end]))
		if gluedToNextWord && !strings.Contains(token, " ") && strings.Contains(token, ".") {
			continue
		}
		return token, true
	}
	return "", false
}

// blankQuotedSpans replaces the text inside single and double quotes with
// spaces, keeping every byte offset. A backslash escapes the next byte outside
// single quotes, as in sh. An unterminated quote blanks to the end; sh -n
// rejects that command anyway.
func blankQuotedSpans(command string) string {
	out := []byte(command)
	var quote byte
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote == 0 && c == '\\':
			i++
		case c == quote:
			quote = 0
		case quote == '"' && c == '\\':
			out[i] = ' '
			if i+1 < len(out) {
				i++
				out[i] = ' '
			}
		case quote != 0:
			out[i] = ' '
		}
	}
	return string(out)
}

// checkInferredCommand rejects an agent-written command that describes a
// command instead of being one. It proves only that the command parses;
// whether it can run a test stays with the dead-runner path. A failure to run
// the parse check at all, including sh killed by a signal, is returned as a
// plain error rather than a discovery result, so it fails the run instead of
// being re-asked or parked.
func checkInferredCommand(ctx context.Context, unit config.TestUnit) error {
	if placeholder, found := templatePlaceholderIn(unit.Command); found {
		return parkOnDiscoveryResult(fmt.Errorf("discovered unit %q command %q still carries the template placeholder %s; report the literal command to run", unit.Name, unit.Command, placeholder))
	}
	// Unit commands run through cmd.exe on Windows, which has no parse-only mode.
	if runtime.GOOS == "windows" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-n", "-c", unit.Command)
	shellenv.ConfigureShellCommand(cmd)
	out, runErr := shellenv.CombinedOutputShellCommand(cmd)
	// A cancelled run kills sh, which is not a verdict on the command.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("check discovered unit %q command: %w", unit.Name, ctxErr)
	}
	if runErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() < 0 {
		return fmt.Errorf("check discovered unit %q command: %w", unit.Name, runErr)
	}
	return parkOnDiscoveryResult(fmt.Errorf("discovered unit %q command %q is not a valid shell command (sh -n: %s)", unit.Name, unit.Command, strings.TrimSpace(string(out))))
}

// discoveryRunbookSection tells the discovery agent how this repository runs
// its tests, so an inferred command uses the runner the maintainer pinned
// instead of improvising one each run. The prompt's own rules stay binding:
// a runbook written for live validation must not widen a unit command past
// the changed files or drop the coverage artifacts the vacuous-green guard reads.
//
// A runbook often writes a command as a template (`dotnet test <dir>/<name>.csproj`),
// and an agent told only to follow it has copied the placeholders into a unit
// command verbatim, so the section says to fill them from the tree.
func discoveryRunbookSection(sctx *pipeline.StepContext) string {
	runbook := trustedTestRunbook(sctx)
	if runbook == "" {
		return ""
	}
	return "\nRepository test runbook (trusted, from the default branch). Follow it for how this repository runs its tests; where it conflicts with them, the rules below still bind:\n" + runbook + "\n" +
		"Where the runbook writes a command as a template with placeholders such as <dir>, <name>, or <path>, fill every placeholder with the concrete path or name from this repository before reporting the command. Never copy a placeholder token into a unit command; a unit command must run as written.\n"
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

func discoverTestUnitsViaAgent(sctx *pipeline.StepContext, baseSHA string, changed []string, failureSection string) (pipeline.TestDiscovery, error) {
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
%s
Task:
- Examine the repository and identify every independently testable unit.
- For each unit, report its name, its path, and the command that tests it.
- Select the units the changed files above touch.
- Do not run any test now. Only report the layout and the selection.

Rules for the command you report:
- Report a runnable command for every unit, including the units you do not select: when a changed file turns out to belong to an unselected unit, its command runs through the shell as written. Each command is the literal shell text to run, with every path and name filled in.
- Each command must scope itself to the changed files under its unit. Local Test is targeted validation of this change; remote CI owns broad regression.
- A command must NOT be the complete repository test suite, even when the unit is the repository itself. Name the specific test targets, directories, packages, or selectors the changed files reach.
- The command runs with NO_MISTAKES_BASE_SHA set to the base commit and NO_MISTAKES_CHANGED_FILES set to the newline-separated changed paths, with NO_MISTAKES_CHANGED_FILE_COUNT carrying the true total. Read those variables in the command when that is how a unit's runner takes a target list.
- A command that walks the whole repository is wrong even if it passes, because it spends the run's budget on work remote CI repeats.
- The command also runs with NO_MISTAKES_COVERAGE_DIR set to a directory OUTSIDE the worktree. It must write a coverage profile (LCOV or Cobertura XML) and a test report (JUnit XML or Visual Studio TRX) into that directory, and must never write coverage output into the worktree. A command that reports neither cannot prove it exercised anything, so the run will park instead of reporting a pass.%s`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			changedList,
			discoveryRunbookSection(sctx),
			failureSection,
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

// deadTestRunner is a unit command that exited non-zero without proving it ran
// a single test: a runner that failed to build, a missing binary, or a shell
// syntax error, rather than a failing test.
type deadTestRunner struct {
	unit     config.TestUnit
	exitCode int
	// output is the bounded projection of the command's output the step
	// already logged, which the rediscovery prompt quotes.
	output string
	// reason says how the coverage directory failed to prove a test ran.
	reason string
}

func (d deadTestRunner) description(multiUnit bool) string {
	description := fmt.Sprintf("test command could not run any test: it exited %d and %s", d.exitCode, d.reason)
	if multiUnit {
		return fmt.Sprintf("unit %s: %s", d.unit.Name, description)
	}
	return description
}

// selectsOnlyCommand reports whether the discovery selects at least one unit
// and every selected unit runs command, so running it would run exactly that
// command again.
func selectsOnlyCommand(d pipeline.TestDiscovery, command string) bool {
	if len(d.Selected) == 0 {
		return false
	}
	want := strings.TrimSpace(command)
	for _, name := range d.Selected {
		unit, ok := findTestUnit(d.Units, name)
		if !ok || strings.TrimSpace(unit.Command) != want {
			return false
		}
	}
	return true
}
