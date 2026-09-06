package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestParseLCOV_ClassicFNAndFNDARecords(t *testing.T) {
	data := []byte(`TN:
SF:internal/steps/test.go
FN:29,execute
FN:515,testAgentContext
FNDA:12,execute
FNDA:0,testAgentContext
DA:30,12
end_of_record
SF:internal/steps/lint.go
FN:8,Execute
FNDA:4,Execute
end_of_record
`)
	profile, err := parseLCOV(data)
	if err != nil {
		t.Fatalf("parseLCOV: %v", err)
	}
	wantFiles := []string{"internal/steps/test.go", "internal/steps/lint.go"}
	if !reflect.DeepEqual(profile.Files, wantFiles) {
		t.Fatalf("Files = %v, want %v", profile.Files, wantFiles)
	}
	want := []coveredFunction{
		{File: "internal/steps/test.go", Name: "execute", Line: 29, Hits: 12},
		{File: "internal/steps/test.go", Name: "testAgentContext", Line: 515, Hits: 0},
		{File: "internal/steps/lint.go", Name: "Execute", Line: 8, Hits: 4},
	}
	if !reflect.DeepEqual(profile.Functions, want) {
		t.Fatalf("Functions = %+v, want %+v", profile.Functions, want)
	}
}

func TestParseLCOV_FNLAndFNARecords(t *testing.T) {
	data := []byte(`SF:src/order.ts
FNL:0,12,20
FNL:1,30,44
FNA:0,7,place
FNA:1,0,cancel
end_of_record
`)
	profile, err := parseLCOV(data)
	if err != nil {
		t.Fatalf("parseLCOV: %v", err)
	}
	want := []coveredFunction{
		{File: "src/order.ts", Name: "place", Line: 12, Hits: 7},
		{File: "src/order.ts", Name: "cancel", Line: 30, Hits: 0},
	}
	if !reflect.DeepEqual(profile.Functions, want) {
		t.Fatalf("Functions = %+v, want %+v", profile.Functions, want)
	}
}

func TestParseLCOV_NoSFRecordErrors(t *testing.T) {
	_, err := parseLCOV([]byte("FN:1,foo\nFNDA:1,foo\nend_of_record\n"))
	if err == nil {
		t.Fatal("expected an error naming the format, got nil")
	}
}

func TestParseCobertura_MethodLinesAndEmptyClass(t *testing.T) {
	data := []byte(`<?xml version="1.0" encoding="utf-8"?>
<coverage line-rate="0.5">
  <packages><package name="Api">
    <classes>
      <class filename="src/Api/Order.cs" name="Api.Order">
        <methods>
          <method name="Place" signature="()" line-rate="1">
            <lines><line number="12" hits="3"/><line number="14" hits="3"/></lines>
          </method>
          <method name="Cancel" signature="()" line-rate="0">
            <lines><line number="30" hits="0"/></lines>
          </method>
        </methods>
      </class>
      <class filename="src/Api/Empty.cs" name="Api.Empty"><methods></methods></class>
    </classes>
  </package></packages>
</coverage>
`)
	profile, err := parseCobertura(data)
	if err != nil {
		t.Fatalf("parseCobertura: %v", err)
	}
	wantFiles := []string{"src/Api/Order.cs", "src/Api/Empty.cs"}
	if !reflect.DeepEqual(profile.Files, wantFiles) {
		t.Fatalf("Files = %v, want %v", profile.Files, wantFiles)
	}
	want := []coveredFunction{
		{File: "src/Api/Order.cs", Name: "Place", Line: 12, Hits: 3},
		{File: "src/Api/Order.cs", Name: "Cancel", Line: 30, Hits: 0},
	}
	if !reflect.DeepEqual(profile.Functions, want) {
		t.Fatalf("Functions = %+v, want %+v", profile.Functions, want)
	}
}

func TestParseJUnitReport_RootTestsuitesTestsAttributeWins(t *testing.T) {
	data := []byte(`<testsuites tests="41" failures="0" skipped="2"><testsuite tests="41" skipped="2"/></testsuites>`)
	report, err := parseJUnitReport(data)
	if err != nil {
		t.Fatalf("parseJUnitReport: %v", err)
	}
	if report.Executed != 39 {
		t.Fatalf("Executed = %d, want 39", report.Executed)
	}
}

func TestParseJUnitReport_RootTestsuitesWithoutTestsAttributeSumsChildren(t *testing.T) {
	data := []byte(`<testsuites><testsuite tests="10" skipped="1"/><testsuite tests="4" skipped="0"/></testsuites>`)
	report, err := parseJUnitReport(data)
	if err != nil {
		t.Fatalf("parseJUnitReport: %v", err)
	}
	if report.Executed != 13 {
		t.Fatalf("Executed = %d, want 13", report.Executed)
	}
}

func TestParseJUnitReport_BareTestsuiteAllSkippedIsZero(t *testing.T) {
	data := []byte(`<testsuite tests="6" skipped="6"/>`)
	report, err := parseJUnitReport(data)
	if err != nil {
		t.Fatalf("parseJUnitReport: %v", err)
	}
	if report.Executed != 0 {
		t.Fatalf("Executed = %d, want 0 (every test was skipped)", report.Executed)
	}
}

// TestParseTRXReport_OnlyExecutedAnswersWhetherAnythingRan pins the counter the
// guard reads. VSTest's total counts every test the run discovered, including
// the ones a filter excluded, so a run that executed nothing reports
// total="10" executed="0"; reading total would green exactly that run.
func TestParseTRXReport_OnlyExecutedAnswersWhetherAnythingRan(t *testing.T) {
	report, err := parseTRXReport([]byte(`<TestRun><ResultSummary outcome="Completed"><Counters total="10" executed="8" passed="8" failed="0"/></ResultSummary></TestRun>`))
	if err != nil {
		t.Fatalf("parseTRXReport: %v", err)
	}
	if report.Executed != 8 {
		t.Fatalf("Executed = %d, want 8", report.Executed)
	}

	report, err = parseTRXReport([]byte(`<TestRun><ResultSummary outcome="Completed"><Counters total="10" executed="0" passed="0" failed="0"/></ResultSummary></TestRun>`))
	if err != nil {
		t.Fatalf("parseTRXReport (filtered run): %v", err)
	}
	if report.Executed != 0 {
		t.Fatalf("Executed = %d, want 0 (a nonzero total must not stand in for it)", report.Executed)
	}

	if _, err := parseTRXReport([]byte(`<TestRun><ResultSummary outcome="Completed"><Counters total="10" passed="8" failed="0"/></ResultSummary></TestRun>`)); err == nil {
		t.Fatal("expected an error when Counters carries no executed attribute")
	}
}

// TestParseJUnitReport_AbsentRootSkippedSumsTheChildren covers jest-junit's
// shape: the root carries the test total but no skipped attribute, and the
// per-suite skipped counts are the only record that nothing actually ran.
func TestParseJUnitReport_AbsentRootSkippedSumsTheChildren(t *testing.T) {
	report, err := parseJUnitReport([]byte(`<testsuites tests="4">
<testsuite tests="3" skipped="3"></testsuite>
<testsuite tests="1" skipped="1"></testsuite>
</testsuites>`))
	if err != nil {
		t.Fatalf("parseJUnitReport: %v", err)
	}
	if report.Executed != 0 {
		t.Fatalf("Executed = %d, want 0 (every child suite was skipped)", report.Executed)
	}

	report, err = parseJUnitReport([]byte(`<testsuites tests="4" skipped="0">
<testsuite tests="3" skipped="3"></testsuite>
<testsuite tests="1" skipped="1"></testsuite>
</testsuites>`))
	if err != nil {
		t.Fatalf("parseJUnitReport (explicit root skipped): %v", err)
	}
	if report.Executed != 4 {
		t.Fatalf("Executed = %d, want 4 (an explicit root skipped=0 wins over the children)", report.Executed)
	}
}

// TestParseCobertura_MalformedLineRateIsAnError keeps a broken profile out of
// the "this method did not run" bucket. Swallowing the parse error recorded
// zero hits for every method the file described, which reads to the guard as a
// change nobody tested and charges an agent with the maintainer's reporting bug.
func TestParseCobertura_MalformedLineRateIsAnError(t *testing.T) {
	_, err := parseCobertura([]byte(`<?xml version="1.0"?><coverage><packages><package><classes>
<class filename="src/Api/Order.cs" name="Api.Order"><methods>
<method name="Place" signature="()" line-rate="1,0"></method>
</methods></class>
</classes></package></packages></coverage>`))
	if err == nil {
		t.Fatal("expected a malformed line-rate attribute to be an error")
	}
	if !strings.Contains(err.Error(), "line-rate") {
		t.Fatalf("error should name the attribute it could not read, got %v", err)
	}
}

func TestClassifyCoverageFile_AllFourKindsAndUnknown(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		want   coverageFileKind
	}{
		{"lcov", "TN:\nSF:a.go\nend_of_record\n", coverageKindLCOV},
		{"cobertura", `<?xml version="1.0"?><coverage line-rate="1"></coverage>`, coverageKindCobertura},
		{"junit-suites", `<testsuites tests="1"></testsuites>`, coverageKindJUnit},
		{"junit-suite", `<testsuite tests="1"></testsuite>`, coverageKindJUnit},
		{"trx", `<TestRun></TestRun>`, coverageKindTRX},
		{"unknown", "ran some tests, all good", coverageKindUnknown},
		{"well-formed-other-xml", `<project name="build"></project>`, coverageKindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyCoverageFile([]byte(tc.prefix))
			if err != nil {
				t.Fatalf("classifyCoverageFile(%q) reported %v; only data that begins like XML and reaches no element is an error", tc.prefix, err)
			}
			if got != tc.want {
				t.Fatalf("classifyCoverageFile(%q) = %v, want %v", tc.prefix, got, tc.want)
			}
		})
	}
}

// A prefix that opens an element and stops mid-token is not a non-XML file; it
// is an artifact the reader saw only part of. Answering unknown with no error
// there let readCoverageArtifacts ignore a real coverage file, and a sibling
// unit's profile then certified the change.
func TestClassifyCoverageFile_TruncatedXMLIsAnError(t *testing.T) {
	kind, err := classifyCoverageFile([]byte(`<?xml version="1.0"?>` + "\n<!-- a long leading comment"))
	if kind != coverageKindUnknown {
		t.Fatalf("kind = %v, want %v", kind, coverageKindUnknown)
	}
	if err == nil {
		t.Fatal("expected truncated XML to report the decoder error")
	}
}

func TestReadCoverageArtifacts_MergesFormatsAndIgnoresUnrecognisedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "lcov.info"), `SF:internal/steps/test.go
FN:29,execute
FNDA:12,execute
end_of_record
`)
	writeFile(t, filepath.Join(dir, "results.xml"), `<testsuites tests="5" skipped="1"></testsuites>`)
	writeFile(t, filepath.Join(dir, "notes.txt"), "ran some tests")
	writeFile(t, filepath.Join(dir, "sub", "coverage.cobertura.xml"), `<?xml version="1.0"?><coverage line-rate="1"><packages><package><classes>
<class filename="src/Api/Order.cs" name="Api.Order"><methods>
<method name="Place" signature="()" line-rate="1"><lines><line number="12" hits="3"/></lines></method>
</methods></class>
</classes></package></packages></coverage>`)

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if !artifacts.HasProfile {
		t.Fatal("HasProfile = false, want true")
	}
	if !artifacts.HasReport {
		t.Fatal("HasReport = false, want true")
	}
	if len(artifacts.Unparseable) != 0 {
		t.Fatalf("Unparseable = %v, want empty", artifacts.Unparseable)
	}
	if len(artifacts.Skipped) != 0 {
		t.Fatalf("Skipped = %v, want empty", artifacts.Skipped)
	}
	if artifacts.Report.Executed != 4 {
		t.Fatalf("Report.Executed = %d, want 4", artifacts.Report.Executed)
	}
	wantFunctions := map[string]bool{"execute": false, "Place": false}
	if len(artifacts.Profile.Functions) != 2 {
		t.Fatalf("Functions = %+v, want 2 records", artifacts.Profile.Functions)
	}
	for _, fn := range artifacts.Profile.Functions {
		if _, ok := wantFunctions[fn.Name]; !ok {
			t.Fatalf("unexpected function %q in merged profile", fn.Name)
		}
	}
}

func TestReadCoverageArtifacts_MalformedLCOVGoesToUnparseable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.lcov")
	writeFile(t, path, "SF:a.go\nFN:notanumber,execute\n")

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if artifacts.HasProfile {
		t.Fatal("HasProfile = true, want false for an unparseable file")
	}
	// The entry names the path AND the parser's diagnosis: this park is the
	// never auto-fixable one, so a maintainer reading it is the only person
	// who can act, and the path alone leaves them to rediscover the reason.
	if len(artifacts.Unparseable) != 1 {
		t.Fatalf("Unparseable = %v, want one entry", artifacts.Unparseable)
	}
	entry := artifacts.Unparseable[0]
	if !strings.HasPrefix(entry, path) {
		t.Fatalf("Unparseable entry %q does not name %s", entry, path)
	}
	if !strings.Contains(entry, "notanumber") {
		t.Fatalf("Unparseable entry %q does not carry the parser diagnosis", entry)
	}
}

func TestReadCoverageArtifacts_MissingDirReturnsZeroValueNoError(t *testing.T) {
	artifacts, err := readCoverageArtifacts(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if !reflect.DeepEqual(artifacts, coverageArtifacts{}) {
		t.Fatalf("artifacts = %+v, want zero value", artifacts)
	}
}

func TestReadCoverageArtifacts_OversizedFileIsSkippedWithoutReading(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.lcov")
	writeFile(t, path, "SF:a.go\nend_of_record\n")

	old := coverageFileSize
	coverageFileSize = func(p string) (int64, error) {
		if p == path {
			return maxCoverageFileBytes + 1, nil
		}
		return old(p)
	}
	defer func() { coverageFileSize = old }()

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if artifacts.HasProfile {
		t.Fatal("HasProfile = true, want false: the oversized file must not be parsed")
	}
	if len(artifacts.Skipped) != 1 || artifacts.Skipped[0] != path {
		t.Fatalf("Skipped = %v, want [%s]", artifacts.Skipped, path)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestReadCoverageArtifacts_UnrecognisedFilesDoNotConsumeTheScanBudget covers
// the jest layout: an lcov-report/ tree of HTML files sorts before lcov.info,
// so charging every visited file against the budget spent it on the HTML and
// the real profile was never reached.
func TestReadCoverageArtifacts_UnrecognisedFilesDoNotConsumeTheScanBudget(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxCoverageFilesScanned+10; i++ {
		writeFile(t, filepath.Join(dir, "lcov-report", fmt.Sprintf("page%03d.html", i)), "<html><body>report</body></html>")
	}
	writeFile(t, filepath.Join(dir, "lcov.info"), "SF:src/order.ts\nFN:3,place\nFNDA:2,place\nend_of_record\n")
	writeFile(t, filepath.Join(dir, "results.xml"), `<testsuite tests="4" skipped="0"></testsuite>`)

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if !artifacts.HasProfile {
		t.Error("HasProfile = false: the HTML report tree consumed the budget before the profile was reached")
	}
	if !artifacts.HasReport {
		t.Error("HasReport = false: the HTML report tree consumed the budget before the report was reached")
	}
	if len(artifacts.Skipped) != 0 {
		t.Errorf("Skipped = %v, want empty: unrecognised files are not artifacts the guard could not read", artifacts.Skipped)
	}
}

// TestReadCoverageArtifacts_OversizedUnrecognisedFileIsNotReportedAsSkipped
// keeps a benign multi-gigabyte runner log from reading as a coverage artifact
// the guard could not use.
func TestReadCoverageArtifacts_OversizedUnrecognisedFileIsNotReportedAsSkipped(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "runner.log")
	writeFile(t, logPath, "ran some tests, all good")
	writeFile(t, filepath.Join(dir, "coverage.lcov"), "SF:a.go\nFN:1,Run\nFNDA:1,Run\nend_of_record\n")
	writeFile(t, filepath.Join(dir, "results.xml"), `<testsuite tests="2" skipped="0"></testsuite>`)

	old := coverageFileSize
	coverageFileSize = func(p string) (int64, error) {
		if p == logPath {
			return maxCoverageFileBytes + 1, nil
		}
		return old(p)
	}
	defer func() { coverageFileSize = old }()

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if len(artifacts.Skipped) != 0 {
		t.Errorf("Skipped = %v, want empty: an oversized non-artifact is not the guard's business", artifacts.Skipped)
	}
	if !artifacts.HasProfile || !artifacts.HasReport {
		t.Errorf("HasProfile = %v, HasReport = %v, want both true", artifacts.HasProfile, artifacts.HasReport)
	}
}

func TestParseJUnitReport_MalformedCounterAttributesAreErrors(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"root skipped", `<testsuites tests="5" skipped="lots"></testsuites>`},
		{"child tests", `<testsuites><testsuite tests="many" skipped="0"/></testsuites>`},
		{"child skipped", `<testsuites><testsuite tests="5" skipped="many"/></testsuites>`},
		{"bare suite tests", `<testsuite tests="many" skipped="0"></testsuite>`},
		{"bare suite skipped", `<testsuite tests="5" skipped="many"></testsuite>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseJUnitReport([]byte(tc.data)); err == nil {
				t.Fatal("expected an error: a malformed counter must reach Unparseable, not be silently zeroed")
			}
		})
	}
}

// TestParseJUnitReport_SkipsExceedingTestsIsUnparseable pins the arithmetic
// the guard reads. A runner reporting root-level skips against per-suite
// totals yields a negative count, which is not evidence a test ran, but it is
// also not evidence a test is missing; it is a reporting defect that belongs
// to the maintainer, so the report is unparseable rather than a zeroed count.
func TestParseJUnitReport_SkipsExceedingTestsIsUnparseable(t *testing.T) {
	_, err := parseJUnitReport([]byte(`<testsuites tests="2" skipped="5"></testsuites>`))
	if err == nil {
		t.Fatal("parseJUnitReport accepted more skipped tests than tests")
	}
	for _, want := range []string{"2", "5"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry the count %s", err, want)
		}
	}
}

// TestParseLCOV_TwoDotXFunctionRecordKeepsPerFunctionPrecision covers the
// lcov 2.x FN:<start>,<end>,<name> shape. Read as the 1.x FN:<line>,<name>
// form the name became "40,place", which matched no FNDA record, while the
// real "place" arrived through the flush fallback with no declaration line and
// so spanned the whole file. Every executed function then satisfied every
// changed line, which is the per-function check degrading to a per-file one.
func TestParseLCOV_TwoDotXFunctionRecordKeepsPerFunctionPrecision(t *testing.T) {
	profile, err := parseLCOV([]byte("SF:api/order.go\nFN:12,40,place\nFNDA:3,place\nFN:50,80,cancel\nFNDA:0,cancel\nend_of_record\n"))
	if err != nil {
		t.Fatalf("parseLCOV: %v", err)
	}
	if len(profile.Functions) != 2 {
		t.Fatalf("functions = %+v, want exactly the two FN records", profile.Functions)
	}
	want := []coveredFunction{
		{File: "api/order.go", Name: "place", Line: 12, Hits: 3},
		{File: "api/order.go", Name: "cancel", Line: 50, Hits: 0},
	}
	for i, fn := range profile.Functions {
		if fn != want[i] {
			t.Errorf("function %d = %+v, want %+v", i, fn, want[i])
		}
	}

	// The behavioural consequence: a change inside the unexecuted cancel must
	// not be certified by the executed place.
	ranges := map[string][]lineRange{"api/order.go": {{Start: 60, End: 60}}}
	if covered := coveredChangedFunctions(profile, "", ranges); len(covered) != 0 {
		t.Errorf("a change inside the unexecuted function was certified by %+v", covered)
	}
}

// TestParseLCOV_OneDotXFunctionRecordStillParses keeps the two-field form
// working beside the three-field one.
func TestParseLCOV_OneDotXFunctionRecordStillParses(t *testing.T) {
	profile, err := parseLCOV([]byte("SF:api/order.go\nFN:12,place\nFNDA:3,place\nend_of_record\n"))
	if err != nil {
		t.Fatalf("parseLCOV: %v", err)
	}
	want := coveredFunction{File: "api/order.go", Name: "place", Line: 12, Hits: 3}
	if len(profile.Functions) != 1 || profile.Functions[0] != want {
		t.Fatalf("functions = %+v, want [%+v]", profile.Functions, want)
	}
}

// TestReadCoverageArtifacts_UnreadableFileIsSkippedNotAnError pins the posture
// toward an I/O fault: a test command owns this directory and can leave a
// file the reader cannot open, and failing the run reports that as a pipeline
// defect instead of the reporting problem it is.
func TestReadCoverageArtifacts_UnreadableFileIsSkippedNotAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file, so the fault this test needs cannot be provoked")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "coverage.lcov"), "SF:api/order.go\nFN:1,Place\nFNDA:2,Place\nend_of_record\n")
	writeFile(t, filepath.Join(dir, "report.xml"), `<testsuite tests="3" skipped="0"></testsuite>`)
	locked := filepath.Join(dir, "locked.lcov")
	writeFile(t, locked, "SF:api/other.go\nend_of_record\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o644) })

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("an unreadable file failed the read instead of being recorded: %v", err)
	}
	if !artifacts.HasProfile || !artifacts.HasReport {
		t.Fatalf("the readable artifacts were lost: %+v", artifacts)
	}
	if !slices.ContainsFunc(artifacts.Skipped, func(s string) bool { return strings.Contains(s, "locked.lcov") }) {
		t.Errorf("Skipped = %v, want the unreadable file recorded", artifacts.Skipped)
	}
}

// TestReadCoverageArtifacts_MalformedReportCannotCancelASibling pins the
// per-report clamp. A root testsuites element reporting more skips than tests
// yields a negative count, and letting it cancel a sibling's real count raised
// a missing-test park against a unit that genuinely ran tests.
func TestReadCoverageArtifacts_MalformedReportCannotCancelASibling(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "report-a.xml"), `<testsuites tests="1"><testsuite tests="3" skipped="3"/></testsuites>`)
	writeFile(t, filepath.Join(dir, "report-b.xml"), `<testsuite tests="2" skipped="0"></testsuite>`)

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if artifacts.Report.Executed != 2 {
		t.Errorf("Executed = %d, want 2; the malformed report cancelled its sibling", artifacts.Report.Executed)
	}
}

// TestParseCobertura_MethodWithNoLineChildrenReadsItsLineRate covers the
// verdict .NET's Cobertura writer relies on. Its methods routinely carry no
// <line> children, so the line-rate attribute is the only thing saying whether
// the method ran; reading it as unexecuted parks a change whose tests cover it.
func TestParseCobertura_MethodWithNoLineChildrenReadsItsLineRate(t *testing.T) {
	xml := `<coverage><packages><package><classes>
<class filename="Services/OrderService.cs">
<methods>
<method name="Place" line-rate="1"/>
<method name="Cancel" line-rate="0"/>
</methods>
<lines></lines>
</class>
</classes></package></packages></coverage>`

	profile, err := parseCobertura([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	hits := map[string]int{}
	for _, fn := range profile.Functions {
		hits[fn.Name] = fn.Hits
	}
	if len(profile.Functions) != 2 {
		t.Fatalf("Functions = %+v, want both methods", profile.Functions)
	}
	if hits["Place"] == 0 {
		t.Error("a method with line-rate=1 and no line children read as never executed")
	}
	if hits["Cancel"] != 0 {
		t.Errorf("Cancel hits = %d, want 0; line-rate=0 means the method never ran", hits["Cancel"])
	}
}

// TestParseCobertura_ClassWithNoFilenameNamesNoFile keeps an unusable profile
// out of the file list. A "" entry made a profile that describes nothing pass
// the maintainer park and then matched every extensionless changed file.
func TestParseCobertura_ClassWithNoFilenameNamesNoFile(t *testing.T) {
	xml := `<coverage><packages><package><classes>
<class><methods><method name="Ghost" line-rate="1"/></methods><lines></lines></class>
</classes></package></packages></coverage>`

	profile, err := parseCobertura([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.Files) != 0 {
		t.Errorf("Files = %q, want none; a class with no filename names no file", profile.Files)
	}
}

// TestParseLCOV_FunctionDataWithoutADeclarationIsStillReported covers the FNDA
// records a runner emits with no matching FN. Dropping them loses the function
// entirely; keeping them with no declaration line is what makes functionSpans
// widen the span to the whole file, which is the conservative direction.
func TestParseLCOV_FunctionDataWithoutADeclarationIsStillReported(t *testing.T) {
	profile, err := parseLCOV([]byte("SF:api/order.go\nFNDA:4,Place\nFNDA:0,Cancel\nend_of_record\n"))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]coveredFunction{}
	for _, fn := range profile.Functions {
		byName[fn.Name] = fn
	}
	if len(profile.Functions) != 2 {
		t.Fatalf("Functions = %+v, want both FNDA records", profile.Functions)
	}
	if byName["Place"].Line != 0 || byName["Place"].Hits != 4 {
		t.Errorf("Place = %+v, want Line 0 and Hits 4", byName["Place"])
	}

	// With no declaration line the span covers the file, so a change anywhere
	// in it counts as exercised by the executed function and not by the
	// unexecuted one.
	covered := coveredChangedFunctions(profile, "", map[string][]lineRange{"api/order.go": {{Start: 120, End: 120}}})
	if len(covered) != 1 || covered[0].Name != "Place" {
		t.Fatalf("covered = %+v, want only the executed Place", covered)
	}
}

// TestParseLCOV_SourceFileWithNoNameNamesNoFile is the LCOV half of the
// empty-filename rule.
func TestParseLCOV_SourceFileWithNoNameNamesNoFile(t *testing.T) {
	profile, err := parseLCOV([]byte("SF:\nFN:1,Ghost\nFNDA:2,Ghost\nend_of_record\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.Files) != 0 {
		t.Errorf("Files = %q, want none; a bare SF record names no file", profile.Files)
	}
}

// TestReadCoverageArtifacts_UntokenizableXMLIsRecordedNotIgnored covers the
// silent drop. The reader classifies from a bounded prefix, so a large
// Cobertura file with a long leading comment, or one a crashed runner
// truncated, reaches no start element in that prefix. Answering "not a
// coverage file" there dropped a real profile, and a sibling unit's artifacts
// then certified the change. The reader re-reads the whole file, and a file
// that still yields no element is recorded so the verdict goes to the
// maintainer.
func TestReadCoverageArtifacts_UntokenizableXMLIsRecordedNotIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "coverage.xml")
	writeFile(t, path, `<?xml version="1.0"?>`+"\n<!-- this comment never closes\n")

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if len(artifacts.Skipped) != 1 || !strings.HasPrefix(artifacts.Skipped[0], path) {
		t.Fatalf("Skipped = %v, want one entry naming %s", artifacts.Skipped, path)
	}
	if artifacts.HasProfile || artifacts.HasReport {
		t.Fatal("an unreadable file must not count as either artifact")
	}
}

// TestReadCoverageArtifacts_LongLeadingCommentIsClassifiedOnAReRead is the
// other half: the re-read has to recover the file, not merely record it.
func TestReadCoverageArtifacts_LongLeadingCommentIsClassifiedOnAReRead(t *testing.T) {
	dir := t.TempDir()
	comment := "<!-- " + strings.Repeat("x", 8*1024) + " -->\n"
	writeFile(t, filepath.Join(dir, "coverage.xml"), `<?xml version="1.0"?>`+"\n"+comment+
		`<coverage><packages><package><classes>
<class filename="src/Api/Order.cs" name="Api.Order"><methods>
<method name="Place" signature="()" line-rate="1.0"></method>
</methods></class>
</classes></package></packages></coverage>`)

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if len(artifacts.Skipped) != 0 {
		t.Fatalf("Skipped = %v, want none once the whole file was read", artifacts.Skipped)
	}
	if !artifacts.HasProfile {
		t.Fatal("the re-read did not recover the coverage profile")
	}
	if len(artifacts.Profile.Files) != 1 || artifacts.Profile.Files[0] != "src/Api/Order.cs" {
		t.Fatalf("Profile.Files = %v, want [src/Api/Order.cs]", artifacts.Profile.Files)
	}
}

// TestReadCoverageArtifacts_UnreadableRootIsSkippedNotAFailure gives the root
// directory the same treatment an entry inside it already gets. The test
// command owns this directory and can leave it mode 000, which is the
// unreadable-artifact case the maintainer park exists for, not a pipeline
// defect that should fail the run outright.
func TestReadCoverageArtifacts_UnreadableRootIsSkippedNotAFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory modes")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads an unreadable directory anyway")
	}
	dir := filepath.Join(t.TempDir(), "coverage")
	if err := os.Mkdir(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("an unreadable coverage root failed the run: %v", err)
	}
	if len(artifacts.Skipped) != 1 || !strings.HasPrefix(artifacts.Skipped[0], dir) {
		t.Fatalf("Skipped = %v, want one entry naming %s", artifacts.Skipped, dir)
	}
	if !strings.Contains(artifacts.Skipped[0], "permission denied") {
		t.Errorf("Skipped entry %q does not carry the real cause", artifacts.Skipped[0])
	}
}

// TestParseJUnitReport_MoreSkippedThanTestsIsUnparseable routes a counter bug
// to the park that can fix it. Clamping the difference to zero reported "no
// test executed", which is the auto-fixable park, so an agent fix round was
// charged with writing a test to answer a reporting defect.
func TestParseJUnitReport_MoreSkippedThanTestsIsUnparseable(t *testing.T) {
	data := []byte(`<testsuite tests="4" skipped="6"></testsuite>`)

	_, err := parseJUnitReport(data)
	if err == nil {
		t.Fatal("parseJUnitReport accepted more skipped tests than tests")
	}
	for _, want := range []string{"4", "6"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry the count %s", err, want)
		}
	}
}

// TestParseJUnitReport_ChildSuiteMoreSkippedThanTestsIsUnparseable holds the
// same rule at the summing branch, where the per-suite subtraction happens
// before the total the outer clamp defends.
func TestParseJUnitReport_ChildSuiteMoreSkippedThanTestsIsUnparseable(t *testing.T) {
	data := []byte(`<testsuites><testsuite tests="1" skipped="9"></testsuite></testsuites>`)

	if _, err := parseJUnitReport(data); err == nil {
		t.Fatal("parseJUnitReport accepted a child suite skipping more tests than it ran")
	}
}

// TestParseTRXReport_NegativeExecutedCountIsUnparseable is the TRX half of the
// same rule.
func TestParseTRXReport_NegativeExecutedCountIsUnparseable(t *testing.T) {
	data := []byte(`<TestRun><ResultSummary><Counters total="3" executed="-2" /></ResultSummary></TestRun>`)

	if _, err := parseTRXReport(data); err == nil {
		t.Fatal("parseTRXReport accepted a negative executed count")
	}
}

// TestReadCoverageArtifacts_RunnerJavaScriptIsNotAnUnreadableArtifact keeps
// jest's own report tree out of the field the guard reads to choose a park.
// block-navigation.js carries `i < len`, and treating any '<' in the prefix as
// the start of XML recorded it as an artifact the reader could not use, which
// routed every later verdict to the maintainer park and killed the
// auto-fixable posture the issue requires.
func TestReadCoverageArtifacts_RunnerJavaScriptIsNotAnUnreadableArtifact(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "lcov-report", "block-navigation.js"),
		"var nextBlock = function () {\n\tfor (var i = 0; i < len; i++) {\n\t\tjump(i);\n\t}\n};\n")
	writeFile(t, filepath.Join(dir, "lcov.info"), "SF:src/order.ts\nFN:3,place\nFNDA:2,place\nend_of_record\n")
	writeFile(t, filepath.Join(dir, "results.xml"), `<testsuite tests="4" skipped="0"></testsuite>`)

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if len(artifacts.Skipped) != 0 {
		t.Errorf("Skipped = %v, want empty: the runner's own JavaScript is not an artifact the guard could not read", artifacts.Skipped)
	}
	if !artifacts.HasProfile || !artifacts.HasReport {
		t.Errorf("HasProfile = %v, HasReport = %v, want both true", artifacts.HasProfile, artifacts.HasReport)
	}
}

// TestReadCoverageArtifacts_TruncatedXMLIsStillReportedAsUnread is the other
// side of that line. A file that really does open as XML and then yields no
// start element is a half-written artifact, and the guard must still say so.
func TestReadCoverageArtifacts_TruncatedXMLIsStillReportedAsUnread(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.xml")
	writeFile(t, path, "\xef\xbb\xbf\n  <?xml version=\"1.0\"?>\n<!-- the writer died here")

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if len(artifacts.Skipped) != 1 || !strings.HasPrefix(artifacts.Skipped[0], path) {
		t.Fatalf("Skipped = %v, want one entry naming %s", artifacts.Skipped, path)
	}
}

// TestReadCoverageArtifacts_ScanBudgetStopsTheWalk exercises the bound itself.
// Nothing tripped it before, so deleting the budget check passed the suite and
// took the parse bound with it. The budget is reported on its own field rather
// than in Skipped, which names the files it lists: what the walk never reached
// has no name to record.
func TestReadCoverageArtifacts_ScanBudgetStopsTheWalk(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxCoverageFilesScanned+2; i++ {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("cov%04d.lcov", i)),
			fmt.Sprintf("SF:src/file%04d.go\nFN:1,Run\nFNDA:1,Run\nend_of_record\n", i))
	}

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if !artifacts.ScanLimited {
		t.Error("ScanLimited = false: the walk read past the scan budget")
	}
	if len(artifacts.Profile.Files) != maxCoverageFilesScanned {
		t.Errorf("parsed %d profiles, want the budget of %d", len(artifacts.Profile.Files), maxCoverageFilesScanned)
	}
	if len(artifacts.Skipped) != 0 {
		t.Errorf("Skipped = %v, want empty: every artifact the budget allowed parsed fine", artifacts.Skipped)
	}
}

// TestReadCoverageArtifacts_EntryBudgetStopsTheWalk covers the other bound. The
// artifact budget counts only recognised artifacts, so a command that dumps an
// HTML coverage tree never trips it while every one of those files still costs
// a prefix read, and the walk ran for as long as the directory was large.
func TestReadCoverageArtifacts_EntryBudgetStopsTheWalk(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxCoverageEntriesVisited+2; i++ {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("page%05d.html", i)), "<html></html>")
	}

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if !artifacts.ScanLimited {
		t.Error("ScanLimited = false: the walk examined every entry in an oversized directory")
	}
}

// TestReadCoverageArtifacts_UnclassifiableXMLIsChargedAgainstTheBudget pins the
// cost the artifact budget was missing. A prefix that opens as XML and yields
// no start element sends the reader back to read the WHOLE file, which is the
// same work parsing an artifact costs, and a directory of truncated XML paid it
// once per file with nothing counting.
func TestReadCoverageArtifacts_UnclassifiableXMLIsChargedAgainstTheBudget(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxCoverageFilesScanned+2; i++ {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("partial%04d.xml", i)),
			"<?xml version=\"1.0\"?>\n<!-- the writer died here")
	}

	artifacts, err := readCoverageArtifacts(dir)
	if err != nil {
		t.Fatalf("readCoverageArtifacts: %v", err)
	}
	if !artifacts.ScanLimited {
		t.Error("ScanLimited = false: whole-file re-reads cost nothing against the budget")
	}
	if len(artifacts.Skipped) > maxCoverageFilesScanned {
		t.Errorf("recorded %d unread artifacts, want at most the budget of %d", len(artifacts.Skipped), maxCoverageFilesScanned)
	}
}
