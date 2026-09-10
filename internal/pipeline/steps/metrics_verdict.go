package steps

import (
	"encoding/json"
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
		if report.Functions != nil {
			for _, fn := range *report.Functions {
				verdict.Measured++
				if metricsPathExempt(fn.File, exemptPaths, workDir) {
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

// maxMetricsOutputScanBytes bounds how much of the command's stdout the report
// scan reads. The report is the last thing a well behaved command prints, so
// only the tail is searched and a command dumping megabytes of noise before it
// cannot make the scan expensive.
const maxMetricsOutputScanBytes = 1 << 20

// maxMetricsUnbalancedStarts bounds how many opening braces that never balance
// the scan walks past. Each one costs a walk to the end of the tail, so stdout
// full of stray braces would otherwise be quadratic. A brace that does balance
// costs nothing extra, because the scan jumps over the whole object, so the
// number of real candidates is unbounded.
const maxMetricsUnbalancedStarts = 64

// parseMetricsReport reads the metrics report out of the command's stdout.
//
// The whole trimmed output is tried first, which is what a command that prints
// nothing but its report produces. Failing that, one forward pass over the tail
// records every TOP-LEVEL balanced object, jumping past each object it finds so
// the report's own per-function entries are never candidates, and the recorded
// objects are then tried newest first. Scanning backwards from every brace
// spent its budget on those nested entries, so a report listing more functions
// than the budget allowed went unparsed and the gate fell back to the exit
// code. Matching a line at a time would not do either: most tools pretty-print,
// so no single line parses on its own, and the rule preferred a trailing debug
// dump over the real report.
func parseMetricsReport(stdout string) (metricsReport, bool) {
	if report, ok := decodeMetricsReport([]byte(strings.TrimSpace(stdout))); ok {
		return report, true
	}

	tail := stdout
	if len(tail) > maxMetricsOutputScanBytes {
		tail = tail[len(tail)-maxMetricsOutputScanBytes:]
	}

	type objectSpan struct{ start, end int }
	var spans []objectSpan
	unbalanced := 0
	for i := 0; i < len(tail); i++ {
		if tail[i] != '{' {
			continue
		}
		objectEnd, balanced := metricsObjectEnd(tail, i)
		if !balanced {
			unbalanced++
			if unbalanced == maxMetricsUnbalancedStarts {
				break
			}
			continue
		}
		spans = append(spans, objectSpan{start: i, end: objectEnd})
		i = objectEnd - 1
	}

	for i := len(spans) - 1; i >= 0; i-- {
		if report, ok := decodeMetricsReport([]byte(tail[spans[i].start:spans[i].end])); ok {
			return report, true
		}
	}
	return metricsReport{}, false
}

// metricsObjectEnd walks forward from the opening brace at start and returns the
// index just past the brace that balances it. String literals are skipped whole
// so a brace inside a summary or a filename does not shift the depth, and a
// backslash escape inside one cannot end it early.
func metricsObjectEnd(s string, start int) (int, bool) {
	depth := 0
	inString := false
	for i := start; i < len(s); i++ {
		switch {
		case inString && s[i] == '\\':
			i++
		case inString && s[i] == '"':
			inString = false
		case inString:
		case s[i] == '"':
			inString = true
		case s[i] == '{':
			depth++
		case s[i] == '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
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

// metricsPathExempt answers whether any exempt glob covers the reported file.
// The published contract asks for a repository-relative path, but the metrics
// command names the file however its own analyser does and absolute paths are
// the common default, so the path is normalised to the "/"-separated,
// repository-relative form matchIgnorePattern expects before matching. Without
// the workDir strip a maintainer's waiver silently misses and the run parks on
// a file the maintainer explicitly exempted.
func metricsPathExempt(file string, exemptPaths []string, workDir string) bool {
	normalised := metricsRelativePath(file, workDir)
	for _, pattern := range exemptPaths {
		if matchIgnorePattern(normalised, pattern) {
			return true
		}
	}
	return false
}

// metricsRelativePath renders one reported file in the repository-relative form
// the exempt globs are written against.
func metricsRelativePath(file, workDir string) string {
	normalised := strings.ReplaceAll(file, `\`, "/")
	if root := strings.TrimSuffix(strings.ReplaceAll(workDir, `\`, "/"), "/"); root != "" {
		if rest, ok := strings.CutPrefix(normalised, root+"/"); ok {
			normalised = rest
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
