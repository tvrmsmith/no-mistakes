package steps

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// lineRange is an inclusive, 1-based line range on the head side of a diff.
type lineRange struct{ Start, End int }

// newLineRange builds a lineRange that cannot be inverted. An inverted range
// intersects nothing, so a caller that produced one would silently discard the
// coverage it describes instead of failing; clamping End up to Start keeps the
// range at its single anchor line.
func newLineRange(start, end int) lineRange {
	if end < start {
		end = start
	}
	return lineRange{Start: start, End: end}
}

// hunkHeaderPattern reads the `@@ -a,b +c,d @@` unified-diff hunk header.
// Only the head side (+c,d) matters here: the guard asks which lines the
// change added or kept, never which lines it removed.
var hunkHeaderPattern = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// changedLineRanges reads the head-side line ranges a diff actually touched,
// per repository-relative path. A coverage profile can mark a function
// executed for reasons that have nothing to do with this change, so the
// vacuous-green guard needs the exact lines a change wrote, not merely the
// file it wrote them in.
//
// A path in `changed` the diff never names is the untracked file fix mode
// reports (`changedPathsSince` adds those, and plain `git diff` cannot see
// them): every line of a file git has no history for is new, so it gets the
// whole-file range instead of no range at all.
func changedLineRanges(ctx context.Context, workDir, baseSHA, headSHA string, fixing bool, changed []string) (map[string][]lineRange, error) {
	// core.quotePath=false because the default C-quotes any path carrying a
	// non-ASCII, control, quote, or backslash byte. The quoted token never
	// matches the plain path in `changed`, so the file fell through to the
	// whole-file fallback below and every function in it counted as changed.
	//
	// The prefixes are pinned on the invocation because the operator's own git
	// configuration still applies: diff.mnemonicPrefix renders the head side as
	// "+++ w/foo.go" and diff.noprefix drops the prefix entirely, so the
	// TrimPrefix below left a path no entry in `changed` matched and the file
	// fell through to the whole-file fallback, downgrading the per-function
	// check to a per-file one.
	args := []string{"-c", "core.quotePath=false", "diff", "-U0", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/"}
	if fixing {
		args = append(args, baseSHA)
	} else {
		args = append(args, baseSHA+".."+headSHA)
	}
	out, err := git.Run(ctx, workDir, args...)
	if err != nil {
		return nil, fmt.Errorf("diff changed lines: %w", err)
	}

	ranges := make(map[string][]lineRange)
	var currentPath string
	var oldPath string
	tracking := false
	// A file header is only read inside a `diff --git` section and only before
	// that section's first hunk. With -U0 every body line carries a leading +
	// or -, so an added line whose own content starts with "++ " renders as
	// "+++ <content>"; reading that as a header attributed the rest of the file
	// to whatever path the line named, which both loses the real file's ranges
	// and credits another file's coverage for lines never touched there.
	inHeader := false
	sawOldHeader := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHeader = true
			sawOldHeader = false
			tracking = false
			currentPath = ""
			oldPath = ""
		case inHeader && !sawOldHeader && strings.HasPrefix(line, "--- "):
			sawOldHeader = true
			oldPath = strings.TrimPrefix(strings.TrimPrefix(line, "--- "), "a/")
		case inHeader && sawOldHeader && strings.HasPrefix(line, "+++ "):
			inHeader = false
			target := strings.TrimPrefix(line, "+++ ")
			if target == "/dev/null" {
				// The head side has no file, so the section is entirely
				// deletions. `git diff --name-only` still lists the removed
				// path, so it is recorded under its old name with no ranges:
				// that is how a caller tells "nothing left here to exercise"
				// from "not touched at all".
				tracking = false
				if oldPath != "" && oldPath != "/dev/null" {
					if _, exists := ranges[oldPath]; !exists {
						ranges[oldPath] = []lineRange{}
					}
				}
				continue
			}
			currentPath = strings.TrimPrefix(target, "b/")
			tracking = true
			// A file with only deletions still needs its key present with an
			// empty slice, because "no ranges recorded" and "not touched at
			// all" are different answers for the guard.
			if _, exists := ranges[currentPath]; !exists {
				ranges[currentPath] = []lineRange{}
			}
		case strings.HasPrefix(line, "@@ "):
			inHeader = false
			m := hunkHeaderPattern.FindStringSubmatch(line)
			if !tracking || m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			count := 1
			if m[2] != "" {
				count, _ = strconv.Atoi(m[2])
			}
			if count == 0 {
				// A hunk that only deletes lines adds no line on the head
				// side, so it contributes no range.
				continue
			}
			ranges[currentPath] = append(ranges[currentPath], newLineRange(start, start+count-1))
		}
	}

	for _, path := range changed {
		if _, exists := ranges[path]; !exists {
			ranges[path] = []lineRange{{Start: 1, End: math.MaxInt}}
		}
	}

	return ranges, nil
}

// normalizeCoveragePath brings a path from either side of the match (a
// changed path from git, a File a coverage tool wrote) to one comparable
// form. Coverage tools emit absolute paths, drive-letter paths, `./`-prefixed
// paths, and backslash separators as readily as clean repository-relative
// ones, so the match has to tolerate all of them.
func normalizeCoveragePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return strings.TrimPrefix(p, "./")
}

// canonicalCoveragePath brings a path to the worktree-relative form both
// sides of a match are compared in. A profile reporting absolute paths under
// the worktree is the common case, and stripping that prefix turns what would
// be a guessed suffix relationship into an exact one.
func canonicalCoveragePath(p, workDir string) string {
	normalized := normalizeCoveragePath(p)
	if workDir == "" {
		return normalized
	}
	root := strings.TrimSuffix(normalizeCoveragePath(filepath.ToSlash(workDir)), "/")
	if root == "" {
		return normalized
	}
	if trimmed := strings.TrimPrefix(normalized, root+"/"); trimmed != normalized {
		return trimmed
	}
	return normalized
}

// coverageMatchStrength grades how a profile-reported path relates to a
// changed path once both are canonical.
type coverageMatchStrength int

const (
	coverageMatchNone coverageMatchStrength = iota
	// coverageMatchRelaxed is a whole-segment suffix relationship rather than
	// equality. Two genuinely different files can satisfy it, so a relaxed
	// match certifies only when it is the sole candidate.
	coverageMatchRelaxed
	coverageMatchExact
)

// coverageMatcher owns "does this coverage profile path name this changed
// file", read from both directions of the verdict. It is built once per
// profile set because the profile-longer direction is decided by the set as a
// whole rather than by one pair of paths.
type coverageMatcher struct {
	files   []string
	workDir string
	// prefix is the leading path prefix a runner reporting deeper-than-
	// worktree paths puts in front of every file it names, derived from the
	// whole set by derivedProfilePrefix, and "" when no single prefix explains
	// the set.
	prefix string
}

func newCoverageMatcher(profileFiles []string, workDir string) coverageMatcher {
	return coverageMatcher{
		files:   profileFiles,
		workDir: workDir,
		prefix:  derivedProfilePrefix(profileFiles, workDir),
	}
}

// match grades how a profile-reported path relates to a changed path.
//
// A profile path SHORTER than the changed path is a runner reporting relative
// to a source root, which Cobertura does by construction: a change at
// src/main/java/com/foo/Bar.java is reported as com/foo/Bar.java. That stays a
// per-file suffix relationship, carried by the ambiguity rule.
//
// A profile path LONGER than the changed path is not judged per file at all. A
// bare suffix test there let any deeper path certify a shallower change, so a
// root main.go borrowed cmd/server/main.go's coverage whenever it was the sole
// candidate, which is the vacuous green this guard exists to refuse. A runner
// that reports deeper paths uses ONE prefix for every file it names (an import
// path, a source root, a workspace root), so the prefix is derived once from
// the whole set and the match must go through it exactly.
func (m coverageMatcher) match(profileFile, changedPath string) coverageMatchStrength {
	profile := canonicalCoveragePath(profileFile, m.workDir)
	changed := canonicalCoveragePath(changedPath, m.workDir)
	if profile == "" || changed == "" {
		return coverageMatchNone
	}
	if profile == changed {
		return coverageMatchExact
	}
	if strings.HasSuffix(changed, "/"+profile) {
		return coverageMatchRelaxed
	}
	if m.prefix != "" && profile == m.prefix+"/"+changed {
		return coverageMatchRelaxed
	}
	return coverageMatchNone
}

// derivedProfilePrefix returns the one leading path prefix that explains a
// profile set reporting paths deeper than the worktree's own, or "" when no
// single prefix does.
//
// A prefix explains the set only when stripping it from EVERY path the profile
// names leaves a path that resolves to a real file in the worktree. The
// longest such prefix wins, so a Go profile naming
// github.com/owner/repo/internal/pkg/foo.go still matches internal/pkg/foo.go
// while a profile naming cmd/server/main.go and internal/x.go derives nothing
// and can never certify a root-level main.go.
//
// A profile that already names a real worktree file reports worktree-relative
// paths, so there is nothing to strip and no relaxation to grant.
func derivedProfilePrefix(profileFiles []string, workDir string) string {
	if workDir == "" {
		return ""
	}
	resolved := map[string]bool{}
	resolves := func(rel string) bool {
		if known, seen := resolved[rel]; seen {
			return known
		}
		known := resolvesInWorktree(workDir, rel)
		resolved[rel] = known
		return known
	}

	var canonical []string
	for _, f := range profileFiles {
		p := canonicalCoveragePath(f, workDir)
		if p == "" {
			continue
		}
		if resolves(p) {
			return ""
		}
		canonical = append(canonical, p)
	}
	if len(canonical) == 0 {
		return ""
	}

	segments := strings.Split(canonical[0], "/")
	for depth := len(segments) - 1; depth > 0; depth-- {
		prefix := strings.Join(segments[:depth], "/")
		explains := true
		for _, p := range canonical {
			rest := strings.TrimPrefix(p, prefix+"/")
			if rest == p || !resolves(rest) {
				explains = false
				break
			}
		}
		if explains {
			return prefix
		}
	}
	return ""
}

// resolvesInWorktree answers whether relPath names a regular file inside
// workDir, under the same containment rule countFileLines applies.
func resolvesInWorktree(workDir, relPath string) bool {
	if workDir == "" || relPath == "" {
		return false
	}
	full := filepath.Join(workDir, filepath.FromSlash(relPath))
	root := filepath.Clean(workDir)
	if cleaned := filepath.Clean(full); cleaned != root && !strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
		return false
	}
	info, err := os.Stat(full)
	return err == nil && info.Mode().IsRegular()
}

// sole returns the canonical profile path that names changedPath, and whether
// the profiles answered that question at all. An exact match wins outright;
// otherwise a relaxed match is used only when it is the only candidate,
// because two candidates mean the guard cannot tell which file the profile
// meant and an ambiguous match must never certify a change.
//
// Candidates are deduplicated by their canonical form, so a profile reporting
// the same file as both "./a.go" and "a.go" stays one candidate rather than
// reading as an ambiguity.
func (m coverageMatcher) sole(changedPath string) (string, bool) {
	var exact, relaxed []string
	seen := map[string]bool{}
	for _, f := range m.files {
		strength := m.match(f, changedPath)
		if strength == coverageMatchNone {
			continue
		}
		canonical := canonicalCoveragePath(f, m.workDir)
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		if strength == coverageMatchExact {
			exact = append(exact, canonical)
		} else {
			relaxed = append(relaxed, canonical)
		}
	}
	if len(exact) == 1 {
		return exact[0], true
	}
	if len(exact) > 1 {
		return "", false
	}
	if len(relaxed) == 1 {
		return relaxed[0], true
	}
	return "", false
}

// changedRangesByProfileFile pairs every profile-reported source path with the
// changed line ranges it describes, keyed by canonical path. It is the single
// owner of the profile-to-change relationship, so the two halves of the verdict
// cannot disagree about which file a profile meant.
//
// Ambiguity refuses in BOTH directions. A changed path with more than one
// candidate profile path contributes nothing (coverageMatcher.sole), and a
// profile path that ends up carrying more than one changed path is dropped
// here: one short Cobertura filename can sit under two changed source roots,
// and unioning their ranges would let coverage of one certify the other.
func changedRangesByProfileFile(matcher coverageMatcher, ranges map[string][]lineRange) map[string][]lineRange {
	type pairing struct {
		ranges  []lineRange
		sources int
	}
	paired := map[string]*pairing{}
	for changedPath, r := range ranges {
		key, ok := matcher.sole(changedPath)
		if !ok {
			continue
		}
		entry := paired[key]
		if entry == nil {
			entry = &pairing{}
			paired[key] = entry
		}
		entry.ranges = append(entry.ranges, r...)
		entry.sources++
	}
	resolved := make(map[string][]lineRange, len(paired))
	for key, entry := range paired {
		if entry.sources != 1 {
			continue
		}
		resolved[key] = entry.ranges
	}
	return resolved
}

// functionSpans infers the line span of every function in the profile, in
// the same order as profile.Functions. Most coverage formats report only a
// function's declaration line, not its extent, so the guard estimates a
// function's span as running to the line before the next function declared
// in the same file, with the last function in a file running to the end.
//
// "The end" is the file's own last line, read through fileLines, not infinity.
// A change that appends code the profile's producer emitted no function record
// for (a closure, a method a Cobertura producer skipped, generated code) sits
// past every recorded declaration, and an unbounded last span swallowed those
// lines so the preceding covered function reported a hit for code no test
// reached. fileLines answers 0 when it cannot read the file, and an unknown
// bound stays unbounded rather than guessing a shorter one.
//
// A function whose Line is 0 (a format that reported no declaration line)
// cannot be pinned to any hunk, so it spans the whole file rather than
// silently dropping out of every range check.
//
// Two functions can share a declaration line: an LCOV profile reports a
// generic and each of its instantiations, and a decorated or overloaded
// declaration reports the wrapper beside the wrapped. The next span therefore
// starts at the next STRICTLY GREATER line, so neither sibling gets a span
// ending before it starts.
func functionSpans(functions []coveredFunction, fileLines func(file string) int) []lineRange {
	spans := make([]lineRange, len(functions))

	byFile := make(map[string][]int)
	for i, fn := range functions {
		byFile[fn.File] = append(byFile[fn.File], i)
	}

	for file, indices := range byFile {
		lastLine := math.MaxInt
		if fileLines != nil {
			if counted := fileLines(file); counted > 0 {
				lastLine = counted
			}
		}
		sort.SliceStable(indices, func(a, b int) bool {
			return functions[indices[a]].Line < functions[indices[b]].Line
		})
		for pos, idx := range indices {
			fn := functions[idx]
			if fn.Line == 0 {
				spans[idx] = newLineRange(1, lastLine)
				continue
			}
			end := lastLine
			for next := pos + 1; next < len(indices); next++ {
				if nextLine := functions[indices[next]].Line; nextLine > fn.Line {
					end = nextLine - 1
					break
				}
			}
			spans[idx] = newLineRange(fn.Line, end)
		}
	}

	return spans
}

// worktreeLineCounter returns a memoized line-count lookup for profile-reported
// paths, resolved against the worktree. A path that does not resolve to a file
// inside workDir counts as unknown (0), which leaves the span unbounded.
func worktreeLineCounter(workDir string) func(string) int {
	counts := map[string]int{}
	return func(file string) int {
		if counted, known := counts[file]; known {
			return counted
		}
		counted := countFileLines(workDir, canonicalCoveragePath(file, workDir))
		counts[file] = counted
		return counted
	}
}

// countFileLines returns the number of lines in the worktree file at relPath,
// or 0 when there is no such readable file. A final line without a trailing
// newline still counts.
func countFileLines(workDir, relPath string) int {
	if workDir == "" || relPath == "" {
		return 0
	}
	full := filepath.Join(workDir, filepath.FromSlash(relPath))
	root := filepath.Clean(workDir)
	if cleaned := filepath.Clean(full); cleaned != root && !strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
		return 0
	}
	f, err := os.Open(full)
	if err != nil {
		return 0
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	lines := 0
	endedWithNewline := true
	read := 0
	for {
		n, err := f.Read(buf)
		if n > 0 {
			read += n
			lines += bytes.Count(buf[:n], []byte("\n"))
			endedWithNewline = buf[n-1] == '\n'
		}
		if err != nil {
			break
		}
	}
	if read == 0 {
		return 0
	}
	if !endedWithNewline {
		lines++
	}
	return lines
}

// coveredChangedFunctions returns the functions the profile recorded as
// executed whose inferred span intersects a changed range. This is the
// vacuous-green check itself: a test command that exits zero without
// exercising the change looks identical to a real pass until this
// intersection is empty.
func coveredChangedFunctions(profile coverageProfile, workDir string, ranges map[string][]lineRange) []coveredFunction {
	paired := changedRangesByProfileFile(newCoverageMatcher(profile.Files, workDir), ranges)
	if len(paired) == 0 {
		return nil
	}
	spans := functionSpans(profile.Functions, worktreeLineCounter(workDir))

	var covered []coveredFunction
	contributed := map[string]bool{}
	// The earliest declaration each named file recorded, counted whether or not
	// a test reached it, which is what bounds the file's preamble below.
	firstDeclared := map[string]coveredFunction{}
	// The earliest executed declaration, which is the record the preamble
	// credit is reported under. Whether that credit is granted at all reads
	// firstDeclared, so a brand-new zero-hit function above it still parks.
	firstExecuted := map[string]coveredFunction{}
	hasExecuted := map[string]bool{}
	for _, fn := range profile.Functions {
		if fn.Line <= 0 {
			continue
		}
		key := canonicalCoveragePath(fn.File, workDir)
		if _, matched := paired[key]; !matched {
			continue
		}
		if earliest, seen := firstDeclared[key]; !seen || fn.Line < earliest.Line {
			firstDeclared[key] = fn
		}
	}
	for i, fn := range profile.Functions {
		if fn.Hits <= 0 {
			continue
		}
		key := canonicalCoveragePath(fn.File, workDir)
		fileRanges, matched := paired[key]
		if !matched {
			continue
		}
		hasExecuted[key] = true
		if fn.Line > 0 {
			if earliest, seen := firstExecuted[key]; !seen || fn.Line < earliest.Line {
				firstExecuted[key] = fn
			}
		}
		span := spans[i]
		for _, r := range fileRanges {
			if span.Start <= r.End && r.Start <= span.End {
				covered = append(covered, fn)
				contributed[key] = true
				break
			}
		}
	}

	// Not every changed line lives in a function. A struct field, a
	// package-level constant, an import: a profile records no function span
	// there, so the intersection above is empty and the run parks asking for a
	// test that cannot be written. When the profile names the file and the
	// tests executed something in it, that preamble is exercised by whatever
	// runs below it, so the change counts as covered.
	//
	// The credit stops at the file's first recorded declaration, executed or
	// not. Extending it to the first EXECUTED one would swallow a brand-new
	// zero-hit function declared above it, which is exactly the untested code
	// this guard exists to catch, and green it under the name of a function
	// that never touches it. A changed range intersecting no function span at
	// all is the case this credit is for; one landing inside a recorded
	// declaration, even a zero-hit declaration, is not.
	for _, key := range sortedKeys(firstExecuted) {
		if contributed[key] || !hasExecuted[key] {
			continue
		}
		limit := firstDeclared[key].Line
		for _, r := range paired[key] {
			if r.End < limit {
				covered = append(covered, firstExecuted[key])
				break
			}
		}
	}
	return covered
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// coveredChangedFunctionsAcrossUnits answers the same question over the
// profiles several test units wrote, one unit at a time.
//
// Merging the profiles first and asking once would let one unit's paths decide
// how another unit's paths are read: derivedProfilePrefix answers "" as soon as
// any path in the set already resolves in the worktree, so a JavaScript unit
// reporting src/app.js turns off the prefix relaxation a Go unit reporting
// github.com/owner/repo/api/order.go depends on, and the changed Go file then
// matches nothing. Each unit's profile carries its own reporting convention, so
// each derives its own prefix.
func coveredChangedFunctionsAcrossUnits(profiles []coverageProfile, workDir string, ranges map[string][]lineRange) []coveredFunction {
	var covered []coveredFunction
	for _, profile := range profiles {
		covered = append(covered, coveredChangedFunctions(profile, workDir, ranges)...)
	}
	return covered
}

// fileExtension returns the final dot segment of a path's base name, or ""
// when it has none, so an extensionless file (a Makefile, a Dockerfile) is
// compared as its own category rather than as a wildcard match.
func fileExtension(path string) string {
	normalized := normalizeCoveragePath(path)
	base := normalized
	if idx := strings.LastIndex(normalized, "/"); idx != -1 {
		base = normalized[idx+1:]
	}
	idx := strings.LastIndex(base, ".")
	if idx == -1 {
		return ""
	}
	return base[idx+1:]
}

// testDirectorySegments are the directory names that hold test material of any
// kind, whether test code or the golden files it reads. A repository that
// writes production source in one of them is ordinary - internal/fixtures owns
// a loader, spec owns a parser, test owns a harness - so the segment is
// corroborating evidence only and never exempts a file on its own; see
// looksLikeTestPath.
var testDirectorySegments = map[string]bool{
	"test": true, "tests": true, "__tests__": true,
	"spec": true, "specs": true,
	"testdata": true, "fixtures": true,
}

// looksLikeTestPath answers whether a path is test code by the naming
// conventions repositories actually use, rather than by any language-specific
// list of source extensions. The guard needs this because a change that adds
// only tests writes files a coverage profile never reports as covered source,
// and demanding a covered function for those files parks the very change that
// answers the missing-test finding.
//
// Only a LANGUAGE CONVENTION decides on its own. A marker grade says the file
// is a test but nothing about the language it is written in, and the markers
// match any extension, so a production payments-test.py (pytest collects
// test_*.py and *_test.py, never *-test.py) was exempted from the gate that
// judges it. A test-ish DIRECTORY is weaker evidence too and decides only for
// a file in a language the repository writes no code in; see
// hasTestDirectorySegment and its caller in classifyChangedFiles.
func looksLikeTestPath(path string) bool {
	return classifyTestFileName(normalizeCoveragePath(path)) == testFileByLanguageConvention
}

// hasTestDirectorySegment reports whether a path sits under a directory
// reserved for test material. It is corroborating evidence only; see
// looksLikeTestPath.
func hasTestDirectorySegment(path string) bool {
	return hasSegment(normalizeCoveragePath(path), testDirectorySegments)
}

func hasSegment(normalized string, names map[string]bool) bool {
	segments := strings.Split(normalized, "/")
	for _, segment := range segments[:len(segments)-1] {
		if names[strings.ToLower(segment)] {
			return true
		}
	}
	return false
}

// changedFileCoverage splits a change's files into the ones a coverage profile
// is expected to describe and the ones it left unexplained.
type changedFileCoverage struct {
	// Coverable are the changed files a covered function is required for.
	Coverable []string
	// Unexplained are changed files that are source code by this repository's
	// own conventions but carry an extension no profile mentions, which means
	// the commands that ran describe a different project than the change.
	Unexplained []string
}

// names answers whether a profile reported this changed file, reading the same
// relationship coveredChangedFunctions does so the two halves of the verdict
// cannot disagree. An ambiguous relaxed match is no answer, so it counts as
// not named.
func (m coverageMatcher) names(path string) bool {
	_, named := m.sole(path)
	return named
}

// classifyChangedFiles decides which changed files the guard may demand
// coverage for. Demanding a covered changed function for every change would
// park a run over a README or a YAML file no coverage tool ever instruments,
// so a file whose extension no profile mentions is normally exempt. Two
// refinements keep that exemption honest.
//
// A changed file the profiles never name and this repository's conventions
// mark as test code drops out of both buckets: a change that only adds tests
// would otherwise park, asking for the tests it just wrote.
//
// A changed file whose extension no profile mentions but which is source code
// in this repository is reported as unexplained instead of exempt. The
// repository's source extensions are read from its own tracked files rather
// than from a hardcoded list: every extension carried by a file this repository
// names as test code is an extension this repository writes code in.
func classifyChangedFiles(ctx context.Context, workDir string, profile coverageProfile, changed []string) (changedFileCoverage, error) {
	var result changedFileCoverage
	if len(profile.Files) == 0 {
		return result, nil
	}

	exts := make(map[string]bool, len(profile.Files))
	for _, f := range profile.Files {
		exts[strings.ToLower(fileExtension(f))] = true
	}

	// The repository's own source extensions answer two questions, and both
	// read this one memoized value: whether a file under a testdata or
	// fixtures directory is a golden file or production source, and whether an
	// unmatched file is unexplained source. It stays lazy because a repository
	// with no such directory and no unmatched file never needs it.
	var sourceExts sourceExtensions
	loadSourceExts := func() error {
		if sourceExts.all != nil {
			return nil
		}
		loaded, err := repositorySourceExtensions(ctx, workDir)
		if err != nil {
			return err
		}
		sourceExts = loaded
		return nil
	}

	matcher := newCoverageMatcher(profile.Files, workDir)

	var unmatched []string
	for _, path := range changed {
		// A test file is what exercises the change, not something the change
		// needs exercised, so it is neither coverable nor unexplained however
		// its extension compares. The one exception is a profile that really
		// does report it, which is coverage evidence for the file itself.
		exempt := looksLikeTestPath(path)
		if !exempt && hasTestDirectorySegment(path) {
			if err := loadSourceExts(); err != nil {
				return changedFileCoverage{}, err
			}
			exempt = !sourceExts.conventionPaired[strings.ToLower(fileExtension(path))]
		}
		if exempt && !matcher.names(path) {
			continue
		}
		if exts[strings.ToLower(fileExtension(path))] {
			result.Coverable = append(result.Coverable, path)
			continue
		}
		unmatched = append(unmatched, path)
	}

	if len(result.Coverable) > 0 || len(unmatched) == 0 {
		return result, nil
	}

	if err := loadSourceExts(); err != nil {
		return changedFileCoverage{}, err
	}
	for _, path := range unmatched {
		if sourceExts.all[strings.ToLower(fileExtension(path))] {
			result.Unexplained = append(result.Unexplained, path)
		}
	}
	return result, nil
}

// codeTestDirectorySegments are the directory names that hold test CODE, the
// subset of testDirectorySegments that excludes the two which hold the data a
// test reads. A .json under testdata is a golden file; a .rs under tests is a
// test binary, and its extension is one the repository writes code in.
var codeTestDirectorySegments = map[string]bool{
	"test": true, "tests": true, "__tests__": true,
	"spec": true, "specs": true,
}

// repositorySourceExtensions reads the extensions this repository writes code
// in, derived from its own tracked files rather than from a list of languages
// that would go stale the moment someone adds one. Three rules credit an
// extension, and each one pairs test-side evidence with a non-test file, since
// a repository's tests are written in the same language as the code they cover
// while test-side evidence alone would admit an extension no code is written
// in and park a change that touches only such a file.
//
// The first rule is the narrowest: the repository carries a language-convention
// test file and a non-test file with the same extension, which is Go, Python,
// Java, and every stack whose tests sit beside their source. The test-side
// evidence must come from a LANGUAGE convention rather than a bare marker,
// because a marker fires on files no runner ever executes: tsconfig.spec.json
// would otherwise make json a source extension and park an Angular repository
// over a package.json edit, and a tracked docs/load-test.md would do the same
// to every README.
//
// That first rule is dead for a stack whose test files carry neither the
// extension nor the naming convention of the code they cover, so a second one
// reads the LAYOUT instead of the file name. A LANGUAGE-CONVENTION test file
// under a test directory whose stem names a non-test file outside every test
// directory credits that file's extension, which closes Elixir, where
// test/orders_test.exs pairs with lib/orders.ex once the convention's own
// affix is stripped. Idiomatic Rust carries no convention at all, so
// tests/orders.rs pairs by its bare stem, and that pairing demands the SAME
// extension and a counterpart under a conventional source directory: without
// both, a tracked tests/README.md pairs with the root README.md and parks
// every documentation-only change.
//
// The two rules answer different questions, so they are returned separately.
// Whether a file sitting under a test directory is production source or test
// material is decided by the strict set only. A layout pairing says the
// repository writes code in that language; it does not say that this
// particular file under tests/ is production code, and reading it that way
// asked a Rust contributor to cover the integration test they just added.
type sourceExtensions struct {
	// conventionPaired is the strict set: a language-convention test file and
	// a non-test file share the extension.
	conventionPaired map[string]bool
	// all adds the layout pairings, and is what the wrong-project check reads.
	all map[string]bool
}

func repositorySourceExtensions(ctx context.Context, workDir string) (sourceExtensions, error) {
	out, err := git.Run(ctx, workDir, "-c", "core.quotePath=false", "ls-files")
	if err != nil {
		return sourceExtensions{}, fmt.Errorf("list tracked files: %w", err)
	}
	testExts := map[string]bool{}
	nonTestExts := map[string]bool{}
	conventionStems := map[string]bool{}
	layoutPairs := map[string]bool{}
	outsideStemExts := map[string][]string{}
	sourcePairs := map[string]bool{}
	for _, path := range strings.Split(out, "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		normalized := normalizeCoveragePath(path)
		ext := strings.ToLower(fileExtension(normalized))
		if ext == "" {
			continue
		}
		inTestDir := hasTestDirectorySegment(normalized)
		kind := classifyTestFileName(normalized)
		switch kind {
		case testFileByLanguageConvention:
			testExts[ext] = true
		case notTestFile:
			nonTestExts[ext] = true
		}
		if inTestDir && hasSegment(normalized, codeTestDirectorySegments) {
			if kind == testFileByLanguageConvention {
				stem := conventionTestStem(normalized)
				if stem == "" {
					stem = pathStem(normalized)
				}
				conventionStems[stem] = true
			}
			layoutPairs[stemExtKey(pathStem(normalized), ext)] = true
		}
		if kind == notTestFile && !inTestDir {
			outsideStemExts[pathStem(normalized)] = append(outsideStemExts[pathStem(normalized)], ext)
			if hasSegment(normalized, sourceDirectorySegments) {
				sourcePairs[stemExtKey(pathStem(normalized), ext)] = true
			}
		}
	}
	strict := map[string]bool{}
	for ext := range testExts {
		if nonTestExts[ext] {
			strict[ext] = true
		}
	}
	all := make(map[string]bool, len(strict))
	for ext := range strict {
		all[ext] = true
	}
	for stem := range conventionStems {
		for _, ext := range outsideStemExts[stem] {
			all[ext] = true
		}
	}
	for key := range layoutPairs {
		if sourcePairs[key] {
			all[extOfStemExtKey(key)] = true
		}
	}
	return sourceExtensions{conventionPaired: strict, all: all}, nil
}

// sourceDirectorySegments are the directory names a repository puts production
// code under by convention. The bare-stem pairing below reads them because a
// tests/README.md matches a root README.md exactly as tests/orders.rs matches
// src/orders.rs, and only one of those pairs says the repository writes code
// in that language.
var sourceDirectorySegments = map[string]bool{
	"src": true, "lib": true, "app": true, "pkg": true,
	"internal": true, "source": true, "cmd": true,
}

func stemExtKey(stem, ext string) string { return stem + "\x00" + ext }

func extOfStemExtKey(key string) string {
	if idx := strings.LastIndex(key, "\x00"); idx != -1 {
		return key[idx+1:]
	}
	return ""
}

// pathStem is a path's base name without its final extension, lowercased, so
// two files of the same name in different languages compare equal.
func pathStem(normalized string) string {
	base := normalized
	if idx := strings.LastIndex(normalized, "/"); idx != -1 {
		base = normalized[idx+1:]
	}
	if idx := strings.LastIndex(base, "."); idx > 0 {
		base = base[:idx]
	}
	return strings.ToLower(base)
}

// conventionTestStem is the stem a language-convention test file names, with
// the convention's own affix removed, or "" when stripping the affix leaves
// nothing. It is what pairs test/foo_test.exs with lib/foo.ex.
var conventionTestAffixes = []string{"_unittest", "_test", "_spec", ".test", ".spec", "-test", "-spec", "test", "tests"}

func conventionTestStem(normalized string) string {
	stem := pathStem(normalized)
	if trimmed := strings.TrimPrefix(stem, "test_"); trimmed != stem {
		return trimmed
	}
	for _, affix := range conventionTestAffixes {
		if trimmed := strings.TrimSuffix(stem, affix); trimmed != stem {
			return trimmed
		}
	}
	return ""
}

// rangesForPaths narrows a diff's ranges to one path set, so a single set of
// paths drives every half of a verdict. changedLineRanges parses the whole
// diff rather than a pathspec-limited one, so its map still describes paths
// the caller excluded, and a caller that filtered its path list but kept the
// full map judges two different changes at once.
func rangesForPaths(ranges map[string][]lineRange, paths []string) map[string][]lineRange {
	kept := make(map[string][]lineRange, len(paths))
	for _, path := range paths {
		if r, recorded := ranges[path]; recorded {
			kept[path] = r
		}
	}
	return kept
}

// changedWithHeadSideLines drops the changed paths whose diff recorded no
// head-side line. A commit that only deletes lines from a file leaves nothing
// there to exercise, so demanding a covered function for it charges an agent
// fix round with writing a test for code the change removed.
func changedWithHeadSideLines(ranges map[string][]lineRange, changed []string) []string {
	kept := make([]string, 0, len(changed))
	for _, path := range changed {
		if r, recorded := ranges[path]; recorded && len(r) == 0 {
			continue
		}
		kept = append(kept, path)
	}
	return kept
}
