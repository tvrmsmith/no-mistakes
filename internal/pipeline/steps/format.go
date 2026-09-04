package steps

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// FormatStep runs the repository's configured formatter. There is
// deliberately no agent fallback when no formatter is configured, unlike
// Lint: formatting is a mechanical transform a tool either does or does not
// provide, not a judgment call an agent should improvise.
type FormatStep struct{}

func (s *FormatStep) Name() types.StepName { return types.StepFormat }

func (s *FormatStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return runValidationStep(sctx, s.Name(), s.execute)
}

func (s *FormatStep) execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	fmtCmd := sctx.Config.Commands.Format

	var fixSummary string
	if sctx.Fixing {
		historySection := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
		fixPrompt := fmt.Sprintf(
			`The formatter could not parse the source in this repository. Make the smallest correct fix so it parses.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Rules:
- Make the smallest correct fix so the formatter can parse the source.
- Do not refactor beyond what is needed for that fix.
- Do not run tests or broader behavioral validation.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			historySection,
		)
		if sctx.PreviousFindings != "" {
			fixPrompt += `

Previous format findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
		}
		summary, err := executeFixMode(sctx, s.Name(), fixExecutionOptions{
			LogMessage:      "asking agent to repair unparsable source...",
			Prompt:          fixPrompt,
			ErrorPrefix:     "agent fix format",
			FallbackSummary: "repair unparsable source",
		})
		if err != nil {
			return nil, err
		}
		fixSummary = summary
	}

	if fmtCmd == "" {
		sctx.Log("no format command configured, skipping")
		return &pipeline.StepOutcome{FixSummary: fixSummary}, nil
	}

	sctx.Log(fmt.Sprintf("running formatter: %s", fmtCmd))
	output, exitCode, err := runStepShellCommand(sctx, fmtCmd)
	if err != nil {
		return nil, fmt.Errorf("run format command: %w", err)
	}

	projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepFormat)

	if exitCode != 0 {
		findings := Findings{
			Items: []Finding{{
				Severity:    "warning",
				Description: fmt.Sprintf("formatter found issues (exit code %d)", exitCode),
			}},
			Summary: projectedOutput,
		}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   true,
			Findings:      string(findingsJSON),
			ExitCode:      exitCode,
			FixSummary:    fixSummary,
		}, nil
	}

	sctx.Log("format passed")
	return &pipeline.StepOutcome{FixSummary: fixSummary}, nil
}
