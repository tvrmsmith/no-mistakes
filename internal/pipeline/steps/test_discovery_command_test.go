package steps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

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
		{name: "input and output redirects", command: "sort <input.txt >out.txt"},
		{name: "redirects around a runner", command: "./t <cases.json >report.txt"},
		{name: "heredoc", command: "cat <<EOF >out\nok\nEOF"},
		{name: "input redirect before an fd redirect", command: "./t <in.txt 2>err.txt"},
		{name: "input redirect before stderr joins stdout", command: "./run <cases.json 2>&1 | tee out"},
		{name: "path placeholder", command: "dotnet test <path/to/project.csproj> --no-build", rejectedWith: "template placeholder <path/to/project.csproj>"},
		{name: "module path placeholder", command: "cargo test <crate::module>", rejectedWith: "template placeholder <crate::module>"},
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
		// writesMarker marks a command that parses, so sh would write its
		// marker if the command ever reached it.
		writesMarker bool
		rejectedWith string
	}{
		{name: "prose placeholder", placeholder: func(string) string { return issuePlaceholderCommand }, rejectedWith: "template placeholder <cli>"},
		{name: "prose that does not parse", placeholder: func(string) string {
			return "run the api tests (per changed service): go test"
		}, rejectedWith: "is not a valid shell command"},
		{name: "placeholder that parses", placeholder: func(marker string) string {
			return markerCommand(marker) + "; dotnet test <svc>/<name>.csproj"
		}, writesMarker: true, rejectedWith: "template placeholder <svc>"},
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
			placeholder := tc.placeholder(placeholderMarker)
			ag := sequencedDiscoveryAgent(
				layout(placeholder),
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
			if tc.writesMarker && fileExists(placeholderMarker) {
				t.Error("the placeholder command reached sh")
			}
			if log := joinedLog(*lines); strings.Contains(log, "could not run any test") {
				t.Errorf("the placeholder command ran as a dead runner instead of being rejected:\n%s", log)
			}
			calls := discoveryCalls(ag)
			if len(calls) != 2 {
				t.Fatalf("discovery calls = %d, want 2: the rejected answer is re-asked once", len(calls))
			}
			for _, want := range []string{"previous answer was rejected", `discovered unit "web"`, strconv.Quote(placeholder), tc.rejectedWith} {
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
	ag := sequencedDiscoveryAgent(
		encodeOneUnitLayout("api", "services/api", "dotnet test <svc>/<name>.csproj", "api"),
		encodeOneUnitLayout("api", "services/api", "dotnet test <project>/tests.csproj", "api"),
	)
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want a maintainer park", outcome)
	}
	if finding := onlyFinding(t, outcome.Findings); !strings.Contains(finding.Description, "template placeholder <project>") {
		t.Errorf("finding %q does not name the second answer's rejection", finding.Description)
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
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("checkInferredCommand = %v, want the missing sh", err)
	}
	var rejection discoveryResultError
	if errors.As(err, &rejection) {
		t.Fatalf("a missing sh was reported as a rejected answer: %v", err)
	}
}

// fakeShOnPath puts an executable sh running body first on PATH, so a parse
// check reaches a shell that misbehaves the way body does.
func fakeShOnPath(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sh"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestCheckInferredCommand_TimedOutRunIsNotARejection keeps a parse check the
// run's deadline killed mid-run from reading as a verdict on the agent's
// answer.
func TestCheckInferredCommand_TimedOutRunIsNotARejection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cmd.exe runs unit commands on Windows, so there is no sh -n parse")
	}
	fakeShOnPath(t, "sleep 30")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := checkInferredCommand(ctx, config.TestUnit{Name: "api", Path: "services/api", Command: "go test ./services/api/..."})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("checkInferredCommand = %v, want the deadline", err)
	}
	var rejection discoveryResultError
	if errors.As(err, &rejection) {
		t.Fatalf("a timed-out check was reported as a rejected answer: %v", err)
	}
}

// TestCheckInferredCommand_SignalledShIsNotARejection keeps an sh that
// something outside the run killed from reading as a verdict on the agent's
// answer.
func TestCheckInferredCommand_SignalledShIsNotARejection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cmd.exe runs unit commands on Windows, so there is no sh -n parse")
	}
	fakeShOnPath(t, "kill -KILL $$")
	err := checkInferredCommand(t.Context(), config.TestUnit{Name: "api", Path: "services/api", Command: "go test ./services/api/..."})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() >= 0 {
		t.Fatalf("checkInferredCommand = %v, want the signalled sh", err)
	}
	var rejection discoveryResultError
	if errors.As(err, &rejection) {
		t.Fatalf("a signalled check was reported as a rejected answer: %v", err)
	}
}

// TestTestStep_SignalledShFailsTheRunWithoutReasking drives an sh invocation
// failure through discovery: it is not a verdict on the agent's answer, so the
// run fails on the first answer instead of spending a re-ask.
func TestTestStep_SignalledShFailsTheRunWithoutReasking(t *testing.T) {
	skipUnlessPOSIXShell(t)
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")
	layout := encodeOneUnitLayout("api", "services/api", coverageFor("true", "services/api/main.go"), "api")
	ag := sequencedDiscoveryAgent(layout, layout)
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)
	lines := capturingLog(sctx)
	fakeShOnPath(t, "kill -KILL $$")

	outcome, err := (&TestStep{}).Execute(sctx)
	if err == nil {
		t.Fatalf("Execute returned no error, outcome = %+v", outcome)
	}
	var rejection discoveryResultError
	if errors.As(err, &rejection) {
		t.Fatalf("a signalled sh was reported as a rejected answer: %v", err)
	}
	if n := len(discoveryCalls(ag)); n != 1 {
		t.Fatalf("discovery calls = %d, want 1: an sh failure is not re-asked", n)
	}
	if log := joinedLog(*lines); strings.Contains(log, "asking again") {
		t.Errorf("an sh failure was re-asked:\n%s", log)
	}
}
