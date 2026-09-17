package steps

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

func linesFile(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "line"
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestChangedLineRanges_ModifiedAndAppendedLinesReportBothRanges(t *testing.T) {
	dir := t.TempDir()
	stepstest.GitCmd(t, dir, "init")

	path := filepath.Join(dir, "api", "order.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(linesFile(40)), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "base")
	baseSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")

	lines := make([]string, 40)
	for i := range lines {
		lines[i] = "line"
	}
	lines[11] = "rewritten"
	lines[12] = "rewritten"
	lines[13] = "rewritten"
	content := strings.Join(lines, "\n") + "\n" + "extra\nextra\nextra\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "rewrite and append")
	headSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")

	ranges, err := changedLineRanges(context.Background(), dir, baseSHA, headSHA, false, []string{"api/order.go"})
	if err != nil {
		t.Fatal(err)
	}

	if len(ranges) != 1 {
		t.Fatalf("want exactly one key, got %v", ranges)
	}
	got, ok := ranges["api/order.go"]
	if !ok {
		t.Fatalf("missing key api/order.go: %v", ranges)
	}
	want := []lineRange{{Start: 12, End: 14}, {Start: 41, End: 43}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestChangedLineRanges_PureDeletionYieldsEmptyRangeSlice(t *testing.T) {
	dir := t.TempDir()
	stepstest.GitCmd(t, dir, "init")

	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte(linesFile(20)), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "base")
	baseSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")

	lines := make([]string, 0, 20)
	for i := 1; i <= 20; i++ {
		if i >= 5 && i <= 9 {
			continue
		}
		lines = append(lines, "line")
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "delete lines 5-9")
	headSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")

	ranges, err := changedLineRanges(context.Background(), dir, baseSHA, headSHA, false, []string{"notes.txt"})
	if err != nil {
		t.Fatal(err)
	}

	got, ok := ranges["notes.txt"]
	if !ok {
		t.Fatalf("missing key notes.txt: %v", ranges)
	}
	if len(got) != 0 {
		t.Fatalf("want empty range slice, got %v", got)
	}
}

func TestChangedLineRanges_PathAbsentFromDiffGetsWholeFileRange(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	ranges, err := changedLineRanges(context.Background(), dir, baseSHA, headSHA, true, []string{"untracked_repair.go"})
	if err != nil {
		t.Fatal(err)
	}

	got, ok := ranges["untracked_repair.go"]
	if !ok {
		t.Fatalf("missing key: %v", ranges)
	}
	want := []lineRange{{Start: 1, End: math.MaxInt}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestChangedLineRanges_AddedFileGetsFullRange(t *testing.T) {
	dir := t.TempDir()
	stepstest.GitCmd(t, dir, "init")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "base")
	baseSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte(linesFile(5)), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "add new file")
	headSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")

	ranges, err := changedLineRanges(context.Background(), dir, baseSHA, headSHA, false, []string{"new.go"})
	if err != nil {
		t.Fatal(err)
	}

	got, ok := ranges["new.go"]
	if !ok {
		t.Fatalf("missing key: %v", ranges)
	}
	if len(got) != 1 || got[0].Start != 1 || got[0].End != 5 {
		t.Fatalf("got %v, want [{1 5}]", got)
	}
}

func TestCoveredChangedFunctions_ChangeInsideUnexecutedFunctionReturnsNothing(t *testing.T) {
	profile := coverageProfile{Files: []string{"api/order.go"}, Functions: []coveredFunction{
		{File: "api/order.go", Name: "Place", Line: 10, Hits: 3},
		{File: "api/order.go", Name: "Cancel", Line: 20, Hits: 0},
		{File: "api/order.go", Name: "Refund", Line: 30, Hits: 5},
	}}
	ranges := map[string][]lineRange{"api/order.go": {{Start: 22, End: 24}}}

	got := coveredChangedFunctions(profile, "", ranges)
	if len(got) != 0 {
		t.Fatalf("want no covered functions, got %v", got)
	}
}

func TestCoveredChangedFunctions_ChangeInsideExecutedLastFunctionReturnsIt(t *testing.T) {
	profile := coverageProfile{Files: []string{"api/order.go"}, Functions: []coveredFunction{
		{File: "api/order.go", Name: "Place", Line: 10, Hits: 3},
		{File: "api/order.go", Name: "Cancel", Line: 20, Hits: 0},
		{File: "api/order.go", Name: "Refund", Line: 30, Hits: 5},
	}}
	ranges := map[string][]lineRange{"api/order.go": {{Start: 31, End: 31}}}

	got := coveredChangedFunctions(profile, "", ranges)
	if len(got) != 1 || got[0].Name != "Refund" {
		t.Fatalf("want only Refund, got %v", got)
	}
}

func TestCoveredChangedFunctions_ExecutedAndUnexecutedBothIntersectingReturnsExecutedOnly(t *testing.T) {
	profile := coverageProfile{Files: []string{"api/order.go"}, Functions: []coveredFunction{
		{File: "api/order.go", Name: "Place", Line: 10, Hits: 3},
		{File: "api/order.go", Name: "Cancel", Line: 20, Hits: 0},
		{File: "api/order.go", Name: "Refund", Line: 30, Hits: 5},
	}}
	ranges := map[string][]lineRange{"api/order.go": {{Start: 10, End: 10}, {Start: 22, End: 24}}}

	got := coveredChangedFunctions(profile, "", ranges)
	if len(got) != 1 || got[0].Name != "Place" {
		t.Fatalf("want only Place, got %v", got)
	}
}

func TestCoveredChangedFunctions_ProfilePathFormsAllMatchChangedPath(t *testing.T) {
	ranges := map[string][]lineRange{"api/order.go": {{Start: 10, End: 10}}}
	workDir := trackedRepo(t, map[string]string{"api/order.go": linesFile(30)})

	forms := []string{
		filepath.Join(workDir, "api", "order.go"),
		"./api/order.go",
		`api\order.go`,
	}
	for _, file := range forms {
		profile := coverageProfile{Files: []string{file}, Functions: []coveredFunction{
			{File: file, Name: "Place", Line: 10, Hits: 1},
		}}
		got := coveredChangedFunctions(profile, workDir, ranges)
		if len(got) != 1 || got[0].Name != "Place" {
			t.Fatalf("file form %q: want match, got %v", file, got)
		}
	}
}

func TestCoveredChangedFunctions_LineZeroFunctionQualifiesRegardlessOfRanges(t *testing.T) {
	profile := coverageProfile{Files: []string{"api/order.go"}, Functions: []coveredFunction{
		{File: "api/order.go", Name: "Anonymous", Line: 0, Hits: 1},
	}}
	ranges := map[string][]lineRange{"api/order.go": {{Start: 500, End: 500}}}

	got := coveredChangedFunctions(profile, "", ranges)
	if len(got) != 1 || got[0].Name != "Anonymous" {
		t.Fatalf("want Anonymous to qualify, got %v", got)
	}
}

func TestClassifyChangedFiles_NewFileWithKnownExtensionIsCoverable(t *testing.T) {
	profile := coverageProfile{Files: []string{"src/a.ts", "src/b.ts"}}
	changed := []string{"src/a.ts", "README.md", "config/app.yaml", "src/new.ts"}

	got, err := classifyChangedFiles(context.Background(), t.TempDir(), profile, changed)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"src/a.ts", "src/new.ts"}
	if len(got.Coverable) != len(want) || got.Coverable[0] != want[0] || got.Coverable[1] != want[1] {
		t.Fatalf("Coverable = %v, want %v", got.Coverable, want)
	}
	if len(got.Unexplained) != 0 {
		t.Fatalf("Unexplained = %v, want none when the profile covers the change", got.Unexplained)
	}
}

func TestClassifyChangedFiles_EmptyProfileFilesYieldsEmptyResult(t *testing.T) {
	got, err := classifyChangedFiles(context.Background(), t.TempDir(), coverageProfile{}, []string{"a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Coverable) != 0 || len(got.Unexplained) != 0 {
		t.Fatalf("want empty result, got %+v", got)
	}
}

func TestClassifyChangedFiles_ExtensionlessFileMatchesExtensionlessProfileEntry(t *testing.T) {
	profile := coverageProfile{Files: []string{"Makefile"}}
	changed := []string{"Makefile", "a.go"}

	got, err := classifyChangedFiles(context.Background(), t.TempDir(), profile, changed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Coverable) != 1 || got.Coverable[0] != "Makefile" {
		t.Fatalf("Coverable = %v, want [Makefile]", got.Coverable)
	}
}

// TestClassifyChangedFiles_TestOnlyChangeIsNotCoverable covers the change that
// answers a missing-test finding. A new test file carries the same extension as
// the code it exercises, but a coverage profile reports the code, not the test,
// so demanding a covered function for the test file itself parks the very fix
// the previous round asked for.
func TestClassifyChangedFiles_TestOnlyChangeIsNotCoverable(t *testing.T) {
	// The repository keeps its tests in directories rather than by file name,
	// so .ts never appears on both sides of the naming convention and a file
	// under test/ or __tests__/ is test code.
	workDir := trackedRepo(t, map[string]string{
		"src/order.ts":         "export const order = 1\n",
		"src/cart.ts":          "export const cart = 1\n",
		"test/helpers.ts":      "export const help = 1\n",
		"src/__tests__/old.ts": "export const old = 1\n",
	})
	profile := coverageProfile{Files: []string{"src/order.ts"}}
	changed := []string{"src/order.spec.ts", "test/helpers.ts", "src/__tests__/cart.ts"}

	got, err := classifyChangedFiles(context.Background(), workDir, profile, changed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Coverable) != 0 {
		t.Fatalf("Coverable = %v, want none for a test-only change", got.Coverable)
	}
	if len(got.Unexplained) != 0 {
		t.Fatalf("Unexplained = %v, want none", got.Unexplained)
	}
}

// TestClassifyChangedFiles_SpecDirectoryOfASourceExtensionStaysCoverable keeps
// the directory rule from swallowing production code. "spec" and "test" name
// plenty of directories that hold shipped source (an API spec package, a
// language's own test harness), so a directory segment exempts a file only
// when the repository does not treat that extension as source, the same
// question testdata and fixtures already ask.
func TestClassifyChangedFiles_SpecDirectoryOfASourceExtensionStaysCoverable(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{
		"src/order.ts":      "export const order = 1\n",
		"src/order.spec.ts": "test('order', () => {})\n",
		"spec/schema.ts":    "export const schema = 1\n",
	})
	profile := coverageProfile{Files: []string{"src/order.ts"}}

	got, err := classifyChangedFiles(context.Background(), workDir, profile, []string{"spec/schema.ts"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Coverable) != 1 || got.Coverable[0] != "spec/schema.ts" {
		t.Fatalf("Coverable = %v, want [spec/schema.ts]", got.Coverable)
	}
}

// TestClassifyChangedFiles_TestFileTheProfileNamesStaysCoverable keeps the
// drop above narrow. A repository whose profile really does report its test
// files has coverage evidence for them, so they are still coverable.
func TestClassifyChangedFiles_TestFileTheProfileNamesStaysCoverable(t *testing.T) {
	profile := coverageProfile{Files: []string{"src/order.spec.ts"}}

	got, err := classifyChangedFiles(context.Background(), t.TempDir(), profile, []string{"src/order.spec.ts"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Coverable) != 1 || got.Coverable[0] != "src/order.spec.ts" {
		t.Fatalf("Coverable = %v, want [src/order.spec.ts]", got.Coverable)
	}
}

// TestClassifyChangedFiles_SourceTheProfileDescribesNoneOfIsUnexplained is the
// wrong-project case: a Go change measured by a profile that only knows
// TypeScript. The extension exemption would read that as "nothing here is
// coverable" and green the run, which is the outcome the guard exists to refuse.
func TestClassifyChangedFiles_SourceTheProfileDescribesNoneOfIsUnexplained(t *testing.T) {
	dir := trackedRepo(t, map[string]string{
		"src/order.spec.ts":          "spec\n",
		"src/order.ts":               "code\n",
		"internal/api/order_test.go": "package api\n",
		"internal/api/client.go":     "package api\n",
		"docs/guide.md":              "docs\n",
	})

	profile := coverageProfile{Files: []string{"src/order.ts"}}
	got, err := classifyChangedFiles(context.Background(), dir, profile, []string{"internal/api/order.go", "docs/guide.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Coverable) != 0 {
		t.Fatalf("Coverable = %v, want none", got.Coverable)
	}
	if len(got.Unexplained) != 1 || got.Unexplained[0] != "internal/api/order.go" {
		t.Fatalf("Unexplained = %v, want [internal/api/order.go]; the markdown file must stay exempt", got.Unexplained)
	}
}

// TestClassifyChangedFiles_DocumentationOnlyChangeStaysExempt is the other half
// of the same decision: a change carrying no source file at all must not park,
// or every README edit does.
func TestClassifyChangedFiles_DocumentationOnlyChangeStaysExempt(t *testing.T) {
	dir := trackedRepo(t, map[string]string{
		"internal/api/order_test.go": "package api\n",
		"internal/api/client.go":     "package api\n",
	})

	profile := coverageProfile{Files: []string{"internal/api/order.go"}}
	got, err := classifyChangedFiles(context.Background(), dir, profile, []string{"README.md", "docs/guide.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Coverable) != 0 || len(got.Unexplained) != 0 {
		t.Fatalf("want a documentation-only change to be fully exempt, got %+v", got)
	}
}

// trackedRepo stages the given files in a fresh repository. Only the index is
// needed, because repositorySourceExtensions reads git ls-files, and skipping
// the commit keeps the fixture independent of a developer's commit signing.
func trackedRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	stepstest.GitCmd(t, dir, "init")
	for rel, content := range files {
		writeRepoFile(t, dir, rel, content)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	return dir
}

func writeRepoFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFunctionSpans_TwoFunctionsOnOneLineKeepUsableSpans pins the span
// invariant. LCOV reports a generic and each instantiation at the same
// declaration line, and deriving the next span as "the next entry's line minus
// one" gave the first of them End < Start, an inverted range that intersects
// nothing and silently discarded the coverage it described.
func TestFunctionSpans_TwoFunctionsOnOneLineKeepUsableSpans(t *testing.T) {
	functions := []coveredFunction{
		{File: "a.go", Name: "Map", Line: 10, Hits: 3},
		{File: "a.go", Name: "Map[int]", Line: 10, Hits: 3},
		{File: "a.go", Name: "Reduce", Line: 40, Hits: 1},
	}
	spans := functionSpans(functions, nil)
	for i, span := range spans {
		if span.End < span.Start {
			t.Fatalf("span %d for %s is inverted: %+v", i, functions[i].Name, span)
		}
	}
	if spans[0] != (lineRange{Start: 10, End: 39}) || spans[1] != (lineRange{Start: 10, End: 39}) {
		t.Fatalf("both same-line functions should span to the next declaration, got %+v and %+v", spans[0], spans[1])
	}

	ranges := map[string][]lineRange{"a.go": {{Start: 12, End: 12}}}
	covered := coveredChangedFunctions(coverageProfile{Files: []string{"a.go"}, Functions: functions}, "", ranges)
	if len(covered) != 2 {
		t.Fatalf("want both same-line functions to cover the change, got %v", covered)
	}
}

// TestChangedLineRanges_PathNeedingQuotingIsReadVerbatim pins core.quotePath
// off. git's default C-quotes a path carrying a non-ASCII byte, and the quoted
// token never matched the plain changed path, so the file fell through to the
// whole-file fallback and every function in it counted as changed.
func TestChangedLineRanges_PathNeedingQuotingIsReadVerbatim(t *testing.T) {
	dir := t.TempDir()
	stepstest.GitCmd(t, dir, "init")

	const rel = "api/ordré.go"
	writeRepoFile(t, dir, rel, linesFile(40))
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "base")
	baseSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")

	writeRepoFile(t, dir, rel, linesFile(40)+"appended\n")
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "append")
	headSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")

	ranges, err := changedLineRanges(context.Background(), dir, baseSHA, headSHA, false, []string{rel})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ranges[rel]
	if !ok {
		t.Fatalf("missing key %q: %v", rel, ranges)
	}
	if len(got) != 1 || got[0] != (lineRange{Start: 41, End: 41}) {
		t.Fatalf("got %v, want the real appended-line range, not the whole-file fallback", got)
	}
}

// TestChangedLineRanges_AddedLineThatLooksLikeAFileHeaderIsNotOne pins the
// file boundary to the `diff --git` section header. With -U0 every body line
// carries a leading +, so adding the literal line "++ b/api/order.go" renders
// as "+++ b/api/order.go" inside the hunk; reading that as a header moved the
// remaining ranges onto api/order.go, which both lost notes.txt's own ranges
// and credited api/order.go with lines nothing touched there.
func TestChangedLineRanges_AddedLineThatLooksLikeAFileHeaderIsNotOne(t *testing.T) {
	dir := t.TempDir()
	stepstest.GitCmd(t, dir, "init")

	notes := filepath.Join(dir, "notes.txt")
	order := filepath.Join(dir, "api", "order.go")
	if err := os.MkdirAll(filepath.Dir(order), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notes, []byte(linesFile(20)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(order, []byte(linesFile(20)), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "base")
	baseSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")

	// The injected token names the other changed file, which is the false-green
	// half: notes.txt's hunk numbers would be recorded as api/order.go ranges.
	if err := os.WriteFile(notes, []byte(linesFile(20)+"++ b/api/order.go\nplain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(order, []byte(linesFile(20)+"appended\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "append to both")
	headSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")

	ranges, err := changedLineRanges(context.Background(), dir, baseSHA, headSHA, false, []string{"api/order.go", "notes.txt"})
	if err != nil {
		t.Fatal(err)
	}

	wantOrder := []lineRange{{Start: 21, End: 21}}
	if got := ranges["api/order.go"]; len(got) != 1 || got[0] != wantOrder[0] {
		t.Errorf("api/order.go ranges = %v, want %v", got, wantOrder)
	}
	wantNotes := []lineRange{{Start: 21, End: 22}}
	if got := ranges["notes.txt"]; len(got) != 1 || got[0] != wantNotes[0] {
		t.Errorf("notes.txt ranges = %v, want %v", got, wantNotes)
	}
}

// TestFunctionSpans_LastFunctionEndsAtTheEndOfItsFile bounds the final span.
// A file's last recorded function used to span to math.MaxInt, so any changed
// line below it counted as inside that function, including lines a profile
// built from a different tree put past the end of the file. The worktree's own
// line count is the honest end, and a file the counter cannot read keeps the
// open-ended span rather than certifying nothing.
func TestFunctionSpans_LastFunctionEndsAtTheEndOfItsFile(t *testing.T) {
	functions := []coveredFunction{
		{File: "api/order.go", Name: "Place", Line: 10, Hits: 1},
		{File: "api/order.go", Name: "Refund", Line: 30, Hits: 1},
		{File: "web/cart.go", Name: "Add", Line: 5, Hits: 1},
	}
	lines := map[string]int{"api/order.go": 40}

	spans := functionSpans(functions, func(file string) int { return lines[file] })

	if spans[1] != (lineRange{Start: 30, End: 40}) {
		t.Fatalf("last function span = %+v, want {30 40}", spans[1])
	}
	if spans[2] != (lineRange{Start: 5, End: math.MaxInt}) {
		t.Fatalf("uncounted file span = %+v, want an open-ended range", spans[2])
	}
}

// TestCoveredChangedFunctions_ChangePastTheEndOfTheFileIsNotCovered is the
// same bound seen from the caller, reading the line count off a real worktree.
func TestCoveredChangedFunctions_ChangePastTheEndOfTheFileIsNotCovered(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{"api/order.go": linesFile(40)})
	profile := coverageProfile{
		Files:     []string{"api/order.go"},
		Functions: []coveredFunction{{File: "api/order.go", Name: "Place", Line: 10, Hits: 3}},
	}
	ranges := map[string][]lineRange{"api/order.go": {{Start: 120, End: 122}}}

	if got := coveredChangedFunctions(profile, workDir, ranges); len(got) != 0 {
		t.Fatalf("a change past the end of the file was certified: %v", got)
	}
	inside := map[string][]lineRange{"api/order.go": {{Start: 35, End: 35}}}
	if got := coveredChangedFunctions(profile, workDir, inside); len(got) != 1 {
		t.Fatalf("a change inside the file's last function was not certified: %v", got)
	}
}

// TestCoveredChangedFunctions_ImportPathPrefixedProfileMatches is the false
// park a longer relative profile path produced. Go coverage tooling writes
// records as the package import path plus the file name, which is neither
// absolute nor repository-relative, so requiring an absolute path before a
// longer profile path could match left a fully tested change reading as
// untested. The prefix the record carries is what has to explain the profile
// set against the worktree.
func TestCoveredChangedFunctions_ImportPathPrefixedProfileMatches(t *testing.T) {
	const record = "github.com/owner/repo/internal/pkg/foo.go"
	workDir := trackedRepo(t, map[string]string{"internal/pkg/foo.go": linesFile(40)})
	profile := coverageProfile{
		Files:     []string{record},
		Functions: []coveredFunction{{File: record, Name: "Foo", Line: 10, Hits: 2}},
	}
	ranges := map[string][]lineRange{"internal/pkg/foo.go": {{Start: 11, End: 11}}}

	got := coveredChangedFunctions(profile, workDir, ranges)
	if len(got) != 1 || got[0].Name != "Foo" {
		t.Fatalf("an import-path-prefixed profile record covered nothing: %v", got)
	}
}

// TestCoveredChangedFunctions_TwoLongerProfilePathsRefuseToCertify is the
// safety the length rule used to provide. Two profile records can both end
// with the changed path, and the guard cannot tell which one the change is, so
// neither may certify it.
func TestCoveredChangedFunctions_TwoLongerProfilePathsRefuseToCertify(t *testing.T) {
	profile := coverageProfile{
		Files: []string{"cmd/server/main.go", "cmd/worker/main.go"},
		Functions: []coveredFunction{
			{File: "cmd/server/main.go", Name: "Serve", Line: 10, Hits: 4},
			{File: "cmd/worker/main.go", Name: "Work", Line: 10, Hits: 4},
		},
	}
	ranges := map[string][]lineRange{"main.go": {{Start: 10, End: 12}}}

	if got := coveredChangedFunctions(profile, "", ranges); len(got) != 0 {
		t.Fatalf("two candidate profile paths certified a change neither one names: %v", got)
	}
}

// TestCoveredChangedFunctions_ProfileRelativeToASourceRootMatches is the false
// park on the other side of the same predicate. Cobertura reports filename
// relative to its <sources> root, so a fully tested change reads as untested
// unless the shorter profile path is accepted.
func TestCoveredChangedFunctions_ProfileRelativeToASourceRootMatches(t *testing.T) {
	profile := coverageProfile{
		Files:     []string{"com/foo/Bar.java"},
		Functions: []coveredFunction{{File: "com/foo/Bar.java", Name: "place", Line: 10, Hits: 2}},
	}
	ranges := map[string][]lineRange{"src/main/java/com/foo/Bar.java": {{Start: 11, End: 11}}}

	if got := coveredChangedFunctions(profile, "", ranges); len(got) != 1 {
		t.Fatalf("a runner reporting relative to a source root was read as covering nothing: %v", got)
	}
}

// TestCoveredChangedFunctions_AbsoluteProfilePathUnderTheWorktreeIsExact keeps
// the ordinary case working: a runner naming files by filesystem path inside
// the worktree resolves to the same repository-relative path git reports.
func TestCoveredChangedFunctions_AbsoluteProfilePathUnderTheWorktreeIsExact(t *testing.T) {
	workDir := t.TempDir()
	profile := coverageProfile{
		Files:     []string{filepath.Join(workDir, "api", "order.go")},
		Functions: []coveredFunction{{File: filepath.Join(workDir, "api", "order.go"), Name: "Place", Line: 10, Hits: 1}},
	}
	ranges := map[string][]lineRange{"api/order.go": {{Start: 10, End: 10}}}

	if got := coveredChangedFunctions(profile, workDir, ranges); len(got) != 1 {
		t.Fatalf("an absolute profile path under the worktree matched nothing: %v", got)
	}
}

// TestCoveredChangedFunctions_AmbiguousRelaxedMatchDoesNotCertify pins the
// ambiguity rule. Two changed files can both satisfy one short profile path,
// and the guard cannot tell which one the profile meant, so it must not use
// either to certify.
func TestCoveredChangedFunctions_AmbiguousRelaxedMatchDoesNotCertify(t *testing.T) {
	profile := coverageProfile{
		Files:     []string{"order.go"},
		Functions: []coveredFunction{{File: "order.go", Name: "Place", Line: 10, Hits: 3}},
	}
	ranges := map[string][]lineRange{
		"api/order.go": {{Start: 10, End: 10}},
		"web/order.go": {{Start: 10, End: 10}},
	}

	if got := coveredChangedFunctions(profile, "", ranges); len(got) != 0 {
		t.Fatalf("an ambiguous path match certified the change: %v", got)
	}
}

// TestClassifyChangedFiles_SourceNamedLikeATestPrefixStaysCoverable covers the
// exemption's own false positive. A bare "test_" prefix rule classified this
// repository's test_discovery.go as test code, which dropped it from both
// buckets and let a run that exercised none of it green.
func TestClassifyChangedFiles_SourceNamedLikeATestPrefixStaysCoverable(t *testing.T) {
	profile := coverageProfile{Files: []string{"internal/pipeline/steps/test.go"}}
	changed := []string{"internal/pipeline/steps/test_discovery.go"}

	got, err := classifyChangedFiles(context.Background(), t.TempDir(), profile, changed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Coverable) != 1 || got.Coverable[0] != changed[0] {
		t.Fatalf("Coverable = %v, want %v; a test_ prefix is not a test-file convention", got.Coverable, changed)
	}
}

// TestClassifyChangedFiles_MarkerNamedNonCodeFileDoesNotMakeItsExtensionSource
// keeps the derived source-extension set honest. An Angular repository tracks
// tsconfig.spec.json, whose name carries a test marker but which no runner
// executes; counting json as a source extension parked every package.json edit.
func TestClassifyChangedFiles_MarkerNamedNonCodeFileDoesNotMakeItsExtensionSource(t *testing.T) {
	dir := trackedRepo(t, map[string]string{
		"tsconfig.spec.json": "{}\n",
		"package.json":       "{}\n",
		"docs/load-test.md":  "docs\n",
		"README.md":          "readme\n",
		"src/order.spec.ts":  "spec\n",
		"src/order.ts":       "code\n",
	})

	profile := coverageProfile{Files: []string{"src/order.ts"}}
	got, err := classifyChangedFiles(context.Background(), dir, profile, []string{"package.json", "README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Coverable) != 0 || len(got.Unexplained) != 0 {
		t.Fatalf("a configuration and documentation change must stay exempt, got %+v", got)
	}
}

// TestChangedWithHeadSideLines_DeletionOnlyFileDrops is the deletion half of
// the coverable decision. A file the change only removed lines from carries
// nothing to exercise, so demanding a covered function there asks an agent to
// test code the change deleted.
func TestChangedWithHeadSideLines_DeletionOnlyFileDrops(t *testing.T) {
	ranges := map[string][]lineRange{
		"api/removed.go": {},
		"api/order.go":   {{Start: 10, End: 12}},
	}
	got := changedWithHeadSideLines(ranges, []string{"api/removed.go", "api/order.go", "api/untracked.go"})

	want := []string{"api/order.go", "api/untracked.go"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v; a path with no recorded ranges at all is a new file, not a deletion", got, want)
	}
}

// TestChangedLineRanges_FixModeReadsTheUncommittedRepair discriminates the two
// arms. A fix round's repair is uncommitted, so a diff of baseSHA..headSHA
// cannot see it and the repair's own new coverage would never intersect any
// recorded range, parking the change again for the test it just wrote.
func TestChangedLineRanges_FixModeReadsTheUncommittedRepair(t *testing.T) {
	dir := t.TempDir()
	stepstest.GitCmd(t, dir, "init")
	stepstest.GitCmd(t, dir, "config", "user.name", "test")
	stepstest.GitCmd(t, dir, "config", "user.email", "test@test.com")
	stepstest.GitCmd(t, dir, "config", "commit.gpgsign", "false")
	writeRepoFile(t, dir, "api/order.go", "one\ntwo\nthree\n")
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "base")
	baseSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
	headSHA := baseSHA

	writeRepoFile(t, dir, "api/order.go", "one\ntwo\nthree\nfour\nfive\n")

	committed, err := changedLineRanges(context.Background(), dir, baseSHA, headSHA, false, []string{"api/order.go"})
	if err != nil {
		t.Fatal(err)
	}
	if got := committed["api/order.go"]; len(got) != 1 || got[0].Start != 1 || got[0].End != math.MaxInt {
		t.Fatalf("committed ranges = %+v, want the whole-file fallback; nothing was committed", got)
	}

	fixing, err := changedLineRanges(context.Background(), dir, baseSHA, headSHA, true, []string{"api/order.go"})
	if err != nil {
		t.Fatal(err)
	}
	want := []lineRange{{Start: 4, End: 5}}
	got := fixing["api/order.go"]
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("fix-mode ranges = %+v, want %+v; the uncommitted repair's own lines", got, want)
	}
}

// TestClassifyTestFileName_LanguageConventions is the derivation's input. An
// extension only becomes a source extension of the repository when a tracked
// file matches its language's test-runner convention, so a convention missing
// here silently disables the wrong-project park for that whole stack.
func TestClassifyTestFileName_LanguageConventions(t *testing.T) {
	cases := []struct {
		path string
		want testFileEvidence
	}{
		{"internal/api/order_test.go", testFileByLanguageConvention},
		{"src/lib_test.rs", testFileByLanguageConvention},
		{"tests/test_order.py", testFileByLanguageConvention},
		{"tests/order_test.py", testFileByLanguageConvention},
		{"test/test_order.rb", testFileByLanguageConvention},
		{"src/OrderTest.java", testFileByLanguageConvention},
		{"src/OrderTests.java", testFileByLanguageConvention},
		{"web/order.spec.ts", testFileByLanguageConvention},
		{"web/order.test.tsx", testFileByLanguageConvention},
		{"Tests/OrderServiceTests.cs", testFileByLanguageConvention},
		{"Tests/OrderServiceTest.cs", testFileByLanguageConvention},
		{"src/OrderTest.kt", testFileByLanguageConvention},
		{"Tests/OrderTests.swift", testFileByLanguageConvention},
		{"tests/OrderTest.php", testFileByLanguageConvention},
		{"test/order_test.exs", testFileByLanguageConvention},
		{"src/order_test.cc", testFileByLanguageConvention},
		{"src/order_unittest.cpp", testFileByLanguageConvention},
		{"src/order.cxx", notTestFile},
		{"Services/OrderService.cs", notTestFile},
		{"internal/pipeline/steps/test_discovery.go", notTestFile},
		{"tsconfig.spec.json", testFileByMarker},
		{"docs/load-test.md", testFileByMarker},
		{"README.md", notTestFile},
	}
	for _, tc := range cases {
		if got := classifyTestFileName(tc.path); got != tc.want {
			t.Errorf("classifyTestFileName(%q) = %d, want %d", tc.path, got, tc.want)
		}
	}
}

// TestClassifyChangedFiles_ProductionSourceUnderATestDirectoryIsCoverable
// holds the directory-segment exemption to corroborating evidence: a Go file
// under internal/fixtures is production code the guard must still demand
// coverage for.
func TestClassifyChangedFiles_ProductionSourceUnderATestDirectoryIsCoverable(t *testing.T) {
	dir := trackedRepo(t, map[string]string{
		"internal/fixtures/loader.go":      "package fixtures\n",
		"internal/fixtures/loader_test.go": "package fixtures\n",
	})
	profile := coverageProfile{Files: []string{"internal/other/thing.go"}}

	got, err := classifyChangedFiles(context.Background(), dir, profile, []string{"internal/fixtures/loader.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Coverable) != 1 || got.Coverable[0] != "internal/fixtures/loader.go" {
		t.Fatalf("Coverable = %v, want the production file under fixtures/", got.Coverable)
	}
}

// TestClassifyChangedFiles_GoldenFileUnderATestDirectoryIsExempt keeps the
// segment rule doing the job it was added for: a golden file in a language the
// repository writes no code in has no coverage to demand.
func TestClassifyChangedFiles_GoldenFileUnderATestDirectoryIsExempt(t *testing.T) {
	dir := trackedRepo(t, map[string]string{
		"internal/api/order.go":           "package api\n",
		"internal/api/order_test.go":      "package api\n",
		"internal/api/testdata/case.json": "{}\n",
	})
	profile := coverageProfile{Files: []string{"internal/api/order.go"}}

	got, err := classifyChangedFiles(context.Background(), dir, profile, []string{"internal/api/testdata/case.json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Coverable) != 0 || len(got.Unexplained) != 0 {
		t.Fatalf("a golden file must stay fully exempt, got %+v", got)
	}
}

// TestClassifyChangedFiles_CSharpSourceIsUnexplainedByATypeScriptProfile is
// the C# half of the derivation, the stack the intent names.
func TestClassifyChangedFiles_CSharpSourceIsUnexplainedByATypeScriptProfile(t *testing.T) {
	dir := trackedRepo(t, map[string]string{
		"Services/OrderService.cs":   "namespace S;\n",
		"Tests/OrderServiceTests.cs": "namespace T;\n",
		"web/src/app.ts":             "export const a = 1;\n",
		"web/src/app.spec.ts":        "describe();\n",
	})
	profile := coverageProfile{Files: []string{"web/src/app.ts"}}

	got, err := classifyChangedFiles(context.Background(), dir, profile, []string{"Services/OrderService.cs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Unexplained) != 1 || got.Unexplained[0] != "Services/OrderService.cs" {
		t.Fatalf("Unexplained = %v, want the C# file; a TypeScript-only profile cannot certify it", got.Unexplained)
	}
}

// TestClassifyChangedFiles_MarkerGradedProductionSourceStaysCoverable holds the
// line between the two grades of test-file evidence. pytest collects test_*.py
// and *_test.py and never *-test.py, so payments-test.py is production source
// carrying a marker, and exempting it handed a change in it a free pass from
// the gate that judges it.
func TestClassifyChangedFiles_MarkerGradedProductionSourceStaysCoverable(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{
		"app/billing.py":      "def bill():\n    return 1\n",
		"app/test_billing.py": "def test_bill():\n    assert True\n",
	})
	profile := coverageProfile{Files: []string{"app/billing.py"}}
	changed := []string{"app/payments-test.py", "app/payments_test.py"}

	got, err := classifyChangedFiles(context.Background(), workDir, profile, changed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Coverable) != 1 || got.Coverable[0] != "app/payments-test.py" {
		t.Fatalf("Coverable = %v, want only the marker-graded production file", got.Coverable)
	}
}

// TestRepositorySourceExtensions_CreditsRubyThroughRspec keeps the
// wrong-project park alive for the Ruby stack. rspec's *_spec.rb is the
// dominant convention, and reading only minitest's test_*.rb credited no
// extension in an rspec repository, so a profile describing another project
// entirely produced no unexplained file and the change greened.
func TestRepositorySourceExtensions_CreditsRubyThroughRspec(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{
		"app/models/user.rb": "class User\nend\n",
		"spec/user_spec.rb":  "describe User do\nend\n",
	})
	profile := coverageProfile{Files: []string{"src/Api/OrderService.cs"}}

	got, err := classifyChangedFiles(context.Background(), workDir, profile, []string{"app/models/user.rb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Unexplained) != 1 || got.Unexplained[0] != "app/models/user.rb" {
		t.Fatalf("Unexplained = %v, want the Ruby source a foreign profile never describes", got.Unexplained)
	}
}

// TestCoveredChangedFunctions_DeeperProfilePathDoesNotCertifyAShallowerChange
// is the false green the per-file suffix match allowed. Root main.go beside
// cmd/server/main.go is an ordinary Go layout, and the sole-candidate rule
// cannot see the difference: the nested file was the only candidate, so its
// executed main() certified a change to the root file no test ever ran.
func TestCoveredChangedFunctions_DeeperProfilePathDoesNotCertifyAShallowerChange(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{
		"main.go":            linesFile(20),
		"cmd/server/main.go": linesFile(30),
		"internal/x.go":      linesFile(10),
	})
	profile := coverageProfile{
		Files:     []string{"cmd/server/main.go", "internal/x.go"},
		Functions: []coveredFunction{{File: "cmd/server/main.go", Name: "main", Line: 1, Hits: 3}},
	}
	ranges := map[string][]lineRange{"main.go": {newLineRange(10, 20)}}

	if covered := coveredChangedFunctions(profile, workDir, ranges); len(covered) != 0 {
		t.Fatalf("covered = %+v, want none: cmd/server/main.go's coverage says nothing about root main.go", covered)
	}
}

// TestCoveredChangedFunctions_ImportPathPrefixStillMatches keeps the case the
// relaxation exists for. Every LCOV converter built on `go tool cover` names a
// worktree file by its import path, and that prefix is the same on every
// record, so the set explains itself and the match is safe.
func TestCoveredChangedFunctions_ImportPathPrefixStillMatches(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{
		"internal/pkg/foo.go": linesFile(20),
		"main.go":             linesFile(8),
	})
	profile := coverageProfile{
		Files: []string{"github.com/owner/repo/internal/pkg/foo.go", "github.com/owner/repo/main.go"},
		Functions: []coveredFunction{
			{File: "github.com/owner/repo/internal/pkg/foo.go", Name: "Foo", Line: 5, Hits: 2},
		},
	}
	ranges := map[string][]lineRange{"internal/pkg/foo.go": {newLineRange(5, 9)}}

	if covered := coveredChangedFunctions(profile, workDir, ranges); len(covered) != 1 {
		t.Fatalf("covered = %+v, want the import-path record to match its worktree file", covered)
	}
}

// TestCoveredChangedFunctions_ProfileSetWithNoSingleExplainingPrefixRelaxesNothing
// is the rule that makes the two cases above distinguishable. A runner reports
// deeper paths under ONE prefix, so a set no single prefix explains is not
// that shape and gets no relaxation at all.
func TestCoveredChangedFunctions_ProfileSetWithNoSingleExplainingPrefixRelaxesNothing(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{"pkg/one.go": linesFile(12)})
	profile := coverageProfile{
		Files:     []string{"vendor/pkg/one.go", "other/two.go"},
		Functions: []coveredFunction{{File: "vendor/pkg/one.go", Name: "One", Line: 1, Hits: 4}},
	}
	ranges := map[string][]lineRange{"pkg/one.go": {newLineRange(1, 5)}}

	if covered := coveredChangedFunctions(profile, workDir, ranges); len(covered) != 0 {
		t.Fatalf("covered = %+v, want none: no single prefix explains this profile set", covered)
	}
}

// TestCoveredChangedFunctions_OneFileNamedInTwoSpellingsIsOneCandidate covers
// the canonical deduplication in the sole-candidate rule. Two artifacts in one
// unit directory can name the same source as "./api/order.go" and
// "api/order.go", and both survive the raw-string merge, so counting them as
// two candidates reads a genuinely covered change as ambiguous and parks it.
func TestCoveredChangedFunctions_OneFileNamedInTwoSpellingsIsOneCandidate(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{"api/order.go": linesFile(30)})
	profile := coverageProfile{
		Files:     []string{"./api/order.go", "api/order.go"},
		Functions: []coveredFunction{{File: "api/order.go", Name: "Cancel", Line: 20, Hits: 3}},
	}
	ranges := map[string][]lineRange{"api/order.go": {newLineRange(20, 25)}}

	if covered := coveredChangedFunctions(profile, workDir, ranges); len(covered) != 1 {
		t.Fatalf("covered = %+v, want one: two spellings of one file are one candidate", covered)
	}
}

// TestCoveredChangedFunctions_AProfilePathThatResolvesGrantsNoPrefix pins the
// short circuit derivedProfilePrefix opens with. A profile whose paths already
// resolve in the worktree reports worktree-relative paths, so there is nothing
// to strip; deriving "src" from src/app.go anyway would let a change to the
// root app.go be certified by the tests of a different file with the same
// base name.
func TestCoveredChangedFunctions_AProfilePathThatResolvesGrantsNoPrefix(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{
		"src/app.go": linesFile(40),
		"app.go":     linesFile(40),
	})
	profile := coverageProfile{
		Files:     []string{"src/app.go"},
		Functions: []coveredFunction{{File: "src/app.go", Name: "Run", Line: 10, Hits: 3}},
	}
	ranges := map[string][]lineRange{"app.go": {{Start: 11, End: 11}}}

	if got := coveredChangedFunctions(profile, workDir, ranges); len(got) != 0 {
		t.Fatalf("a change to the root app.go was certified by src/app.go's tests: %v", got)
	}
}

// TestCoveredChangedFunctions_ChangeAboveTheFirstFunctionIsCovered is a struct
// field edit. No profile records a function span over a field declaration, so
// the intersection is empty and the run parked asking for a test that cannot be
// written. The file's tests ran and executed code below the field, which is
// what exercises it.
func TestCoveredChangedFunctions_ChangeAboveTheFirstFunctionIsCovered(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{"api/order.go": linesFile(60)})
	profile := coverageProfile{
		Files:     []string{"api/order.go"},
		Functions: []coveredFunction{{File: "api/order.go", Name: "Cancel", Line: 40, Hits: 3}},
	}
	ranges := map[string][]lineRange{"api/order.go": {{Start: 15, End: 15}}}

	if got := coveredChangedFunctions(profile, workDir, ranges); len(got) != 1 {
		t.Fatalf("a struct field change in a file whose tests ran was reported as uncovered: %v", got)
	}
}

// TestCoveredChangedFunctions_PackageLevelConstIsCovered is the same hole at
// the top of the file, where the imports and the package-level declarations
// live.
func TestCoveredChangedFunctions_PackageLevelConstIsCovered(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{"api/order.go": linesFile(60)})
	profile := coverageProfile{
		Files:     []string{"api/order.go"},
		Functions: []coveredFunction{{File: "api/order.go", Name: "Cancel", Line: 40, Hits: 3}},
	}
	ranges := map[string][]lineRange{"api/order.go": {{Start: 3, End: 4}}}

	if got := coveredChangedFunctions(profile, workDir, ranges); len(got) != 1 {
		t.Fatalf("a package-level const change in a file whose tests ran was reported as uncovered: %v", got)
	}
}

// TestCoveredChangedFunctions_ChangeBelowEveryRecordedFunctionStaysUncovered is
// the false green the rule above must not reopen. Code appended past every
// recorded declaration is code no test reached, and crediting it to the file's
// first function certifies work nothing ran.
func TestCoveredChangedFunctions_ChangeBelowEveryRecordedFunctionStaysUncovered(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{"api/order.go": linesFile(60)})
	profile := coverageProfile{
		Files:     []string{"api/order.go"},
		Functions: []coveredFunction{{File: "api/order.go", Name: "Cancel", Line: 40, Hits: 3}},
	}
	ranges := map[string][]lineRange{"api/order.go": {{Start: 70, End: 72}}}

	if got := coveredChangedFunctions(profile, workDir, ranges); len(got) != 0 {
		t.Fatalf("code below every recorded function and past the file's end was credited: %v", got)
	}
}

// TestCoveredChangedFunctions_PreambleChangeNeedsAnExecutedFunction is the
// other half of the same boundary. A profile that names the file and executed
// nothing in it says the tests did not reach that file at all, so there is no
// run below the preamble to credit it to.
func TestCoveredChangedFunctions_PreambleChangeNeedsAnExecutedFunction(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{"api/order.go": linesFile(60)})
	profile := coverageProfile{
		Files:     []string{"api/order.go"},
		Functions: []coveredFunction{{File: "api/order.go", Name: "Cancel", Line: 40, Hits: 0}},
	}
	ranges := map[string][]lineRange{"api/order.go": {{Start: 15, End: 15}}}

	if got := coveredChangedFunctions(profile, workDir, ranges); len(got) != 0 {
		t.Fatalf("a file whose tests executed nothing certified a change above its first function: %v", got)
	}
}

// TestCoveredChangedFunctionsAcrossUnits_OneUnitsPathsDoNotDisableAnothers is
// the monorepo false park. derivedProfilePrefix answers "" as soon as any path
// in the set already resolves, so merging a JavaScript unit's worktree-relative
// records into a Go unit's import-path-prefixed ones turned off the relaxation
// the Go records need and left a fully tested Go change reading as untested.
func TestCoveredChangedFunctionsAcrossUnits_OneUnitsPathsDoNotDisableAnothers(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{
		"web/src/app.js": linesFile(20),
		"api/order.go":   linesFile(40),
	})
	const record = "github.com/owner/repo/api/order.go"
	web := coverageProfile{
		Files:     []string{"web/src/app.js"},
		Functions: []coveredFunction{{File: "web/src/app.js", Name: "render", Line: 3, Hits: 2}},
	}
	api := coverageProfile{
		Files:     []string{record},
		Functions: []coveredFunction{{File: record, Name: "Cancel", Line: 10, Hits: 4}},
	}
	ranges := map[string][]lineRange{"api/order.go": {{Start: 11, End: 11}}}

	merged := coverageProfile{
		Files:     append(append([]string{}, web.Files...), api.Files...),
		Functions: append(append([]coveredFunction{}, web.Functions...), api.Functions...),
	}
	if got := coveredChangedFunctions(merged, workDir, ranges); len(got) != 0 {
		t.Fatalf("the merged read is expected to lose the Go unit's prefix; it found %v", got)
	}

	got := coveredChangedFunctionsAcrossUnits([]coverageProfile{web, api}, workDir, ranges)
	if len(got) != 1 || got[0].Name != "Cancel" {
		t.Fatalf("the Go unit's own tests did not certify the Go change: %v", got)
	}
}

// TestCoveredChangedFunctions_ZeroHitFunctionAboveAnExecutedOneStaysUncovered
// bounds the preamble credit at the first DECLARED function rather than the
// first executed one. A new function nothing ran, declared above a function
// the tests did run, is the untested code this guard exists to catch.
func TestCoveredChangedFunctions_ZeroHitFunctionAboveAnExecutedOneStaysUncovered(t *testing.T) {
	workDir := trackedRepo(t, map[string]string{"api/order.go": linesFile(60)})
	profile := coverageProfile{
		Files: []string{"api/order.go"},
		Functions: []coveredFunction{
			{File: "api/order.go", Name: "Discount", Line: 10, Hits: 0},
			{File: "api/order.go", Name: "Total", Line: 22, Hits: 3},
		},
	}
	ranges := map[string][]lineRange{"api/order.go": {{Start: 10, End: 20}}}

	if got := coveredChangedFunctions(profile, workDir, ranges); len(got) != 0 {
		t.Fatalf("a zero-hit function was credited to the executed function below it: %v", got)
	}
}
