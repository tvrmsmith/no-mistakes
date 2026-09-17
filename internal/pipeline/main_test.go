package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates this package's git fixtures from the developer's own
// environment. The uncertified-range tests build real repositories and commit
// into them, so an ambient ~/.gitconfig (commit.gpgsign against a locked
// signing agent, core.hooksPath, gpg.format) or a harness-injected GIT_CONFIG_*
// decides whether a fixture commit succeeds. A locked 1Password signing agent
// failed every one of them with "failed to write commit object", which reads as
// a code regression rather than a signing error.
//
// Only git config is overridden here. HOME is left alone, unlike
// internal/eval/main_test.go and internal/cli/helpers_test.go, because nothing
// in this package resolves a path through it and a narrower override cannot
// disturb a test that does.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nm-pipeline-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create pipeline test environment: %v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Agent harnesses inject git config (e.g. safe.bareRepository=explicit) via
	// GIT_CONFIG_COUNT/KEY_n/VALUE_n; a test that needs it re-sets it with
	// t.Setenv.
	_ = os.Unsetenv("GIT_CONFIG_COUNT")

	code := m.Run()

	_ = os.RemoveAll(dir)
	os.Exit(code)
}
