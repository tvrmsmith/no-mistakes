//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These journeys drive the real CLI, daemon, and gate with a canned agent
// through a Test step whose agent-inferred layout gives an unselected unit a
// command that describes a command instead of being one (issue #59).

const reaskPromptMarker = "Your previous answer was rejected before anything ran"

// issuePlaceholderProse is the prose an agent wrote as an unselected unit's
// command in the run that exposed the unchecked command.
const issuePlaceholderProse = `<cli> verify (per changed .NET service from <manifest>): dotnet test <svc>/<name>.csproj --filter "..." --settings <nearest coverlet.runsettings> ...`

// placeholderLayoutScenario writes a scenario whose first discovery selects
// the api unit and gives the unselected web unit firstWebCommand, and whose
// re-ask (when reached) gives web secondWebCommand.
func placeholderLayoutScenario(t *testing.T, firstWebCommand, secondWebCommand string) string {
	t.Helper()
	quote := func(s string) string {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// The selected command carries a quoted generic type, which the placeholder
	// check must leave alone.
	rootCommand := quote(InferredUnitCommand + ` --filter "FullyQualifiedName~Cache<Key>"`)
	layout := func(match, webCommand string) string {
		return `  - match: ` + quote(match) + `
    text: "layout"
    structured:
      units:
        - name: api
          path: "api"
          command: ` + rootCommand + `
        - name: web
          path: "web"
          command: ` + quote(webCommand) + `
      selected: ["api"]
`
	}
	data := "actions:\n" + layout(reaskPromptMarker, secondWebCommand) + layout(discoveryPromptMarker, firstWebCommand)
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return appendCleanDefault(t, path)
}

// runUnderSelectedJourney pushes a change touching a file the selected api unit
// owns and a file only the unselected web unit owns, so under-selection would run web's
// command with no agent turn in between.
func runUnderSelectedJourney(t *testing.T, h *Harness, branch string) {
	t.Helper()
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "web/page.txt", "synthetic page\n", "add page")
	h.CommitChange(branch, "api/feature.txt", "synthetic feature\n", "add feature")
	out, err := h.Run("axi", "run", "--intent", "Validate the synthetic feature", "--skip", "pr,ci")
	t.Logf("=== axi run ===\n%s", out)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
}

func testLogLines(t *testing.T, h *Harness, runID string, needles ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.NMHome, "logs", runID, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		for _, n := range needles {
			if strings.Contains(line, n) {
				kept = append(kept, line)
				break
			}
		}
	}
	return strings.Join(kept, "\n")
}

// #59: a placeholder command on an unselected unit is rejected before any
// unit runs, re-asked once naming the command, and the corrected layout's
// under-selection expansion runs the literal replacement.
func TestDiscoveryCommandJourney_UnselectedPlaceholderIsReasked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first func(marker string) string
		want  string
	}{
		{name: "prose", first: func(string) string { return issuePlaceholderProse }, want: "template placeholder <cli>"},
		// This one parses, so without the check sh would run it and the marker
		// it touches first would prove the command reached sh.
		{name: "parses", first: func(marker string) string { return "touch " + marker + "; dotnet test <svc>/<name>.csproj" }, want: "template placeholder <svc>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "placeholder.ran")
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: placeholderLayoutScenario(t, tc.first(marker), "nm-web-unit")})
			h.WriteTestCommand("nm-web-unit", "echo web-unit-ran")
			runUnderSelectedJourney(t, h, "reask-"+tc.name)
			run := h.WaitForRun("reask-"+tc.name, 180*time.Second)
			logStatus(t, h)
			invs := h.AgentInvocations()
			if n := countInvocations(invs, discoveryPromptMarker); n != 2 {
				t.Fatalf("discovery ran %d times, want 2 (answer plus one re-ask)", n)
			}
			prompt := findInvocationContaining(invs, reaskPromptMarker)
			idx := strings.Index(prompt, reaskPromptMarker)
			if idx < 0 {
				t.Fatal("no discovery prompt carried the rejection")
			}
			t.Logf("=== re-ask prompt rejection section ===\n%s", prompt[idx:])
			for _, want := range []string{`discovered unit "web"`, tc.want} {
				if !strings.Contains(prompt[idx:], want) {
					t.Fatalf("re-ask prompt lacks %q", want)
				}
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the placeholder command reached sh")
			}
			t.Logf("=== test.log ===\n%s", testLogLines(t, h, run.ID, "rejected", "under-selected", "unit web", "unit api", "expand", "scope fault"))
			if !strings.Contains(testLogLines(t, h, run.ID, "expanding selection"), "expanding selection with web") {
				t.Fatal("under-selection never ran the corrected web command")
			}
			if run.Status != types.RunCompleted {
				t.Fatalf("run status = %s, want completed", run.Status)
			}
			if countInvocations(invs, evidencePromptMarker) == 0 {
				t.Fatal("evidence pass never ran after the corrected layout")
			}
		})
	}
}

// #59 adversarial: a re-ask that answers with another placeholder parks for
// the maintainer, and neither placeholder ever reaches sh.
func TestDiscoveryCommandJourney_SecondRejectionParks(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.ran")
	second := filepath.Join(dir, "second.ran")
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: placeholderLayoutScenario(t,
		"touch "+first+"; dotnet test <svc>/<name>.csproj",
		"touch "+second+"; dotnet test <project>/tests.csproj",
	)})
	runUnderSelectedJourney(t, h, "rejected-twice")
	waitForStepStatus(t, h, "rejected-twice", types.StepTest, types.StepStatusAwaitingApproval, 120*time.Second)
	logStatus(t, h)
	rec := readTestStep(t, h, "rejected-twice")
	t.Logf("test step findings: %s", rec.findingsJSON)
	if len(rec.findings) == 0 {
		t.Fatal("parked with no finding")
	}
	f := rec.findings[0]
	if f.Action != types.ActionAskUser || !strings.Contains(f.Description, "template placeholder <project>") {
		t.Fatalf("unexpected finding %+v", f)
	}
	invs := h.AgentInvocations()
	if n := countInvocations(invs, discoveryPromptMarker); n != 2 {
		t.Fatalf("discovery ran %d times, want 2", n)
	}
	if n := countInvocations(invs, evidencePromptMarker); n != 0 {
		t.Fatalf("evidence pass ran %d times before the park", n)
	}
	for _, m := range []string{first, second} {
		if _, err := os.Stat(m); err == nil {
			t.Fatalf("a placeholder command reached sh (%s)", m)
		}
	}
}

// #59 boundary: a configured command skips the placeholder check, so a
// legitimate redirect shaped like <in.txt > still runs.
func TestDiscoveryCommandJourney_ConfiguredRedirectIsNotRejected(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	h.WriteTestCommand("nm-configured-redirect", `cat; echo configured-redirect-ran >&2`)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange("configured", ".no-mistakes.yaml", "commands:\n  test: 'nm-configured-redirect <in.txt >\"$NO_MISTAKES_COVERAGE_DIR/out.txt\"'\n  lint: 'exit 0'\n", "configure test command")
	h.CommitChange("configured", "in.txt", "redirected input\n", "add input")
	out, err := h.Run("axi", "run", "--intent", "Validate the synthetic feature", "--skip", "pr,ci")
	t.Logf("=== axi run ===\n%s", out)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	run := h.WaitForRun("configured", 180*time.Second)
	logStatus(t, h)
	rec := readTestStep(t, h, "configured")
	t.Logf("test step findings: %s", rec.findingsJSON)
	if n := countInvocations(h.AgentInvocations(), discoveryPromptMarker); n != 0 {
		t.Fatalf("a configured command reached agent discovery %d times", n)
	}
	if strings.Contains(rec.findingsJSON, "template placeholder") {
		t.Fatalf("the configured redirect was rejected as a placeholder: %s", rec.findingsJSON)
	}
	t.Logf("=== test.log ===\n%s", testLogLines(t, h, run.ID, "configured-redirect", "unit repository", "exit"))
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", run.Status)
	}
}
