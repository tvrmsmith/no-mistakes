package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// MetricsStep gates the branch on a code-health metric the repository's own
// command measures against the coverage the Test step produced.
//
// There is deliberately no agent fallback when no metrics command is
// configured, unlike Lint and Document. CRAP needs measured complexity joined
// to measured coverage, and an agent producing those numbers by inspection is
// producing fiction.
type MetricsStep struct{}

func (s *MetricsStep) Name() types.StepName { return types.StepMetrics }

func (s *MetricsStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return runValidationStep(sctx, s.Name(), s.execute)
}

func (s *MetricsStep) execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	if sctx.Config.Commands.Metrics == "" {
		sctx.Log("no metrics command configured, skipping metrics gate")
		return &pipeline.StepOutcome{}, nil
	}

	if !coveragePresent(sctx.CoverageDir) {
		return noCoverageOutcome(sctx), nil
	}

	metricsCmd := sctx.Config.Commands.Metrics
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	changed, err := changedPathsSince(sctx.Ctx, sctx.WorkDir, baseSHA, sctx.Run.HeadSHA, sctx.Fixing)
	if err != nil {
		return nil, err
	}
	changedFilesEnv, _ := changedFilesEnvValue(changed)

	var fixSummary string
	if sctx.Fixing {
		summary, err := executeFixMode(sctx, s.Name(), fixExecutionOptions{
			LogMessage:      "asking agent to fix the metrics breach...",
			Prompt:          metricsFixPrompt(sctx, baseSHA),
			ErrorPrefix:     "agent fix metrics",
			FallbackSummary: "reduce metrics breach",
		})
		if err != nil {
			return nil, err
		}
		fixSummary = summary
	}

	sctx.Log(fmt.Sprintf("running metrics command: %s", metricsCmd))
	output, exitCode, err := runStepShellCommandEnv(sctx, metricsCmd, []string{
		envTestBaseSHA + "=" + baseSHA,
		envTestChangedFiles + "=" + changedFilesEnv,
		envTestChangedFileCount + "=" + strconv.Itoa(len(changed)),
		envMetricsCoverageRoot + "=" + sctx.CoverageDir,
	})
	if err != nil {
		return nil, fmt.Errorf("run metrics command: %w", err)
	}
	projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepMetrics)

	verdict := evaluateMetricsOutput(output, exitCode, sctx.Config.Metrics.Threshold, sctx.Config.Metrics.ExemptPaths)

	// Evidence is written before the gate, so a run that parks on a breach and
	// is never resumed still leaves its verdict on disk.
	writeMetricsEvidence(sctx, verdict)

	if !verdict.Breached {
		sctx.Log(fmt.Sprintf("metrics passed: %d function(s) measured against a %s threshold of %s",
			verdict.Measured, metricName(verdict), formatMetricsScore(verdict.Threshold)))
		return &pipeline.StepOutcome{FixSummary: fixSummary}, nil
	}

	findings := Findings{Items: metricsBreachFindings(verdict)}
	if !verdict.FromJSON {
		findings.Summary = projectedOutput
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   true,
		Findings:      string(findingsJSON),
		ExitCode:      verdict.ExitCode,
		FixSummary:    fixSummary,
	}, nil
}

// metricsFixPrompt asks the agent to bring the breaching functions under the
// threshold.
//
// The summary rule is the load-bearing part. Coverage enters the CRAP formula
// cubed, so adding tests to a hairball drops its score far faster than
// simplifying it does, and that is the remedy the formula over-rewards. The
// maintainer reading the commit has to be able to see which lever the agent
// pulled without diffing the whole round.
func metricsFixPrompt(sctx *pipeline.StepContext, baseSHA string) string {
	historySection := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
	prompt := fmt.Sprintf(
		`Functions in this repository breach the configured code-health threshold. Bring them under it.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Two levers reduce the score: add tests that cover the function, or reduce the function's complexity.
- State in your summary whether you added tests or reduced complexity, because coverage weighs far more heavily in the score than complexity does and the maintainer needs to see which lever you pulled.
- Prefer reducing complexity where the function is genuinely doing too much. Do not add tests that assert nothing just to move the number.
- Make the smallest correct change. Do not refactor beyond the breaching functions.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		historySection,
	)
	if sctx.PreviousFindings != "" {
		prompt += `

Previous metrics findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}
	return prompt
}

// maxMetricsBreachFindings bounds how many per-function items a breach reports.
// A repository adopting the gate can breach on hundreds of functions at once,
// and a findings list that long is unreadable in the gate prompt and in the PR
// body alike, so the remainder is summarised in one trailing item.
const maxMetricsBreachFindings = 20

// metricsBreachFindings renders the breach as findings a maintainer and a fix
// round both read.
func metricsBreachFindings(verdict metricsVerdict) []Finding {
	if !verdict.FromJSON {
		// A fallback verdict is a materially weaker gate than a parsed report,
		// so the finding says so rather than leaving that only in the evidence
		// file. The operator is deciding on an exit code alone.
		return []Finding{{
			Severity: types.FindingSeverityWarning,
			Action:   types.ActionAutoFix,
			Description: fmt.Sprintf(
				"the metrics command exited %d and its output did not parse as a metrics report, so the exit code alone is the verdict and no function-level breach is known",
				verdict.ExitCode),
		}}
	}

	items := make([]Finding, 0, len(verdict.Breaches)+1)
	for i, fn := range verdict.Breaches {
		if i == maxMetricsBreachFindings {
			items = append(items, Finding{
				Severity:    types.FindingSeverityError,
				Action:      types.ActionAutoFix,
				ID:          "metrics-breach",
				Description: fmt.Sprintf("%d more function(s) breached the %s threshold of %s", len(verdict.Breaches)-i, metricName(verdict), formatMetricsScore(verdict.Threshold)),
			})
			break
		}
		items = append(items, Finding{
			Severity:    types.FindingSeverityError,
			Action:      types.ActionAutoFix,
			ID:          "metrics-breach",
			File:        fn.File,
			Line:        fn.Line,
			Description: metricsBreachDescription(verdict, fn),
		})
	}
	if verdict.ExitCode != 0 {
		items = append(items, Finding{
			Severity:    types.FindingSeverityWarning,
			Action:      types.ActionAutoFix,
			Description: fmt.Sprintf("the metrics command exited %d after emitting its report, so the report may be partial", verdict.ExitCode),
		})
	}
	return items
}

// metricsBreachDescription names the lever a maintainer would pull. Complexity
// and coverage are rendered when the report carried them, because the pair is
// what says whether the remedy is more tests or a smaller function.
func metricsBreachDescription(verdict metricsVerdict, fn metricsFunction) string {
	description := fmt.Sprintf("%s %s scores %s, above the threshold of %s",
		metricName(verdict), fn.Function, formatMetricsScore(fn.Score), formatMetricsScore(verdict.Threshold))
	if fn.Complexity != nil {
		description += fmt.Sprintf("; complexity %d", *fn.Complexity)
	}
	if fn.Coverage != nil {
		description += fmt.Sprintf("; coverage %s%%", formatMetricsScore(*fn.Coverage*100))
	}
	return description
}

// metricName falls back to a neutral word, because `metric` is a free-form
// string the command chooses and may omit.
func metricName(verdict metricsVerdict) string {
	if verdict.Metric == "" {
		return "metric"
	}
	return verdict.Metric
}

// formatMetricsScore renders a score without a trailing ".0", so a threshold of
// 30 reads as 30 rather than 30.000000.
func formatMetricsScore(score float64) string {
	return strconv.FormatFloat(score, 'f', -1, 64)
}

// coveragePresent answers whether the Test step left anything for the metrics
// command to measure. It stops at the first regular file, so a run whose
// coverage tree is large costs one directory read rather than a full walk.
func coveragePresent(coverageDir string) bool {
	if coverageDir == "" {
		return false
	}
	found := false
	// A walk error is not evidence of coverage, so it leaves found false and
	// the step parks rather than measuring against a directory it cannot read.
	_ = filepath.WalkDir(coverageDir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.Type().IsRegular() {
			found = true
			return errStopCoverageWalk
		}
		return nil
	})
	return found
}

// errStopCoverageWalk ends the coverage walk at the first file it finds.
var errStopCoverageWalk = errors.New("stop coverage walk")

// noCoverageOutcome parks the run for the MAINTAINER rather than for an agent
// fix round.
//
// Running the command against an empty directory yields an empty report that
// parses clean and passes, which is the vacuous green issue 9 closed for Test,
// so the command deliberately does not run at all. And an agent cannot fix
// "the Test step produced no coverage": the causes all sit in configuration or
// in the Test step's own path, so a fix round would rewrite tests to answer a
// question nobody asked.
func noCoverageOutcome(sctx *pipeline.StepContext) *pipeline.StepOutcome {
	description := "the metrics gate needs test coverage, and this run produced none. " +
		"Reachable causes: the Test step took its agent-evidence path, which runs no unit command and writes no coverage profile; " +
		"the run was started with --skip test; or the repository config sets skip_steps: [test]. " +
		"Configure test.units or commands.test so the Test step writes coverage, or drop commands.metrics."
	sctx.Log(description)
	findings := Findings{Items: []Finding{{
		Severity:    types.FindingSeverityError,
		Action:      types.ActionAskUser,
		Description: description,
	}}}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
	}
}

// metricsEvidence is the published shape of metrics.json.
//
// It is an explicit exported-field struct with json tags rather than a marshal
// of metricsVerdict, because evidence.Publish copies this file onto the
// repository's evidence branch, so its field names are a contract a reader
// outside this package depends on. Renaming an unexported verdict field must
// not silently rename a published one.
//
// Raw coverage is never part of it. The profiles stay in sctx.CoverageDir,
// which nothing copies and the daemon removes when the run ends.
type metricsEvidence struct {
	Metric    string  `json:"metric"`
	Threshold float64 `json:"threshold"`
	Measured  int     `json:"measured"`
	Exempted  int     `json:"exempted"`
	Breached  bool    `json:"breached"`
	FromJSON  bool    `json:"from_json"`
	ExitCode  int     `json:"exit_code"`
	Summary   string  `json:"summary"`
	// Breaches is always non-nil, so an unbreached verdict renders [] rather
	// than null and a consumer can iterate it without a nil check.
	// metricsVerdict.Breaches is nil when nothing breached, so the
	// normalisation happens here where the published shape is decided.
	Breaches []metricsEvidenceBreach `json:"breaches"`
}

// metricsEvidenceBreach is one breaching function as the evidence file records
// it.
type metricsEvidenceBreach struct {
	File       string   `json:"file"`
	Function   string   `json:"function"`
	Line       int      `json:"line"`
	Score      float64  `json:"score"`
	Complexity *int     `json:"complexity,omitempty"`
	Coverage   *float64 `json:"coverage,omitempty"`
}

// writeMetricsEvidence records the verdict beside the run's test evidence.
//
// The write is best effort: evidence is a record, not a gate, so a failure logs
// and the run continues rather than failing a branch over a file nobody gates
// on.
func writeMetricsEvidence(sctx *pipeline.StepContext, verdict metricsVerdict) {
	if !sctx.Config.Test.Evidence.StoreInRepo {
		return
	}
	evidenceDir := testEvidenceDir(sctx)
	if evidenceDir == "" {
		return
	}

	record := metricsEvidence{
		Metric:    verdict.Metric,
		Threshold: verdict.Threshold,
		Measured:  verdict.Measured,
		Exempted:  verdict.Exempted,
		Breached:  verdict.Breached,
		FromJSON:  verdict.FromJSON,
		ExitCode:  verdict.ExitCode,
		Summary:   verdict.Summary,
		Breaches:  make([]metricsEvidenceBreach, 0, len(verdict.Breaches)),
	}
	for _, fn := range verdict.Breaches {
		record.Breaches = append(record.Breaches, metricsEvidenceBreach{
			File:       fn.File,
			Function:   fn.Function,
			Line:       fn.Line,
			Score:      fn.Score,
			Complexity: fn.Complexity,
			Coverage:   fn.Coverage,
		})
	}

	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not encode the metrics verdict for evidence: %v", err))
		return
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		sctx.Log(fmt.Sprintf("warning: could not create the evidence dir for the metrics verdict: %v", err))
		return
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "metrics.json"), append(encoded, '\n'), 0o644); err != nil {
		sctx.Log(fmt.Sprintf("warning: could not write the metrics verdict to evidence: %v", err))
	}
}
