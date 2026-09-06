package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The vacuous-green guard's behaviour at the Test step's own seam. Every test
// here drives a real TestStep against a real temp git repository, because the
// guard's verdict depends on three things only a whole step run produces: the
// diff's changed lines, the per-unit coverage directory the command was handed,
// and the artifacts that command actually wrote.

// coverageStepContext builds a Test-step context with a configured
// commands.test, which discovery collapses into the single "repository" unit.
// It sets CoverageDir explicitly because the package's newTestContext leaves it
// empty and the guard has nowhere to read artifacts from without it.
func coverageStepContext(t *testing.T, ag agent.Agent, dir, baseSHA, headSHA, command string) *pipeline.StepContext {
	t.Helper()
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)
	sctx.Config.Commands.Test = command
	sctx.CoverageDir = filepath.Join(t.TempDir(), "coverage", "run-1")
	return sctx
}

// findingDescriptions pulls the human-readable half of an outcome, which is
// what a maintainer reads at the gate and therefore what these tests assert on.
func findingDescriptions(t *testing.T, outcome *pipeline.StepOutcome) string {
	t.Helper()
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings %q: %v", outcome.Findings, err)
	}
	descriptions := make([]string, 0, len(findings.Items))
	for _, item := range findings.Items {
		descriptions = append(descriptions, item.Description)
	}
	return strings.Join(descriptions, "\n")
}

// skipUnlessPOSIXShell guards the fixtures that write coverage artifacts.
// stepstest.CoverageCommand owns the rationale.
func skipUnlessPOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the coverage fixture command writes its artifacts through POSIX shell redirection")
	}
}

// newSourceRepo builds a committed repository from an explicit file layout,
// for the cases whose verdict turns on which languages the repository writes
// code in rather than on the shared services/api fixture.
func newSourceRepo(t *testing.T, files map[string]string) (dir, baseSHA string) {
	t.Helper()
	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "config", "commit.gpgsign", "false")
	gitCmd(t, dir, "checkout", "-b", "main")
	for rel, content := range files {
		writeRepoFile(t, dir, rel, content)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "-b", "feature")
	return dir, baseSHA
}

// fixRoundAgent stands in for the repair agent a fix round invokes. The repair
// itself is staged on disk by the test, so this only has to answer.
func fixRoundAgent() agent.Agent {
	return &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"summary":"added the missing test"}`)}, nil
		},
	}
}

// appendToFile leaves an UNCOMMITTED edit, which is what a fix round's repair
// looks like to the guard.
func appendToFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTestStep_UnitCommandWritingNoCoverageParksForTheMaintainer(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, "exit 0")

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("a command that wrote no coverage reported a passing gate, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a missing coverage profile is a configuration problem, so an agent fix round must not paper over it")
	}
	got := findingDescriptions(t, outcome)
	if !strings.Contains(got, `"repository"`) {
		t.Errorf("finding does not name the unit, got:\n%s", got)
	}
	if !strings.Contains(got, filepath.Join(sctx.CoverageDir, "repository")) {
		t.Errorf("finding does not name the directory the profile was expected in, got:\n%s", got)
	}
}

// The two tests below separate the missing-profile park from the
// missing-report park. A command that writes neither trips whichever check
// runs first, so without a fixture that writes exactly one artifact, deleting
// either check leaves the suite green while the guard has lost half its
// evidence.

func TestTestStep_UnitWritingOnlyAReportParksForTheMaintainer(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	command := `printf '%s' '<testsuite tests="3" skipped="0"></testsuite>' > "$NO_MISTAKES_COVERAGE_DIR/report.xml"`
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("a reported test count with no coverage profile reported a passing gate, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a missing coverage profile is a configuration problem, so an agent fix round must not paper over it")
	}
	if got := findingDescriptions(t, outcome); !strings.Contains(got, "no coverage profile") {
		t.Errorf("finding does not say the profile is what is missing, got:\n%s", got)
	}
}

func TestTestStep_UnitWritingOnlyAProfileParksForTheMaintainer(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	command := `printf '%s' 'SF:services/api/main.go
FN:2,Changed
FNDA:5,Changed
end_of_record
' > "$NO_MISTAKES_COVERAGE_DIR/coverage.lcov"`
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("coverage with no test report reported a passing gate, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a missing test report is a configuration problem, so an agent fix round must not paper over it")
	}
	if got := findingDescriptions(t, outcome); !strings.Contains(got, "no test report") {
		t.Errorf("finding does not say the report is what is missing, got:\n%s", got)
	}
}

func TestTestStep_UnitReportingZeroExecutedTestsParksAutoFixable(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	// The profile records the changed function as executed, so only the report
	// separates this from a real pass: a suite whose every test was filtered
	// out exercised nothing.
	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "services/api/main.go",
		Function: "Changed",
		Line:     2,
		Hits:     5,
		Tests:    3,
		Skipped:  3,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("a suite that executed no test reported a passing gate, findings: %s", outcome.Findings)
	}
	if !outcome.AutoFixable {
		t.Error("a suite that executed no test is a missing-test problem an agent fix round can address")
	}
	if got := findingDescriptions(t, outcome); !strings.Contains(got, "0 executed tests") {
		t.Errorf("finding does not name the executed-test count, got:\n%s", got)
	}
}

func TestTestStep_CoverageMissingTheChangedFunctionParksAutoFixable(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	// Other sits above the changed line and ran; Changed owns the appended line
	// and did not. A file-level coverage check would call this covered.
	profile := "SF:services/api/main.go\nFN:1,Other\nFNDA:3,Other\nFN:2,Changed\nFNDA:0,Changed\nend_of_record\n"
	command := stepstest.WriteCoverageArtifactsCommand(profile, `<testsuite tests="5" skipped="0"></testsuite>`)
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("tests that never reached the changed lines reported a passing gate, findings: %s", outcome.Findings)
	}
	if !outcome.AutoFixable {
		t.Error("an untested change is what an agent fix round is for")
	}
	got := findingDescriptions(t, outcome)
	if !strings.Contains(got, "no test exercised a changed function") {
		t.Errorf("finding does not say the change went unexercised, got:\n%s", got)
	}
	if !strings.Contains(got, "services/api/main.go") {
		t.Errorf("finding does not name the changed coverable file, got:\n%s", got)
	}
}

func TestTestStep_CoverageOfAChangedFunctionPasses(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "services/api/main.go",
		Function: "Changed",
		Line:     2,
		Hits:     4,
		Tests:    5,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("a run that exercised the changed function parked, findings: %s", outcome.Findings)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	want := "repository: " + command
	if len(findings.Tested) != 1 || findings.Tested[0] != want {
		t.Errorf("tested = %q, want [%q]", findings.Tested, want)
	}
}

// TestTestStep_CoverageArtifactsStayOutOfTheWorktree is the acceptance
// criterion that a pushed branch carries no coverage artifacts, so it asserts
// both halves: the worktree is untouched and the profile really landed in the
// run's coverage directory.
func TestTestStep_CoverageArtifactsStayOutOfTheWorktree(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "services/api/main.go",
		Function: "Changed",
		Line:     2,
		Hits:     4,
		Tests:    5,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("a run that exercised the changed function parked, findings: %s", outcome.Findings)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Errorf("coverage artifacts reached the worktree, git status --porcelain:\n%s", status)
	}
	profile := filepath.Join(sctx.CoverageDir, "repository", "coverage.lcov")
	if !fileExists(profile) {
		t.Errorf("no coverage profile at %s, so the guard passed on nothing", profile)
	}
}

func TestTestStep_DocsOnlyChangeDoesNotRequireACoveredFunction(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# readme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "document the thing")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// The profile describes .go files and covers nothing the change touched.
	// No coverage tool instruments a README, so demanding a covered function
	// here would park every documentation change.
	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "services/api/main.go",
		Function: "Untouched",
		Line:     1,
		Hits:     0,
		Tests:    5,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("a documentation-only change parked over coverage, findings: %s", outcome.Findings)
	}
}

func TestTestStep_StaleCoverageFromAnEarlierAttemptDoesNotGreenTheNextOne(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	good := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "services/api/main.go",
		Function: "Changed",
		Line:     2,
		Hits:     4,
		Tests:    5,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, good)

	step := &TestStep{}
	first, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.NeedsApproval {
		t.Fatalf("the first attempt parked, findings: %s", first.Findings)
	}

	sctx.Config.Commands.Test = "exit 0"
	second, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !second.NeedsApproval {
		t.Fatalf("the earlier attempt's profile greened an attempt that wrote nothing, findings: %s", second.Findings)
	}
}

func TestTestStep_UnitCommandReceivesTheCoverageDir(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")
	outFile := filepath.Join(t.TempDir(), "coveragedir.out")

	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, `printf '%s' "$NO_MISTAKES_COVERAGE_DIR" > `+outFile)

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	recorded, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(recorded)
	if got == "" {
		t.Fatal("the unit command saw no NO_MISTAKES_COVERAGE_DIR")
	}
	if strings.HasPrefix(got, dir+string(filepath.Separator)) {
		t.Errorf("coverage dir %q is inside the worktree %q, so its artifacts would reach the branch", got, dir)
	}
	if base := filepath.Base(got); base != "repository" {
		t.Errorf("coverage dir last segment = %q, want the unit name %q", base, "repository")
	}
}

// TestTestStep_NoUnitCommandRanSkipsTheGuard covers the agent-evidence path.
// No command ran there, so nothing could have written an artifact and the
// guard has no evidence to judge; firing would park every such run.
func TestTestStep_NoUnitCommandRanSkipsTheGuard(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/web/main.go")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "Derive this repository's independently testable units") {
				// The one unit owns nothing the change touched, so the selection
				// is empty and no command runs.
				return &agent.Result{Output: json.RawMessage(`{"units":[{"name":"api","path":"services/api","command":"exit 0"}],"selected":[]}`)}, nil
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"testing_summary":"exercised the change by hand"}`)}, nil
		},
	}
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)
	sctx.CoverageDir = filepath.Join(t.TempDir(), "coverage", "run-1")
	log := capturingLog(sctx)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("the guard fired on a path where no unit command ran, findings: %s", outcome.Findings)
	}
	// The guard's own exemption line is emitted only once the guard has been
	// entered, so its absence is what proves the guard was skipped rather than
	// entered and cleared on an empty profile.
	if got := joinedLog(*log); strings.Contains(got, "no changed file has an extension the coverage profiles describe") {
		t.Errorf("the guard was entered with no unit command run, log:\n%s", got)
	}
}

// TestTestStep_OnlyAnUnparseableProfileReadsAsAMissingProfile covers the
// verdict a unit whose single profile is broken actually gets: the file never
// becomes a profile, so the guard reports the missing profile and carries the
// unparseable file as the reason it is missing.
func TestTestStep_OnlyAnUnparseableProfileReadsAsAMissingProfile(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	// The SF: record classifies the file as LCOV; the FN: record then fails the
	// grammar, which is a broken emitter rather than an untested change.
	profile := "SF:services/api/main.go\nFN:notanumber,Changed\nend_of_record\n"
	command := stepstest.WriteCoverageArtifactsCommand(profile, `<testsuite tests="5" skipped="0"></testsuite>`)
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("an unparseable profile reported a passing gate, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a profile the guard cannot parse is a configuration problem, not one an agent fix round should answer")
	}
	got := findingDescriptions(t, outcome)
	if !strings.Contains(got, "wrote no coverage profile") {
		t.Errorf("finding does not report the missing profile, got:\n%s", got)
	}
	if !strings.Contains(got, "artifacts that could not be parsed") {
		t.Errorf("finding does not carry the unparseable file that explains the miss, got:\n%s", got)
	}
}

// TestTestStep_UnparseableCoverageBesideGoodArtifactsParksForTheMaintainer
// reaches the guard's own unparseable branch, which the single-broken-file case
// above never does: with a second, valid profile present the unit has both a
// profile and a report, so the broken file is the only thing left to report.
func TestTestStep_UnparseableCoverageBesideGoodArtifactsParksForTheMaintainer(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	good := stepstest.WriteCoverageArtifactsCommand(
		"SF:services/api/main.go\nFN:1,Changed\nFNDA:3,Changed\nend_of_record\n",
		`<testsuite tests="5" skipped="0"></testsuite>`,
	)
	broken := `printf 'SF:services/api/main.go\nFN:notanumber,Changed\nend_of_record\n' > "$NO_MISTAKES_COVERAGE_DIR/broken.lcov"`
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, good+"; "+broken)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("an unparseable artifact reported a passing gate, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("an artifact the guard cannot parse is a configuration problem, not one an agent fix round should answer")
	}
	got := findingDescriptions(t, outcome)
	if !strings.Contains(got, "wrote a coverage artifact that could not be parsed") {
		t.Errorf("finding does not use the unparseable-artifact wording, got:\n%s", got)
	}
	if !strings.Contains(got, "broken.lcov") {
		t.Errorf("finding does not name the file the guard could not parse, got:\n%s", got)
	}
}

// TestTestStep_TwoUnitsGetSeparateCoverageDirectories pins the reason each unit
// owns a directory: a shared one would let one unit's profile satisfy another
// unit's guard.
func TestTestStep_TwoUnitsGetSeparateCoverageDirectories(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	recordDir := t.TempDir()
	apiOut := filepath.Join(recordDir, "api.dir")
	webOut := filepath.Join(recordDir, "web.dir")

	changeUnitFile(t, dir, "services/api/main.go")
	headSHA := changeUnitFile(t, dir, "services/web/main.go")

	units := []config.TestUnit{
		{Name: "api", Path: "services/api", Command: `printf '%s' "$NO_MISTAKES_COVERAGE_DIR" > ` + apiOut},
		{Name: "web", Path: "services/web", Command: `printf '%s' "$NO_MISTAKES_COVERAGE_DIR" > ` + webOut},
	}
	sctx := unitTestContext(t, nil, dir, baseSHA, headSHA, units)
	sctx.CoverageDir = filepath.Join(t.TempDir(), "coverage", "run-1")

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	apiDir, err := os.ReadFile(apiOut)
	if err != nil {
		t.Fatal(err)
	}
	webDir, err := os.ReadFile(webOut)
	if err != nil {
		t.Fatal(err)
	}
	if string(apiDir) == string(webDir) {
		t.Errorf("both units wrote coverage into %q, so either one's profile could green the other", apiDir)
	}
}

// TestTestStep_EmptyButValidProfileParksForTheMaintainer covers the profile
// that parses and describes nothing. `<coverage><packages/></coverage>` is
// valid Cobertura, so HasProfile is true while the profile names no source
// file; read as "this change touches nothing a profile could describe" it
// greened a run whose instrumenter matched no source at all.
func TestTestStep_EmptyButValidProfileParksForTheMaintainer(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	command := stepstest.WriteCoverageArtifactsCommand(
		`<?xml version="1.0"?><coverage line-rate="0"><packages/></coverage>`,
		`<testsuite tests="5" skipped="0"></testsuite>`,
	)
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("a profile naming no source file reported a passing gate, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a profile that describes nothing is a reporting problem, so an agent fix round must not paper over it")
	}
	if got := findingDescriptions(t, outcome); !strings.Contains(got, "name no source file") {
		t.Errorf("finding does not say the profile describes nothing, got:\n%s", got)
	}
}

// TestTestStep_NegativeExecutedCountParksForTheMaintainer covers a runner that
// reports root-level skips against per-suite totals. The arithmetic goes
// negative, which is no evidence a test ran, but it is a counter bug in the
// report rather than a missing test, so it takes the maintainer park with the
// numbers attached instead of charging an agent fix round with writing a test.
func TestTestStep_NegativeExecutedCountParksForTheMaintainer(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	profile := "SF:services/api/main.go\nFN:2,Changed\nFNDA:5,Changed\nend_of_record\n"
	command := stepstest.WriteCoverageArtifactsCommand(profile, `<testsuites tests="2" skipped="5"></testsuites>`)
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("a negative executed count reported a passing gate, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a counter bug in the report is not a test an agent fix round can write")
	}
	// The whole rendered clause, not the two numbers on their own: a temp-dir
	// path in the same message satisfies "contains 2" and "contains 5" by
	// accident, so those assertions could not fail.
	got := findingDescriptions(t, outcome)
	if want := "junit: skipped count 5 exceeds tests count 2"; !strings.Contains(got, want) {
		t.Errorf("finding does not carry the parser's diagnosis %q, got:\n%s", want, got)
	}
}

// TestTestStep_UnrecognisedFileBesideGoodArtifactsDoesNotPark keeps a runner's
// own output (an HTML report tree, a log) from parking a repository whose
// profile and report are both there and readable.
func TestTestStep_UnrecognisedFileBesideGoodArtifactsDoesNotPark(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "services/api/main.go",
		Function: "Changed",
		Line:     2,
		Hits:     4,
		Tests:    5,
	}) + `; mkdir -p "$NO_MISTAKES_COVERAGE_DIR/lcov-report"; i=0; while [ $i -lt 300 ]; do printf '%s' '<html></html>' > "$NO_MISTAKES_COVERAGE_DIR/lcov-report/page$i.html"; i=$((i+1)); done`
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("a runner's own HTML output parked a correctly reported run, findings: %s", outcome.Findings)
	}
}

// TestTestStep_TwoUnitsMergeTheirProfilesForTheChangedFunctionCheck pins the
// cross-unit merge: a change can span two units and either one's tests may be
// what exercises it, so the check runs once over both profiles.
//
// Each half of the merge is load-bearing here. Neither profile names both
// changed files, so only the union of their file lists explains the change,
// and only the web unit records an executed function in changed lines, so a
// guard judging the api unit on its own profile would park this run.
func TestTestStep_TwoUnitsMergeTheirProfilesForTheChangedFunctionCheck(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)

	changeUnitFile(t, dir, "services/api/main.go")
	headSHA := changeUnitFile(t, dir, "services/web/main.go")

	apiCommand := stepstest.WriteCoverageArtifactsCommand(
		"SF:services/api/main.go\nFN:1,Changed\nFNDA:0,Changed\nend_of_record\n",
		`<testsuite tests="4" skipped="0"></testsuite>`,
	)
	webCommand := stepstest.WriteCoverageArtifactsCommand(
		"SF:services/web/main.go\nFN:1,WebChanged\nFNDA:2,WebChanged\nend_of_record\n",
		`<testsuite tests="2" skipped="0"></testsuite>`,
	)
	units := []config.TestUnit{
		{Name: "api", Path: "services/api", Command: apiCommand},
		{Name: "web", Path: "services/web", Command: webCommand},
	}
	sctx := unitTestContext(t, nil, dir, baseSHA, headSHA, units)
	sctx.CoverageDir = filepath.Join(t.TempDir(), "coverage", "run-1")

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil {
		t.Fatal("the step returned no outcome, so it never reached the guard")
	}
	if outcome.NeedsApproval {
		t.Fatalf("the guard judged the units separately instead of merging their profiles, findings: %s", outcome.Findings)
	}
}

// TestTestStep_UnconfiguredCoverageDirFailsBeforeTheCommandRuns pins the
// diagnostic for a run whose StepContext carries no coverage directory. The
// step cannot hand a command NO_MISTAKES_COVERAGE_DIR then, and running it
// anyway would produce an unjudgeable green.
func TestTestStep_UnconfiguredCoverageDirFailsBeforeTheCommandRuns(t *testing.T) {
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")
	marker := filepath.Join(t.TempDir(), "ran.marker")

	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, `printf '%s' ran > `+marker)
	sctx.CoverageDir = ""

	outcome, err := (&TestStep{}).Execute(sctx)
	if err == nil {
		t.Fatalf("an unconfigured coverage dir produced outcome %+v instead of an error", outcome)
	}
	if !strings.Contains(err.Error(), "coverage dir is not configured") {
		t.Errorf("error does not name the unconfigured coverage dir: %v", err)
	}
	if fileExists(marker) {
		t.Error("the unit command ran without a coverage directory to report into")
	}
}

// TestTestUnitCoverageDir_EveryNameStaysOneChildOfTheRunRoot pins the
// containment the caller depends on: it wipes the returned directory before
// the unit's command runs, so a name resolving to the run root or to the root
// every run shares would destroy another unit's or another run's artifacts.
// Two names that sanitize alike must not share one directory either.
func TestTestUnitCoverageDir_EveryNameStaysOneChildOfTheRunRoot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "coverage", "run-1")
	sctx := &pipeline.StepContext{CoverageDir: root}

	names := []string{".", "..", "../..", "repository", "api/v1", "api v1", "  ", "-"}
	seen := map[string]string{}
	for _, name := range names {
		dir, err := testUnitCoverageDir(sctx, name)
		if err != nil {
			t.Errorf("testUnitCoverageDir(%q): %v", name, err)
			continue
		}
		if parent := filepath.Dir(dir); parent != filepath.Clean(root) {
			t.Errorf("unit %q resolved to %q, whose parent %q is not the run root %q", name, dir, parent, root)
		}
		if other, ok := seen[dir]; ok {
			t.Errorf("units %q and %q share coverage directory %q", other, name, dir)
		}
		seen[dir] = name
	}
}

// TestTestStep_GuardRunsAfterUnderSelectionExpansion pins where the guard sits
// in the attempt. Under-selection expands the selection and runs a unit the
// first pass left out; a guard placed before that expansion would judge only
// the originally selected unit and green an attempt whose expanded unit
// reported nothing at all.
func TestTestStep_GuardRunsAfterUnderSelectionExpansion(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)

	f, err := os.OpenFile(filepath.Join(dir, "services", "web", "main.go"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("// changed\n")
	f.Close()
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "change web too")
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	// api reports proper artifacts and web reports none, while discovery selects
	// api alone. Only the expansion brings web into the attempt, so only a guard
	// running after it can see the unit that certified nothing.
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			out := `{"units":[{"name":"api","path":"services/api","command":` + jsonString(t, coverageFor("true", "services/api/main.go")) + `},{"name":"web","path":"services/web","command":"true"}],"selected":["api"]}`
			return &agent.Result{Output: json.RawMessage(out)}, nil
		},
	}
	sctx := unitTestContext(t, ag, dir, baseSHA, headSHA, nil)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("the expanded unit wrote no coverage yet the attempt reported a passing gate, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a unit that wrote no artifacts is a command to fix, not an agent fix round")
	}
	if got := findingDescriptions(t, outcome); !strings.Contains(got, `test unit "web"`) {
		t.Errorf("finding does not name the expanded unit, got:\n%s", got)
	}
}

// TestTestStep_UnreadableFileBesideGoodArtifactsDoesNotFailTheRun pins the
// posture toward an I/O fault in the coverage directory. An arbitrary test
// command owns that directory and can leave a file the guard cannot open;
// returning a Go error there reported the command's mess as a pipeline defect
// and killed a run whose tests really did cover the change.
func TestTestStep_UnreadableFileBesideGoodArtifactsDoesNotFailTheRun(t *testing.T) {
	skipUnlessPOSIXShell(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file, so the fault this test needs cannot be staged")
	}
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	good := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "services/api/main.go",
		Function: "Changed",
		Line:     2,
		Hits:     4,
		Tests:    5,
	})
	locked := `: > "$NO_MISTAKES_COVERAGE_DIR/locked.lcov"; chmod 000 "$NO_MISTAKES_COVERAGE_DIR/locked.lcov"`
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, good+"; "+locked)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("an unreadable file in the coverage directory failed the run: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("a run whose tests covered the change parked, findings: %s", outcome.Findings)
	}
}

// TestTestStep_UnreadableArtifactRoutesTheVacuityVerdictToTheMaintainer is the
// other half. Once the guard could not read something, an absent covered
// function is no longer evidence that the tests are missing; the coverage may
// be sitting in the file the reader skipped. Handing that to an agent fix
// round has it write tests to answer a reporting fault.
func TestTestStep_UnreadableArtifactRoutesTheVacuityVerdictToTheMaintainer(t *testing.T) {
	skipUnlessPOSIXShell(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file, so the fault this test needs cannot be staged")
	}
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	uncovered := stepstest.WriteCoverageArtifactsCommand(
		"SF:services/api/main.go\nFN:1,Other\nFNDA:3,Other\nFN:2,Changed\nFNDA:0,Changed\nend_of_record\n",
		`<testsuite tests="5" skipped="0"></testsuite>`,
	)
	locked := `: > "$NO_MISTAKES_COVERAGE_DIR/locked.lcov"; chmod 000 "$NO_MISTAKES_COVERAGE_DIR/locked.lcov"`
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, uncovered+"; "+locked)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("an unexercised change reported a passing gate, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("the guard could not read an artifact, so the missing coverage may be a reporting fault rather than a missing test")
	}
	if got := findingDescriptions(t, outcome); !strings.Contains(got, "locked.lcov") {
		t.Errorf("finding does not name the artifact the guard could not read, got:\n%s", got)
	}
}

// TestTestStep_DeletionOnlyChangeDoesNotParkForAMissingTest covers the file a
// change only removed lines from. There is nothing left in it to exercise, so
// demanding a covered function asks an agent to write a test for code the
// commit deleted.
func TestTestStep_DeletionOnlyChangeDoesNotParkForAMissingTest(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	// Both shapes of a deletion: a file emptied of every line, and a file
	// removed outright. git reports the second one's path too, so leaving it
	// out of the rule parks the change just the same.
	writeRepoFile(t, dir, "services/api/main.go", "")
	gitCmd(t, dir, "rm", "services/web/main.go")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "drop the entrypoints")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "services/other/thing.go",
		Function: "Untouched",
		Line:     1,
		Hits:     2,
		Tests:    5,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("a commit that only deleted code parked over coverage, findings: %s", outcome.Findings)
	}
}

// TestTestStep_ProfileDescribingAnotherProjectParksForTheMaintainer holds the
// guard's wrong-project branch at the step seam. A command pointed at other
// code exits zero and writes a perfectly valid profile, so nothing except this
// branch stops it certifying a change it never touched; the park must stay a
// maintainer decision, because an agent fix round would answer a misconfigured
// command by writing tests.
func TestTestStep_ProfileDescribingAnotherProjectParksForTheMaintainer(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "config", "commit.gpgsign", "false")
	gitCmd(t, dir, "checkout", "-b", "main")
	// The tracked pair is what makes "go" a source extension of this
	// repository, which is how a changed .go file no profile mentions reads as
	// unexplained rather than as an uninstrumentable file.
	writeRepoFile(t, dir, "internal/api/order.go", "package api\n")
	writeRepoFile(t, dir, "internal/api/order_test.go", "package api\n")
	writeRepoFile(t, dir, "web/app.ts", "export const a = 1;\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "-b", "feature")
	writeRepoFile(t, dir, "internal/api/order.go", "package api\n\nfunc Place() {}\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "place an order")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "web/app.ts",
		Function: "render",
		Line:     1,
		Hits:     7,
		Tests:    9,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("a profile describing another project certified the change, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a command pointed at other code is a configuration problem, so an agent fix round must not answer it")
	}
	got := findingDescriptions(t, outcome)
	if !strings.Contains(got, "describe none of the source files") {
		t.Errorf("finding does not use the wrong-project wording, got:\n%s", got)
	}
	if !strings.Contains(got, "internal/api/order.go") {
		t.Errorf("finding does not name the unexplained source file, got:\n%s", got)
	}
}

// autoFixableItems is what the executor's auto-fix branch actually sees: the
// findings that resolve to auto-fix, not the outcome's AutoFixable flag. The
// two parks are only different postures if this set differs between them.
func autoFixableItems(t *testing.T, outcome *pipeline.StepOutcome) []types.Finding {
	t.Helper()
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings %q: %v", outcome.Findings, err)
	}
	return types.AutoFixableFindings(findings, "").Items
}

// TestTestStep_VacuousGreenParkIsActuallyAutoFixable discriminates the two
// park postures where it counts. Both set NeedsApproval, so the only thing
// separating "an agent writes the missing test" from "a maintainer fixes the
// command" is whether the findings resolve to auto-fix. Leaving the action off
// made them runtime-identical and the auto-fixable park never got its fix
// round.
func TestTestStep_VacuousGreenParkIsActuallyAutoFixable(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	noTestsRan := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "services/api/main.go",
		Function: "Changed",
		Line:     2,
		Hits:     0,
		Tests:    0,
	})
	vacuous, err := (&TestStep{}).Execute(coverageStepContext(t, nil, dir, baseSHA, headSHA, noTestsRan))
	if err != nil {
		t.Fatal(err)
	}
	if !vacuous.AutoFixable {
		t.Fatalf("zero executed tests did not park auto-fixable, findings: %s", vacuous.Findings)
	}
	if len(autoFixableItems(t, vacuous)) == 0 {
		t.Errorf("the auto-fixable park carries no finding an agent fix round would act on: %s", vacuous.Findings)
	}

	maintainer, err := (&TestStep{}).Execute(coverageStepContext(t, nil, dir, baseSHA, headSHA, "exit 0"))
	if err != nil {
		t.Fatal(err)
	}
	if maintainer.AutoFixable {
		t.Fatalf("a command that wrote no artifacts parked auto-fixable, findings: %s", maintainer.Findings)
	}
	if got := autoFixableItems(t, maintainer); len(got) != 0 {
		t.Errorf("the maintainer park offered an agent fix round: %+v", got)
	}
}

// TestTestStep_ProductionSourceUnderAFixturesDirectoryStaysCoverable holds the
// test-directory exemption to corroborating evidence. internal/fixtures is a
// perfectly ordinary place for production Go code, and exempting it on the
// directory name alone greened a change whose tests never reached it.
func TestTestStep_ProductionSourceUnderAFixturesDirectoryStaysCoverable(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newSourceRepo(t, map[string]string{
		"internal/fixtures/loader.go":      "package fixtures\n",
		"internal/fixtures/loader_test.go": "package fixtures\n",
		"internal/fixtures/golden.json":    "{}\n",
	})
	headSHA := changeUnitFile(t, dir, "internal/fixtures/loader.go")

	// The profile describes .go files and never names loader.go, so the change
	// is source the tests that ran did not reach.
	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "internal/other/thing.go",
		Function: "Untouched",
		Line:     1,
		Hits:     3,
		Tests:    12,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("production source under a fixtures directory certified itself, findings: %s", outcome.Findings)
	}
	if got := findingDescriptions(t, outcome); !strings.Contains(got, "internal/fixtures/loader.go") {
		t.Errorf("finding does not name the uncovered source file, got:\n%s", got)
	}
}

// TestTestStep_GoldenFileUnderATestdataDirectoryStaysExempt is the other side
// of the same rule. A .json golden file is not code this repository writes, so
// the directory segment is allowed to decide and the change must not park.
func TestTestStep_GoldenFileUnderATestdataDirectoryStaysExempt(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newSourceRepo(t, map[string]string{
		"internal/api/order.go":           "package api\n",
		"internal/api/order_test.go":      "package api\n",
		"internal/api/testdata/case.json": "{}\n",
	})
	headSHA := changeUnitFile(t, dir, "internal/api/testdata/case.json")

	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "internal/api/order.go",
		Function: "Place",
		Line:     1,
		Hits:     3,
		Tests:    12,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("a golden-file change parked over coverage, findings: %s", outcome.Findings)
	}
}

// TestTestStep_CSharpChangeReachesTheWrongProjectPark proves the source
// extension derivation covers C#, which the intent names as a target stack.
// Without a C# test-file convention, sourceExts never contained cs, so a .cs
// change measured by a TypeScript-only profile took the documentation
// exemption and greened.
func TestTestStep_CSharpChangeReachesTheWrongProjectPark(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newSourceRepo(t, map[string]string{
		"Services/OrderService.cs":   "namespace S;\n",
		"Tests/OrderServiceTests.cs": "namespace T;\n",
		"web/src/app.ts":             "export const a = 1;\n",
		"web/src/app.spec.ts":        "describe();\n",
	})
	headSHA := changeUnitFile(t, dir, "Services/OrderService.cs")

	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "web/src/app.ts",
		Function: "render",
		Line:     1,
		Hits:     7,
		Tests:    9,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("a C# change measured by a TypeScript-only profile certified itself, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a command pointed at another project is a configuration problem, not an agent fix round")
	}
	got := findingDescriptions(t, outcome)
	if !strings.Contains(got, "describe none of the source files") {
		t.Errorf("finding does not use the wrong-project wording, got:\n%s", got)
	}
	if !strings.Contains(got, "Services/OrderService.cs") {
		t.Errorf("finding does not name the unexplained C# file, got:\n%s", got)
	}
}

// TestTestStep_ProfileNamingNoFileParksForTheMaintainer covers the malformed
// profile that parses cleanly and describes nothing. A bare SF: record used to
// put "" into the file list, which passed the "profile describing nothing"
// maintainer park and then matched every extensionless changed file.
func TestTestStep_ProfileNamingNoFileParksForTheMaintainer(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newSourceRepo(t, map[string]string{
		"Makefile":                   "all:\n\techo hi\n",
		"internal/api/order.go":      "package api\n",
		"internal/api/order_test.go": "package api\n",
	})
	headSHA := changeUnitFile(t, dir, "Makefile")

	command := stepstest.WriteCoverageArtifactsCommand(
		"SF:\nend_of_record\n",
		`<testsuite tests="3" skipped="0"></testsuite>`,
	)
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("a profile naming no file certified the change, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a profile that describes nothing is an artifact to repair, not a missing test")
	}
	if got := findingDescriptions(t, outcome); !strings.Contains(got, "name no source file") {
		t.Errorf("finding does not use the describes-nothing wording, got:\n%s", got)
	}
}

// TestTestStep_FixModeCoverageOfTheUncommittedRepairClearsTheGuard closes the
// loop the guard exists to drive: it parks auto-fixable, an agent writes the
// test, and the next attempt clears. A fix round's repair is UNCOMMITTED, so
// the guard only sees its lines if changedLineRanges diffs the working tree
// against the base rather than the committed head.
func TestTestStep_FixModeCoverageOfTheUncommittedRepairClearsTheGuard(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	// The repair an agent fix round would leave behind: a further edit to the
	// changed file, not yet committed.
	appendToFile(t, filepath.Join(dir, "services", "api", "main.go"), "// repaired\n")

	covered := stepstest.WriteCoverageArtifactsCommand(
		"SF:services/api/main.go\nFN:1,Other\nFNDA:0,Other\nFN:3,Repaired\nFNDA:2,Repaired\nend_of_record\n",
		`<testsuite tests="6" skipped="0"></testsuite>`,
	)
	sctx := coverageStepContext(t, fixRoundAgent(), dir, baseSHA, headSHA, covered)
	sctx.Fixing = true

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	// A nil outcome is a failure here, not a pass: the assertion has to see the
	// step reach a verdict, or every way of returning early reads as green.
	if outcome == nil {
		t.Fatal("the step returned no outcome, so it never reached the guard")
	}
	if outcome.NeedsApproval {
		t.Fatalf("the repair's own coverage did not clear the guard, findings: %s", outcome.Findings)
	}
}

// TestTestStep_FixModeUncoveredRepairStillParks is the discriminating half:
// the same fix-mode run whose profile reaches nothing the repair wrote must
// still park, so the test above cannot pass by the guard being inert in fix
// mode.
func TestTestStep_FixModeUncoveredRepairStillParks(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")
	appendToFile(t, filepath.Join(dir, "services", "api", "main.go"), "// repaired\n")

	uncovered := stepstest.WriteCoverageArtifactsCommand(
		"SF:services/api/main.go\nFN:1,Other\nFNDA:4,Other\nFN:2,Untouched\nFNDA:0,Untouched\nend_of_record\n",
		`<testsuite tests="6" skipped="0"></testsuite>`,
	)
	sctx := coverageStepContext(t, fixRoundAgent(), dir, baseSHA, headSHA, uncovered)
	sctx.Fixing = true

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatal("a fix-mode attempt whose coverage reached none of the repair reported a passing gate")
	}
	if !outcome.AutoFixable {
		t.Errorf("an unexercised repair is a missing test, findings: %s", outcome.Findings)
	}
}

// uncoveredAPIChange builds the run every ignore_patterns case below starts
// from: the change touches services/api/main.go, the unit command runs real
// tests and writes both artifacts, and the profile records the changed file
// with no executed function. Left alone that parks auto-fixable, so anything
// that greens it did so by exempting the changed file.
func uncoveredAPIChange(t *testing.T) *pipeline.StepContext {
	t.Helper()
	skipUnlessPOSIXShell(t)
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")
	command := stepstest.WriteCoverageArtifactsCommand(
		"SF:services/api/main.go\nFN:1,Changed\nFNDA:0,Changed\nend_of_record\n",
		`<testsuite tests="3" skipped="0"></testsuite>`,
	)
	return coverageStepContext(t, nil, dir, baseSHA, headSHA, command)
}

// TestTestStep_GuardWithoutAnIgnoreEntryParksTheUncoveredChange is the control
// the two ignore_patterns cases below are read against.
func TestTestStep_GuardWithoutAnIgnoreEntryParksTheUncoveredChange(t *testing.T) {
	t.Parallel()
	sctx := uncoveredAPIChange(t)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("an uncovered change did not park, outcome: %+v", outcome)
	}
}

// TestTestStep_TrustedIgnorePatternExemptsTheChangedFile is the maintainer's
// half. An ignore_patterns entry on the default branch says this path is not
// the repository's concern, so the guard does not demand a covered function
// for it.
func TestTestStep_TrustedIgnorePatternExemptsTheChangedFile(t *testing.T) {
	t.Parallel()
	sctx := uncoveredAPIChange(t)
	sctx.Config.TrustedIgnorePatterns = []string{"services/api/**"}

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil {
		t.Fatal("the step returned no outcome, so it never reached the guard")
	}
	if outcome.NeedsApproval {
		t.Fatalf("a trusted ignore_patterns entry did not exempt the changed file, findings: %s", findingDescriptions(t, outcome))
	}
}

// TestTestStep_PushedIgnorePatternCannotExemptTheChangedFile is the boundary.
// ignore_patterns on the pushed branch narrows what the run works on, but
// switching off the gate that judges the change is the contributor exempting
// their own code, so the guard reads only the trusted copy.
func TestTestStep_PushedIgnorePatternCannotExemptTheChangedFile(t *testing.T) {
	t.Parallel()
	sctx := uncoveredAPIChange(t)
	sctx.Config.IgnorePatterns = []string{"services/api/**"}

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("a pushed-branch ignore_patterns entry switched the guard off, outcome: %+v", outcome)
	}
	if !outcome.AutoFixable {
		t.Errorf("an uncovered change is a missing test, findings: %s", findingDescriptions(t, outcome))
	}
}

// The two tests below pin the fix round's opening instruction. A vacuous-green
// park and a failing test need opposite work: there is no failing case to
// reproduce when the commands exited zero, so "reproduce the specific failure"
// sends the agent hunting for something that does not exist and it reports a
// re-run instead of the test the change is missing.

func fixRoundPrompt(t *testing.T, previousFindings string) string {
	t.Helper()
	skipUnlessPOSIXShell(t)
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"summary":"fixed"}`)}, nil
	}}
	sctx := coverageStepContext(t, ag, dir, baseSHA, headSHA, "exit 0")
	sctx.Fixing = true
	sctx.PreviousFindings = previousFindings

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("the fix round invoked no agent")
	}
	return ag.calls[0].Prompt
}

func findingsJSONWithID(t *testing.T, id, description string) string {
	t.Helper()
	raw, err := json.Marshal(types.Findings{Items: []types.Finding{{
		ID:          id,
		Severity:    types.FindingSeverityError,
		Action:      types.ActionAutoFix,
		Description: description,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestTestStep_VacuousGreenFixRoundAsksForTheMissingTest(t *testing.T) {
	t.Parallel()
	prompt := fixRoundPrompt(t, findingsJSONWithID(t, vacuousGreenFindingID,
		"no test exercised a changed function; the units that ran (repository) recorded no executed function in the changed lines of services/api/main.go"))

	for _, want := range []string{
		"Write the missing test for this change",
		"There is no failing case to reproduce",
		"Write a new test (or extend an existing one) that executes the changed code",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("vacuous-green fix prompt missing %q, got:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Reproduce the specific failing case first") {
		t.Errorf("vacuous-green fix prompt still asks for a failure to reproduce, got:\n%s", prompt)
	}
}

func TestTestStep_FailingTestFixRoundStillAsksForAReproduction(t *testing.T) {
	t.Parallel()
	prompt := fixRoundPrompt(t, findingsJSONWithID(t, "", `test unit "repository" failed: FAIL services/api`))

	for _, want := range []string{
		"Fix the failing tests in this repository. Reproduce the specific failure",
		"Reproduce the specific failing case first",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("failing-test fix prompt missing %q, got:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Write the missing test for this change") {
		t.Errorf("a genuine test failure got the write-a-missing-test prompt, got:\n%s", prompt)
	}
}

// TestTestStep_VacuousGreenParkCarriesItsIDIntoTheNextRound joins the two ends
// of the branch above. The prompt is selected on the finding's ID, so the park
// has to set it and it has to survive the auto-fixable filter the executor
// runs before it hands the findings to the fix round.
func TestTestStep_VacuousGreenParkCarriesItsIDIntoTheNextRound(t *testing.T) {
	t.Parallel()
	sctx := uncoveredAPIChange(t)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.AutoFixable {
		t.Fatalf("an uncovered change did not park auto-fixable, outcome: %+v", outcome)
	}
	parked, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	carried, err := types.MarshalFindingsJSON(types.AutoFixableFindings(parked, types.FindingSeverityWarning))
	if err != nil {
		t.Fatal(err)
	}
	if !previousFindingsIncludeVacuousGreen(carried) {
		t.Fatalf("the vacuous-green marker did not survive into the fix round's findings: %s", carried)
	}
}

// TestTestStep_CoverageOfATrustedIgnoredPathCannotCertifyAGuardedOne is the
// one-path-set rule. changedLineRanges parses the whole diff rather than a
// pathspec-limited one, so the ranges map still carries the paths the trusted
// ignore filter excluded; handing that map to the coverage check let an
// executed function in an ignored file certify a guarded file nothing tested.
func TestTestStep_CoverageOfATrustedIgnoredPathCannotCertifyAGuardedOne(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	changeUnitFile(t, dir, "services/web/main.go")
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	command := stepstest.WriteCoverageArtifactsCommand(
		"SF:services/api/main.go\nFN:1,ApiChanged\nFNDA:0,ApiChanged\nend_of_record\n"+
			"SF:services/web/main.go\nFN:1,WebChanged\nFNDA:7,WebChanged\nend_of_record\n",
		`<testsuite tests="5" skipped="0"></testsuite>`,
	)
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)
	sctx.Config.TrustedIgnorePatterns = []string{"services/web/**"}

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("an ignored file's coverage certified an untested guarded file, outcome: %+v", outcome)
	}
	if !outcome.AutoFixable {
		t.Errorf("an uncovered guarded change is a missing test, findings: %s", findingDescriptions(t, outcome))
	}
	if got := findingDescriptions(t, outcome); !strings.Contains(got, "services/api/main.go") {
		t.Errorf("the finding does not name the uncovered guarded file, got:\n%s", got)
	}
}

// TestTestStep_DeletionOnlyPathDoesNotBlockACoveredChange is the same rule
// read from the other side. A deletion-only path is recorded with an empty
// range slice, which still counted as a source when two changed paths relaxed-
// matched one short profile key, so the key was dropped as ambiguous and a
// genuinely covered change parked auto-fixable.
func TestTestStep_DeletionOnlyPathDoesNotBlockACoveredChange(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	if err := os.Remove(filepath.Join(dir, "services", "web", "main.go")); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "delete the web entrypoint")
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	// The profile names the file the way a runner reporting relative to a
	// source root does, so both changed paths are candidates for the one key.
	command := stepstest.WriteCoverageArtifactsCommand(
		"SF:main.go\nFN:1,Changed\nFNDA:4,Changed\nend_of_record\n",
		`<testsuite tests="5" skipped="0"></testsuite>`,
	)
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil {
		t.Fatal("the step returned no outcome, so it never reached the guard")
	}
	if outcome.NeedsApproval {
		t.Fatalf("a deleted path made a covered change look ambiguous, findings: %s", findingDescriptions(t, outcome))
	}
}

// TestTestStep_ElixirChangeReachesTheWrongProjectPark covers a stack whose test
// files carry a different extension from the code they cover. Pairing a
// convention test file with a non-test file of the SAME extension never
// credited ex, because Elixir writes tests as .exs, so an Elixir change
// measured by a TypeScript-only profile took the documentation exemption and
// greened.
func TestTestStep_ElixirChangeReachesTheWrongProjectPark(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newSourceRepo(t, map[string]string{
		"lib/orders.ex":        "defmodule Orders do\nend\n",
		"test/orders_test.exs": "defmodule OrdersTest do\nend\n",
		"web/src/app.ts":       "export const a = 1;\n",
		"web/src/app.spec.ts":  "describe();\n",
	})
	headSHA := changeUnitFile(t, dir, "lib/orders.ex")

	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "web/src/app.ts",
		Function: "render",
		Line:     1,
		Hits:     7,
		Tests:    9,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("an Elixir change measured by a TypeScript-only profile certified itself, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a command pointed at another project is a configuration problem, not an agent fix round")
	}
	got := findingDescriptions(t, outcome)
	if !strings.Contains(got, "describe none of the source files") {
		t.Errorf("finding does not use the wrong-project wording, got:\n%s", got)
	}
	if !strings.Contains(got, "lib/orders.ex") {
		t.Errorf("finding does not name the unexplained Elixir file, got:\n%s", got)
	}
}

// TestTestStep_RustChangeReachesTheWrongProjectPark covers the other half of
// the same hole. Idiomatic Rust names neither its integration tests
// (tests/orders.rs) nor its unit tests (a #[cfg(test)] module inside the source
// file) by any convention, so nothing marked rs as a language this repository
// writes code in and a Rust change greened under a TypeScript-only profile.
func TestTestStep_RustChangeReachesTheWrongProjectPark(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newSourceRepo(t, map[string]string{
		"src/lib.rs":          "pub fn place() {}\n",
		"src/orders.rs":       "pub fn cancel() {}\n",
		"tests/orders.rs":     "#[test]\nfn cancels() {}\n",
		"web/src/app.ts":      "export const a = 1;\n",
		"web/src/app.spec.ts": "describe();\n",
	})
	headSHA := changeUnitFile(t, dir, "src/orders.rs")

	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "web/src/app.ts",
		Function: "render",
		Line:     1,
		Hits:     7,
		Tests:    9,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("a Rust change measured by a TypeScript-only profile certified itself, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a command pointed at another project is a configuration problem, not an agent fix round")
	}
	got := findingDescriptions(t, outcome)
	if !strings.Contains(got, "describe none of the source files") {
		t.Errorf("finding does not use the wrong-project wording, got:\n%s", got)
	}
	if !strings.Contains(got, "src/orders.rs") {
		t.Errorf("finding does not name the unexplained Rust file, got:\n%s", got)
	}
}

// TestTestStep_MarkdownOnlyChangeStillGreens is the boundary the two tests
// above must not cross. A repository that writes prose beside its code has no
// business parking over a README, so the derivation may never credit an
// extension the repository only documents itself in.
func TestTestStep_MarkdownOnlyChangeStillGreens(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newSourceRepo(t, map[string]string{
		"README.md":            "# readme\n",
		"docs/guide.md":        "# guide\n",
		"lib/orders.ex":        "defmodule Orders do\nend\n",
		"test/orders_test.exs": "defmodule OrdersTest do\nend\n",
	})
	headSHA := changeUnitFile(t, dir, "README.md")

	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "lib/orders.ex",
		Function: "place",
		Line:     1,
		Hits:     7,
		Tests:    9,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("a documentation-only change parked over coverage, findings: %s", outcome.Findings)
	}
}

// TestTestStep_UnexaminedArtifactsTakeTheMaintainerPark proves the scan budget
// changes which park a vacuity verdict reaches. The reader stopped early, so
// the coverage it is about to call missing may be sitting in a file it never
// opened, and charging an agent fix round with writing a test for that is the
// wrong repair.
func TestTestStep_UnexaminedArtifactsTakeTheMaintainerPark(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newUnitRepo(t)
	headSHA := changeUnitFile(t, dir, "services/api/main.go")

	// One good pair, then enough recognised profiles to exhaust the budget, so
	// the walk reports a limit against artifacts that all parse.
	command := stepstest.WriteCoverageArtifactsCommand(
		"SF:services/api/other.go\nFN:2,Other\nFNDA:1,Other\nend_of_record\n",
		`<testsuite tests="3" skipped="0"></testsuite>`,
	) + `; i=0; while [ $i -lt ` + strconv.Itoa(maxCoverageFilesScanned+8) + ` ]; do printf 'SF:services/api/other.go\nend_of_record\n' > "$NO_MISTAKES_COVERAGE_DIR/zextra$i.info"; i=$((i+1)); done`
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("an uncovered change certified itself, findings: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("a directory the reader never finished is a reporting problem, so the park must not be auto-fixable")
	}
	if got := findingDescriptions(t, outcome); !strings.Contains(got, "stopped reading after") {
		t.Errorf("finding does not say the reader stopped early, got:\n%s", got)
	}
}

// TestTestStep_ZeroHitFunctionAboveAnExecutedOneParks pins the bound on the
// preamble credit. A brand-new function declared above an executed one is
// untested code sitting in the file the profile names, so crediting it to the
// executed function below greens exactly the vacuity this guard refuses.
func TestTestStep_ZeroHitFunctionAboveAnExecutedOneParks(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	base := "package api\n\n// order helpers\n\n// notes\n\n// more\n\n\nfunc Total() int {\n\treturn 1\n}\n"
	dir, baseSHA := newSourceRepo(t, map[string]string{
		"api/order.go":      base,
		"api/order_test.go": "package api\n",
	})
	added := "func Discount() int {\n\treturn 0\n}\n\n// pad\n\n// pad\n\n// pad\n\n// pad\n\n"
	writeRepoFile(t, dir, "api/order.go", strings.Replace(base, "func Total() int {", added+"func Total() int {", 1))
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add Discount")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	command := stepstest.WriteCoverageArtifactsCommand(
		"SF:api/order.go\nFN:10,Discount\nFNDA:0,Discount\nFN:22,Total\nFNDA:3,Total\nend_of_record\n",
		`<testsuite tests="2" skipped="0"></testsuite>`,
	)
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("a new untested function certified itself through the preamble credit, findings: %s", outcome.Findings)
	}
	if !outcome.AutoFixable {
		t.Error("a missing test is what an agent fix round writes, so the park must be auto-fixable")
	}
}

// TestTestStep_TestDirectoryReadmeStillGreensAMarkdownChange guards the layout
// pairing's blast radius. A tracked tests/README.md names the root README.md,
// and reading that as evidence the repository writes code in markdown parks
// every documentation-only change.
func TestTestStep_TestDirectoryReadmeStillGreensAMarkdownChange(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newSourceRepo(t, map[string]string{
		"README.md":            "# readme\n",
		"tests/README.md":      "# how to run the tests\n",
		"lib/orders.ex":        "defmodule Orders do\nend\n",
		"test/orders_test.exs": "defmodule OrdersTest do\nend\n",
	})
	headSHA := changeUnitFile(t, dir, "README.md")

	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "lib/orders.ex",
		Function: "place",
		Line:     1,
		Hits:     7,
		Tests:    9,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("a README edit parked because a test directory carries a README too, findings: %s", outcome.Findings)
	}
}

// TestTestStep_TestDirectoryConfigStillGreensAJSONChange is the same boundary
// for a configuration file. test/tsconfig.json mirrors the root tsconfig.json
// by name, which says nothing about json being a language this repository
// writes code in.
func TestTestStep_TestDirectoryConfigStillGreensAJSONChange(t *testing.T) {
	skipUnlessPOSIXShell(t)
	t.Parallel()
	dir, baseSHA := newSourceRepo(t, map[string]string{
		"tsconfig.json":       "{}\n",
		"test/tsconfig.json":  "{}\n",
		"web/src/app.ts":      "export const a = 1;\n",
		"web/src/app.spec.ts": "describe();\n",
	})
	headSHA := changeUnitFile(t, dir, "tsconfig.json")

	command := stepstest.CoverageCommand(stepstest.CoverageFixture{
		File:     "web/src/app.ts",
		Function: "render",
		Line:     1,
		Hits:     7,
		Tests:    9,
	})
	sctx := coverageStepContext(t, nil, dir, baseSHA, headSHA, command)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("a json edit parked because a test directory carries a tsconfig.json too, findings: %s", outcome.Findings)
	}
}
