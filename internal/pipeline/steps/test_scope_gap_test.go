package steps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// apiAndWebLayout renders a discovery answer with an api and a web unit that
// selects only api, so a change under services/web is under-selected.
func apiAndWebLayout(t *testing.T, apiCommand, webCommand string, selected ...string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"units": []config.TestUnit{
			{Name: "api", Path: "services/api", Command: apiCommand},
			{Name: "web", Path: "services/web", Command: webCommand},
		},
		"selected": selected,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// scopeFaultParkFixture replays the run issue #63 was filed from: discovery
// selects api and leaves out web, whose command is a placeholder template that
// cannot run. The first scope fault expands into web, web is a dead runner,
// rediscovery returns the same under-selecting layout, and the second scope
// fault parks. It returns the parked outcome and a context the next attempt
// can clone.
type scopeFaultParkFixture struct {
	dir, baseSHA string
	apiMarker    string
	webMarker    string
	shared       *pipeline.RunShared
	parked       *pipeline.StepOutcome
}

func parkOnSecondScopeFault(t *testing.T) scopeFaultParkFixture {
	t.Helper()
	dir, baseSHA := newUnitRepo(t)
	markers := t.TempDir()
	f := scopeFaultParkFixture{
		dir:       dir,
		baseSHA:   baseSHA,
		apiMarker: filepath.Join(markers, "api.done"),
		webMarker: filepath.Join(markers, "web.done"),
	}
	changeUnitFile(t, dir, "services/web/main.go")
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	layout := apiAndWebLayout(t, coverageFor(markerCommand(f.apiMarker), "services/api/main.go"), deadRunnerCommand, "api")
	ag := sequencedDiscoveryAgent(layout)
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("fixture did not park on the second scope fault: %s", outcome.Findings)
	}
	if !parkedOnScopeFault(t, outcome.Findings) {
		t.Fatalf("fixture parked without the scope-fault finding: %s", outcome.Findings)
	}
	f.shared = sctx.Shared
	f.parked = outcome
	return f
}

func parkedOnScopeFault(t *testing.T, raw string) bool {
	t.Helper()
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return len(types.FilterFindings(findings, []string{testScopeFaultFindingID}).Items) == 1
}

// nextAttempt commits a fix that edits only a file already in the changed set,
// the review fix round from the observed run, and returns a Test context for
// the re-test that shares the run's state with the parked attempt.
func (f scopeFaultParkFixture) nextAttempt(t *testing.T, ag *mockAgent) *pipeline.StepContext {
	t.Helper()
	headSHA := changeUnitFile(t, f.dir, "services/api/main.go")
	for _, marker := range []string{f.apiMarker, f.webMarker} {
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	sctx := unitTestContext(t, ag, f.dir, f.baseSHA, headSHA, nil)
	sctx.Shared = f.shared
	return sctx
}

func TestTestStep_ApprovedScopeFaultParkDoesNotParkTheNextAttemptOnTheSameChangedSet(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	f := parkOnSecondScopeFault(t)

	// The executor calls this when the operator approves a Test gate.
	f.shared.AcceptParkedTestScopeGap()

	ag := sequencedDiscoveryAgent(`{"units":[],"selected":[]}`)
	sctx := f.nextAttempt(t, ag)
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("re-test parked again on a gap the operator already approved: %s", outcome.Findings)
	}
	if calls := discoveryCalls(ag); len(calls) != 0 {
		t.Fatalf("discovery calls = %d, want the cached layout reused", len(calls))
	}
	if !fileExists(f.apiMarker) || fileExists(f.webMarker) {
		t.Fatalf("api ran = %v, web ran = %v; want api only, the approved gap left out", fileExists(f.apiMarker), fileExists(f.webMarker))
	}
	if !strings.Contains(outcome.Findings, "as an operator accepted at an earlier scope-fault park in this run: web") {
		t.Fatalf("the accepted gap must stay visible on the outcome, got: %s", outcome.Findings)
	}
}

func TestTestStep_UnapprovedScopeFaultParkStillParksTheNextAttempt(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	f := parkOnSecondScopeFault(t)

	sctx := f.nextAttempt(t, sequencedDiscoveryAgent(`{"units":[],"selected":[]}`))
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !parkedOnScopeFault(t, outcome.Findings) {
		t.Fatalf("a gap nobody approved must park again, got: %+v", outcome)
	}
}

// A gap accepted for one changed-file set says nothing about another, so a
// fix that adds a file to the change is a new question.
func TestTestStep_ScopeGapAcceptanceDoesNotCarryToAMovedChangedSet(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	f := parkOnSecondScopeFault(t)
	f.shared.AcceptParkedTestScopeGap()

	if err := os.WriteFile(filepath.Join(f.dir, "services", "web", "extra.go"), []byte("package web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	layout := apiAndWebLayout(t, coverageFor(markerCommand(f.apiMarker), "services/api/main.go"), deadRunnerCommand, "api")
	sctx := f.nextAttempt(t, sequencedDiscoveryAgent(layout))
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !parkedOnScopeFault(t, outcome.Findings) {
		t.Fatalf("a moved changed-file set must not inherit the approval, got: %+v", outcome)
	}
}

// Selecting the scope-fault finding for a fix is the operator's way to force a
// fresh discovery without aborting the run: the cached layout and the fault
// counts are forgotten, and no repair turn edits code over a layout problem.
func TestTestStep_FixSelectingTheScopeFaultRediscoversInsteadOfRepairing(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	f := parkOnSecondScopeFault(t)

	fixed := apiAndWebLayout(t,
		coverageFor(markerCommand(f.apiMarker), "services/api/main.go"),
		coverageFor(markerCommand(f.webMarker), "services/web/main.go"),
		"api", "web")
	ag := sequencedDiscoveryAgent(fixed)
	sctx := f.nextAttempt(t, ag)
	sctx.Fixing = true
	parked, err := types.ParseFindingsJSON(f.parked.Findings)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := types.MarshalFindingsJSON(types.FilterFindings(parked, []string{testScopeFaultFindingID}))
	if err != nil {
		t.Fatal(err)
	}
	sctx.PreviousFindings = selected

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("rediscovered layout should pass, got: %s", outcome.Findings)
	}
	if n := testFixRounds(ag); n != 0 {
		t.Fatalf("repair turns = %d, want 0 for a layout problem", n)
	}
	if calls := discoveryCalls(ag); len(calls) != 1 {
		t.Fatalf("discovery calls = %d, want one fresh pass", len(calls))
	}
	if !fileExists(f.apiMarker) || !fileExists(f.webMarker) {
		t.Fatalf("api ran = %v, web ran = %v; want both units of the rediscovered layout", fileExists(f.apiMarker), fileExists(f.webMarker))
	}
	if count := sctx.Shared.NoteTestScopeFault(); count != 1 {
		t.Fatalf("scope faults after rediscovery = %d, want a fresh count", count)
	}
}

// An evidence agent's own finding must never claim the scope-fault ID, or
// selecting it for a fix would silently discard the run's layout.
func TestParseTestAnalyzerOutput_StripsTheScopeFaultID(t *testing.T) {
	raw := strings.Replace(neutralEvidenceFindingsJSON, `"findings":[]`, `"findings":[{"id":"`+testScopeFaultFindingID+`","severity":"warning","action":"auto-fix","description":"agent note"}]`, 1)
	findings, err := parseTestAnalyzerOutput(&agent.Result{Output: json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].ID != "" {
		t.Fatalf("findings = %+v, want the step-owned ID cleared", findings.Items)
	}
}

// A runbook describing its command as a template is where the placeholder
// command in issue #63 came from, so the discovery prompt tells the agent to
// fill the template from the tree.
func TestDiscoverTestUnits_PromptTellsTheAgentToFillRunbookTemplates(t *testing.T) {
	const runbook = "For each changed .NET service run: dotnet test <dir>/<name>.csproj --settings <path>/coverlet.runsettings"
	ag := discoveryAgent(t, `{
		"units": [{"name": "repository", "path": ".", "command": "go test ./internal/x"}],
		"selected": ["repository"]
	}`)
	sctx := discoveryTestContext(t, ag)
	trusted := &config.RepoConfig{Test: config.TestRaw{Instructions: runbook}}
	sctx.Config = config.Merge(&config.GlobalConfig{}, config.EffectiveRepoConfig(nil, trusted, true))

	if _, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"internal/x/x.go"}); err != nil {
		t.Fatal(err)
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{runbook, "fill every placeholder with the concrete path or name from this repository", "Never copy a placeholder token into a unit command"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("discovery prompt missing %q:\n%s", want, prompt)
		}
	}
}
