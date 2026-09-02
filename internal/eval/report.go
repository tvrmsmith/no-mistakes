package eval

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Interval is a finite-sample recall range over cases. Repeats are averaged
// inside each case so a noisy provider does not inflate apparent sample size.
type Interval struct {
	Lower float64
	Upper float64
	Cases int
}

// CandidateReport is one locally observed candidate slice, scoped to one
// pipeline layout: the two populations a scope change to the pipeline
// produces are never blended into one row (see Report).
type CandidateReport struct {
	PipelineVersion PipelineVersion
	Cohort          string
	Summary         EvaluationSummary
	RepeatCount     int
	Confidence      *Interval
	AverageTokens   *float64
	AverageWallMS   float64
	OnFrontier      bool
}

// Report loads every local evaluation result grouped by candidate. It never
// contacts a forge, agent provider, telemetry endpoint, or remote case store.
// It is ReportForPipeline with no pipeline filter.
func Report(store *Store) ([]CandidateReport, error) {
	return ReportForPipeline(store, PipelineAny)
}

// ReportForPipeline narrows the report to one pipeline layout tag.
// PipelineAny, Report's behavior, groups every evaluation by (pipeline
// version, cohort, candidate) rather than filtering: once Review moves
// relative to the cheap gates, a share of the old gold-labelled findings can
// no longer occur, so blending the two populations into one row would read as
// a review-quality regression that is really a scope change. An evaluation
// recorded before PipelineVersion existed is read as PipelineReviewEarly (see
// normalizePipelineVersion).
func ReportForPipeline(store *Store, version PipelineVersion) ([]CandidateReport, error) {
	evaluations, err := store.evaluations()
	if err != nil {
		return nil, err
	}
	byGroup := make(map[string][]Evaluation)
	for _, evaluation := range evaluations {
		pipelineVersion := normalizePipelineVersion(evaluation.PipelineVersion)
		if version != PipelineAny && pipelineVersion != version {
			continue
		}
		cohort := evaluation.Cohort
		if cohort == "" {
			cohort = "legacy-unmatched"
		}
		key := string(pipelineVersion) + "\x00" + cohort + "\x00" + evaluation.Candidate
		byGroup[key] = append(byGroup[key], evaluation)
	}
	reports := make([]CandidateReport, 0, len(byGroup))
	for key, rows := range byGroup {
		fields := strings.SplitN(key, "\x00", 3)
		pipelineVersion, cohort, candidate := fields[0], fields[1], fields[2]
		summary := SummarizeEvaluations(rows)
		repeats := repeatCount(rows)
		report := CandidateReport{
			PipelineVersion: PipelineVersion(pipelineVersion),
			Cohort:          cohort,
			Summary:         summary,
			RepeatCount:     repeats,
			Confidence:      confidenceInterval(candidate, rows),
			AverageWallMS:   averageWallMS(rows),
		}
		if cost, ok := averageTokens(rows); ok {
			report.AverageTokens = &cost
		}
		reports = append(reports, report)
	}
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].PipelineVersion != reports[j].PipelineVersion {
			return reports[i].PipelineVersion < reports[j].PipelineVersion
		}
		if reports[i].Cohort != reports[j].Cohort {
			return reports[i].Cohort < reports[j].Cohort
		}
		return reports[i].Summary.Candidate < reports[j].Summary.Candidate
	})
	markFrontier(reports)
	return reports, nil
}

func (s *Store) evaluations() ([]Evaluation, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("eval registry is closed")
	}
	rows, err := s.db.Query(`SELECT path FROM evaluations ORDER BY completed_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list eval results: %w", err)
	}
	defer rows.Close()
	var result []Evaluation
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan eval result: %w", err)
		}
		var evaluation Evaluation
		if err := readJSON(path, &evaluation); err != nil {
			return nil, fmt.Errorf("read eval result: %w", err)
		}
		result = append(result, evaluation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list eval results: %w", err)
	}
	return result, nil
}

func repeatCount(rows []Evaluation) int {
	seen := map[int]bool{}
	for _, row := range rows {
		seen[row.Repeat] = true
	}
	return len(seen)
}

func averageWallMS(rows []Evaluation) float64 {
	if len(rows) == 0 {
		return 0
	}
	var total int64
	for _, row := range rows {
		total += row.DurationMS
	}
	return float64(total) / float64(len(rows))
}

func averageTokens(rows []Evaluation) (float64, bool) {
	if len(rows) == 0 {
		return 0, false
	}
	var total int64
	for _, row := range rows {
		if !row.TokensReported {
			return 0, false
		}
		total += row.FreshInputTokens + row.OutputTokens
	}
	return float64(total) / float64(len(rows)), true
}

func confidenceInterval(_ string, rows []Evaluation) *Interval {
	// Each case becomes a mean recall over labeled repeats. Unlabeled
	// replays stay out of this interval and are reported as pending.
	perCase := map[string][]float64{}
	for _, row := range rows {
		if row.GoldCount == 0 {
			continue
		}
		if row.Status != "completed" {
			perCase[row.CaseID] = append(perCase[row.CaseID], 0)
			continue
		}
		denom := row.TruePositive + row.FalseNegative
		if denom == 0 {
			continue
		}
		perCase[row.CaseID] = append(perCase[row.CaseID], float64(row.TruePositive)/float64(denom))
	}
	values := make([]float64, 0, len(perCase))
	for _, scores := range perCase {
		var total float64
		for _, score := range scores {
			total += score
		}
		values = append(values, total/float64(len(scores)))
	}
	if len(values) < 2 {
		return nil
	}
	var total float64
	for _, value := range values {
		total += value
	}
	n := float64(len(values))
	proportion := total / n
	const z = 1.959963984540054
	z2 := z * z
	denominator := 1 + z2/n
	center := (proportion + z2/(2*n)) / denominator
	halfWidth := z * math.Sqrt((proportion*(1-proportion)+z2/(4*n))/n) / denominator
	return &Interval{Lower: center - halfWidth, Upper: center + halfWidth, Cases: len(values)}
}

func markFrontier(reports []CandidateReport) {
	for i := range reports {
		if reports[i].AverageTokens == nil || reports[i].Summary.Labeled == 0 || reports[i].Summary.Failures > 0 {
			continue
		}
		dominated := false
		for j := range reports {
			if i == j || reports[i].PipelineVersion != reports[j].PipelineVersion || reports[i].Cohort != reports[j].Cohort || reports[j].AverageTokens == nil || reports[j].Summary.Labeled == 0 || reports[j].Summary.Failures > 0 {
				continue
			}
			betterRecall := reports[j].Summary.Recall() >= reports[i].Summary.Recall()
			cheaper := *reports[j].AverageTokens <= *reports[i].AverageTokens
			strict := reports[j].Summary.Recall() > reports[i].Summary.Recall() || *reports[j].AverageTokens < *reports[i].AverageTokens
			if betterRecall && cheaper && strict {
				dominated = true
				break
			}
		}
		reports[i].OnFrontier = !dominated
	}
}

// SetSummary lets users inspect corpus coverage before an eval consumes tokens.
// SelfScore is the recorded source reviews of the set scored against their own
// gold (see SelfScoreRecordedReviews); it is computed from already-captured
// local files, never from a fresh replay.
type SetSummary struct {
	Name           string
	Cases          int
	GoldCases      int
	TruePositive   int
	FalseNegative  int
	FalsePositive  int
	Unlabeled      int
	QueuedFindings int
	// PinCount is the corpus-wide size of the diversified pin set, which is
	// what the cap governs. It is deliberately not narrowed by a pipeline
	// filter, so a filtered Cases beside it is a subset of these pins rather
	// than a contradiction; the renderer labels it corpus-wide for that reason.
	PinCount    int
	Cap         int
	Warning     string
	Composition []CompositionRow
	SelfScore   EvaluationSummary
	// Pipelines buckets this set's cases by their own PipelineVersion tag, so
	// an operator can see how much of a set exists under each pipeline layout
	// before running a filtered report. It is independent of any pipeline
	// filter InspectSetsForPipeline was called with.
	Pipelines []PipelineCountRow
	// ScoredPipelines buckets the FILTERED cases, which are the ones SelfScore
	// actually folds into one number. A filter that leaves a single layout
	// makes that score comparable even though Pipelines still lists two, so
	// the not-comparable caveat reads this rather than Pipelines.
	ScoredPipelines []PipelineCountRow
}

// PipelineCountRow is one pipeline-layout bucket of a case set.
type PipelineCountRow struct {
	PipelineVersion PipelineVersion
	Cases           int
	GoldCases       int
}

// CompositionRow is one stratum bucket of a case set: the same axes the
// diversified holdout stratifies on.
type CompositionRow struct {
	// Repo is the repository's display identity: its resolved name when the
	// store was given one (see Store.SetRepoNames), else the short fingerprint.
	Repo        string
	Language    string
	Size        string
	Severity    string
	FindingType string
	Cases       int
}

// InspectSets summarizes all logical sets and their diversified mix. It reads
// only local registry rows and captured case files, so it stays instant no
// matter how expensive a replay of the same sets would be.
func InspectSets(store *Store) ([]SetSummary, error) {
	return InspectSetsForPipeline(store, PipelineAny)
}

// InspectSetsForPipeline is InspectSets narrowed to one pipeline layout tag.
// InspectSets is this with no filter (PipelineAny).
func InspectSetsForPipeline(store *Store, version PipelineVersion) ([]SetSummary, error) {
	sets := []string{"all", "labeled", "diversified", "tune"}
	// Resolving a set re-reads every case directory off disk, so "all" is
	// resolved once here, unfiltered, and both the labeled count and the loop's
	// own "all" iteration narrow that same slice in memory.
	allResolved, err := store.ListCasesForPipeline("all", PipelineAny)
	if err != nil {
		return nil, err
	}
	labeledCount := 0
	for _, c := range filterCasesByPipeline(allResolved, version) {
		if c.Labels.HasGold() {
			labeledCount++
		}
	}
	queuedByCase, err := store.pendingFindingCounts()
	if err != nil {
		return nil, err
	}
	result := make([]SetSummary, 0, len(sets))
	for _, name := range sets {
		// The set is resolved once unfiltered and narrowed in memory: the
		// layout breakdown has to describe the whole set (that is what makes it
		// useful before choosing a filter), while every other field describes
		// the filtered view.
		resolved := allResolved
		if name != "all" {
			r, err := store.ListCasesForPipeline(name, PipelineAny)
			if err != nil {
				return nil, err
			}
			resolved = r
		}
		cases := filterCasesByPipeline(resolved, version)
		summary := SetSummary{
			Name:            name,
			Cases:           len(cases),
			Cap:             store.diversifiedSize,
			SelfScore:       SelfScoreRecordedReviews(cases),
			Pipelines:       pipelineCountRows(resolved),
			ScoredPipelines: pipelineCountRows(cases),
		}
		if name == "diversified" {
			if n, err := store.pinCount(); err == nil {
				summary.PinCount = n
			}
			if len(cases) == 0 && labeledCount == 0 {
				summary.Warning = "diversified is empty: no labeled gold (unlabeled cases are not filled)"
			}
		}
		if name == "tune" && len(cases) == 0 && labeledCount > 0 {
			summary.Warning = "tune is empty; do not fit matcher thresholds on diversified"
		}
		type compositionKey struct {
			repoFingerprint string
			language        string
			size            string
			severity        string
			findingType     string
		}
		composition := map[compositionKey]int{}
		for _, c := range cases {
			if c.Labels.HasGold() {
				summary.GoldCases++
				for _, finding := range c.Labels.Findings {
					switch finding.Kind {
					case GoldTruePositive:
						summary.TruePositive++
					case GoldFalseNegative:
						summary.FalseNegative++
					case GoldFalsePositive:
						summary.FalsePositive++
					}
				}
			} else {
				summary.Unlabeled++
			}
			summary.QueuedFindings += queuedByCase[c.ID]
			language, size, severity := caseComposition(c)
			composition[compositionKey{
				repoFingerprint: c.RepoFingerprint,
				language:        language,
				size:            size,
				severity:        severity,
				findingType:     findingType(c),
			}]++
		}
		rows := make([]CompositionRow, 0, len(composition))
		for key, n := range composition {
			rows = append(rows, CompositionRow{
				Repo:        store.repoDisplay(key.repoFingerprint),
				Language:    key.language,
				Size:        key.size,
				Severity:    key.severity,
				FindingType: key.findingType,
				Cases:       n,
			})
		}
		summary.Composition = sortedCompositionRows(rows)
		result = append(result, summary)
	}
	return result, nil
}

// PipelineLayoutsInEvaluations counts the distinct pipeline layouts a slice of
// evaluations spans, reading an untagged evaluation as pre-reorder. Any summary
// that folds those evaluations into one score is comparable only when the count
// is one.
func PipelineLayoutsInEvaluations(evaluations []Evaluation) int {
	seen := map[PipelineVersion]bool{}
	for _, evaluation := range evaluations {
		seen[normalizePipelineVersion(evaluation.PipelineVersion)] = true
	}
	return len(seen)
}

// pipelineCountRows buckets cases by their own PipelineVersion tag, sorted by
// version string ascending.
func pipelineCountRows(cases []Case) []PipelineCountRow {
	byVersion := map[PipelineVersion]*PipelineCountRow{}
	for _, c := range cases {
		row, ok := byVersion[c.PipelineVersion]
		if !ok {
			row = &PipelineCountRow{PipelineVersion: c.PipelineVersion}
			byVersion[c.PipelineVersion] = row
		}
		row.Cases++
		if c.Labels.HasGold() {
			row.GoldCases++
		}
	}
	rows := make([]PipelineCountRow, 0, len(byVersion))
	for _, row := range byVersion {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].PipelineVersion < rows[j].PipelineVersion })
	return rows
}

func sortedCompositionRows(rows []CompositionRow) []CompositionRow {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Language != b.Language {
			return a.Language < b.Language
		}
		if a.Size != b.Size {
			return a.Size < b.Size
		}
		if a.Severity != b.Severity {
			return a.Severity < b.Severity
		}
		if a.FindingType != b.FindingType {
			return a.FindingType < b.FindingType
		}
		return a.Cases < b.Cases
	})
	return rows
}

// SelfScoreRecordedReviews scores each case's recorded source-review findings
// against that case's own gold, exactly as a replayed candidate would be
// scored. Everything it reads was captured when the case was frozen, so the
// result is available instantly without invoking an agent, touching a gate, or
// re-running anything. It answers "what would eval report for the reviews that
// produced this set" - the baseline a candidate has to beat.
func SelfScoreRecordedReviews(cases []Case) EvaluationSummary {
	evaluations := make([]Evaluation, 0, len(cases))
	for _, c := range cases {
		evaluation := Evaluation{
			CaseID:            c.ID,
			Candidate:         "recorded-review",
			Status:            "completed",
			HasFindingGold:    c.Labels.HasGold(),
			GoldCount:         c.Labels.TrueIssueCount(),
			FalsePositiveGold: c.Labels.FalsePositiveCount(),
		}
		findings, err := osReadRoundFindings(c)
		if err != nil {
			evaluation.Status = "failed"
		} else {
			score := ScoreCandidate(c.Labels, findings)
			evaluation.TruePositive = score.TruePositive
			evaluation.TruePositiveExact = score.TruePositiveExact
			evaluation.TruePositiveFuzzy = score.TruePositiveFuzzy
			evaluation.FalseNegative = score.FalseNegative
			evaluation.FalsePositive = score.FalsePositive
			evaluation.FalsePositiveGold = score.FalsePositiveGold
			evaluation.Pending = score.Pending
		}
		evaluations = append(evaluations, evaluation)
	}
	return SummarizeEvaluations(evaluations)
}

func shortFingerprint(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

// RenderReport is a stable human-readable local comparison. Scores are
// finding-level. Unmatched candidate findings stay pending and are never
// called false positives. A replay with no gold is unlabeled, not a pass.
func RenderReport(reports []CandidateReport) string {
	if len(reports) == 0 {
		return "LOCAL-ONLY EVAL REPORT\nno candidate replays recorded yet\n"
	}
	var b strings.Builder
	b.WriteString("LOCAL-ONLY EVAL REPORT\n")
	for _, report := range reports {
		s := report.Summary
		fmt.Fprintf(&b, "\n%s (pipeline %s, cohort %s)\n", s.Candidate, report.PipelineVersion, report.Cohort)
		fmt.Fprintf(&b, "  replays: %d across %d repeat(s); labeled: %d; failures: %d\n", s.Total, report.RepeatCount, s.Labeled, s.Failures)
		if s.Labeled == 0 {
			b.WriteString("  finding scores: unlabeled / pending (no finding-level gold yet)\n")
		} else {
			fmt.Fprintf(&b, "  finding scores: true-positive %d, false-negative %d, false-positive %d, pending %d\n", s.TruePositive, s.FalseNegative, s.FalsePositive, s.Pending)
			if s.TruePositive+s.FalseNegative == 0 {
				b.WriteString("  recall: unavailable (no true-issue gold)\n")
			} else {
				fmt.Fprintf(&b, "  recall: %.1f%% (%d/%d gold issues)\n", 100*s.Recall(), s.TruePositive, s.TruePositive+s.FalseNegative)
				if s.TruePositiveFuzzy > 0 {
					fmt.Fprintf(&b, "  recall-if-exact-only: %.1f%% (%d/%d)\n", 100*s.ExactRecall(), s.TruePositiveExact, s.TruePositive+s.FalseNegative)
				}
				if report.Confidence != nil {
					fmt.Fprintf(&b, "  case-level recall range: %.1f%%-%.1f%% over %d case(s)\n", 100*report.Confidence.Lower, 100*report.Confidence.Upper, report.Confidence.Cases)
				}
			}
			fmt.Fprintf(&b, "  precision bounds: %.1f%%-%.1f%% (adjudicated %.1f%%; pending treated as FP for the lower bound)\n",
				100*s.PrecisionLower(), 100*s.Precision(), 100*s.Precision())
			if s.HasFalsePositiveGold() {
				fmt.Fprintf(&b, "  F1: %.1f%% (headline; false-positive gold is present)\n", 100*s.F1())
			} else {
				fmt.Fprintf(&b, "  F1: withheld (no false-positive gold; precision in [%.1f%%, %.1f%%])\n", 100*s.PrecisionLower(), 100*s.Precision())
			}
		}
		if s.Pending > 0 {
			fmt.Fprintf(&b, "  queued unmatched candidate findings: %d (not scored as false-positive)\n", s.Pending)
		}
		if report.AverageTokens == nil {
			b.WriteString("  token cost: unknown (token usage was not reported for every replay)\n")
		} else {
			fmt.Fprintf(&b, "  token cost: %.0f fresh-input + output tokens per reported replay\n", *report.AverageTokens)
		}
		fmt.Fprintf(&b, "  wall time: %.1fs average\n", report.AverageWallMS/1000)
		if report.AverageTokens != nil {
			if s.TruePositive+s.FalseNegative == 0 {
				b.WriteString("  recall-vs-cost frontier: unavailable (no true-issue gold)\n")
			} else {
				fmt.Fprintf(&b, "  recall-vs-cost frontier: %t\n", report.OnFrontier)
			}
		}
	}
	return b.String()
}
