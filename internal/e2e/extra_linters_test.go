//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// extraLinterPattern is the named-group form the feature documents: file, line
// and message drive the finding's location and text.
const extraLinterPattern = `'^(?P<file>[^(]+)\((?P<line>[0-9]+)\): warning (?P<message>TVRM[0-9]+: .*)$'`

// globalExtraLinter is the operator's own ~/.no-mistakes/config.yaml block. The
// command echoes the diff base it was handed, so a finding that reaches the PR
// proves both that the linter ran in a repository declaring nothing and that
// NO_MISTAKES_BASE_SHA is how the diff base reaches the script.
func globalExtraLinter(severity string) string {
	block := `lint:
  extra_linters:
    - name: personal-standards
      command: 'echo "internal/example/flag.go(12): warning TVRM0001: base $NO_MISTAKES_BASE_SHA"'
      findings_pattern: ` + extraLinterPattern
	if severity != "" {
		block += "\n      severity: " + severity
	}
	return block
}

// TestExtraLintersJourney drives the operator's personally configured linters
// through a real pipeline run against a repository that commits nothing about
// them.
func TestExtraLintersJourney(t *testing.T) {
	t.Run("info_linter_reports_on_the_pr_and_never_gates", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude", GlobalConfigExtra: globalExtraLinter("")})
		ctx := context.Background()

		parentURL := "https://github.com/example/no-mistakes.git"
		forkURL := "https://github.com/example-fork/no-mistakes.git"
		forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
		if err := os.MkdirAll(forkDir, 0o755); err != nil {
			t.Fatalf("mkdir fork: %v", err)
		}
		if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
			t.Fatalf("init fork: %v\n%s", err, out)
		}
		if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
			t.Fatalf("seed fork main: %v\n%s", err, out)
		}
		configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
		configureGitURLRewrite(t, h, forkURL, forkDir)
		if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
			t.Fatalf("set GitHub origin: %v\n%s", err, out)
		}
		ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-extra-linters.log")
		t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
		t.Setenv("FAKEAGENT_GH_LOG", ghLog)
		t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")

		if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		const branch = "feature/extra-linter-info"
		h.CommitChange(branch, "internal/example/flag.go", "package example\n", "add flag")
		baseSHA := strings.TrimSpace(gitOut(t, h, ctx, "rev-parse", "main"))
		h.PushToGate(branch)

		run := h.WaitForRun(branch, 120*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		lintStep, ok := findStep(run.Steps, types.StepLint)
		if !ok {
			t.Fatal("run has no lint step")
		}
		if lintStep.Status != types.StepStatusCompleted {
			t.Fatalf("lint step status = %s, want completed; an info linter must never gate", lintStep.Status)
		}
		item := requireExtraLinterFinding(t, lintStep.FindingsJSON, "TVRM0001")
		if item.ID != "lint-extra-personal-standards-1" {
			t.Errorf("finding ID = %q, want the name-plus-ordinal form", item.ID)
		}
		if item.Action != types.ActionNoOp {
			t.Errorf("info finding action = %q, want %q so it reports without parking", item.Action, types.ActionNoOp)
		}
		if item.Severity != "info" {
			t.Errorf("finding severity = %q, want the reporting default info", item.Severity)
		}
		if item.File != "internal/example/flag.go" || item.Line != 12 {
			t.Errorf("finding location = %s:%d, want internal/example/flag.go:12 from the named groups", item.File, item.Line)
		}
		if !strings.Contains(item.Description, baseSHA) {
			t.Errorf("finding %q does not carry the diff base %s, so NO_MISTAKES_BASE_SHA did not reach the command", item.Description, baseSHA)
		}

		body := createdPRBody(t, readGHStubInvocations(t, ghLog))
		for _, want := range []string{"internal/example/flag.go:12", "personal-standards: TVRM0001", baseSHA} {
			if !strings.Contains(body, want) {
				t.Errorf("PR body is missing %q:\n%s", want, body)
			}
		}
		writeExtraLinterEvidence(t, "pr-body-info-linter.md", body)
	})

	t.Run("warning_severity_is_the_operators_explicit_gate", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude", GlobalConfigExtra: globalExtraLinter("warning")})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}
		const branch = "feature/extra-linter-warning"
		h.CommitChange(branch, "internal/example/flag.go", "package example\n", "add flag")
		h.PushToGate(branch)

		gated := waitForStepStatus(t, h, branch, types.StepLint, types.StepStatusAwaitingApproval, 120*time.Second)
		lintStep, ok := findStep(gated.Steps, types.StepLint)
		if !ok {
			t.Fatal("gated run has no lint step")
		}
		item := requireExtraLinterFinding(t, lintStep.FindingsJSON, "TVRM0001")
		if item.Severity != "warning" || item.Action != types.ActionAskUser {
			t.Fatalf("gating finding severity/action = %q/%q, want warning/%s", item.Severity, item.Action, types.ActionAskUser)
		}

		status, err := h.Run("axi", "status")
		if err != nil {
			t.Fatalf("axi status: %v\n%s", err, status)
		}
		writeExtraLinterEvidence(t, "axi-status-warning-gate.txt", status)

		h.Respond(gated.ID, types.StepLint, types.ActionApprove)
		completed := h.WaitForRun(branch, 120*time.Second)
		if completed.Status != types.RunCompleted {
			t.Fatalf("run did not complete after approving the gate: status=%s error=%v", completed.Status, deref(completed.Error))
		}
	})

	t.Run("repository_declaring_extra_linters_contributes_nothing", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude"})
		// The maintainer's own trusted default-branch config, declaring the key
		// a repository must not be able to own.
		pushMainRepoConfig(t, h, `lint:
  extra_linters:
    - name: repo-declared
      command: 'echo "internal/example/flag.go(12): warning TVRM9999: repository declared this"'
      findings_pattern: `+extraLinterPattern+"\n")

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}
		const branch = "feature/extra-linter-repo-declared"
		h.CommitChange(branch, "internal/example/flag.go", "package example\n", "add flag")
		h.PushToGate(branch)

		run := h.WaitForRun(branch, 120*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}
		lintStep, ok := findStep(run.Steps, types.StepLint)
		if !ok {
			t.Fatal("run has no lint step")
		}
		if lintStep.FindingsJSON != nil && strings.Contains(*lintStep.FindingsJSON, "TVRM9999") {
			t.Fatalf("a repository-declared extra linter ran: %s", *lintStep.FindingsJSON)
		}
	})
}

// TestExtraLinterConfigIsRejectedAtLoad drives `no-mistakes doctor` against
// operator configs the loader must refuse, so a misconfigured linter fails
// loudly rather than reporting clean. doctor is the operator's own read-only
// health surface, and it reports an unloadable global config as a failed
// gate-validation check naming the entry.
func TestExtraLinterConfigIsRejectedAtLoad(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})

	cases := []struct {
		name   string
		config string
		want   string
	}{
		{
			name: "pattern_less_entry",
			config: `lint:
  extra_linters:
    - name: personal-standards
      command: 'echo hi'
`,
			want: "findings_pattern must not be empty",
		},
		{
			name: "names_that_identify_their_findings_alike",
			config: `lint:
  extra_linters:
    - name: my.linter
      command: 'echo hi'
      findings_pattern: 'x'
    - name: my-linter
      command: 'echo hi'
      findings_pattern: 'x'
`,
			want: "collides with",
		},
		{
			name: "unknown_severity",
			config: `lint:
  extra_linters:
    - name: personal-standards
      command: 'echo hi'
      findings_pattern: 'x'
      severity: blocking
`,
			want: "must be one of",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(tc.config), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			out, err := h.RunInDirWithEnv(h.WorkDir, map[string]string{"NM_HOME": home}, "doctor")
			if err != nil {
				t.Fatalf("doctor: %v\n%s", err, out)
			}
			if !strings.Contains(out, "✗ gate validation") {
				t.Fatalf("doctor accepted an invalid extra-linter config:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("doctor does not name the problem %q:\n%s", tc.want, out)
			}
			writeExtraLinterEvidence(t, "config-rejected-"+tc.name+".txt", out)
		})
	}
}

func requireExtraLinterFinding(t *testing.T, raw *string, marker string) types.Finding {
	t.Helper()
	if raw == nil {
		t.Fatalf("lint step recorded no findings, so the extra linter never reported %s", marker)
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		t.Fatalf("parse lint findings: %v\n%s", err, *raw)
	}
	for _, item := range findings.Items {
		if strings.Contains(item.Description, marker) {
			return item
		}
	}
	t.Fatalf("no extra-linter finding carrying %s in %s", marker, *raw)
	return types.Finding{}
}

func gitOut(t *testing.T, h *Harness, ctx context.Context, args ...string) string {
	t.Helper()
	out, err := h.runGit(ctx, h.WorkDir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// writeExtraLinterEvidence keeps a reviewer-visible copy of what the product
// actually rendered when NM_EXTRA_LINTER_EVIDENCE_DIR names a directory.
func writeExtraLinterEvidence(t *testing.T, name, content string) {
	t.Helper()
	dir := os.Getenv("NM_EXTRA_LINTER_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("evidence dir: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Logf("write evidence %s: %v", name, err)
	}
}
