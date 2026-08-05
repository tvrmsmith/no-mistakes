package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadGlobal_SCM(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte("scm:\n  cli_wrapper: [\"op\", \"plugin\", \"run\", \"--\"]\n  gh_config_dir: /gh-config\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(cfg.SCM.CLIWrapper, " "), "op plugin run --"; got != want {
		t.Fatalf("scm.cli_wrapper = %q, want %q", got, want)
	}
	if cfg.SCM.GHConfigDir != "/gh-config" {
		t.Fatalf("scm.gh_config_dir = %q, want %q", cfg.SCM.GHConfigDir, "/gh-config")
	}
}

// The parsed global settings only matter if Merge carries them into the config
// the pipeline actually runs with: without that propagation every gh call
// authenticates as the unwrapped default identity.
func TestMerge_CarriesGlobalSCMIntoTheEffectiveConfig(t *testing.T) {
	t.Parallel()

	global := &GlobalConfig{}
	global.SCM.CLIWrapper = []string{"op", "plugin", "run", "--"}
	global.SCM.GHConfigDir = "/gh-config"

	merged := Merge(global, &RepoConfig{})
	if got, want := strings.Join(merged.SCM.CLIWrapper, " "), "op plugin run --"; got != want {
		t.Errorf("merged scm.cli_wrapper = %q, want %q", got, want)
	}
	if merged.SCM.GHConfigDir != "/gh-config" {
		t.Errorf("merged scm.gh_config_dir = %q, want %q", merged.SCM.GHConfigDir, "/gh-config")
	}
}

// scm is global-only: it selects which credentials the daemon's gh calls use,
// so a repository config - which a contributor can push - must never reach it.
func TestMerge_RepoConfigCannotSetSCM(t *testing.T) {
	t.Parallel()

	repo, err := LoadRepoFromBytes([]byte("scm:\n  cli_wrapper: [\"sh\", \"-c\"]\n  gh_config_dir: /attacker\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}

	merged := Merge(&GlobalConfig{}, repo)
	if len(merged.SCM.CLIWrapper) != 0 || merged.SCM.GHConfigDir != "" {
		t.Fatalf("merged scm = %+v, want the zero value (repo config must not override)", merged.SCM)
	}
}

func TestLoadGlobal_SCMDefaultsEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	// Empty must stay empty: a default wrapper would silently change which
	// credentials every gh call authenticates with.
	if len(cfg.SCM.CLIWrapper) != 0 || cfg.SCM.GHConfigDir != "" {
		t.Fatalf("scm = %+v, want zero value", cfg.SCM)
	}
}
