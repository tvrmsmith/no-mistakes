package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAgentCommitRestartsValidationEndToEnd joins the two halves the other
// restart tests cover separately: the shared exit helper in this package and
// the executor seam in internal/pipeline. It drives the real Format, Review,
// and Document steps through a real executor over a real git repository, so
// the whole observable chain runs once - the document agent edits the
// worktree, the helper commits that edit and attributes it to the agent, the
// executor rewinds to Format, and the run ends with a certification covering
// the post-restart head and a restart recorded on the run row.
//
// The slice carries Format because it is the restart boundary: an executor
// whose steps do not include the boundary rejects the restart outright, which
// is the executor's guard rather than anything about this scenario.
func TestAgentCommitRestartsValidationEndToEnd(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	var turns []string
	documentTurns := 0
	ag := &mockAgent{name: "evidence", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(strings.ToLower(opts.Prompt), "documentation") {
			documentTurns++
			turns = append(turns, fmt.Sprintf("document agent turn %d", documentTurns))
			if documentTurns == 1 {
				// The documentation agent edits the worktree, exactly the
				// case the restart exists for.
				if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# documented by the agent\n"), 0o644); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"documented the new flag"}`)}, nil
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs already current"}`)}, nil
		}
		turns = append(turns, fmt.Sprintf("review agent turn %d", len(turns)+1-documentTurns))
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"looks good"}`)}, nil
	}}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	steps := []pipeline.Step{&FormatStep{}, &ReviewStep{}, &DocumentStep{}}
	exec := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, steps, nil)

	headBefore := strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))
	if err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err != nil {
		t.Fatalf("execute: %v", err)
	}
	headAfter := strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	results, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatalf("get step results: %v", err)
	}
	type roundRow struct {
		step  types.StepName
		round int
	}
	var rounds []roundRow
	for _, sr := range results {
		rs, err := sctx.DB.GetRoundsByStep(sr.ID)
		if err != nil {
			t.Fatalf("get rounds for %s: %v", sr.StepName, err)
		}
		for _, r := range rs {
			rounds = append(rounds, roundRow{step: sr.StepName, round: r.Round})
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "pipeline: format -> review -> document (real steps, real executor, real git repo %s)\n\n", dir)
	fmt.Fprintf(&b, "head before run: %s\n", headBefore[:8])
	fmt.Fprintln(&b, "agent turns, in order:")
	for _, turn := range turns {
		fmt.Fprintf(&b, "  - %s\n", turn)
	}
	fmt.Fprintf(&b, "\ncommit the document step made at its exit:\n%s\n",
		strings.TrimSpace(gitCmd(t, dir, "log", "-1", "--format=  %h %s")))
	fmt.Fprintf(&b, "\nhead after run:  %s (advanced: %v)\n", headAfter[:8], headAfter != headBefore)
	fmt.Fprintf(&b, "runs.restart_count:            %d\n", run.RestartCount)
	fmt.Fprintf(&b, "runs.review_approved_head_sha: %s\n", certificationState(run.ReviewApprovedHeadSHA, headAfter))
	fmt.Fprintln(&b, "\nstep rounds recorded in the database:")
	for _, r := range rounds {
		fmt.Fprintf(&b, "  %s round %d\n", r.step, r.round)
	}

	t.Log("\n" + b.String())

	if documentTurns != 2 {
		t.Fatalf("document agent turns = %d, want 2 (the restart re-enters validation and returns)", documentTurns)
	}
	if run.RestartCount != 1 {
		t.Fatalf("restart count = %d, want 1", run.RestartCount)
	}
	if headAfter == headBefore {
		t.Fatal("the agent's edit was never committed at the step's exit")
	}
	var reviewRounds int
	for _, r := range rounds {
		if r.step == types.StepReview {
			reviewRounds++
		}
	}
	if reviewRounds < 2 {
		t.Fatalf("review rounds = %d, want at least 2 (validation re-entered)", reviewRounds)
	}
	// The certification the run ends on must cover the commit the document
	// agent made, not the head the first review saw. Push accepts a certified
	// ancestor, so a surviving pre-restart approval would authorise a tree
	// nothing reviewed.
	if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != headAfter {
		t.Fatalf("review_approved_head_sha = %v, want the post-restart head %s",
			run.ReviewApprovedHeadSHA, headAfter)
	}
}

func certificationState(sha *string, head string) string {
	if sha == nil {
		return "<null>"
	}
	if *sha == head {
		return (*sha)[:8] + " (the post-restart head)"
	}
	return (*sha)[:8] + " (STALE, an ancestor of the head)"
}
