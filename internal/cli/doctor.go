package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/claudetrust"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"github.com/spf13/cobra"
)

type doctorAgentCheck struct {
	name     string
	binaries []string
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check system health and dependencies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommandStatus("doctor", func() (string, error) {
				w := cmd.OutOrStdout()
				allOK := true

				ok := func(label, detail string) {
					fmt.Fprintf(w, "  %s %s  %s\n", sGreen.Render("✓"), sDim.Render(label), detail)
				}
				warn := func(label, detail string) {
					fmt.Fprintf(w, "  %s %s  %s\n", sYellow.Render("–"), sDim.Render(label), detail)
				}
				fail := func(label, detail string) {
					fmt.Fprintf(w, "  %s %s  %s\n", sRed.Render("✗"), sDim.Render(label), detail)
				}
				// note continues the row above it, for detail too long to sit in
				// a checklist column whose other values are a word or two.
				note := func(detail string) {
					fmt.Fprintf(w, "      %s\n", sDim.Render(detail))
				}

				fmt.Fprintf(w, "  %s\n", sCyan.Render("System"))

				if _, err := exec.LookPath("git"); err != nil {
					fail("git           ", "not found")
					allOK = false
				} else {
					gitCmd := exec.Command("git", "--version")
					winproc.Harden(gitCmd)
					out, err := gitCmd.Output()
					if err != nil {
						fail("git           ", fmt.Sprintf("error (%v)", err))
						allOK = false
					} else {
						ok("git           ", strings.TrimSpace(string(out)))
					}
				}

				if _, err := exec.LookPath("gh"); err != nil {
					warn("gh            ", "not found "+sDim.Render("(optional, needed for PR/CI)"))
				} else {
					ok("gh            ", "ok")
				}

				if _, err := exec.LookPath("az"); err != nil {
					warn("az            ", "not found "+sDim.Render("(optional, needed for Azure DevOps PR/CI)"))
				} else {
					ok("az            ", "ok")
				}

				p, err := paths.New()
				if err != nil {
					fail("data directory", fmt.Sprintf("error resolving paths (%v)", err))
					allOK = false
				} else if _, err := os.Stat(p.Root()); os.IsNotExist(err) {
					fail("data directory", fmt.Sprintf("not found (%s)", p.Root()))
					allOK = false
				} else {
					ok("data directory", p.Root())
				}

				if p != nil {
					if _, err := os.Stat(p.DB()); os.IsNotExist(err) {
						warn("database      ", "not found "+sDim.Render("(will be created on first use)"))
					} else {
						d, err := db.Open(p.DB())
						if err != nil {
							fail("database      ", fmt.Sprintf("error (%v)", err))
							allOK = false
						} else {
							d.Close()
							ok("database      ", "ok")
						}
					}
				}

				if p != nil {
					alive, err := daemon.IsRunning(p)
					var mismatch *ipc.VersionMismatchError
					switch {
					case alive:
						ok("daemon        ", "running")
					case ipc.IsVersionMismatch(err):
						// errors.As only supplies the struct that splits the
						// short row from its remedy; a peer-reported mismatch
						// carries no struct, so the row falls back to the
						// error's own text rather than rendering nothing.
						if errors.As(err, &mismatch) {
							warn("daemon        ", mismatch.Summary())
							note(mismatch.Remedy())
						} else {
							warn("daemon        ", "protocol version mismatch")
							note(err.Error())
						}
					default:
						warn("daemon        ", "stopped")
					}
				}

				agents := doctorAgentChecks()
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s\n", sCyan.Render("Agents"))
				for _, a := range agents {
					label := fmt.Sprintf("%-14s", a.name)
					var found, missing []string
					for _, bin := range a.binaries {
						if path, err := exec.LookPath(bin); err != nil {
							missing = append(missing, bin)
						} else {
							found = append(found, path)
						}
					}
					switch {
					case len(missing) == 0:
						ok(label, strings.Join(found, ", "))
					case len(a.binaries) > 1:
						warn(label, "not found ("+strings.Join(missing, ", ")+")")
					default:
						warn(label, "not found")
					}
				}

				if p == nil {
					fail("gate validation", "unavailable: data directory could not be resolved")
					allOK = false
				} else {
					globalCfg, err := config.LoadGlobal(p.ConfigFile())
					if err != nil {
						fail("gate validation", fmt.Sprintf("unavailable: load config (%v)", err))
						allOK = false
					} else {
						if !doctorForgeProfiles(cmd.Context(), w, globalCfg.ForgeProfiles, ok, fail) {
							allOK = false
						}
						cfg := config.Merge(globalCfg, &config.RepoConfig{})
						if err := cfg.ResolveAgent(cmd.Context(), exec.LookPath); err != nil {
							fail("gate validation", err.Error())
							allOK = false
						} else {
							ok("gate validation", fmt.Sprintf("%s is runnable", cfg.Agent))
						}
					}
				}

				doctorClaudeWorkspaceTrust(p, ok, warn)

				if !allOK {
					fmt.Fprintln(w)
					fmt.Fprintf(w, "  %s\n", sRed.Render("some checks failed"))
					return "error", nil
				}

				return "success", nil
			})
		},
	}
}

func doctorForgeProfiles(
	ctx context.Context,
	w io.Writer,
	profiles config.ForgeProfiles,
	ok func(string, string),
	fail func(string, string),
) bool {
	if len(profiles) == 0 {
		return true
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s\n", sCyan.Render("Forge profiles"))
	hosts := make([]string, 0, len(profiles))
	for host := range profiles {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	allOK := true
	for _, profileHost := range hosts {
		remote := "git@" + profileHost + ":no-mistakes/doctor.git"
		resolved, err := forgecontext.Resolve(ctx, config.ForgeProfiles{profileHost: profiles[profileHost]}, remote, "")
		label := fmt.Sprintf("%-14s", "forge "+profileHost)
		if err != nil {
			fail(label, err.Error())
			allOK = false
			continue
		}
		if resolved == nil {
			fail(label, "profile did not resolve")
			allOK = false
			continue
		}

		var name string
		var args []string
		switch resolved.Provider {
		case scm.ProviderGitHub:
			name = "gh"
			args = []string{"auth", "status", "--active", "--hostname", resolved.Host}
		case scm.ProviderGitLab:
			name = "glab"
			args = []string{"auth", "status", "--hostname", resolved.Host}
		default:
			fail(label, fmt.Sprintf("unsupported provider %s", resolved.Provider))
			allOK = false
			continue
		}
		if _, err := exec.LookPath(name); err != nil {
			fail(label, fmt.Sprintf("%s not found", name))
			allOK = false
			continue
		}
		check := exec.CommandContext(ctx, name, args...)
		check.Env = resolved.Environment.Apply(nil)
		shellenv.ConfigureShellCommand(check)
		if output, err := shellenv.CombinedOutputShellCommand(check); err != nil {
			detail := strings.TrimSpace(string(output))
			if detail == "" {
				detail = err.Error()
			}
			fail(label, fmt.Sprintf("authentication failed (%s)", detail))
			allOK = false
			continue
		}
		ok(label, fmt.Sprintf("%s authenticated for %s", resolved.Provider, resolved.Host))
	}
	return allOK
}

// doctorClaudeWorkspaceTrust reports every registered gate repository whose
// path Claude Code has not been through its interactive trust dialog for.
// Untrusted, Claude Code discards the repo's project-scoped permission
// entries; internal/claudetrust owns the mechanism and which categories still
// cost anything under --dangerously-skip-permissions.
//
// It never fails doctor, only warns, because doctor cannot determine whether
// the condition will actually apply to a run. `agent` is a per-repo field read
// from each repository's trusted default branch, the operator's global is
// routinely `agent: auto`, and doctor runs in the CLI process while the gate
// agent runs as a child of the DAEMON, which may hold a different HOME and
// CLAUDE_CONFIG_DIR and therefore consult a different ~/.claude.json. Failing
// on a fact this process cannot establish produces false red.
//
// It reports nothing at all when claude is absent from PATH or no repositories
// are registered: both are the ordinary state of an operator this cannot
// affect.
func doctorClaudeWorkspaceTrust(p *paths.Paths, ok, warn func(string, string)) {
	if _, err := exec.LookPath("claude"); err != nil {
		return
	}
	if p == nil {
		return
	}
	label := fmt.Sprintf("%-14s", "claude trust")
	// Read-only: doctor reports state, so it must not create the database or
	// run migrations. A missing file is the fresh-install case the "database"
	// row above already reports, so it stays silent here.
	d, err := db.OpenReadOnly(p.DB())
	if err != nil {
		if !os.IsNotExist(err) {
			warn(label, fmt.Sprintf("cannot open database (%v)", err))
		}
		return
	}
	defer d.Close()
	repos, err := d.GetRepos()
	if err != nil {
		// Reading read-only means doctor no longer migrates, so a database one
		// upgrade behind is missing a column this query names. That is the
		// ordinary state between an upgrade and the daemon's next write, not a
		// condition an operator can act on, so it stays as quiet as the
		// missing-database case above.
		if !schemaNotMigrated(err) {
			warn(label, fmt.Sprintf("cannot list gate repositories (%v)", err))
		}
		return
	}
	if len(repos) == 0 {
		return
	}

	report := warn

	// Canonical, not raw: Claude Code stores and looks up its projects key
	// realpath'd, so a remedy naming the raw path under a symlinked NM_HOME
	// steers the operator into writing a key Claude Code never consults, while
	// claudetrust.Load canonicalizes that same key on the next doctor run and
	// reports the gate trusted over a still-degraded run.
	gatePaths := make([]string, 0, len(repos))
	for _, r := range repos {
		gatePaths = append(gatePaths, claudetrust.CanonicalWorkspace(p.RepoDir(r.ID)))
	}

	configPath, err := claudetrust.ConfigPath()
	if err != nil {
		report(label, fmt.Sprintf("cannot resolve Claude Code config path (%v)", err))
		return
	}

	cfg, err := claudetrust.Load(configPath)
	if err != nil {
		// Claude Code rewrites this file live, so a parse failure can simply be
		// a concurrent write rather than a corrupt config. Reported, never fatal.
		report(label, fmt.Sprintf("unreadable Claude Code config at %s: %s", configPath, err.Error()))
		return
	}

	untrusted := cfg.Untrusted(gatePaths)
	if len(untrusted) == 0 {
		ok(label, fmt.Sprintf("%d %s trusted", len(gatePaths), gateRepoNoun(len(gatePaths))))
		return
	}

	var prefix string
	if !cfg.Present() {
		prefix = fmt.Sprintf("no Claude Code config at %s; ", configPath)
	}
	remedy := claudetrust.Remedy("", configPath)
	if len(untrusted) == 1 {
		remedy = claudetrust.Remedy(untrusted[0], configPath)
	}
	report(label, fmt.Sprintf("%s%d %s untrusted: %s; %s",
		prefix, len(untrusted), gateRepoNoun(len(untrusted)), strings.Join(untrusted, ", "), remedy))
}

// schemaNotMigrated reports whether err is SQLite complaining that a table or
// column a query names does not exist, which is how a database written by an
// older build reads before the next writer runs its migrations.
func schemaNotMigrated(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such column") || strings.Contains(msg, "no such table")
}

// gateRepoNoun pluralizes "gate repository" for doctorClaudeWorkspaceTrust's detail lines.
func gateRepoNoun(n int) string {
	if n == 1 {
		return "gate repository"
	}
	return "gate repositories"
}

func doctorAgentChecks() []doctorAgentCheck {
	agents := []doctorAgentCheck{
		{"claude", []string{"claude"}},
		{"codex", []string{"codex"}},
		{"grok", []string{"grok"}},
		{"rovodev", []string{"acli"}},
		{"opencode", []string{"opencode"}},
		{"pi", []string{"pi"}},
		{"copilot", []string{"copilot"}},
		{"antigravity", []string{"agy"}},
		{"acpx", []string{"acpx"}},
	}
	for _, alias := range types.ACPAliases() {
		agents = append(agents, doctorAgentCheck{
			name: string(alias.Name),
			binaries: []string{
				alias.DefaultCommandBinary(),
				"acpx",
			},
		})
	}
	return agents
}
