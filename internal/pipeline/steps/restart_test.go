package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func commitCount(t *testing.T, dir string) int {
	t.Helper()
	count, err := strconv.Atoi(strings.TrimSpace(gitCmd(t, dir, "rev-list", "--count", "HEAD")))
	if err != nil {
		t.Fatalf("parse commit count: %v", err)
	}
	return count
}

// validationStepBody is satisfied by exactly the steps that route their exit
// through runValidationStep: the wrapper Execute is public, the real body is
// the unexported execute this interface names.
type validationStepBody interface {
	execute(*pipeline.StepContext) (*pipeline.StepOutcome, error)
}

func validationSteps() []pipeline.Step {
	var out []pipeline.Step
	for _, step := range AllSteps() {
		if _, ok := step.(validationStepBody); ok {
			out = append(out, step)
		}
	}
	return out
}

// TestRunValidationStep_CommitAttributionMatrix crosses "an agent ran this
// round" with the kind of path the commit touches. Attribution is the only
// axis that decides a restart today: the documentation-glob exemption is issue
// #6 and does not exist yet, so a docs-only agent commit restarts exactly like
// a code one. The row that writes nothing pins the other half of attribution:
// an agent that ran and changed nothing leaves no commit to restart on.
func TestRunValidationStep_CommitAttributionMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		agentRan    bool
		dirtyPath   string
		wantRestart types.StepName
		wantCommits int
	}{
		{name: "agent_docs", agentRan: true, dirtyPath: "README.md", wantRestart: types.StepReview, wantCommits: 1},
		{name: "agent_code", agentRan: true, dirtyPath: "main.go", wantRestart: types.StepReview, wantCommits: 1},
		{name: "tool_docs", agentRan: false, dirtyPath: "README.md", wantRestart: "", wantCommits: 1},
		{name: "tool_code", agentRan: false, dirtyPath: "main.go", wantRestart: "", wantCommits: 1},
		{name: "agent_no_write", agentRan: true, dirtyPath: "", wantRestart: "", wantCommits: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", headSHA)

			write := func() {
				if tc.dirtyPath == "" {
					return
				}
				if err := os.WriteFile(filepath.Join(dir, tc.dirtyPath), []byte("changed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				write()
				return &agent.Result{Output: json.RawMessage(`{"summary":"edit"}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Shared = &pipeline.RunShared{}
			before := commitCount(t, dir)

			outcome, err := runValidationStep(sctx, types.StepDocument, func(inner *pipeline.StepContext) (*pipeline.StepOutcome, error) {
				if tc.agentRan {
					if _, err := inner.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
						return nil, err
					}
				} else {
					write()
				}
				return &pipeline.StepOutcome{}, nil
			})
			if err != nil {
				t.Fatalf("runValidationStep() error = %v", err)
			}
			if outcome.RestartFrom != tc.wantRestart {
				t.Fatalf("RestartFrom = %q, want %q", outcome.RestartFrom, tc.wantRestart)
			}
			if got := commitCount(t, dir) - before; got != tc.wantCommits {
				t.Fatalf("new commits = %d, want %d", got, tc.wantCommits)
			}
			if status := gitStatusPorcelain(t, dir); status != "" {
				t.Fatalf("worktree = %q, want clean", status)
			}
		})
	}
}

// TestRunValidationStep_CertifyingStepParksOverResidue covers the step that
// records the review-approved head. Committing at its exit would leave the SHA
// it just certified a strict ancestor of the new head, and push accepts an
// ancestor, so the residue would ship unjudged. It parks instead: nothing is
// committed, nothing is destroyed, and the certification stands.
func TestRunValidationStep_CertifyingStepParksOverResidue(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("edited\n"), 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("scratch\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}
	before := commitCount(t, dir)

	outcome, err := runValidationStep(sctx, types.StepReview, func(inner *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if _, err := inner.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{ReviewApprovedHeadSHA: headSHA, AutoFixable: true}, nil
	})
	if err != nil {
		t.Fatalf("runValidationStep() error = %v", err)
	}
	if outcome.AutoFixable {
		t.Fatal("AutoFixable = true; the executor auto-fixes before it parks, so the residue would be committed and re-certified with nobody told the certifying step left a mess")
	}
	if outcome.ReviewApprovedHeadSHA != headSHA {
		t.Fatalf("ReviewApprovedHeadSHA = %q, want %q", outcome.ReviewApprovedHeadSHA, headSHA)
	}
	if outcome.RestartFrom != "" {
		t.Fatalf("RestartFrom = %q, want empty", outcome.RestartFrom)
	}
	if !outcome.NeedsApproval {
		t.Fatal("NeedsApproval = false, want true")
	}
	if got := commitCount(t, dir) - before; got != 0 {
		t.Fatalf("new commits = %d, want 0", got)
	}
	if status := gitStatusPorcelain(t, dir); status == "" {
		t.Fatal("worktree is clean, want the residue preserved for the gate")
	}

	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse residue findings: %v", err)
	}
	if types.HasActionableFindings(findings) {
		t.Fatal("residue findings are actionable, want every item no-op so --yes discards")
	}
	severityByFile := map[string]string{}
	for _, item := range findings.Items {
		severityByFile[item.File] = item.Severity
	}
	if got := severityByFile["feature.txt"]; got != "warning" {
		t.Fatalf("tracked-file severity = %q, want warning", got)
	}
	if got := severityByFile["scratch.txt"]; got != "info" {
		t.Fatalf("untracked-file severity = %q, want info", got)
	}
}

// TestRunValidationStep_CertifyingStepCleanExitKeepsCertification is the other
// half of the branch above: a review round that ends clean, including one whose
// own fix mode already committed, passes its certification straight through.
func TestRunValidationStep_CertifyingStepCleanExitKeepsCertification(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("fixed\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"fix it"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}

	outcome, err := runValidationStep(sctx, types.StepReview, func(inner *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if _, err := inner.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
			return nil, err
		}
		if err := commitAgentFixes(inner, types.StepReview, "fix it", "fix it"); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{ReviewApprovedHeadSHA: inner.Run.HeadSHA}, nil
	})
	if err != nil {
		t.Fatalf("runValidationStep() error = %v", err)
	}
	if outcome.ReviewApprovedHeadSHA != sctx.Run.HeadSHA {
		t.Fatalf("ReviewApprovedHeadSHA = %q, want the new head %q", outcome.ReviewApprovedHeadSHA, sctx.Run.HeadSHA)
	}
	if outcome.NeedsApproval {
		t.Fatal("NeedsApproval = true, want false for a fix round that ended clean")
	}
}

// TestRunValidationStep_FailedRoundCommitsNothing proves a step body that
// errors takes its residue with it. Committing there would publish work no
// gate ever accepted, from a round the run is about to fail on.
func TestRunValidationStep_FailedRoundCommitsNothing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}
	before := commitCount(t, dir)

	boom := errors.New("step body failed")
	_, err := runValidationStep(sctx, types.StepDocument, func(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if writeErr := os.WriteFile(filepath.Join(dir, "half-done.txt"), []byte("partial\n"), 0o644); writeErr != nil {
			return nil, writeErr
		}
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("runValidationStep() error = %v, want %v", err, boom)
	}
	if got := commitCount(t, dir) - before; got != 0 {
		t.Fatalf("new commits = %d, want 0", got)
	}
}

// TestReviewStep_DiscardApprovalResidue proves what approving a residue gate
// means: the paths the park recorded go back to HEAD or are deleted, gitignored
// build output is left alone, and a file edited while the run sat parked - which
// the park never recorded and nobody ruled on - survives untouched.
func TestReviewStep_DiscardApprovalResidue(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "ignore build output")

	tracked := filepath.Join(dir, "feature.txt")
	original, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracked, []byte("residue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build", "out.bin"), []byte("binary\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.HeadSHA = strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))
	before := commitCount(t, dir)

	recordResidue(sctx, []string{"feature.txt"}, []string{"scratch.txt"})

	// The gate is raised, and only then does a human edit the worktree.
	edited := filepath.Join(dir, "base.txt")
	if err := os.WriteFile(edited, []byte("edited while parked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes-while-parked.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&ReviewStep{}).DiscardApprovalResidue(sctx); err != nil {
		t.Fatalf("DiscardApprovalResidue() error = %v", err)
	}

	restored, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatalf("tracked file = %q, want it restored to %q", restored, original)
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked residue still present (stat err = %v), want it removed", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "build", "out.bin")); err != nil {
		t.Fatalf("gitignored output was removed: %v", err)
	}
	survived, err := os.ReadFile(edited)
	if err != nil {
		t.Fatalf("tracked file edited while parked was destroyed: %v", err)
	}
	if string(survived) != "edited while parked\n" {
		t.Fatalf("tracked file edited while parked = %q, want it untouched", survived)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes-while-parked.txt")); err != nil {
		t.Fatalf("untracked file created while parked was destroyed: %v", err)
	}
	if got := commitCount(t, dir) - before; got != 0 {
		t.Fatalf("new commits = %d, want 0", got)
	}
}

// recordResidue writes the path list a residue park records, so a discard test
// drives the same trusted state the executor's approval path reads.
func recordResidue(sctx *pipeline.StepContext, modified, untracked []string) {
	if sctx.Shared == nil {
		sctx.Shared = &pipeline.RunShared{}
	}
	sctx.Shared.SetValidationResidue(types.StepReview, pipeline.ValidationResidue{
		Modified:  modified,
		Untracked: untracked,
	})
}

// residueShapedFindings is the payload a review agent can write on its own: no-op
// items carrying the same IDs and files the residue park uses. Nothing about it
// is trusted, so discard must ignore it entirely.
func residueShapedFindings(t *testing.T, modified, untracked []string) string {
	t.Helper()
	var findings Findings
	for i, file := range modified {
		findings.Items = append(findings.Items, Finding{
			ID:       "residue-tracked-" + strconv.Itoa(i+1),
			Severity: "warning",
			Action:   types.ActionNoOp,
			File:     file,
		})
	}
	for i, file := range untracked {
		findings.Items = append(findings.Items, Finding{
			ID:       "residue-untracked-" + strconv.Itoa(i+1),
			Severity: "info",
			Action:   types.ActionNoOp,
			File:     file,
		})
	}
	raw, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestReviewStep_DiscardApprovalResidueTreatsRecordedPathsAsNames drives the
// same "outside the recorded list survives" guarantee through a filename that
// happens to hold glob characters. The recorded paths are names git handed
// back, never patterns anyone wrote, so a discard that lets git expand them
// destroys a neighbour nobody ruled on and still reports success, since every
// path it did record really is gone.
func TestReviewStep_DiscardApprovalResidueTreatsRecordedPathsAsNames(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	const globbed = "notes*.txt"
	const bracketed = "draft[1].txt"
	for _, name := range []string{bracketed, "draft1.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("committed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add a bracketed path and its neighbour")
	headSHA = strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))

	if err := os.WriteFile(filepath.Join(dir, bracketed), []byte("residue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, globbed), []byte("residue\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.HeadSHA = headSHA

	recordResidue(sctx, []string{bracketed}, []string{globbed})

	// The neighbours arrive after the park, so nothing ruled on them. Each is
	// matched by one of the recorded names read as a pattern: notes*.txt by the
	// glob, draft[1].txt by the character class.
	if err := os.WriteFile(filepath.Join(dir, "notes1.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "draft1.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&ReviewStep{}).DiscardApprovalResidue(sctx); err != nil {
		t.Fatalf("DiscardApprovalResidue() error = %v", err)
	}

	restored, err := os.ReadFile(filepath.Join(dir, bracketed))
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != "committed\n" {
		t.Fatalf("recorded tracked file = %q, want it restored", restored)
	}
	if _, err := os.Stat(filepath.Join(dir, globbed)); !os.IsNotExist(err) {
		t.Fatalf("recorded untracked file still present (stat err = %v), want it removed", err)
	}
	for _, neighbour := range []string{"notes1.txt", "draft1.txt"} {
		got, err := os.ReadFile(filepath.Join(dir, neighbour))
		if err != nil {
			t.Fatalf("%s was outside the recorded list and was destroyed: %v", neighbour, err)
		}
		if string(got) != "mine\n" {
			t.Fatalf("%s = %q, want it untouched", neighbour, got)
		}
	}
}

// TestRunValidationStep_ParkedResidueRecordsBothHalvesOfARename pins the one
// case where git reports fewer paths than it changed. Rename detection collapses
// a staged rename into the new path, so a park that recorded only that path
// leaves the staged deletion of the old one behind, discard reports success
// because every recorded path really is gone, and the deletion nobody ruled on
// rides into the next validation step's exit commit.
func TestRunValidationStep_ParkedResidueRecordsBothHalvesOfARename(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "mv", "feature.txt", "renamed.txt")
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}

	parked, err := runValidationStep(sctx, types.StepReview, func(inner *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if _, err := inner.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{ReviewApprovedHeadSHA: headSHA}, nil
	})
	if err != nil {
		t.Fatalf("runValidationStep() error = %v", err)
	}
	if !parked.NeedsApproval {
		t.Fatal("NeedsApproval = false, want the certifying step parked over its residue")
	}

	recorded, ok := sctx.Shared.ValidationResidue(types.StepReview)
	if !ok {
		t.Fatal("the park recorded no residue, want the paths it refused to commit")
	}
	for _, want := range []string{"feature.txt", "renamed.txt"} {
		if !slices.Contains(recorded.Modified, want) {
			t.Fatalf("park recorded %v, want both halves of the rename including %q", recorded.Modified, want)
		}
	}

	if err := (&ReviewStep{}).DiscardApprovalResidue(sctx); err != nil {
		t.Fatalf("DiscardApprovalResidue() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); err != nil {
		t.Fatalf("the rename's source was not restored: %v", err)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree = %q, want the rename fully discarded", status)
	}
}

// TestReviewStep_DiscardApprovalResidueLeavesANonResidueGateAlone covers every
// other gate the certifying step raises. Those record no residue, so discard
// has nothing to remove and must not reach for the worktree at all.
//
// The findings here are the attack: Finding.ID is a free-form string the review
// agent writes and NormalizeFindings keeps verbatim, so an ordinary round can
// return no-op items spelled exactly like a residue park's and name any file it
// likes. Approving that round is an ordinary approval, and it must destroy
// nothing.
func TestReviewStep_DiscardApprovalResidueLeavesANonResidueGateAlone(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}

	forged := residueShapedFindings(t, []string{"feature.txt"}, []string{"scratch.txt"})
	round, err := runValidationStep(sctx, types.StepReview, func(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
		return &pipeline.StepOutcome{
			NeedsApproval:         true,
			Findings:              forged,
			ReviewApprovedHeadSHA: headSHA,
		}, nil
	})
	if err != nil {
		t.Fatalf("runValidationStep() error = %v", err)
	}
	if !round.NeedsApproval {
		t.Fatal("NeedsApproval = false, want the round's own gate")
	}

	// The round exited clean, so the files below are a human's work during the
	// park. Nothing recorded them.
	tracked := filepath.Join(dir, "feature.txt")
	if err := os.WriteFile(tracked, []byte("uncommitted work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&ReviewStep{}).DiscardApprovalResidue(sctx); err != nil {
		t.Fatalf("DiscardApprovalResidue() error = %v", err)
	}
	got, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "uncommitted work\n" {
		t.Fatalf("tracked file = %q, want it untouched by a gate that recorded no residue", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch.txt")); err != nil {
		t.Fatalf("untracked file named only by the agent's findings was destroyed: %v", err)
	}
}

// TestRunValidationStep_ParkedResidueIsWhatDiscardRemoves closes the loop
// between the two halves of the residue contract. The park writes the path
// list into its findings and discard reads it back out of them, so a test that
// hand-builds that payload on both sides passes while the two disagree in
// production. Here the park's own outcome.Findings is what discard is handed,
// exactly as the executor hands it.
//
// The awkward names are the point of half the cases. core.quotePath C-quotes a
// path holding a non-ASCII, quote, or backslash byte, so a park that recorded
// the quoted spelling hands git a pathspec matching nothing and discard reports
// success over a file still sitting there. The embedded repository is the same
// failure from the other side: git status reports it as one entry, and clean
// without -ffd exits 0 without touching it.
func TestRunValidationStep_ParkedResidueIsWhatDiscardRemoves(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)

	const accented = "café.txt"
	if err := os.WriteFile(filepath.Join(dir, accented), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", accented)
	gitCmd(t, dir, "commit", "-m", "add an accented path")
	headSHA := strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	tracked := filepath.Join(dir, "feature.txt")
	original, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}

	const quoted = `say "hi".txt`
	const backslashed = `back\slash.txt`
	const embedded = "vendor-clone"

	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(tracked, []byte("residue\n"), 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, accented), []byte("residue\n"), 0o644); err != nil {
			return nil, err
		}
		for _, name := range []string{"scratch.txt", quoted, backslashed} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("scratch\n"), 0o644); err != nil {
				return nil, err
			}
		}
		// An embedded repository. git status collapses it to a single entry
		// naming the directory, so discard is handed a directory path and not
		// the files inside it.
		if err := os.MkdirAll(filepath.Join(dir, embedded), 0o755); err != nil {
			return nil, err
		}
		gitCmd(t, filepath.Join(dir, embedded), "init")
		if err := os.WriteFile(filepath.Join(dir, embedded, "junk.txt"), []byte("junk\n"), 0o644); err != nil {
			return nil, err
		}
		// A path the agent staged. git diff against HEAD reports it, so the
		// park records it as tracked residue even though HEAD has no such file.
		if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
			return nil, err
		}
		gitCmd(t, dir, "add", "staged.txt")
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}

	parked, err := runValidationStep(sctx, types.StepReview, func(inner *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if _, err := inner.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{ReviewApprovedHeadSHA: headSHA}, nil
	})
	if err != nil {
		t.Fatalf("runValidationStep() error = %v", err)
	}
	if !parked.NeedsApproval {
		t.Fatal("NeedsApproval = false, want the certifying step parked over its residue")
	}

	residue, ok := sctx.Shared.ValidationResidue(types.StepReview)
	if !ok {
		t.Fatal("the park recorded no residue, want the paths it refused to commit")
	}
	recorded := append(append([]string{}, residue.Modified...), residue.Untracked...)
	// git reports the embedded repository with a trailing slash, and discard
	// has to cope with the directory spelling it was actually handed.
	for _, want := range []string{accented, quoted, backslashed, embedded + "/"} {
		if !slices.Contains(recorded, want) {
			t.Fatalf("park recorded %q, want it to carry %q verbatim", recorded, want)
		}
	}

	if err := (&ReviewStep{}).DiscardApprovalResidue(sctx); err != nil {
		t.Fatalf("DiscardApprovalResidue() error = %v", err)
	}
	restored, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatalf("tracked file = %q, want it restored to %q", restored, original)
	}
	accentedRestored, err := os.ReadFile(filepath.Join(dir, accented))
	if err != nil {
		t.Fatal(err)
	}
	if string(accentedRestored) != "committed\n" {
		t.Fatalf("accented tracked file = %q, want it restored to its committed content", accentedRestored)
	}
	for _, gone := range []string{"scratch.txt", "staged.txt", quoted, backslashed, embedded} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Fatalf("residue %q still present (stat err = %v), want it removed", gone, err)
		}
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree = %q, want the recorded residue fully discarded", status)
	}
}

// TestRunValidationStep_ResidueGateFixAnswerCommitsAndRecertifies drives the
// other answer the residue gate offers. Fixing keeps the leftovers: the step's
// own fix round commits them and certifies the head that results, so the run
// ships the work rather than the tree the park refused to certify over.
func TestRunValidationStep_ResidueGateFixAnswerCommitsAndRecertifies(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("residue\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"keep the residue"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}

	parked, err := runValidationStep(sctx, types.StepReview, func(inner *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if _, err := inner.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{ReviewApprovedHeadSHA: headSHA}, nil
	})
	if err != nil {
		t.Fatalf("runValidationStep() error = %v", err)
	}
	if !parked.NeedsApproval {
		t.Fatal("NeedsApproval = false, want the residue gate")
	}

	sctx.Fixing = true
	fixed, err := runValidationStep(sctx, types.StepReview, func(inner *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if err := commitAgentFixes(inner, types.StepReview, "keep the residue", "keep the residue"); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{ReviewApprovedHeadSHA: inner.Run.HeadSHA}, nil
	})
	if err != nil {
		t.Fatalf("fix round error = %v", err)
	}
	if fixed.NeedsApproval {
		t.Fatal("NeedsApproval = true, want the fix round to clear the gate")
	}
	if fixed.ReviewApprovedHeadSHA == headSHA {
		t.Fatal("the fix round certified the pre-residue head, want the head its commit produced")
	}
	if fixed.ReviewApprovedHeadSHA != sctx.Run.HeadSHA {
		t.Fatalf("ReviewApprovedHeadSHA = %q, want the new head %q", fixed.ReviewApprovedHeadSHA, sctx.Run.HeadSHA)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree = %q, want the residue committed", status)
	}
	if got := strings.TrimSpace(gitCmd(t, dir, "show", "HEAD:feature.txt")); got != "residue" {
		t.Fatalf("committed feature.txt = %q, want the residue kept", got)
	}
}

// TestRunValidationStep_NoProgressCommitParksInsteadOfWalkingOn covers the
// churn path. A step that re-commits the tree its own last restart produced
// holds the run at a gate naming the step and the repeated tree, rather than
// walking forward to a push that fails on a missing certification and explains
// nothing. The certification is left intact so approving really can ship it.
func TestRunValidationStep_NoProgressCommitParksInsteadOfWalkingOn(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	agentWrites := "package main\n"
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		// Round 2 writes the same content back over a file the pipeline
		// reverted, so its commit lands on a tree identical to round 1's.
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(agentWrites), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}
	recordReviewApproval(t, sctx, headSHA)

	run := func() *pipeline.StepOutcome {
		t.Helper()
		outcome, err := runValidationStep(sctx, types.StepDocument, func(inner *pipeline.StepContext) (*pipeline.StepOutcome, error) {
			if _, err := inner.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
				return nil, err
			}
			return &pipeline.StepOutcome{AutoFixable: true}, nil
		})
		if err != nil {
			t.Fatalf("runValidationStep() error = %v", err)
		}
		return outcome
	}

	if got := run().RestartFrom; got != types.StepReview {
		t.Fatalf("first round RestartFrom = %q, want %q", got, types.StepReview)
	}
	// Undo the first commit's content so the second round has something to
	// commit whose result is the very tree the first round already produced.
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("reverted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	revert, err := runValidationStep(sctx, types.StepDocument, func(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
		return &pipeline.StepOutcome{}, nil
	})
	if err != nil {
		t.Fatalf("revert commit error = %v", err)
	}
	if revert.RestartFrom != "" {
		t.Fatalf("tool-authored revert RestartFrom = %q, want empty", revert.RestartFrom)
	}
	churned := run()
	if churned.RestartFrom != "" {
		t.Fatalf("second round RestartFrom = %q, want empty (no progress)", churned.RestartFrom)
	}
	if !churned.NeedsApproval {
		t.Fatal("NeedsApproval = false, want the run parked over the repeating tree")
	}
	if churned.AutoFixable {
		t.Fatal("AutoFixable = true; the executor auto-fixes before it parks, so the churn gate would never be seen")
	}
	findings, err := types.ParseFindingsJSON(churned.Findings)
	if err != nil {
		t.Fatalf("parse churn findings: %v", err)
	}
	if !types.HasActionableFindings(findings) {
		t.Fatal("churn finding is no-op, want it actionable so --yes cannot wave a repeating tree through")
	}
	tree := strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD^{tree}"))
	var described bool
	for _, item := range findings.Items {
		if strings.Contains(item.Description, string(types.StepDocument)) && strings.Contains(item.Description, tree) {
			described = true
		}
	}
	if !described {
		t.Fatalf("no finding names the churning step and tree %s: %s", tree, churned.Findings)
	}

	// Approving means "ship it anyway", so the certification the run already
	// holds must still be there for push to accept.
	if sctx.Run.ReviewApprovedHeadSHA == nil {
		t.Fatal("in-memory ReviewApprovedHeadSHA = nil, want the certification left intact for the gate's approve answer")
	}
	stored, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ReviewApprovedHeadSHA == nil {
		t.Fatal("stored ReviewApprovedHeadSHA = NULL, want the certification left intact")
	}

	// The gate's fix answer: the step runs once more, and a round that finally
	// produces a different tree restarts normally. The recorded tree narrows
	// the loop rather than wedging the run at the gate forever.
	agentWrites = "package main // moved on\n"
	sctx.Fixing = true
	progressed, err := runValidationStep(sctx, types.StepDocument, func(inner *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if _, err := inner.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{}, nil
	})
	if err != nil {
		t.Fatalf("fix round error = %v", err)
	}
	if progressed.NeedsApproval {
		t.Fatal("NeedsApproval = true, want the gate cleared once the tree changed")
	}
	if progressed.RestartFrom != types.StepReview {
		t.Fatalf("RestartFrom = %q, want %q once the step produced a different tree", progressed.RestartFrom, types.StepReview)
	}
}

// TestValidationStep_ExecuteRoutesThroughTheSharedExitHelper drives each
// validation step's public Execute with an agent that leaves the worktree
// dirty, and asserts the behavior only runValidationStep produces. Deleting a
// step's wrapper line leaves that step's edits uncommitted with no restart
// asked for, which is exactly what each case here fails on.
//
// Review is the boundary and the certifier today, so it parks over the residue
// instead of committing it. The other three commit and re-enter validation.
func TestValidationStep_ExecuteRoutesThroughTheSharedExitHelper(t *testing.T) {
	t.Parallel()
	cleanReview, err := json.Marshal(Findings{
		Summary:       "no issues",
		RiskLevel:     "low",
		RiskRationale: "small change",
		RiskScope:     types.FindingsRiskScopeSourceOrExternal,
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		step   pipeline.Step
		output json.RawMessage
	}{
		{name: "review", step: &ReviewStep{}, output: cleanReview},
		{name: "test", step: &TestStep{}, output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"none","risk_scope":"source-or-external","summary":"ok"}`)},
		{name: "document", step: &DocumentStep{}, output: json.RawMessage(`{"findings":[],"summary":"update README"}`)},
		{name: "lint", step: &LintStep{}, output: json.RawMessage(`{"findings":[],"summary":"lint clean"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", headSHA)

			output := tc.output
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Updated\n"), 0o644); err != nil {
					return nil, err
				}
				return &agent.Result{Output: output}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Shared = &pipeline.RunShared{}
			before := commitCount(t, dir)

			outcome, err := tc.step.Execute(sctx)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			if tc.step.Name() == pipeline.RestartBoundary {
				if !outcome.NeedsApproval {
					t.Fatalf("NeedsApproval = false, want the certifying step parked over its residue; findings=%s", outcome.Findings)
				}
				if got := commitCount(t, dir) - before; got != 0 {
					t.Fatalf("new commits = %d, want 0 from the step that certifies", got)
				}
				if !strings.Contains(outcome.Findings, "residue-") || !strings.Contains(outcome.Findings, "README.md") {
					t.Fatalf("gate does not record the leftover file: %s", outcome.Findings)
				}
				return
			}
			if outcome.RestartFrom != pipeline.RestartBoundary {
				t.Fatalf("RestartFrom = %q, want %q", outcome.RestartFrom, pipeline.RestartBoundary)
			}
			if got := commitCount(t, dir) - before; got != 1 {
				t.Fatalf("new commits = %d, want the exit commit", got)
			}
			if status := gitStatusPorcelain(t, dir); status != "" {
				t.Fatalf("worktree = %q, want clean", status)
			}
		})
	}
}

// TestStep_RestartCarryoverReachesTheEvidencePass proves the non-fix path
// reads the findings a restart carried back. A restart re-entry is deliberately
// not a fix round, so a step that only consults PreviousFindings while fixing
// re-derives from scratch the verdict it already reported.
func TestStep_RestartCarryoverReachesTheEvidencePass(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	// The Test step runs two agent passes, discovery then evidence. Only the
	// evidence prompt is under test here, so discovery gets a valid layout
	// rather than the findings payload, which would fail its validation and
	// park the step before the evidence pass ever runs.
	var evidencePrompt string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(opts.Prompt, "Derive this repository's independently testable units") {
			return &agent.Result{Output: json.RawMessage(`{"units":[{"name":"repository","path":".","command":"exit 0"}],"selected":["repository"]}`)}, nil
		}
		evidencePrompt = opts.Prompt
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"none","risk_scope":"source-or-external","summary":"ok"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}
	sctx.Fixing = false
	sctx.PreviousFindings = `{"findings":[{"id":"carried-test-finding","severity":"warning","description":"assertion never ran","action":"no-op"}],"summary":"1 issue"}`

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if evidencePrompt == "" {
		t.Fatal("the evidence pass did not run")
	}
	if !strings.Contains(evidencePrompt, "carried-test-finding") {
		t.Fatalf("evidence prompt does not carry the restarted findings:\n%s", evidencePrompt)
	}
}

// TestLintStep_RestartCarryoverBypassesTheHousekeepingStash is the same rule on
// the lint side. The stashed combined document+lint result was produced before
// the restart, so it cannot have accounted for the findings the restart carried
// back; consuming it would answer the gate with a verdict that predates them.
func TestLintStep_RestartCarryoverBypassesTheHousekeepingStash(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"lint clean"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}
	sctx.Shared.SetHousekeepingLint(pipeline.HousekeepingLintResult{
		FindingsJSON: `{"findings":[],"summary":"stale"}`,
		Summary:      "stale",
	})
	sctx.Fixing = false
	sctx.PreviousFindings = `{"findings":[{"id":"carried-lint-finding","severity":"warning","description":"vet warning","action":"no-op"}],"summary":"1 issue"}`

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1 (the stash must not answer a restart re-entry)", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "carried-lint-finding") {
		t.Fatalf("lint prompt does not carry the restarted findings:\n%s", ag.calls[0].Prompt)
	}
	if outcome.FixSummary == "stale" {
		t.Fatal("outcome came from the stale housekeeping stash")
	}
}

// TestRestartBoundaryIsTheEarliestValidationStep pins the constant against the
// executor's real step list rather than a second hand-written copy of it. A
// boundary that is not itself a validation step, or that sits after one, would
// let that earlier step's agent commit skip re-validation entirely. Deriving
// the set from AllSteps means the Format step of issues #7/#8 is picked up the
// moment it exists.
func TestRestartBoundaryIsTheEarliestValidationStep(t *testing.T) {
	t.Parallel()
	validation := validationSteps()
	if len(validation) == 0 {
		t.Fatal("no validation steps found in AllSteps()")
	}
	earliest := validation[0].Name()
	for _, step := range validation[1:] {
		if step.Name().Order() < earliest.Order() {
			earliest = step.Name()
		}
	}
	if pipeline.RestartBoundary != earliest {
		t.Fatalf("RestartBoundary = %q, want the earliest validation step %q", pipeline.RestartBoundary, earliest)
	}
}

// TestCommitsOwnWorkAtExitMatchesTheValidationSteps keeps the predicate the
// daemon reads for the gate diff in step with the steps that actually route
// their exit through runValidationStep. A step that drifts onto the wrong side
// of it is either shown another step's commits as its own work or shown
// nothing when it committed.
func TestCommitsOwnWorkAtExitMatchesTheValidationSteps(t *testing.T) {
	t.Parallel()
	routed := map[types.StepName]bool{}
	for _, step := range validationSteps() {
		routed[step.Name()] = true
	}
	for _, step := range AllSteps() {
		name := step.Name()
		if got := pipeline.CommitsOwnWorkAtExit(name); got != routed[name] {
			t.Fatalf("CommitsOwnWorkAtExit(%q) = %v, want %v", name, got, routed[name])
		}
	}
}

// TestValidationStepsAreNotApprovalGateReconcilers guards the commit
// attribution counter. StepContext is copyable and a copy counts its agent
// turns independently, so a turn run through the copy reconcileApprovalGate
// makes would be invisible to the original and its commit would be attributed
// to a deterministic tool.
func TestValidationStepsAreNotApprovalGateReconcilers(t *testing.T) {
	t.Parallel()
	for _, step := range validationSteps() {
		if _, ok := step.(pipeline.ApprovalGateReconciler); ok {
			t.Fatalf("validation step %s implements ApprovalGateReconciler; its agent turns would escape commit attribution", step.Name())
		}
	}
}
