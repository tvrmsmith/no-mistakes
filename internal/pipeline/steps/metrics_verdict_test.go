package steps

import (
	"strings"
	"testing"
)

// metricsFixtureReport is the compact three-function report every scenario that
// does not need a bespoke shape evaluates.
const metricsFixtureReport = `{"metric":"crap","functions":[{"file":"internal/pipeline/steps/metrics.go","function":"execute","line":42,"score":42.5,"complexity":7,"coverage":0},{"file":"internal/config/config.go","function":"Merge","line":3000,"score":12,"complexity":4,"coverage":0.9},{"file":"vendor/lib/gen.go","function":"Big","line":1,"score":90,"complexity":30,"coverage":0}],"summary":"3 functions measured"}`

func TestEvaluateMetricsOutput_ScoreAboveThresholdBreaches(t *testing.T) {
	verdict := evaluateMetricsOutput(metricsFixtureReport, 0, 30, nil)

	if !verdict.Breached {
		t.Error("want Breached true")
	}
	if !verdict.FromJSON {
		t.Error("want FromJSON true")
	}
	if verdict.Measured != 3 {
		t.Errorf("Measured = %d, want 3", verdict.Measured)
	}
	if verdict.Exempted != 0 {
		t.Errorf("Exempted = %d, want 0", verdict.Exempted)
	}
	if verdict.Metric != "crap" {
		t.Errorf("Metric = %q, want %q", verdict.Metric, "crap")
	}
	if verdict.Summary != "3 functions measured" {
		t.Errorf("Summary = %q, want %q", verdict.Summary, "3 functions measured")
	}
	if verdict.Threshold != 30 {
		t.Errorf("Threshold = %v, want 30", verdict.Threshold)
	}
	if len(verdict.Breaches) != 2 {
		t.Fatalf("len(Breaches) = %d, want 2", len(verdict.Breaches))
	}
	if verdict.Breaches[0].Function != "Big" {
		t.Errorf("Breaches[0].Function = %q, want %q", verdict.Breaches[0].Function, "Big")
	}
	if verdict.Breaches[1].Function != "execute" {
		t.Errorf("Breaches[1].Function = %q, want %q", verdict.Breaches[1].Function, "execute")
	}
}

func TestEvaluateMetricsOutput_EveryScoreUnderThresholdPasses(t *testing.T) {
	verdict := evaluateMetricsOutput(metricsFixtureReport, 0, 100, nil)

	if verdict.Breached {
		t.Error("want Breached false")
	}
	if len(verdict.Breaches) != 0 {
		t.Errorf("len(Breaches) = %d, want 0", len(verdict.Breaches))
	}
	if verdict.Measured != 3 {
		t.Errorf("Measured = %d, want 3", verdict.Measured)
	}
	if !verdict.FromJSON {
		t.Error("want FromJSON true")
	}
}

func TestEvaluateMetricsOutput_ExemptPathsClearTheBreach(t *testing.T) {
	verdict := evaluateMetricsOutput(metricsFixtureReport, 0, 30, []string{"vendor/**", "internal/pipeline/**"})

	if verdict.Breached {
		t.Error("want Breached false")
	}
	if verdict.Exempted != 2 {
		t.Errorf("Exempted = %d, want 2", verdict.Exempted)
	}
	if verdict.Measured != 3 {
		t.Errorf("Measured = %d, want 3", verdict.Measured)
	}
}

// metricsOneFunctionReport builds a report carrying a single measured function.
func metricsOneFunctionReport(file string, score string) string {
	return `{"metric":"crap","functions":[{"file":"` + file + `","function":"F","line":1,"score":` + score + `}]}`
}

func TestEvaluateMetricsOutput_ThresholdComparisonIsStrictlyGreater(t *testing.T) {
	if evaluateMetricsOutput(metricsOneFunctionReport("a.go", "30"), 0, 30, nil).Breached {
		t.Error("score 30 at threshold 30: want Breached false")
	}
	if !evaluateMetricsOutput(metricsOneFunctionReport("a.go", "30.5"), 0, 30, nil).Breached {
		t.Error("score 30.5 at threshold 30: want Breached true")
	}
}

func TestEvaluateMetricsOutput_ZeroThresholdBreachesAnyPositiveScore(t *testing.T) {
	if evaluateMetricsOutput(metricsOneFunctionReport("a.go", "0"), 0, 0, nil).Breached {
		t.Error("score 0 at threshold 0: want Breached false")
	}
	if !evaluateMetricsOutput(metricsOneFunctionReport("a.go", "0.1"), 0, 0, nil).Breached {
		t.Error("score 0.1 at threshold 0: want Breached true")
	}
}

func TestEvaluateMetricsOutput_EmptyFunctionsArrayPasses(t *testing.T) {
	verdict := evaluateMetricsOutput(`{"metric":"crap","functions":[]}`, 0, 30, nil)

	if !verdict.FromJSON {
		t.Error("want FromJSON true")
	}
	if verdict.Breached {
		t.Error("want Breached false")
	}
	if verdict.Measured != 0 {
		t.Errorf("Measured = %d, want 0", verdict.Measured)
	}
}

func TestEvaluateMetricsOutput_NullFunctionsIsAReport(t *testing.T) {
	verdict := evaluateMetricsOutput(`{"metric":"crap","functions":null}`, 0, 30, nil)

	if !verdict.FromJSON {
		t.Error("want FromJSON true")
	}
	if verdict.Measured != 0 {
		t.Errorf("Measured = %d, want 0", verdict.Measured)
	}
	if verdict.Breached {
		t.Error("want Breached false")
	}
}

func TestEvaluateMetricsOutput_ObjectWithoutFunctionsIsNotAReport(t *testing.T) {
	verdict := evaluateMetricsOutput(`{"metric":"crap"}`, 0, 30, nil)

	if verdict.FromJSON {
		t.Error("want FromJSON false")
	}
	if verdict.Breached {
		t.Error("want Breached false")
	}
	if verdict.Metric != "" {
		t.Errorf("Metric = %q, want empty", verdict.Metric)
	}
}

func TestEvaluateMetricsOutput_UnparseableOutputFallsBackToTheExitCode(t *testing.T) {
	clean := evaluateMetricsOutput("crap: command not found\n", 0, 30, nil)
	if clean.FromJSON {
		t.Error("exit 0: want FromJSON false")
	}
	if clean.Breached {
		t.Error("exit 0: want Breached false")
	}

	failed := evaluateMetricsOutput("crap: command not found\n", 3, 30, nil)
	if failed.FromJSON {
		t.Error("exit 3: want FromJSON false")
	}
	if !failed.Breached {
		t.Error("exit 3: want Breached true")
	}
	if failed.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", failed.ExitCode)
	}
}

func TestEvaluateMetricsOutput_NonzeroExitBlocksEvenWhenTheReportIsClean(t *testing.T) {
	verdict := evaluateMetricsOutput(metricsFixtureReport, 1, 100, nil)

	if !verdict.Breached {
		t.Error("want Breached true")
	}
	if !verdict.FromJSON {
		t.Error("want FromJSON true")
	}
	if len(verdict.Breaches) != 0 {
		t.Errorf("len(Breaches) = %d, want 0", len(verdict.Breaches))
	}
	if verdict.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", verdict.ExitCode)
	}
}

func TestEvaluateMetricsOutput_ReadsAPrettyPrintedReportAfterLogLines(t *testing.T) {
	stdout := `running crap analysis...
scanned 3 files
{
  "metric": "crap",
  "functions": [
    {"file": "a.go", "function": "F", "line": 1, "score": 55}
  ]
}
`
	verdict := evaluateMetricsOutput(stdout, 0, 30, nil)

	if !verdict.FromJSON {
		t.Fatal("want FromJSON true")
	}
	if !verdict.Breached {
		t.Error("want Breached true")
	}
	if len(verdict.Breaches) != 1 || verdict.Breaches[0].Function != "F" {
		t.Errorf("Breaches = %+v, want the single function F", verdict.Breaches)
	}
}

func TestEvaluateMetricsOutput_ATrailingLogObjectDoesNotHideTheReport(t *testing.T) {
	stdout := `{"metric":"crap","functions":[{"file":"a.go","function":"F","line":1,"score":55}]}
{"level":"info","msg":"done"}
`
	verdict := evaluateMetricsOutput(stdout, 0, 30, nil)

	if !verdict.FromJSON {
		t.Fatal("want FromJSON true")
	}
	if len(verdict.Breaches) != 1 || verdict.Breaches[0].Function != "F" {
		t.Errorf("Breaches = %+v, want the single function F", verdict.Breaches)
	}
}

func TestEvaluateMetricsOutput_ABraceInsideAStringDoesNotConfuseTheScan(t *testing.T) {
	stdout := `noise {not json
{"metric":"crap","summary":"a } brace","functions":[{"file":"a.go","function":"F","line":1,"score":55}]}
`
	verdict := evaluateMetricsOutput(stdout, 0, 30, nil)

	if !verdict.FromJSON {
		t.Fatal("want FromJSON true")
	}
	if verdict.Summary != "a } brace" {
		t.Errorf("Summary = %q, want %q", verdict.Summary, "a } brace")
	}
	if len(verdict.Breaches) != 1 || verdict.Breaches[0].Function != "F" {
		t.Errorf("Breaches = %+v, want the single function F", verdict.Breaches)
	}
}

// An escaped quote inside a string is what makes the scan's escape handling
// load bearing. Read without it, the \" closes the summary early and the { that
// follows raises the depth, so the object never balances and the whole report is
// lost behind the log line in front of it.
func TestEvaluateMetricsOutput_AnEscapedQuoteInsideAStringDoesNotEndIt(t *testing.T) {
	stdout := `noise {not json
{"metric":"crap","summary":"it printed \"{\" once","functions":[{"file":"a.go","function":"F","line":1,"score":55}]}
`
	verdict := evaluateMetricsOutput(stdout, 0, 30, nil)

	if !verdict.FromJSON {
		t.Fatal("want FromJSON true")
	}
	if verdict.Summary != `it printed "{" once` {
		t.Errorf("Summary = %q, want %q", verdict.Summary, `it printed "{" once`)
	}
	if len(verdict.Breaches) != 1 || verdict.Breaches[0].Function != "F" {
		t.Errorf("Breaches = %+v, want the single function F", verdict.Breaches)
	}
}

func TestEvaluateMetricsOutput_ExemptMatchNormalisesThePath(t *testing.T) {
	for _, file := range []string{"./vendor/lib/gen.go", `vendor\lib\gen.go`} {
		verdict := evaluateMetricsOutput(metricsOneFunctionReport(metricsJSONEscape(file), "90"), 0, 30, []string{"vendor/**"})

		if verdict.Exempted != 1 {
			t.Errorf("%s: Exempted = %d, want 1", file, verdict.Exempted)
		}
		if verdict.Breached {
			t.Errorf("%s: want Breached false", file)
		}
	}
}

// metricsJSONEscape renders a path safe to paste into the report fixture, so a Windows
// separator reaches the decoder as a literal backslash.
func metricsJSONEscape(s string) string {
	return strings.ReplaceAll(s, `\`, `\\`)
}

func TestEvaluateMetricsOutput_BreachesSortTieBreaksOnFileThenLine(t *testing.T) {
	stdout := `{"metric":"crap","functions":[` +
		`{"file":"b.go","function":"B1","line":1,"score":55},` +
		`{"file":"a.go","function":"A9","line":9,"score":55},` +
		`{"file":"a.go","function":"A2","line":2,"score":55}]}`

	verdict := evaluateMetricsOutput(stdout, 0, 30, nil)

	want := []struct {
		file string
		line int
	}{{"a.go", 2}, {"a.go", 9}, {"b.go", 1}}
	if len(verdict.Breaches) != len(want) {
		t.Fatalf("len(Breaches) = %d, want %d", len(verdict.Breaches), len(want))
	}
	for i, w := range want {
		if verdict.Breaches[i].File != w.file || verdict.Breaches[i].Line != w.line {
			t.Errorf("Breaches[%d] = (%s, %d), want (%s, %d)", i, verdict.Breaches[i].File, verdict.Breaches[i].Line, w.file, w.line)
		}
	}
}

func TestEvaluateMetricsOutput_EmptyOutputWithAZeroExitPasses(t *testing.T) {
	verdict := evaluateMetricsOutput("", 0, 30, nil)

	if verdict.FromJSON {
		t.Error("want FromJSON false")
	}
	if verdict.Breached {
		t.Error("want Breached false")
	}
}
