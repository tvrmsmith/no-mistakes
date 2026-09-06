package steps

import (
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// The vocabulary the vacuous-green guard reads. A test command that exercises
// nothing exits zero, so the exit code alone cannot tell a real pass from a
// misconfigured filter, an empty suite, or a runner that found no tests. The
// guard reads two artifacts the command writes into its unit's coverage
// directory instead: a coverage profile, which says which functions the run
// actually executed, and a test report, which says how many tests it ran.
//
// The types are deliberately format-neutral. Four wire formats reach them
// (LCOV and Cobertura for coverage, JUnit XML and Visual Studio TRX for
// counts), and the guard judges only what they have in common.

// coveredFunction is one function's coverage record, as a profile reported it.
//
// Line is the function's declaration line, 1-based, and 0 when the format did
// not report one. Hits above zero means the run executed the function.
type coveredFunction struct {
	// File is the source file the function lives in, exactly as the profile
	// named it. Coverage tools emit absolute paths as often as
	// repository-relative ones, so matching a changed path against this is the
	// reader's job rather than the parser's.
	File string
	Name string
	Line int
	Hits int
}

// coverageProfile is one parsed coverage artifact.
type coverageProfile struct {
	Functions []coveredFunction
	// Files lists every source file the profile references, in first-seen
	// order and deduplicated. A profile can name a file it recorded no
	// function for, and the guard's coverable-extension rule reads the whole
	// set rather than only the files a function landed in.
	Files []string
}

// testReport is the executed-test count one test-report artifact carried.
//
// Executed excludes skipped tests on purpose: a suite whose every test was
// filtered out exercised nothing, which is the case this guard exists to
// catch, and the runner reports that as tests-minus-skipped rather than as
// zero total.
type testReport struct {
	Executed int
}

// coverageArtifacts is everything one unit's coverage directory yielded.
//
// A directory the command wrote nothing into is not an error: it is the
// evidence that the unit produced no coverage, which the guard reports as a
// configuration problem for the maintainer. Only a read that could not
// complete returns a Go error.
type coverageArtifacts struct {
	Profile coverageProfile
	Report  testReport
	// HasProfile and HasReport record whether any file in the directory parsed
	// as that family at all, which is what separates "wrote nothing" from
	// "wrote something that measured nothing".
	HasProfile bool
	HasReport  bool
	// Unparseable holds the paths of files that classified as a known format
	// and then failed to parse. A file that classified as nothing is ignored,
	// because a test command is free to drop logs and temp files here.
	Unparseable []string
	// Skipped holds the paths of recognised artifacts the reader could not
	// use, because the file exceeded maxCoverageFileBytes or an I/O fault
	// stopped the read. The guard reports these rather than treating an unread
	// directory as an empty one, and routes its verdict to the maintainer
	// park, so nothing the reader read successfully belongs here.
	Skipped []string
	// ScanLimited records that the walk stopped at one of its budgets,
	// maxCoverageFilesScanned or maxCoverageEntriesVisited, so the directory
	// holds artifacts nobody examined. It carries the same verdict Skipped
	// does, since an unexamined artifact may hold the very coverage the guard
	// is about to report as missing, but it is deliberately not a Skipped
	// ENTRY: Skipped names the files it lists, and the whole point here is
	// that the reader never reached them.
	ScanLimited bool
}

// runCoverageDir is where this run's test coverage artifacts belong, always
// outside the worktree so a profile can never enter the branch under
// validation or appear in its pull request diff.
//
// The executor resolves the path once (see pipeline.StepContext.CoverageDir),
// exactly as it does for evidence, so the Test step that writes it and the
// Metrics step that later reads it name the same directory. Unlike evidence,
// coverage is never published: profiles are large, they churn every run, and
// the run's own guard is their only consumer.
func runCoverageDir(sctx *pipeline.StepContext) string {
	if sctx == nil {
		return ""
	}
	return sctx.CoverageDir
}
