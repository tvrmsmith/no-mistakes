package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

// TestRunValidationStep_CommitAttributionMatrix crosses "an agent ran this
// round" with the kind of path the commit touches. Attribution is the only
// axis that decides a restart today: the documentation-glob exemption is issue
// #6 and does not exist yet, so a docs-only agent commit restarts exactly like
// a code one.
func TestRunValidationStep_CommitAttributionMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		agentRan    bool
		dirtyPath   string
		wantRestart types.StepName
	}{
		{name: "agent_docs", agentRan: true, dirtyPath: "README.md", wantRestart: types.StepReview},
		{name: "agent_code", agentRan: true, dirtyPath: "main.go", wantRestart: types.StepReview},
		{name: "tool_docs", agentRan: false, dirtyPath: "README.md", wantRestart: ""},
		{name: "tool_code", agentRan: false, dirtyPath: "main.go", wantRestart: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", headSHA)

			write := func() {
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
			if got := commitCount(t, dir) - before; got != 1 {
				t.Fatalf("new commits = %d, want 1", got)
			}
			if status := gitStatusPorcelain(t, dir); status != "" {
				t.Fatalf("worktree = %q, want clean", status)
			}
		})
	}
}

// TestRunValidationStep_BoundaryStepCommitDropsCertification covers the step
// that cannot restart into itself. Its own exit commit leaves the SHA it just
// certified a strict ancestor of the new head, and push accepts an ancestor,
// so the certification is dropped instead.
func TestRunValidationStep_BoundaryStepCommitDropsCertification(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}

	outcome, err := runValidationStep(sctx, types.StepReview, func(inner *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if _, err := inner.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{ReviewApprovedHeadSHA: "sha-x"}, nil
	})
	if err != nil {
		t.Fatalf("runValidationStep() error = %v", err)
	}
	if outcome.ReviewApprovedHeadSHA != "" {
		t.Fatalf("ReviewApprovedHeadSHA = %q, want empty", outcome.ReviewApprovedHeadSHA)
	}
	if outcome.RestartFrom != "" {
		t.Fatalf("RestartFrom = %q, want empty", outcome.RestartFrom)
	}
}

// TestRunValidationStep_NoProgressCommitDoesNotRestart proves a step that
// commits the same tree twice stops asking for a restart. This is what makes
// the loop terminate without a cap.
func TestRunValidationStep_NoProgressCommitDoesNotRestart(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	round := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		round++
		// Round 2 writes the same content back over a file the pipeline
		// reverted, so its commit lands on a tree identical to round 1's.
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}

	run := func() *pipeline.StepOutcome {
		t.Helper()
		outcome, err := runValidationStep(sctx, types.StepDocument, func(inner *pipeline.StepContext) (*pipeline.StepOutcome, error) {
			if _, err := inner.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
				return nil, err
			}
			return &pipeline.StepOutcome{}, nil
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
	if got := run().RestartFrom; got != "" {
		t.Fatalf("second round RestartFrom = %q, want empty (no progress)", got)
	}
}

// TestDocumentStep_AgentCommitRestartsValidation proves a real step routes its
// exit through the shared helper rather than reimplementing attribution.
func TestDocumentStep_AgentCommitRestartsValidation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Updated\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update README"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}

	outcome, err := (&DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome.RestartFrom != types.StepReview {
		t.Fatalf("RestartFrom = %q, want %q", outcome.RestartFrom, types.StepReview)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree = %q, want clean", status)
	}
}
