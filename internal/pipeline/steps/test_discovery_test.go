package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestUnitOwnsPath(t *testing.T) {
	cases := []struct {
		name string
		unit config.TestUnit
		path string
		want bool
	}{
		{"root unit owns anything", config.TestUnit{Path: "."}, "anything/x.go", true},
		{"unit owns its own file", config.TestUnit{Path: "services/api"}, "services/api/main.go", true},
		{"unit owns its own path exactly", config.TestUnit{Path: "services/api"}, "services/api", true},
		{"unit does not own a sibling with a shared prefix", config.TestUnit{Path: "services/api"}, "services/apiary/main.go", false},
		{"unit does not own an unrelated path", config.TestUnit{Path: "services/api"}, "services/web/main.go", false},
		{"trailing slash still owns the unit's files", config.TestUnit{Path: "services/api/"}, "services/api/main.go", true},
		{"dot prefix still owns the unit's files", config.TestUnit{Path: "./services/api"}, "services/api/main.go", true},
		{"backslash path still owns the unit's files", config.TestUnit{Path: `services\api`}, "services/api/main.go", true},
		{"trailing slash does not widen to a sibling", config.TestUnit{Path: "services/api/"}, "services/apiary/main.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unitOwnsPath(tc.unit, tc.path); got != tc.want {
				t.Fatalf("unitOwnsPath(%+v, %q) = %v, want %v", tc.unit, tc.path, got, tc.want)
			}
		})
	}
}

func TestSelectUnitsForPaths_SelectsOnlyTouchedUnits(t *testing.T) {
	units := []config.TestUnit{
		{Name: "api", Path: "services/api"},
		{Name: "web", Path: "services/web"},
	}
	changed := []string{"services/api/main.go", "README.md"}

	got := selectUnitsForPaths(units, changed)
	if len(got) != 1 || got[0] != "api" {
		t.Fatalf("selected = %v, want [api]", got)
	}
}

func TestSelectUnitsForPaths_IsDeterministicAndDeduplicated(t *testing.T) {
	units := []config.TestUnit{
		{Name: "api", Path: "services/api"},
		{Name: "web", Path: "services/web"},
	}
	changed := []string{"services/api/main.go", "services/api/handler.go", "services/web/index.tsx"}

	got := selectUnitsForPaths(units, changed)
	if len(got) != 2 || got[0] != "api" || got[1] != "web" {
		t.Fatalf("selected = %v, want [api web]", got)
	}
}

func TestUnderSelectedUnits_FindsTheOmittedUnit(t *testing.T) {
	units := []config.TestUnit{
		{Name: "api", Path: "services/api"},
		{Name: "web", Path: "services/web"},
	}
	changed := []string{"services/api/main.go", "services/web/index.tsx"}
	selected := []string{"api"}

	got := underSelectedUnits(units, changed, selected)
	if len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("under-selected = %+v, want [web]", got)
	}
}

func TestUnderSelectedUnits_EmptyWhenSelectionCoversEveryChangedFile(t *testing.T) {
	units := []config.TestUnit{
		{Name: "api", Path: "services/api"},
		{Name: "web", Path: "services/web"},
	}
	changed := []string{"services/api/main.go", "services/web/index.tsx"}
	selected := []string{"api", "web"}

	got := underSelectedUnits(units, changed, selected)
	if len(got) != 0 {
		t.Fatalf("under-selected = %+v, want none", got)
	}
}

func TestChangedFilesFingerprint_OrderIndependentAndSetSensitive(t *testing.T) {
	a := []string{"b", "a"}
	b := []string{"a", "b"}
	if changedFilesFingerprint(a) != changedFilesFingerprint(b) {
		t.Fatal("fingerprints differ for the same set in different orders")
	}
	if a[0] != "b" || a[1] != "a" {
		t.Fatalf("caller's slice was reordered: %v", a)
	}

	c := []string{"a", "c"}
	if changedFilesFingerprint(b) == changedFilesFingerprint(c) {
		t.Fatal("fingerprints match for different sets")
	}
}

func discoveryTestContext(t *testing.T, ag agent.Agent) *pipeline.StepContext {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}
	return sctx
}

func TestDiscoverTestUnits_ConfigLayoutWins(t *testing.T) {
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			t.Fatal("agent must not be called when test.units is configured")
			return &agent.Result{}, nil
		},
	}
	sctx := discoveryTestContext(t, ag)
	sctx.Config.Test.Units = []config.TestUnit{{Name: "repository", Path: ".", Command: "go test ./..."}}
	sctx.Config.Commands.Test = "make test"

	d, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Source != "config" {
		t.Fatalf("Source = %q, want config", d.Source)
	}
}

func TestDiscoverTestUnits_CommandFallsBackToOneRepositoryUnit(t *testing.T) {
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			t.Fatal("agent must not be called when commands.test is configured")
			return &agent.Result{}, nil
		},
	}
	sctx := discoveryTestContext(t, ag)
	sctx.Config.Commands.Test = "make test"

	d, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Source != "command" {
		t.Fatalf("Source = %q, want command", d.Source)
	}
	if len(d.Units) != 1 || d.Units[0].Name != "repository" || d.Units[0].Path != "." || d.Units[0].Command != "make test" {
		t.Fatalf("Units = %+v, want one repository unit", d.Units)
	}
	if len(d.Selected) != 1 || d.Selected[0] != "repository" {
		t.Fatalf("Selected = %v, want [repository]", d.Selected)
	}
}

func discoveryAgent(t *testing.T, output string) *mockAgent {
	t.Helper()
	return &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(output)}, nil
		},
	}
}

func TestDiscoverTestUnits_AgentInfersLayoutWhenNothingIsConfigured(t *testing.T) {
	ag := discoveryAgent(t, `{
		"units": [
			{"name": "api", "path": "services/api", "command": "go test ./services/api/..."},
			{"name": "web", "path": "services/web", "command": "npm test"}
		],
		"selected": ["api"]
	}`)
	sctx := discoveryTestContext(t, ag)

	d, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"services/api/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Source != "agent" {
		t.Fatalf("Source = %q, want agent", d.Source)
	}
	if len(d.Units) != 2 {
		t.Fatalf("Units = %+v, want 2", d.Units)
	}
	if len(d.Selected) != 1 || d.Selected[0] != "api" {
		t.Fatalf("Selected = %v, want [api]", d.Selected)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
}

func TestDiscoverTestUnits_SecondCallReusesTheCachedAgentResult(t *testing.T) {
	ag := discoveryAgent(t, `{
		"units": [{"name": "repository", "path": ".", "command": "go test ./..."}],
		"selected": ["repository"]
	}`)
	sctx := discoveryTestContext(t, ag)
	changed := []string{"main.go"}

	if _, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, changed); err != nil {
		t.Fatal(err)
	}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	if _, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, changed); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "reusing discovered test units from earlier in this run") {
			found = true
		}
	}
	if !found {
		t.Fatalf("logs = %v, want the reuse log line", logs)
	}
}

func TestDiscoverTestUnits_ChangedFileSetMoveRecomputes(t *testing.T) {
	ag := discoveryAgent(t, `{
		"units": [{"name": "repository", "path": ".", "command": "go test ./..."}],
		"selected": ["repository"]
	}`)
	sctx := discoveryTestContext(t, ag)

	if _, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"main.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"other.go"}); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want 2", len(ag.calls))
	}
}

func TestDiscoverTestUnits_UnparseableAgentOutputIsAnError(t *testing.T) {
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`not json`)}, nil
		},
	}
	sctx := discoveryTestContext(t, ag)

	_, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"main.go"})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestDiscoverTestUnits_EmptyUnitListIsAnError(t *testing.T) {
	ag := discoveryAgent(t, `{"units": [], "selected": []}`)
	sctx := discoveryTestContext(t, ag)

	_, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"main.go"})
	if err == nil || err.Error() != "discovery returned no test units" {
		t.Fatalf("err = %v, want %q", err, "discovery returned no test units")
	}
}

func TestDiscoverTestUnits_UnitWithNoCommandIsAnError(t *testing.T) {
	ag := discoveryAgent(t, `{
		"units": [{"name": "api", "path": "services/api", "command": ""}],
		"selected": []
	}`)
	sctx := discoveryTestContext(t, ag)

	_, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"main.go"})
	want := `discovered unit "api" has no test command`
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// TestDiscoverTestUnits_DuplicateUnitNameIsAnError pins the reason a repeated
// name cannot be tolerated: findTestUnit resolves a name to the first unit
// carrying it, so the second unit's command would never run while
// under-selection saw the name as already selected, and the run would report
// green having never tested it.
func TestDiscoverTestUnits_DuplicateUnitNameIsAnError(t *testing.T) {
	ag := discoveryAgent(t, `{
		"units": [
			{"name": "api", "path": "services/api", "command": "go test ./services/api/..."},
			{"name": "api", "path": "services/api-v2", "command": "go test ./services/api-v2/..."}
		],
		"selected": ["api"]
	}`)
	sctx := discoveryTestContext(t, ag)

	_, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"services/api/main.go"})
	want := `discovery returned duplicate unit name "api"`
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestDiscoverTestUnits_SelectedNameIsTrimmedBeforeItIsResolved(t *testing.T) {
	ag := discoveryAgent(t, `{
		"units": [{"name": "api", "path": "services/api", "command": "go test ./services/api/..."}],
		"selected": ["  api  "]
	}`)
	sctx := discoveryTestContext(t, ag)

	d, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"services/api/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Selected) != 1 || d.Selected[0] != "api" {
		t.Fatalf("Selected = %q, want [api]", d.Selected)
	}
}

func TestDiscoverTestUnits_UncleanUnitPathStillSelectsItsChangedFiles(t *testing.T) {
	ag := discoveryAgent(t, `{
		"units": [{"name": "api", "path": "services/api/", "command": "go test ./services/api/..."}],
		"selected": []
	}`)
	sctx := discoveryTestContext(t, ag)

	d, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"services/api/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Units[0].Path != "services/api" {
		t.Fatalf("Path = %q, want the cleaned path", d.Units[0].Path)
	}
	missing := underSelectedUnits(d.Units, []string{"services/api/main.go"}, d.Selected)
	if len(missing) != 1 || missing[0].Name != "api" {
		t.Fatalf("under-selected = %+v, want [api]: an uncleaned path owns nothing and goes untested", missing)
	}
}

func TestFindTestUnit_ReportsAMissInsteadOfAnEmptyUnit(t *testing.T) {
	units := []config.TestUnit{{Name: "api", Path: "services/api", Command: "go test ./..."}}
	if _, ok := findTestUnit(units, "ghost"); ok {
		t.Fatal("findTestUnit vouched for a unit that is not in the layout")
	}
	unit, ok := findTestUnit(units, "api")
	if !ok || unit.Command != "go test ./..." {
		t.Fatalf("findTestUnit(api) = %+v, %v", unit, ok)
	}
}

func TestChangedFilesEnvValue_OmitsWhatItCannotRepresent(t *testing.T) {
	value, omitted := changedFilesEnvValue([]string{"a.go", "we\nird.go", "b.go"})
	if value != "a.go\nb.go" {
		t.Fatalf("value = %q, want the two representable paths", value)
	}
	if omitted != 1 {
		t.Fatalf("omitted = %d, want 1", omitted)
	}

	big := make([]string, 0, 20000)
	for i := 0; i < 20000; i++ {
		big = append(big, "some/reasonably/long/path/to/a/file/number.go")
	}
	value, omitted = changedFilesEnvValue(big)
	if value != "" {
		t.Fatalf("an oversized list should be dropped whole, got %d bytes", len(value))
	}
	if omitted != len(big) {
		t.Fatalf("omitted = %d, want %d", omitted, len(big))
	}
}

func TestDiscoverTestUnits_SelectedUnknownUnitIsAnError(t *testing.T) {
	ag := discoveryAgent(t, `{
		"units": [{"name": "api", "path": "services/api", "command": "go test"}],
		"selected": ["ghost"]
	}`)
	sctx := discoveryTestContext(t, ag)

	_, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"main.go"})
	want := `discovery selected unknown unit "ghost"`
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}
