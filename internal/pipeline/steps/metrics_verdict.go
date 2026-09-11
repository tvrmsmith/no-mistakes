package steps

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// metricsFunction is one measured function as the metrics command reports it.
type metricsFunction struct {
	File       string   `json:"file"`
	Function   string   `json:"function"`
	Line       int      `json:"line"`
	Score      float64  `json:"score"`
	Complexity *int     `json:"complexity,omitempty"`
	Coverage   *float64 `json:"coverage,omitempty"`
}

// metricsReport is the JSON contract the metrics command emits on stdout.
type metricsReport struct {
	Metric    string             `json:"metric"`
	Functions *[]metricsFunction `json:"functions"`
	Summary   string             `json:"summary,omitempty"`
}

// metricsVerdict is what the step gates on and what the evidence file records.
type metricsVerdict struct {
	Breached  bool
	Breaches  []metricsFunction // score descending, then file, then line
	Measured  int
	Exempted  int
	Threshold float64
	Metric    string
	Summary   string
	FromJSON  bool // false means the exit code was the verdict, a materially weaker gate
	ExitCode  int
}

// evaluateMetricsOutput reads the metrics command's stdout and exit code into
// the verdict the step gates on. It is pure, so a test states a command's
// output rather than installing a metrics tool.
//
// stdout is the command's standard output ALONE. The step captures the two
// streams separately for exactly this reason: a report longer than the pipe
// buffer interleaves with anything the command writes to stderr, and a
// corrupted report reads as no report at all, which fails open onto the exit
// code. stderr still reaches the log and the failure output.
//
// A non-exempt function breaches when its score is STRICTLY above the
// threshold, so a threshold is the highest score a repository accepts rather
// than the first score it rejects.
//
// The exit code blocks either way. On a failed parse there is nothing to gate
// on but the exit code, and on a successful parse a nonzero exit still blocks,
// because a command that emitted a partial report and then crashed is otherwise
// indistinguishable from a clean repository. The contract the other side of
// that rule is short: a command that emits a valid report exits 0.
func evaluateMetricsOutput(stdout string, exitCode int, threshold float64, exemptPaths []string, workDir string) metricsVerdict {
	verdict := metricsVerdict{Threshold: threshold, ExitCode: exitCode}

	if report, ok := parseMetricsReport(stdout); ok {
		verdict.FromJSON = true
		verdict.Metric = report.Metric
		verdict.Summary = report.Summary
		roots := metricsWorkDirRoots(workDir)
		if report.Functions != nil {
			for _, fn := range *report.Functions {
				fn.File = metricsRelativePath(fn.File, roots)
				verdict.Measured++
				if metricsPathExempt(fn.File, exemptPaths) {
					verdict.Exempted++
					continue
				}
				if fn.Score > threshold {
					verdict.Breaches = append(verdict.Breaches, fn)
				}
			}
		}
	}

	sortMetricsBreaches(verdict.Breaches)
	verdict.Breached = len(verdict.Breaches) > 0 || exitCode != 0
	return verdict
}

// maxMetricsReportCandidates bounds how many balanced objects the scan keeps as
// candidates. It bounds work rather than input: the scan reads all of stdout,
// because cutting the output to a fixed tail severs the opening brace of any
// report bigger than the cut and loses it entirely. Eviction drops the OLDEST
// candidate, so only metricsFunctionsKey keeps this bound off a real report: a
// command that prints its report and then a long trailing progress stream would
// otherwise evict the report and pass a breaching repository on exit 0.
const maxMetricsReportCandidates = 256

// metricsFunctionsKey is the key decodeMetricsReport requires, so an object
// whose text does not contain it can never be the report and takes a candidate
// slot for nothing. Checking on close is what keeps a stream of log objects
// from evicting the report.
const metricsFunctionsKey = `"functions"`

// parseMetricsReport reads the metrics report out of the command's stdout.
//
// The whole trimmed output is tried first, which is what a command that prints
// nothing but its report produces. Failing that, one linear pass matches braces
// with a stack and records every balanced object it closes, AT EVERY DEPTH, and
// the recorded objects are tried newest first. Depth matters because a report
// can arrive nested inside a wrapper object that carries no top-level functions
// key, and recording only top-level objects jumped straight over it. Trying
// newest first still prefers the outer object of a nesting, since an enclosing
// object closes after everything inside it.
//
// The stack is what makes the pass immune to unmatched braces in log noise: an
// unmatched opening brace is simply never popped, and an unmatched closing brace
// with nothing on the stack is dropped. Probing each brace for its match instead
// cost a walk to the end of the output per stray brace, which needed a cap that
// then hid real reports behind it.
//
// Matching a line at a time would not do either: most tools pretty-print, so no
// single line parses on its own, and the rule preferred a trailing debug dump
// over the real report.
func parseMetricsReport(stdout string) (metricsReport, bool) {
	if report, ok := decodeMetricsReport([]byte(strings.TrimSpace(stdout))); ok {
		return report, true
	}

	spans := metricsObjectSpans(stdout)
	for i := len(spans) - 1; i >= 0; i-- {
		if report, ok := decodeMetricsReport([]byte(stdout[spans[i].start:spans[i].end])); ok {
			return report, true
		}
	}
	return metricsReport{}, false
}

// metricsObjectSpan is one balanced JSON object found in the command's output.
type metricsObjectSpan struct{ start, end int }

// metricsObjectSpans returns the balanced objects in s, in the order they
// close, keeping at most the most recent maxMetricsReportCandidates.
//
// String literals are skipped whole so a brace inside a summary or a filename
// does not shift the depth, and a backslash escape inside one cannot end it
// early. A raw newline also ends a string, because JSON forbids a literal
// control character inside one: without that the scan stays desynchronised for
// the rest of the output after a single unpaired quote in a log line.
func metricsObjectSpans(s string) []metricsObjectSpan {
	var opens []int
	var spans []metricsObjectSpan
	inString := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\n':
			inString = false
		case inString && c == '\\':
			i++
		case inString && c == '"':
			inString = false
		case inString:
		case c == '"':
			inString = true
		case c == '{':
			opens = append(opens, i)
		case c == '}':
			if len(opens) == 0 {
				continue
			}
			start := opens[len(opens)-1]
			opens = opens[:len(opens)-1]
			if !strings.Contains(s[start:i+1], metricsFunctionsKey) {
				continue
			}
			spans = append(spans, metricsObjectSpan{start: start, end: i + 1})
			if len(spans) > maxMetricsReportCandidates {
				spans = spans[1:]
			}
		}
	}
	return spans
}

// decodeMetricsReport decodes one JSON object and answers whether it is a
// metrics report at all. The deciding evidence is the presence of the
// "functions" key, not a successful decode: every JSON object decodes into
// metricsReport, so a log line would otherwise pass as an empty clean report. A
// literal null counts as present and means zero measured functions.
func decodeMetricsReport(candidate []byte) (metricsReport, bool) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(candidate, &keys); err != nil {
		return metricsReport{}, false
	}
	if _, ok := keys["functions"]; !ok {
		return metricsReport{}, false
	}
	var report metricsReport
	if err := json.Unmarshal(candidate, &report); err != nil {
		return metricsReport{}, false
	}
	return report, true
}

// metricsPathExempt answers whether any exempt glob covers the reported file,
// which arrives already in the repository-relative form matchIgnorePattern and
// the maintainer's globs are both written against.
func metricsPathExempt(file string, exemptPaths []string) bool {
	for _, pattern := range exemptPaths {
		if matchIgnorePattern(file, pattern) {
			return true
		}
	}
	return false
}

// metricsWorkDirRoots renders the worktree in every "/"-separated form a
// reported path can name it by.
//
// The symlink-resolved form is there because the strip is a literal prefix
// match: a worktree reached through a symlink (macOS /var against /private/var,
// or a symlinked NM_HOME) is named one way by the daemon and the other by an
// analyser that resolved it, and a missed strip leaves an absolute path in the
// findings and in published evidence. It is resolved once per report rather
// than per function, so a report listing thousands of functions costs one
// filesystem walk.
func metricsWorkDirRoots(workDir string) []string {
	root := strings.TrimSuffix(strings.ReplaceAll(workDir, `\`, "/"), "/")
	if root == "" {
		return nil
	}
	roots := []string{root}
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		if resolved = strings.TrimSuffix(strings.ReplaceAll(resolved, `\`, "/"), "/"); resolved != "" && resolved != root {
			roots = append(roots, resolved)
		}
	}
	return roots
}

// metricsRelativePath renders one reported file in the repository-relative form
// every consumer reads.
//
// The published contract asks for a repository-relative path, but the metrics
// command names the file however its own analyser does and absolute paths are
// the common default. Normalising here, where the report is read, is what keeps
// the daemon host's worktree path out of the findings the fix agent and the PR
// body carry and out of metrics.json, which is published to the evidence branch
// with no redaction of file contents.
func metricsRelativePath(file string, roots []string) string {
	normalised := strings.ReplaceAll(file, `\`, "/")
	for _, root := range roots {
		if rest, ok := strings.CutPrefix(normalised, root+"/"); ok {
			normalised = rest
			break
		}
	}
	return strings.TrimPrefix(normalised, "./")
}

// sortMetricsBreaches orders the breach list worst first, then by file and line
// so a repeated run reports the same order.
func sortMetricsBreaches(breaches []metricsFunction) {
	sort.SliceStable(breaches, func(i, j int) bool {
		a, b := breaches[i], breaches[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
}
