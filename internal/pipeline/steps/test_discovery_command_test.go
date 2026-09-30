package steps

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// issuePlaceholderCommand is the prose an agent wrote as an unselected .NET
// unit's command in the run that exposed the unchecked command.
const issuePlaceholderCommand = `<cli> verify (per changed .NET service from <manifest>): dotnet test <svc>/<name>.csproj --filter "..." --settings <nearest coverlet.runsettings> ...`

func TestCheckInferredCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		command string
		// rejectedWith is empty for a command the check accepts.
		rejectedWith string
		// needsSh marks a case only the sh -n parse can reject.
		needsSh bool
	}{
		{name: "plain command", command: "go test ./services/api/..."},
		{name: "pipes and redirects", command: `go test ./services/api/... 2>&1 | tee "$NO_MISTAKES_COVERAGE_DIR/out.txt"`},
		{name: "generic type in a filter", command: `dotnet test --filter "FullyQualifiedName~Cache<Key>"`},
		{name: "prose placeholder", command: issuePlaceholderCommand, rejectedWith: "template placeholder <cli>"},
		{name: "placeholder that parses", command: "dotnet test <svc>/<name>.csproj", rejectedWith: "template placeholder <svc>"},
		{name: "placeholder with a space", command: "dotnet test --settings <nearest coverlet.runsettings>", rejectedWith: "template placeholder <nearest coverlet.runsettings>"},
		{name: "prose that does not parse", command: "run the api tests (per changed service): go test", rejectedWith: "is not a valid shell command", needsSh: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.needsSh && runtime.GOOS == "windows" {
				t.Skip("cmd.exe runs unit commands on Windows, so there is no sh -n parse")
			}
			err := checkInferredCommand(t.Context(), config.TestUnit{Name: "api", Path: "services/api", Command: tc.command})
			if tc.rejectedWith == "" {
				if err != nil {
					t.Fatalf("checkInferredCommand(%q) = %v, want accepted", tc.command, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.rejectedWith) {
				t.Fatalf("checkInferredCommand(%q) = %v, want a rejection containing %q", tc.command, err, tc.rejectedWith)
			}
			var rejection discoveryResultError
			if !errors.As(err, &rejection) {
				t.Fatalf("rejection %v is not a discovery result, so it would fail the run instead of re-asking", err)
			}
		})
	}
}

// TestTestStep_UnselectedUnitsPlaceholderCommandIsReaskedBeforeItRuns drives
// the observed run: the selected unit passes and a changed file belongs to an
// unselected unit whose command is a placeholder. Under-selection would run
// that command with no agent pass in between, so discovery must reject it and
// re-ask the agent before anything runs.
func TestTestStep_UnselectedUnitsPlaceholderCommandIsReaskedBeforeItRuns(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	for _, tc := range []struct {
		name        string
		placeholder func(marker string) string
	}{
		{name: "prose that does not parse", placeholder: func(string) string { return issuePlaceholderCommand }},
		// This one parses, so without the check sh would run it and the
		// marker it writes first would prove the command reached sh.
		{name: "placeholder that parses", placeholder: func(marker string) string {
			return markerCommand(marker) + "; dotnet test <svc>/<name>.csproj"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA := newUnitRepo(t)
			changeUnitFile(t, dir, "services/web/main.go")
			headSHA := changeUnitFile(t, dir, "services/api/main.go")
			markerDir := t.TempDir()
			placeholderMarker := filepath.Join(markerDir, "placeholder.ran")
			webMarker := filepath.Join(markerDir, "web.done")

			apiCommand := coverageFor("true", "services/api/main.go")
			layout := func(webCommand string) string {
				return `{"units":[{"name":"api","path":"services/api","command":` + jsonString(t, apiCommand) + `},{"name":"web","path":"services/web","command":` + jsonString(t, webCommand) + `}],"selected":["api"]}`
			}
			ag := sequencedDiscoveryAgent(
				layout(tc.placeholder(placeholderMarker)),
				layout(coverageFor(markerCommand(webMarker), "services/web/main.go")),
			)
			sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)
			lines := capturingLog(sctx)

			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.NeedsApproval {
				t.Fatalf("expected the corrected layout to pass, got: %s", outcome.Findings)
			}
			if fileExists(placeholderMarker) {
				t.Error("the placeholder command reached sh")
			}
			if log := joinedLog(*lines); strings.Contains(log, "could not run any test") {
				t.Errorf("the placeholder command ran as a dead runner instead of being rejected:\n%s", log)
			}
			calls := discoveryCalls(ag)
			if len(calls) != 2 {
				t.Fatalf("discovery calls = %d, want 2: the rejected answer is re-asked once", len(calls))
			}
			for _, want := range []string{"previous answer was rejected", `discovered unit "web"`} {
				if !strings.Contains(calls[1].Prompt, want) {
					t.Errorf("re-ask prompt missing %q", want)
				}
			}
			if !fileExists(webMarker) {
				t.Error("the corrected web command did not run for the under-selected file")
			}
		})
	}
}

func TestTestStep_RepeatedlyRejectedDiscoveryAnswerParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")
	ag := sequencedDiscoveryAgent(encodeOneUnitLayout("api", "services/api", "dotnet test <svc>/<name>.csproj", "api"))
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want a maintainer park", outcome)
	}
	if finding := onlyFinding(t, outcome.Findings); !strings.Contains(finding.Description, "template placeholder <svc>") {
		t.Errorf("finding %q does not name the rejection", finding.Description)
	}
	if n := len(discoveryCalls(ag)); n != 2 {
		t.Fatalf("discovery calls = %d, want 2: one answer and one re-ask", n)
	}
}

// TestCheckInferredCommand_MissingShIsNotARejection keeps a parse check that
// could not run an invocation failure, which fails the run, rather than a
// verdict on the agent's answer.
func TestCheckInferredCommand_MissingShIsNotARejection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cmd.exe runs unit commands on Windows, so there is no sh -n parse")
	}
	t.Setenv("PATH", t.TempDir())
	err := checkInferredCommand(t.Context(), config.TestUnit{Name: "api", Path: "services/api", Command: "go test ./services/api/..."})
	if err == nil {
		t.Fatal("checkInferredCommand accepted a command it could not parse-check")
	}
	var rejection discoveryResultError
	if errors.As(err, &rejection) {
		t.Fatalf("a missing sh was reported as a rejected answer: %v", err)
	}
}

// TestCheckInferredCommand_CancelledRunIsNotARejection keeps a parse check the
// run's cancellation killed from reading as a verdict on the agent's answer.
func TestCheckInferredCommand_CancelledRunIsNotARejection(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("cmd.exe runs unit commands on Windows, so there is no sh -n parse")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := checkInferredCommand(ctx, config.TestUnit{Name: "api", Path: "services/api", Command: "go test ./services/api/..."})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("checkInferredCommand = %v, want the cancellation", err)
	}
	var rejection discoveryResultError
	if errors.As(err, &rejection) {
		t.Fatalf("a cancelled check was reported as a rejected answer: %v", err)
	}
}
