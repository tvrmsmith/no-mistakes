package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// skip_steps removes whole validation phases from every run of a repository,
// so it is trusted-only for the same reason no_ci is: a pushed branch must not
// be able to switch off the gate that reviews it.

func TestLoadRepoFromBytes_SkipSteps(t *testing.T) {
	t.Run("parses and dedupes", func(t *testing.T) {
		cfg, err := LoadRepoFromBytes([]byte("skip_steps: [pr, ci, pr]\n"))
		if err != nil {
			t.Fatalf("LoadRepoFromBytes: %v", err)
		}
		want := []types.StepName{types.StepPR, types.StepCI}
		if !slices.Equal(cfg.SkipSteps, want) {
			t.Fatalf("SkipSteps = %v, want %v", cfg.SkipSteps, want)
		}
	})

	t.Run("rejects an unknown step", func(t *testing.T) {
		_, err := LoadRepoFromBytes([]byte("skip_steps: [pr, publish]\n"))
		if err == nil {
			t.Fatal("LoadRepoFromBytes = nil error, want a failure naming the unknown step")
		}
		if !strings.Contains(err.Error(), "publish") {
			t.Fatalf("error = %v, want it to name the unknown step", err)
		}
	})

	t.Run("absent key skips nothing", func(t *testing.T) {
		cfg, err := LoadRepoFromBytes([]byte("commands:\n  test: go test ./...\n"))
		if err != nil {
			t.Fatalf("LoadRepoFromBytes: %v", err)
		}
		if len(cfg.SkipSteps) != 0 {
			t.Fatalf("SkipSteps = %v, want none", cfg.SkipSteps)
		}
	})
}

func TestEffectiveRepoConfig_SkipStepsTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{SkipSteps: []types.StepName{types.StepReview, types.StepTest}}

	t.Run("a pushed branch cannot skip its own gates", func(t *testing.T) {
		trusted := &RepoConfig{SkipSteps: []types.StepName{types.StepCI}}

		got := EffectiveRepoConfig(pushed, trusted, false)

		if !slices.Equal(got.SkipSteps, trusted.SkipSteps) {
			t.Fatalf("SkipSteps = %v, want the trusted list %v", got.SkipSteps, trusted.SkipSteps)
		}
	})

	t.Run("allow_repo_commands does not widen it", func(t *testing.T) {
		trusted := &RepoConfig{SkipSteps: []types.StepName{types.StepCI}}

		got := EffectiveRepoConfig(pushed, trusted, true)

		if !slices.Equal(got.SkipSteps, trusted.SkipSteps) {
			t.Fatalf("SkipSteps = %v, want the trusted list %v even with allow_repo_commands", got.SkipSteps, trusted.SkipSteps)
		}
	})

	t.Run("no trusted copy skips nothing", func(t *testing.T) {
		got := EffectiveRepoConfig(pushed, nil, false)

		if len(got.SkipSteps) != 0 {
			t.Fatalf("SkipSteps = %v, want none", got.SkipSteps)
		}
	})
}

// The standing list and the per-run flag are additive: neither can re-enable
// what the other switched off.
func TestConfigSkippedSteps(t *testing.T) {
	cases := []struct {
		name     string
		standing []types.StepName
		runSkips []types.StepName
		want     []types.StepName
	}{
		{name: "neither", want: nil},
		{name: "standing only", standing: []types.StepName{types.StepPR}, want: []types.StepName{types.StepPR}},
		{name: "run only", runSkips: []types.StepName{types.StepLint}, want: []types.StepName{types.StepLint}},
		{
			name:     "union with the standing list first",
			standing: []types.StepName{types.StepPR, types.StepCI},
			runSkips: []types.StepName{types.StepLint},
			want:     []types.StepName{types.StepPR, types.StepCI, types.StepLint},
		},
		{
			name:     "overlap is deduped",
			standing: []types.StepName{types.StepPR, types.StepCI},
			runSkips: []types.StepName{types.StepCI},
			want:     []types.StepName{types.StepPR, types.StepCI},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{SkipSteps: slices.Clone(tc.standing)}

			got := cfg.SkippedSteps(tc.runSkips)

			if !slices.Equal(got, tc.want) {
				t.Fatalf("SkippedSteps(%v) = %v, want %v", tc.runSkips, got, tc.want)
			}
			if !slices.Equal(cfg.SkipSteps, tc.standing) {
				t.Errorf("cfg.SkipSteps mutated: %v", cfg.SkipSteps)
			}
		})
	}
}

func TestMerge_CarriesTrustedSkipSteps(t *testing.T) {
	repo := &RepoConfig{SkipSteps: []types.StepName{types.StepPR, types.StepCI}}

	cfg := Merge(&GlobalConfig{}, repo)

	if !slices.Equal(cfg.SkipSteps, repo.SkipSteps) {
		t.Fatalf("SkipSteps = %v, want %v", cfg.SkipSteps, repo.SkipSteps)
	}
	cfg.SkipSteps[0] = types.StepLint
	if repo.SkipSteps[0] != types.StepPR {
		t.Error("Merge aliased the repo config's slice")
	}
}

// The working-path copy is where a maintainer without default-branch access
// states skip_steps, so it has to survive the trusted-config resolution.
func TestResolveWorkingPathTrusted_SkipSteps(t *testing.T) {
	trusted := &RepoConfig{SkipSteps: []types.StepName{types.StepDocument}}
	workingPath := &RepoConfig{SkipSteps: []types.StepName{types.StepPR, types.StepCI}}

	got := ResolveWorkingPathTrusted(trusted, workingPath)

	if !slices.Equal(got.SkipSteps, workingPath.SkipSteps) {
		t.Fatalf("SkipSteps = %v, want the working-path list %v", got.SkipSteps, workingPath.SkipSteps)
	}
	if !slices.Equal(trusted.SkipSteps, []types.StepName{types.StepDocument}) {
		t.Errorf("trusted config mutated: %v", trusted.SkipSteps)
	}
}
