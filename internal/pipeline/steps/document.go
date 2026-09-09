package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// DocumentStep keeps documentation accurate for the change under its
// placement policy.
type DocumentStep struct{}

func (s *DocumentStep) Name() types.StepName { return types.StepDocument }

// documentPlacementPolicy is the fail-safe default placement policy. It
// replaces the old exhaustive-synchronization incentive: the agent is
// rewarded for updating each fact's single owner and for consolidation,
// deletion, and pointers - not for synchronizing every prose copy. A trusted
// repository-specific policy (config document.instructions) may narrow or
// clarify these rules but never weaken them.
const documentPlacementPolicy = `Documentation placement policy (fail-safe defaults; repository-specific instructions may narrow or clarify them, never weaken them):
- Every fact or contract has exactly one authoritative owner document. Update the owner; never synchronize prose copies of the same fact.
- When this change leaves an existing duplicate stale, remove the duplicate or reduce it to a short pointer to the owner instead of updating another full copy.
- Do not create a new documentation surface merely to close a perceived gap.
- Do not add incident narratives or postmortems to AGENTS.md. For a durable incident lesson, preserve the operative invariant in its owner document and point to the regression test or authoritative implementation.
- AGENTS.md is only for high-value project-intrinsic knowledge useful to almost every future session.
- README.md owns the user-facing product introduction and common usage.
- CONTRIBUTING.md owns contribution mechanics, not product or architecture inventories.
- Code comments own non-obvious local intent, safety invariants, and external constraints - never prose that merely restates code.
- Deep reference docs own detailed conditional material; link to them instead of copying them into always-loaded guidance.
- Generated or schema-backed facts must be generated from their authoritative source and checked for drift, never hand-copied.`

// documentScopeDiscipline bounds the pass to documentation this change made
// stale, replacing the old "be exhaustive across the corpus" instruction.
const documentScopeDiscipline = `Scope discipline:
- Only touch documentation this change made stale, plus direct contradictions that analysis reveals.
- Do not opportunistically rewrite, expand, or restructure unrelated documentation, and do not perform a broad documentation architecture migration here.
- When a larger consolidation is warranted but out of scope, leave this change safe and report one finding proposing the follow-up instead of multiplying edits.
- Preserve load-bearing user guidance, security rationale, compatibility constraints, and onboarding material. A long document is not a defect by itself; duplication and wrong placement are.
- Prefer consolidation, deletion, and pointers to the owner over addition and synchronization.`

func (s *DocumentStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return runValidationStep(sctx, s.Name(), s.execute)
}

func (s *DocumentStep) execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	ignorePatterns := "none"
	if len(sctx.Config.IgnorePatterns) > 0 {
		ignorePatterns = strings.Join(sctx.Config.IgnorePatterns, ", ")
	}

	// Skip entirely when nothing the agent would document has changed.
	changedFiles, err := git.Run(ctx, sctx.WorkDir, "diff", "--name-only", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("get changed files: %w", err)
	}
	if !hasNonIgnoredDocumentChanges(changedFiles, sctx.Config.IgnorePatterns) {
		sctx.Log("no changes to document")
		return &pipeline.StepOutcome{}, nil
	}

	sctx.Log("updating documentation...")

	result, err := sctx.RunAgentContext(ctx, agent.RunOpts{
		Prompt:     s.buildPrompt(sctx, baseSHA, ignorePatterns),
		CWD:        sctx.WorkDir,
		JSONSchema: findingsSchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    "document",
	})
	if err != nil {
		return nil, fmt.Errorf("agent document: %w", err)
	}

	// Commit whatever the agent edited, regardless of how trustworthy its
	// structured output turns out to be.
	commitSummary := extractDocumentSummary(result.Output, "")
	if err := commitAgentFixes(sctx, s.Name(), commitSummary, "update documentation"); err != nil {
		return nil, err
	}

	// Without trustworthy structured output we cannot confirm the agent
	// resolved every gap, so surface it for human review.
	var findings Findings
	if result.Output == nil {
		summary := fallbackDocumentSummary(result.Text)
		sctx.Log("missing structured output, requiring approval")
		return documentApprovalOutcome(summary), nil
	} else if err := unmarshalRequiredFindings(result.Output, &findings); err != nil {
		summary := fallbackDocumentSummary(extractDocumentSummary(result.Output, result.Text))
		sctx.Log("could not parse structured output, requiring approval")
		return documentApprovalOutcome(summary), nil
	}

	needsApproval := len(findings.Items) > 0
	findingsJSON, _ := json.Marshal(findings)

	sctx.Log(fmt.Sprintf("document findings: %d unresolved items", len(findings.Items)))

	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
		FixSummary:    findings.Summary,
	}, nil
}

// buildPrompt assembles the document prompt: the placement policy, scope
// discipline, trusted repository-specific policy, and the task.
func (s *DocumentStep) buildPrompt(sctx *pipeline.StepContext, baseSHA, ignorePatterns string) string {
	historySection := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)

	prompt := fmt.Sprintf(
		`Keep the project documentation accurate for this change. Analyze what the change made stale, fix each stale fact in its one authoritative location, and report only what you could not resolve.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- default branch: %s
- ignore patterns: %s

%s

%s%s

Task:

1. Understand the change
   - Read the diff and changed files to understand what was added, modified, or removed, and the intent of the change.

2. Find what this change made stale
   - For each fact or contract the change altered, locate its one authoritative owner document (README, docs/, doc comments, config examples, etc.).
   - Locate existing duplicates of those facts that are now stale.

3. Fix in the authoritative location
   - Update each altered fact in its owner document. Changed user-facing behavior must leave its authoritative user documentation accurate.
   - Remove stale duplicates or reduce them to a short pointer to the owner; do not synchronize full copies.
   - Re-read what you changed to verify it now reflects the code.

4. Report only what remains
   - Return a finding only for gaps you could not resolve, judgment calls (e.g. ambiguous intent or conflicting docs), or an out-of-scope consolidation worth a follow-up.
   - Do not report gaps you already fixed.
   - If nothing remains, return an empty findings array.

Rules:
- Only edit documentation files or doc comments. Do not change executable behavior or tests.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		sctx.Repo.DefaultBranch,
		ignorePatterns,
		documentPlacementPolicy,
		documentScopeDiscipline,
		trustedDocumentPolicySection(sctx),
		historySection,
	)
	if sctx.PreviousFindings != "" {
		prompt += `

Previous findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}
	return prompt
}

// trustedDocumentPolicySection renders the repository-specific documentation
// ownership policy. The value comes from the trusted default-branch copy of
// .no-mistakes.yaml (config.EffectiveRepoConfig), so a contributor's pushed
// branch cannot weaken the rules that gate its own review.
func trustedDocumentPolicySection(sctx *pipeline.StepContext) string {
	if sctx.Config == nil {
		return ""
	}
	instructions := strings.TrimSpace(sctx.Config.Document.Instructions)
	if instructions == "" {
		return ""
	}
	return "\n\nRepository documentation ownership policy (trusted, from the default branch; augments the defaults above and cannot weaken them):\n" +
		sanitizePromptMultilineText(instructions)
}

// documentApprovalOutcome builds a single ask-user finding for cases where the
// agent's structured output is missing or unparsable, so a human can confirm
// the documentation state instead of silently trusting an opaque response.
func documentApprovalOutcome(summary string) *pipeline.StepOutcome {
	findings := Findings{
		Items: []Finding{{
			Severity:    "warning",
			Description: summary,
			Action:      types.ActionAskUser,
		}},
		Summary: summary,
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
		FixSummary:    summary,
	}
}

func hasNonIgnoredDocumentChanges(changedFiles string, ignorePatterns []string) bool {
	for _, path := range strings.Split(changedFiles, "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		ignored := false
		for _, pattern := range ignorePatterns {
			if matchIgnorePattern(path, pattern) {
				ignored = true
				break
			}
		}
		if !ignored {
			return true
		}
	}
	return false
}

func fallbackDocumentSummary(text string) string {
	cleaned := strings.TrimSpace(text)
	if cleaned == "" {
		return "agent returned no structured output"
	}
	return cleaned
}

func extractDocumentSummary(raw []byte, fallback string) string {
	var payload struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && strings.TrimSpace(payload.Summary) != "" {
		return payload.Summary
	}
	return fallback
}

func unmarshalRequiredFindings(raw []byte, findings *Findings) error {
	parsed, err := types.ParseFindingsJSON(string(raw))
	if err != nil {
		return err
	}
	var payload struct {
		Summary  *string            `json:"summary"`
		Findings *[]json.RawMessage `json:"findings"`
		Items    *[]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.Findings == nil && payload.Items == nil {
		return fmt.Errorf("missing findings array")
	}
	if payload.Summary == nil || strings.TrimSpace(*payload.Summary) == "" {
		return fmt.Errorf("missing summary")
	}
	for i, item := range parsed.Items {
		if strings.TrimSpace(item.Severity) == "" {
			return fmt.Errorf("finding %d missing severity", i)
		}
		if strings.TrimSpace(item.Description) == "" {
			return fmt.Errorf("finding %d missing description", i)
		}
		if strings.TrimSpace(item.Action) == "" {
			return fmt.Errorf("finding %d missing action", i)
		}
	}
	*findings = parsed
	return nil
}
