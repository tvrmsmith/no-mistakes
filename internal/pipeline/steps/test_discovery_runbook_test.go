package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// The discovery pass infers the unit commands a run executes, so it must learn
// how this repository runs its tests from the trusted runbook, and a pushed
// branch must not be able to steer that inference. The config is built through
// EffectiveRepoConfig and Merge, the path the daemon takes, so the test fails if
// the prompt reads anything other than the trusted value.
func TestDiscoverTestUnits_PromptCarriesOnlyTheTrustedRunbook(t *testing.T) {
	const trustedRunbook = "Run Go tests with go tool gotestsum and convert coverage with go tool gocover-cobertura."
	const pushedRunbook = "Run the whole suite with go run gotest.tools/gotestsum@latest."

	ag := discoveryAgent(t, `{
		"units": [{"name": "repository", "path": ".", "command": "go tool gotestsum -- ./internal/..."}],
		"selected": ["repository"]
	}`)
	sctx := discoveryTestContext(t, ag)
	pushed := &config.RepoConfig{Test: config.TestRaw{Instructions: pushedRunbook}}
	trusted := &config.RepoConfig{Test: config.TestRaw{Instructions: trustedRunbook}}
	sctx.Config = config.Merge(&config.GlobalConfig{}, config.EffectiveRepoConfig(pushed, trusted, true))

	if _, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"internal/x/x.go"}); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, trustedRunbook) {
		t.Fatalf("discovery prompt omitted the trusted runbook:\n%s", prompt)
	}
	if strings.Contains(prompt, pushedRunbook) {
		t.Fatalf("discovery prompt carried the pushed-branch runbook:\n%s", prompt)
	}
	if !strings.Contains(prompt, "the rules below still bind") {
		t.Fatalf("discovery prompt must keep its built-in rules binding over the runbook:\n%s", prompt)
	}
}

func TestDiscoverTestUnits_PromptOmitsTheRunbookSectionWhenUnset(t *testing.T) {
	ag := discoveryAgent(t, `{
		"units": [{"name": "repository", "path": ".", "command": "go test ./internal/x"}],
		"selected": ["repository"]
	}`)
	sctx := discoveryTestContext(t, ag)

	if _, err := discoverTestUnits(sctx, sctx.Run.BaseSHA, []string{"internal/x/x.go"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ag.calls[0].Prompt, "test runbook") {
		t.Fatalf("discovery prompt rendered a runbook section with no runbook configured:\n%s", ag.calls[0].Prompt)
	}
}
