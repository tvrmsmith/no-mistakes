package config

import (
	"slices"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The working-path copy exists for gated repos whose default branch is owned by
// a team that does not run no-mistakes, so there is nowhere trusted to commit
// commands. Exactly one config file is trusted per run: when the working-path
// copy is present it IS that file, so it replaces the default-branch copy
// rather than layering over it.

func TestResolveWorkingPathTrusted_NilWorkingPathIsNoOp(t *testing.T) {
	trusted := &RepoConfig{Commands: Commands{Test: "go test ./..."}}
	if got := ResolveWorkingPathTrusted(trusted, nil); got != trusted {
		t.Fatalf("ResolveWorkingPathTrusted(trusted, nil) = %v, want the trusted config unchanged", got)
	}
}

// A present file is the whole answer, so the fields it leaves out are unset -
// not inherited. Otherwise a maintainer could never retire a default-branch
// command from the working-path copy, and the run would execute a command they
// cannot see in the file they were told steers the gate.
func TestResolveWorkingPathTrusted_ReplacesRatherThanLayers(t *testing.T) {
	trusted := &RepoConfig{Commands: Commands{Test: "go test ./...", Lint: "golangci-lint run"}}
	workingPath := &RepoConfig{Commands: Commands{Test: "imr verify --integration"}}

	got := ResolveWorkingPathTrusted(trusted, workingPath)

	if got.Commands.Test != "imr verify --integration" {
		t.Errorf("Commands.Test = %q, want the working-path value", got.Commands.Test)
	}
	if got.Commands.Lint != "" {
		t.Errorf("Commands.Lint = %q, want it unset: the working-path copy replaces the default-branch copy outright", got.Commands.Lint)
	}
}

func TestResolveWorkingPathTrusted_EmptyFileClearsTheTrustedCopy(t *testing.T) {
	trusted := &RepoConfig{Commands: Commands{Test: "go test ./...", Lint: "golangci-lint run"}}

	got := ResolveWorkingPathTrusted(trusted, &RepoConfig{})

	if got.Commands != (Commands{}) {
		t.Fatalf("Commands = %+v, want empty: a present-but-empty working-path file states there are no trusted commands", got.Commands)
	}
}

func TestResolveWorkingPathTrusted_DoesNotAdoptAllowRepoCommands(t *testing.T) {
	// allow_repo_commands hands commands/agent to whatever a contributor
	// pushed. A local convenience override must not widen that boundary, so it
	// is the one field still read from the default-branch copy.
	t.Run("working path cannot enable it", func(t *testing.T) {
		got := ResolveWorkingPathTrusted(&RepoConfig{AllowRepoCommands: false}, &RepoConfig{AllowRepoCommands: true})
		if got.AllowRepoCommands {
			t.Fatal("AllowRepoCommands = true, want false: the working-path copy must not be able to enable pushed-branch commands")
		}
	})
	t.Run("default branch still owns it", func(t *testing.T) {
		got := ResolveWorkingPathTrusted(&RepoConfig{AllowRepoCommands: true}, &RepoConfig{})
		if !got.AllowRepoCommands {
			t.Fatal("AllowRepoCommands = false, want true: replacement must not silently retract the default branch's own opt-in")
		}
	})
}

// agent selects which process launches with the maintainer's credentials, so
// each way the working-path copy states it is pinned: a list wins with its head
// re-derived, a bare agent wins with no leftover list, and an absent agent
// leaves the trusted selection behind along with the rest of that copy.
func TestResolveWorkingPathTrusted_AgentSelection(t *testing.T) {
	trustedConfig := func() *RepoConfig {
		return &RepoConfig{
			Agent:  types.AgentClaude,
			Agents: []types.AgentName{types.AgentClaude, types.AgentCodex},
		}
	}

	t.Run("working-path list wins and re-derives its head", func(t *testing.T) {
		trusted := trustedConfig()
		workingPath := &RepoConfig{Agents: []types.AgentName{types.AgentCodex, types.AgentClaude}}

		got := ResolveWorkingPathTrusted(trusted, workingPath)

		if !slices.Equal(got.Agents, workingPath.Agents) {
			t.Errorf("Agents = %v, want the working-path list %v", got.Agents, workingPath.Agents)
		}
		if got.Agent != types.AgentCodex {
			t.Errorf("Agent = %q, want %q re-derived from the working-path list head", got.Agent, types.AgentCodex)
		}
		if !slices.Equal(trusted.Agents, trustedConfig().Agents) {
			t.Errorf("trusted config mutated: %v", trusted.Agents)
		}
	})

	t.Run("working-path single agent leaves no fallback list", func(t *testing.T) {
		got := ResolveWorkingPathTrusted(trustedConfig(), &RepoConfig{Agent: types.AgentCodex})

		if got.Agent != types.AgentCodex {
			t.Errorf("Agent = %q, want the working-path value", got.Agent)
		}
		if len(got.Agents) != 0 {
			t.Errorf("Agents = %v, want none: a leftover trusted list would outrank the agent the maintainer chose", got.Agents)
		}
	})

	t.Run("absent working-path agent selects nothing", func(t *testing.T) {
		got := ResolveWorkingPathTrusted(trustedConfig(), &RepoConfig{Commands: Commands{Test: "imr verify"}})

		if got.Agent != "" || len(got.Agents) != 0 {
			t.Errorf("Agent = %q, Agents = %v, want no selection: the replaced copy's agent does not survive", got.Agent, got.Agents)
		}
	})
}

func TestResolveWorkingPathTrusted_DisableProjectSettingsTrueWins(t *testing.T) {
	// A plain bool cannot distinguish "set to false" from "absent", so
	// replacement may turn the opt-out on but never off.
	t.Run("working path turns it on", func(t *testing.T) {
		got := ResolveWorkingPathTrusted(&RepoConfig{}, &RepoConfig{DisableProjectSettings: true})
		if !got.DisableProjectSettings {
			t.Fatal("DisableProjectSettings = false, want true")
		}
	})
	t.Run("replacement cannot turn it off", func(t *testing.T) {
		got := ResolveWorkingPathTrusted(&RepoConfig{DisableProjectSettings: true}, &RepoConfig{})
		if !got.DisableProjectSettings {
			t.Fatal("DisableProjectSettings = false, want true: the weakening direction must not be reachable")
		}
	})
}

func TestResolveWorkingPathTrusted_DoesNotMutateEitherInput(t *testing.T) {
	trusted := &RepoConfig{AllowRepoCommands: true, Commands: Commands{Test: "go test ./..."}}
	workingPath := &RepoConfig{Commands: Commands{Test: "imr verify"}}

	got := ResolveWorkingPathTrusted(trusted, workingPath)
	got.Commands.Test = "mutated"

	if trusted.Commands.Test != "go test ./..." {
		t.Errorf("trusted.Commands.Test = %q, want it untouched", trusted.Commands.Test)
	}
	if workingPath.Commands.Test != "imr verify" {
		t.Errorf("workingPath.Commands.Test = %q, want it untouched", workingPath.Commands.Test)
	}
	if workingPath.AllowRepoCommands {
		t.Error("workingPath.AllowRepoCommands = true, want it untouched")
	}
}

// review.path_instructions is a maintainer-owned trusted field like
// document.instructions, so a maintainer who cannot commit to the default
// branch must be able to supply it from the working path - and, equally, to
// retire a default-branch rule by leaving it out.
func TestResolveWorkingPathTrusted_ReviewPathInstructions(t *testing.T) {
	trustedConfig := func() *RepoConfig {
		return &RepoConfig{Review: ReviewRaw{PathInstructions: []PathInstruction{
			{Path: "internal/**", Instructions: "default-branch rule"},
		}}}
	}

	t.Run("working-path list wins", func(t *testing.T) {
		trusted := trustedConfig()
		workingPath := &RepoConfig{Review: ReviewRaw{PathInstructions: []PathInstruction{
			{Path: "internal/db/**", Instructions: "working-path rule"},
		}}}

		got := ResolveWorkingPathTrusted(trusted, workingPath)

		if len(got.Review.PathInstructions) != 1 || got.Review.PathInstructions[0].Instructions != "working-path rule" {
			t.Fatalf("Review.PathInstructions = %+v, want the working-path list", got.Review.PathInstructions)
		}
		if len(trusted.Review.PathInstructions) != 1 || trusted.Review.PathInstructions[0].Instructions != "default-branch rule" {
			t.Errorf("trusted config mutated: %+v", trusted.Review.PathInstructions)
		}
	})

	t.Run("absent working-path list retires the default-branch rules", func(t *testing.T) {
		got := ResolveWorkingPathTrusted(trustedConfig(), &RepoConfig{})

		if len(got.Review.PathInstructions) != 0 {
			t.Fatalf("Review.PathInstructions = %+v, want none: the replaced copy's rules do not survive", got.Review.PathInstructions)
		}
	})
}
