package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates this package from the developer's own environment. Capture,
// relabel, and the object pool all shell out to git, so an ambient ~/.gitconfig
// (commit.gpgsign against a locked signing agent, core.hooksPath, gpg.format,
// init.templateDir) or harness-injected GIT_CONFIG_* would otherwise decide
// whether a fixture commit succeeds. A locked 1Password signing agent failed
// every capture fixture here with "failed to write commit object" after a ~50s
// timeout, which reads as a hang rather than a signing error.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nm-eval-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create eval test environment: %v\n", err)
		os.Exit(1)
	}
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create eval test HOME: %v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("USERPROFILE", home)
	_ = os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Agent harnesses inject git config (e.g. safe.bareRepository=explicit) via
	// GIT_CONFIG_COUNT/KEY_n/VALUE_n; a test that needs it re-sets it with
	// t.Setenv.
	_ = os.Unsetenv("GIT_CONFIG_COUNT")
	_ = os.Setenv("NO_MISTAKES_TELEMETRY", "off")
	_ = os.Setenv("NO_MISTAKES_NO_UPDATE_CHECK", "1")

	code := m.Run()

	_ = os.RemoveAll(dir)
	os.Exit(code)
}
