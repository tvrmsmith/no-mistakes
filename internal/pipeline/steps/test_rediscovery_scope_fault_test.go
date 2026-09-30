package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

// scopeFaultMapping is how the rediscovery prompt names the under-selected
// root file and the unit the fixture layout assigns it to.
const scopeFaultMapping = "- packages.lock.json belongs to dotnet"

// scopeFaultRepo changes the web unit and adds a root lockfile, the observed
// shape: the web unit is the only one the agent selects, and a broad "."
// unit is the lockfile's most specific owner.
func scopeFaultRepo(t *testing.T) (dir, baseSHA, headSHA string) {
	t.Helper()
	dir, baseSHA = newUnitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "packages.lock.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add lockfile")
	return dir, baseSHA, changeUnitFile(t, dir, "services/web/main.go")
}

// scopeFaultLayout selects only web. dotnetPath "." gives the dead dotnet unit
// the root lockfile; a narrower path gives it nothing that changed.
func scopeFaultLayout(t *testing.T, dotnetPath string) string {
	t.Helper()
	return `{"units":[{"name":"web","path":"services/web","command":` + jsonString(t, coverageFor("true", "services/web/main.go")) + `},{"name":"dotnet","path":"` + dotnetPath + `","command":` + jsonString(t, deadRunnerCommand) + `}],"selected":["web"]}`
}

// scopeFaultAgent answers the first discovery with the under-selecting layout
// and each rediscovery with correct(prompt).
func scopeFaultAgent(t *testing.T, correct func(prompt string) string) *mockAgent {
	t.Helper()
	answered := false
	return &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !isDiscoveryCall(opts) {
			return &agent.Result{Output: json.RawMessage(neutralEvidenceFindingsJSON)}, nil
		}
		if !answered {
			answered = true
			return &agent.Result{Output: json.RawMessage(scopeFaultLayout(t, "."))}, nil
		}
		return &agent.Result{Output: json.RawMessage(correct(opts.Prompt))}, nil
	}}
}

// TestTestStep_RediscoveryAfterAnExpansionNamesTheUnderSelectedFiles drives
// the observed sequence: the selection passes, under-selection expands it to
// a unit whose command is dead, and the rediscovery must tell the agent which
// changed file its own layout gave that unit. The agent corrects the layout
// only when told, so without the mapping this parks on the second scope fault.
func TestTestStep_RediscoveryAfterAnExpansionNamesTheUnderSelectedFiles(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA, headSHA := scopeFaultRepo(t)
	ag := scopeFaultAgent(t, func(prompt string) string {
		if strings.Contains(prompt, scopeFaultMapping) {
			return scopeFaultLayout(t, "services/dotnet")
		}
		return scopeFaultLayout(t, ".")
	})
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected a verdict from the corrected layout, got: %s", outcome.Findings)
	}
	calls := discoveryCalls(ag)
	if len(calls) != 2 {
		t.Fatalf("discovery calls = %d, want 2", len(calls))
	}
	for _, want := range []string{deadRunnerCommand, `Unit "dotnet" ran only because the selection you reported left out changed files`, scopeFaultMapping} {
		if !strings.Contains(calls[1].Prompt, want) {
			t.Errorf("rediscovery prompt missing %q", want)
		}
	}
}

// TestTestStep_RediscoveryThatRepeatsTheUnderSelectionStillParks keeps the
// vacuous-green guard: told the mapping, an agent that repeats the selection
// under the same layout parks on the second scope fault.
func TestTestStep_RediscoveryThatRepeatsTheUnderSelectionStillParks(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA, headSHA := scopeFaultRepo(t)
	ag := scopeFaultAgent(t, func(string) string { return scopeFaultLayout(t, ".") })
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want a maintainer park", outcome)
	}
	if finding := onlyFinding(t, outcome.Findings); !strings.Contains(finding.Description, "under-selected twice in this run") {
		t.Fatalf("description = %q, want the second scope fault", finding.Description)
	}
	calls := discoveryCalls(ag)
	if len(calls) != 2 || !strings.Contains(calls[1].Prompt, scopeFaultMapping) {
		t.Fatalf("the rediscovery was not told the mapping before the park (calls = %d)", len(calls))
	}
}
