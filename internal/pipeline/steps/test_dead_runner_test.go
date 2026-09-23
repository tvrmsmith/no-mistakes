package steps

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// deadRunnerCommand fails before any test runs and writes no test report, the
// shape of a runner that did not build or a shell that rejected the command.
const deadRunnerCommand = "printf 'runner did not build'; exit 2"

// sequencedDiscoveryAgent answers the discovery passes with layouts in order,
// repeating the last one, answers fix rounds with a summary, and answers every
// evidence pass with a neutral live-validation payload.
func sequencedDiscoveryAgent(layouts ...string) *mockAgent {
	n := 0
	return &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if isDiscoveryCall(opts) {
			layout := layouts[min(n, len(layouts)-1)]
			n++
			return &agent.Result{Output: json.RawMessage(layout)}, nil
		}
		if strings.HasPrefix(opts.Prompt, testFixTask) {
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix tests"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(neutralEvidenceFindingsJSON)}, nil
	}}
}

func discoveryCalls(ag *mockAgent) []agent.RunOpts {
	var out []agent.RunOpts
	for _, call := range ag.calls {
		if isDiscoveryCall(call) {
			out = append(out, call)
		}
	}
	return out
}

func testFixRounds(ag *mockAgent) int {
	n := 0
	for _, call := range ag.calls {
		if strings.HasPrefix(call.Prompt, testFixTask) {
			n++
		}
	}
	return n
}

func onlyFinding(t *testing.T, raw string) types.Finding {
	t.Helper()
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want exactly one", findings.Items)
	}
	return findings.Items[0]
}

func TestTestStep_DeadConfiguredCommandParksForTheMaintainer(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := sequencedDiscoveryAgent(oneRepositoryUnitLayout)
	sctx := newTestContextWithCoverage(t, ag, dir, baseSHA, headSHA, config.Commands{Test: deadRunnerCommand})
	sctx.Config.AutoFix.Test = 3

	outcome, err := stepstest.ExecuteWithAutoFix(t, &TestStep{}, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable || outcome.ExitCode != 2 {
		t.Fatalf("outcome = %+v, want a maintainer park carrying exit code 2", outcome)
	}
	if n := testFixRounds(ag); n != 0 {
		t.Fatalf("fix rounds = %d, want 0: an agent must not rewrite tests to satisfy a broken runner", n)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent calls = %d, want 0: the park comes before discovery or evidence would help", len(ag.calls))
	}
	finding := onlyFinding(t, outcome.Findings)
	if finding.Action != types.ActionAskUser || finding.Category != types.FindingCategoryTestCommand {
		t.Fatalf("finding = %+v, want an ask-user test-command finding", finding)
	}
	if !strings.Contains(finding.Description, "test command could not run any test: it exited 2 and wrote no test report") {
		t.Fatalf("description = %q", finding.Description)
	}

	persistTestStepFindings(t, sctx, outcome.ExitCode, outcome.Findings)
	reason, err := (&TestStep{}).VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "could not run any test") {
		t.Fatalf("override reason = %q, want approving over a dead command recorded as an override", reason)
	}
}

func TestTestStep_FailingExitWithZeroExecutedTestsIsADeadRunner(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the report fixture is written through POSIX shell quoting")
	}
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")
	command := stepstest.CoverageCommand(stepstest.CoverageFixture{File: "services/api/main.go", Function: "Changed", Line: 1}) + "; exit 1"
	sctx := unitTestContext(t, nil, dir, baseSHA, headSHA, []config.TestUnit{{Name: "api", Path: "services/api", Command: command}})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want a maintainer park", outcome)
	}
	if finding := onlyFinding(t, outcome.Findings); !strings.Contains(finding.Description, "reported 0 executed tests") {
		t.Fatalf("description = %q", finding.Description)
	}
}

func TestTestStep_DeadInferredCommandIsRediscoveredWithItsFailure(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the replacement command writes coverage through POSIX shell quoting")
	}
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")
	replacement := coverageFor("true", "services/api/main.go")
	ag := sequencedDiscoveryAgent(
		encodeOneUnitLayout("api", "services/api", deadRunnerCommand, "api"),
		encodeOneUnitLayout("api", "services/api", replacement, "api"),
	)
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected the replacement command to pass, got: %s", outcome.Findings)
	}
	calls := discoveryCalls(ag)
	if len(calls) != 2 {
		t.Fatalf("discovery calls = %d, want 2", len(calls))
	}
	for _, want := range []string{deadRunnerCommand, "runner did not build", "exited 2 and wrote no test report"} {
		if !strings.Contains(calls[1].Prompt, want) {
			t.Errorf("rediscovery prompt missing %q", want)
		}
	}
	if cached, ok := sctx.Shared.TestDiscovery(changedFilesFingerprint([]string{"services/api/main.go"})); !ok || cached.Units[0].Command != replacement {
		t.Fatalf("cached discovery = %+v, want the replacement so later attempts reuse it", cached)
	}
}

func TestTestStep_RediscoveryKeepingTheCommandTreatsItAsFailingTests(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")
	layout := encodeOneUnitLayout("api", "services/api", deadRunnerCommand, "api")
	ag := sequencedDiscoveryAgent(layout, layout)
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want an auto-fixable failing-tests park", outcome)
	}
	finding := onlyFinding(t, outcome.Findings)
	if finding.Action != types.ActionAutoFix || finding.Description != "tests failed with exit code 2" {
		t.Fatalf("finding = %+v, want the auto-fix failing-tests finding", finding)
	}

	// The run's one rediscovery is spent, so the next attempt parks for the
	// maintainer instead of asking discovery again.
	outcome, err = (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("second attempt = %+v, want a maintainer park", outcome)
	}
	if finding := onlyFinding(t, outcome.Findings); !strings.Contains(finding.Description, "already replaced a dead inferred command once in this run") {
		t.Fatalf("description = %q", finding.Description)
	}
	if n := len(discoveryCalls(ag)); n != 2 {
		t.Fatalf("discovery calls = %d, want 2", n)
	}
}

func TestTestStep_ReplacementThatAlsoCannotRunParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")
	const secondDead = "printf 'still broken'; exit 3"
	ag := sequencedDiscoveryAgent(
		encodeOneUnitLayout("api", "services/api", deadRunnerCommand, "api"),
		encodeOneUnitLayout("api", "services/api", secondDead, "api"),
	)
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable || outcome.ExitCode != 3 {
		t.Fatalf("outcome = %+v, want a maintainer park on the replacement's exit code", outcome)
	}
	finding := onlyFinding(t, outcome.Findings)
	for _, want := range []string{"exited 3 and wrote no test report", deadRunnerCommand} {
		if !strings.Contains(finding.Description, want) {
			t.Errorf("description %q missing %q", finding.Description, want)
		}
	}
	if n := len(discoveryCalls(ag)); n != 2 {
		t.Fatalf("discovery calls = %d, want 2", n)
	}
}
