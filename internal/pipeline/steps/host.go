package steps

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/azuredevops"
	"github.com/kunchenguid/no-mistakes/internal/scm/forgejo"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitea"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
)

// hostSkip explains why buildHost could not build a host. Reason is the
// human-readable text. Unnameable separates the two cases a caller must not
// treat alike: the repository is hosted on a provider this run understands but
// cannot be named, which leaves the forge's answer unknown, versus every other
// case, where there is simply no provider integration to consult here. A step
// that verifies forge state may complete on the second and must not on the
// first, because an unknown answer is not an answer.
type hostSkip struct {
	Reason     string
	Unnameable bool
}

func (h hostSkip) String() string { return h.Reason }

func skipReasonf(format string, args ...any) hostSkip {
	return hostSkip{Reason: fmt.Sprintf(format, args...)}
}

// unnameableRepo is the one rule every provider shares: this run understands
// the provider but cannot name the repository on it, so the forge's answer is
// unknown rather than empty. Every provider arm builds the classification here
// instead of repeating the flag, because three independently editable copies of
// one rule is how the providers drift apart. Having no integration configured
// at all is a different case and is not built through this.
func unnameableRepo(reason string) hostSkip {
	return hostSkip{Reason: reason, Unnameable: true}
}

// resolvedProvider returns the run-scoped provider selected by forge profile
// routing. Runs without a selected profile retain the legacy URL-based
// detection, including the PR URL fallback used during recovery.
func resolvedProvider(sctx *pipeline.StepContext) scm.Provider {
	if sctx.ForgeContext != nil {
		return sctx.ForgeContext.Provider
	}
	provider := detectProviderForStep(sctx, sctx.Repo.UpstreamURL)
	if provider == scm.ProviderUnknown && sctx.Run.PRURL != nil {
		provider = detectProviderForStep(sctx, *sctx.Run.PRURL)
	}
	return provider
}

func resolvedHost(sctx *pipeline.StepContext, remote string) string {
	if sctx.ForgeContext != nil && sctx.ForgeContext.Host != "" {
		return sctx.ForgeContext.Host
	}
	return scm.ResolveHost(sctx.Ctx, remote)
}

// buildHost returns a scm.Host for the given provider, wired to sctx's
// working directory and environment. When the host cannot be constructed
// (unknown provider, missing Bitbucket config, etc) it returns nil and a
// hostSkip describing why.
func buildHost(sctx *pipeline.StepContext, provider scm.Provider) (scm.Host, hostSkip) {
	cmdFactory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return stepCmdContext(sctx, ctx, name, args...)
	}
	switch provider {
	case scm.ProviderGitHub:
		// Resolve the slug so gh commands carry --repo and work from the
		// daemon's fixed (non-repo) working directory. For GitHub Enterprise
		// Server, HostPrefixedSlug returns "host/owner/name" which is the
		// format gh requires for --repo on GHE. Fall back to the PR URL when
		// the upstream remote URL is unavailable. The hostname also scopes
		// the auth-status check so a stale token on any other configured gh
		// host cannot make this repo look unauthenticated.
		host := resolvedHost(sctx, sctx.Repo.UpstreamURL)
		repo := github.HostPrefixedSlugForHost(sctx.Repo.UpstreamURL, host)
		if repo == "" && sctx.Run.PRURL != nil {
			prHost := resolvedHost(sctx, *sctx.Run.PRURL)
			repo = github.HostPrefixedSlugForHost(*sctx.Run.PRURL, prHost)
			if host == "" {
				host = prHost
			}
		}
		if repo == "" {
			// Fail closed. Without a slug gh carries no --repo, so it infers
			// both the repository AND the branch from its working directory -
			// which, with an scm.cli_wrapper configured, is the developer's
			// live checkout on whatever branch they left it on. Skipping the
			// step is the only safe answer; there is nothing that names the
			// repository this run belongs to.
			return nil, unnameableRepo("could not resolve the GitHub repository from the remote URL")
		}
		forkRepo := ""
		if sctx.Repo.ForkURL != "" {
			// forkRepo is only used to extract the fork owner for --head owner:branch;
			// the plain slug (without host prefix) is correct here.
			forkRepo = github.RepoSlug(sctx.Repo.ForkURL)
		}
		return github.NewWithFork(scmCLIFactory(sctx, cmdFactory), func() bool { return stepCLIAvailable(sctx, provider) }, host, repo, forkRepo), hostSkip{}
	case scm.ProviderGitLab:
		if sctx.Repo.ForkURL != "" {
			// Fork MR routing for GitLab is intentionally not half-wired.
			// The push step may use fork_url, but PR creation must skip until
			// GitLab source-project routing is implemented end to end.
			return nil, hostSkip{Reason: "fork PR routing for GitLab is not implemented"}
		}
		return gitlab.New(
			cmdFactory,
			func() bool { return stepCLIAvailable(sctx, provider) },
			resolvedHost(sctx, sctx.Repo.UpstreamURL),
			gitlab.ProjectPath(sctx.Repo.UpstreamURL),
		), hostSkip{}
	case scm.ProviderBitbucket:
		if sctx.Repo.ForkURL != "" {
			// Fork PR routing for Bitbucket is intentionally not half-wired.
			// The API needs distinct source and destination repositories before
			// this provider can safely consume fork_url for PR creation.
			return nil, hostSkip{Reason: "fork PR routing for Bitbucket is not implemented"}
		}
		client, err := bitbucket.NewClientFromEnv(sctx.Env)
		if err != nil {
			return nil, hostSkip{Reason: err.Error()}
		}
		repo, err := resolveBitbucketRepoRef(sctx.Repo.UpstreamURL, sctx.Run.PRURL)
		if err != nil {
			return nil, unnameableRepo(err.Error())
		}
		return bitbucket.NewHost(client, repo), hostSkip{}
	case scm.ProviderAzureDevOps:
		if sctx.Repo.ForkURL != "" {
			// Fork PR routing for Azure DevOps is intentionally not half-wired,
			// mirroring GitLab and Bitbucket: the push step may use fork_url, but
			// PR creation must skip until cross-repository routing is implemented
			// end to end.
			return nil, hostSkip{Reason: "fork PR routing for Azure DevOps is not implemented"}
		}
		org, project, repo, ok := azuredevops.ParseRemote(sctx.Repo.UpstreamURL)
		if !ok && sctx.Run.PRURL != nil {
			org, project, repo, ok = azuredevops.ParseRemote(*sctx.Run.PRURL)
		}
		if !ok {
			return nil, unnameableRepo("could not resolve Azure DevOps organization, project, and repository from the remote URL")
		}
		return azuredevops.New(cmdFactory, func() bool { return stepCLIAvailable(sctx, provider) }, org, project, repo), hostSkip{}
	case scm.ProviderForgejo:
		if sctx.Repo.ForkURL != "" {
			return nil, skipReasonf("fork PR routing for Forgejo is not implemented")
		}
		baseURL := forgejoBaseURLForStep(sctx)
		remote := sctx.Repo.UpstreamURL
		if strings.TrimSpace(remote) == "" && sctx.Run.PRURL != nil {
			remote = *sctx.Run.PRURL
		}
		resolvedBase, repo, err := forgejo.ResolveRemote(remote, baseURL, scm.ResolveHost(sctx.Ctx, remote))
		if err != nil {
			return nil, unnameableRepo(fmt.Sprintf("could not resolve Forgejo host and repository: %v", err))
		}
		executable := "forgejo-axi"
		if sctx.Config != nil && strings.TrimSpace(sctx.Config.ForgejoAXIPath) != "" {
			executable = strings.TrimSpace(sctx.Config.ForgejoAXIPath)
		}
		tokenEnv := forgejoTokenEnvForStep(sctx, resolvedBase)
		return forgejo.New(forgejo.Options{
			CommandFactory: cmdFactory,
			CLIAvailable:   func(name string) bool { return stepExecutableAvailable(sctx, name) },
			Executable:     executable,
			BaseURL:        resolvedBase,
			Repository:     repo,
			TokenEnv:       tokenEnv,
			Secrets:        forgejoTokenValuesForStep(sctx),
		}), hostSkip{}
	case scm.ProviderGitea:
		if sctx.Repo.ForkURL != "" {
			// Fork PR routing for Gitea is intentionally not half-wired,
			// mirroring GitLab, Bitbucket, and Azure DevOps: cross-repository
			// routing needs distinct source/destination handling this
			// provider does not implement yet.
			return nil, skipReasonf("fork PR routing for Gitea is not implemented")
		}
		host := scm.ResolveHost(sctx.Ctx, sctx.Repo.UpstreamURL)
		repoSlug := scm.RepoPath(sctx.Repo.UpstreamURL)
		if repoSlug == "" {
			return nil, unnameableRepo("could not resolve Gitea owner/repo from the remote URL")
		}
		// login comes from tea's own config.yml (see scm.ResolveGiteaLogin); an
		// empty login is tolerated here and surfaces as an actionable error
		// from Host.Available instead of failing host construction outright.
		login := scm.ResolveGiteaLogin(host)
		return gitea.New(cmdFactory, func() bool { return stepCLIAvailable(sctx, provider) }, host, login, repoSlug), hostSkip{}
	default:
		return nil, skipReasonf("provider %s is not supported yet", provider)
	}
}

// scmCLIFactory applies the global scm settings to gh invocations, and only to
// gh: the GitLab and Bitbucket hosts are built with an unwrapped factory. The daemon
// execs gh directly from a fixed, non-repo working directory, so it never sees
// the login shell where a credential manager is wired up. When scm.cli_wrapper
// is set, gh runs under that wrapper (for example `op plugin run -- gh`) from
// the repo's own working path, so a wrapper that scopes credentials by
// directory hands back the identity belonging to that repo rather than a
// global default. When scm.gh_config_dir is set it replaces GH_CONFIG_DIR, so
// gh reads no stored accounts and its auth state is exactly the token the
// wrapper injected. Both unset returns base unchanged.
func scmCLIFactory(sctx *pipeline.StepContext, base github.CmdFactory) github.CmdFactory {
	if sctx.Config == nil {
		return base
	}
	wrapper := sctx.Config.SCM.CLIWrapper
	configDir := strings.TrimSpace(sctx.Config.SCM.GHConfigDir)
	if len(wrapper) == 0 && configDir == "" {
		return base
	}
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(wrapper) > 0 {
			wrapped := make([]string, 0, len(wrapper)-1+1+len(args))
			wrapped = append(wrapped, wrapper[1:]...)
			wrapped = append(wrapped, name)
			wrapped = append(wrapped, args...)
			name, args = wrapper[0], wrapped
		}
		cmd := base(ctx, name, args...)
		// The wrapper resolves credentials relative to its working directory,
		// which is what makes directory-scoped credentials resolve correctly.
		// Pointing at the user's real checkout is safe only because gh
		// addresses the repository through --repo rather than the cwd, and
		// buildHost fails closed rather than building a GitHub host whose slug
		// is unknown, so no gh invocation can fall back to inferring the
		// repository or branch from that checkout. It is the wrapper's
		// requirement alone: gh_config_dir travels in the environment, so
		// setting only it must leave the working directory exactly as the base
		// factory chose it.
		if len(wrapper) > 0 && sctx.Repo != nil && sctx.Repo.WorkingPath != "" {
			cmd.Dir = sctx.Repo.WorkingPath
		}
		if configDir != "" {
			// A nil Env means "inherit"; appending to it would instead reduce
			// the child's environment to this single variable.
			if cmd.Env == nil {
				cmd.Env = os.Environ()
			}
			cmd.Env = append(cmd.Env, "GH_CONFIG_DIR="+configDir)
		}
		return cmd
	}
}

// BuildHostForTest exposes buildHost to tests in other packages.
func BuildHostForTest(sctx *pipeline.StepContext, provider scm.Provider) (scm.Host, string) {
	host, skip := buildHost(sctx, provider)
	return host, skip.Reason
}

func detectProviderForStep(sctx *pipeline.StepContext, remoteURL string) scm.Provider {
	return scm.DetectProviderContextWithForgejoBaseURL(sctx.Ctx, remoteURL, forgejoBaseURLForStep(sctx))
}

func forgejoBaseURLForStep(sctx *pipeline.StepContext) string {
	if value, ok := effectiveStepEnvValue(sctx, "FORGEJO_BASE_URL"); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func effectiveStepEnvValue(sctx *pipeline.StepContext, key string) (string, bool) {
	if sctx != nil {
		if value, ok := envValue(sctx.Env, key); ok {
			return value, true
		}
	}
	return os.LookupEnv(key)
}

// forgejoTokenEnvForStep mirrors forgejo-axi's documented host-key encoding so
// an explicit --base-url retains host-scoped token precedence.
func forgejoTokenEnvForStep(sctx *pipeline.StepContext, baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err == nil && parsed.Host != "" {
		var key strings.Builder
		key.WriteString("FORGEJO_TOKEN_")
		for _, char := range strings.ToUpper(parsed.Host) {
			if char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
				key.WriteRune(char)
			} else {
				fmt.Fprintf(&key, "_%X_", char)
			}
		}
		name := key.String()
		if value, ok := effectiveStepEnvValue(sctx, name); ok && value != "" {
			return name
		}
	}
	if value, ok := effectiveStepEnvValue(sctx, "FORGEJO_TOKEN"); ok && value != "" {
		return "FORGEJO_TOKEN"
	}
	return ""
}

func forgejoTokenValuesForStep(sctx *pipeline.StepContext) []string {
	env := os.Environ()
	if sctx != nil && len(sctx.Env) > 0 {
		env = mergeEnv(sctx.Env)
	}
	var secrets []string
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(strings.ToUpper(key), "FORGEJO_TOKEN") && value != "" {
			secrets = append(secrets, value)
		}
	}
	return secrets
}
