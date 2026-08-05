//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestWorkingPathRepoConfig covers trust_working_path_config, the opt-in that
// lets the maintainer's own checkout supply gate commands for a repo whose
// default branch cannot carry .no-mistakes.yaml.
//
// Both subtests use the same fixture: the trusted default-branch copy carries
// no commands, and the working path holds an UNCOMMITTED .no-mistakes.yaml
// whose lint command writes a marker file. That split is what makes the two
// copies distinguishable — the gate runs in an ephemeral worktree, so only the
// working-path read can see the on-disk edit.
func TestWorkingPathRepoConfig(t *testing.T) {
	t.Run("ignored_by_default", func(t *testing.T) {
		optOut := false
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := pushWithWorkingPathRepoConfig(t, h, "wp-off")

		run := h.WaitForRun("wp-off", 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}
		if _, err := os.Stat(markerPath); err == nil {
			t.Fatalf("working-path lint command ran with trust_working_path_config unset (marker %s exists); the opt-in must default off", markerPath)
		}
	})

	t.Run("applied_when_enabled", func(t *testing.T) {
		// Same fixture with the opt-in on. The command MUST run, which is what
		// proves the check above is a real guard and not a no-op.
		optOut := false
		h := NewHarness(t, SetupOpts{
			Agent:                  "claude",
			Scenario:               cleanReviewScenario(t),
			AllowRepoCommands:      &optOut,
			TrustWorkingPathConfig: true,
		})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := pushWithWorkingPathRepoConfig(t, h, "wp-on")

		run := h.WaitForRun("wp-on", 90*time.Second)
		if _, err := os.Stat(markerPath); err != nil {
			t.Fatalf("working-path lint command did not run with trust_working_path_config: true (marker %s missing); run status=%s err=%v", markerPath, run.Status, deref(run.Error))
		}
	})
}

// Exactly one .no-mistakes.yaml is trusted per run, so a present working-path
// copy REPLACES the default-branch copy instead of layering over it. The
// default branch carries a lint command and the working path carries only a
// test command: layering would run both, replacement runs only the test one.
// The lint marker is what makes the two semantics distinguishable.
func TestWorkingPathRepoConfigReplacesDefaultBranchCopy(t *testing.T) {
	optOut := false
	h := NewHarness(t, SetupOpts{
		Agent:                  "claude",
		Scenario:               cleanReviewScenario(t),
		AllowRepoCommands:      &optOut,
		TrustWorkingPathConfig: true,
	})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	markers := t.TempDir()
	defaultBranchMarker := filepath.Join(markers, "from-default-branch")
	workingPathMarker := filepath.Join(markers, "from-working-path")

	// Committed to the default branch, so it is the pinned-SHA trusted copy.
	writeRepoConfig(t, h.WorkDir, fmt.Sprintf("allow_repo_commands: false\ncommands:\n  lint: \"echo default > %s\"\n", defaultBranchMarker))
	for _, args := range [][]string{
		{"add", ".no-mistakes.yaml"},
		{"commit", "-m", "default-branch lint command"},
		{"push", "origin", "main"},
	} {
		if out, err := h.runGit(context.Background(), h.WorkDir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	h.CommitChange("wp-replace", "wp-replace.txt", "change to gate\n", "add wp-replace change")

	// Uncommitted, so only the working-path read can see it. It states a test
	// command and no lint command at all.
	writeRepoConfig(t, h.WorkDir, fmt.Sprintf("commands:\n  test: \"echo local > %s\"\n", workingPathMarker))

	h.PushToGate("wp-replace")
	run := h.WaitForRun("wp-replace", 90*time.Second)

	if _, err := os.Stat(workingPathMarker); err != nil {
		t.Fatalf("working-path test command did not run (marker %s missing); run status=%s err=%v", workingPathMarker, run.Status, deref(run.Error))
	}
	if _, err := os.Stat(defaultBranchMarker); err == nil {
		t.Fatalf("default-branch lint command ran (marker %s exists); a present working-path copy must replace it, not layer under it", defaultBranchMarker)
	}
}

// skip_steps is the standing form of --skip. It removes whole validation
// phases, so it is trusted-only; here the maintainer states it in the
// working-path copy, which is the whole point of the opt-in for a repo whose
// default branch cannot carry the file.
func TestWorkingPathRepoConfigSkipSteps(t *testing.T) {
	optOut := false
	h := NewHarness(t, SetupOpts{
		Agent:                  "claude",
		Scenario:               cleanReviewScenario(t),
		AllowRepoCommands:      &optOut,
		TrustWorkingPathConfig: true,
	})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	h.CommitChange("wp-skip", "wp-skip.txt", "change to gate\n", "add wp-skip change")
	writeRepoConfig(t, h.WorkDir, "skip_steps:\n  - document\n  - lint\n")

	h.PushToGate("wp-skip")
	run := h.WaitForRun("wp-skip", 90*time.Second)

	for _, name := range []types.StepName{types.StepDocument, types.StepLint} {
		step, ok := findStep(run.Steps, name)
		if !ok {
			t.Fatalf("expected a %s step in the run", name)
		}
		if step.Status != types.StepStatusSkipped {
			t.Fatalf("%s status = %s, want skipped: the trusted skip_steps list applies to every run", name, step.Status)
		}
	}
	review, ok := findStep(run.Steps, types.StepReview)
	if !ok || review.Status == types.StepStatusSkipped {
		t.Fatalf("review status = %+v, want it still run: skip_steps must skip only what it lists", review)
	}
}

func writeRepoConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write repo config: %v", err)
	}
}

// pushWithWorkingPathRepoConfig leaves an uncommitted .no-mistakes.yaml in the
// working path whose lint command writes a marker file, pushes an unrelated
// change through the gate, and returns the marker path. The pushed branch and
// the default branch both carry the harness config (no commands), so a marker
// can only come from the working-path read.
func pushWithWorkingPathRepoConfig(t *testing.T, h *Harness, branch string) string {
	t.Helper()
	markerPath := filepath.Join(t.TempDir(), "from-working-path")

	// A real change so rebase has a non-empty diff. Done first so the branch
	// exists before the working tree is dirtied.
	h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")

	// Uncommitted, so it reaches neither the pushed SHA nor the default branch.
	workingCfg := fmt.Sprintf("commands:\n  lint: \"echo local > %s\"\n", markerPath)
	if err := os.WriteFile(filepath.Join(h.WorkDir, ".no-mistakes.yaml"), []byte(workingCfg), 0o644); err != nil {
		t.Fatalf("write working-path repo config: %v", err)
	}

	h.PushToGate(branch)
	return markerPath
}
