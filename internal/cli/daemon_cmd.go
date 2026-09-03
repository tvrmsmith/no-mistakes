package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

var (
	daemonRun         = daemon.Run
	daemonStartFn     = daemon.Start
	daemonStopFn      = daemon.Stop
	daemonIsRunningFn = daemon.IsRunning
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the no-mistakes daemon",
	}

	cmd.AddCommand(newDaemonStartCmd())
	cmd.AddCommand(newDaemonStopCmd())
	cmd.AddCommand(newDaemonRestartCmd())
	cmd.AddCommand(newDaemonStatusCmd())
	cmd.AddCommand(newDaemonRunCmd())
	cmd.AddCommand(newDaemonAdmitPushCmd())
	cmd.AddCommand(newDaemonNotifyPushCmd())

	return cmd
}

func newDaemonAdmitPushCmd() *cobra.Command {
	var gate string
	cmd := &cobra.Command{
		Use:    "admit-push",
		Short:  "Authorize a managed gate ref update",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			gatePath, err := normalizeNotifyGatePath(gate)
			if err != nil {
				return err
			}
			p, err := paths.New()
			if err != nil {
				return err
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()
			var result ipc.AdmitPushResult
			if err := client.Call(ipc.MethodAdmitPush, &ipc.AdmitPushParams{Gate: gatePath}, &result); err != nil {
				return err
			}
			// This pre-receive hook is the enforcing gate: a non-nil error here
			// rejects the ref update. The hooks are the one caller with no
			// health probe ahead of them, so the daemon's version rides on the
			// reply itself, and verifying it before reading Context keeps a
			// stale daemon whose reply shape has drifted from decoding as a
			// permissive not-nested verdict.
			if err := ipc.DaemonVersionMismatch(result.ProtocolVersion); err != nil {
				return err
			}
			if !result.Context.Nested {
				return nil
			}
			return emitGateContextRefusal(cmd, gatecontext.Result{
				Nested:           result.Context.Nested,
				ManagedGit:       result.Context.ManagedGit,
				AgentDescendant:  result.Context.AgentDescendant,
				DaemonDescendant: result.Context.DaemonDescendant,
				MarkerPresent:    result.Context.MarkerPresent,
				RunID:            result.Context.RunID,
				Phase:            result.Context.Phase,
			})
		},
	}
	cmd.Flags().StringVar(&gate, "gate", "", "bare repo path that is about to receive a push")
	_ = cmd.MarkFlagRequired("gate")
	return cmd
}

func newDaemonNotifyPushCmd() *cobra.Command {
	var gate string
	var ref string
	var oldSHA string
	var newSHA string
	var pushOptions []string

	cmd := &cobra.Command{
		Use:    "notify-push",
		Short:  "Notify daemon about a git push",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			skipSteps, err := parseSkipPushOptions(pushOptions)
			if err != nil {
				return err
			}
			intent, err := parseIntentPushOptions(pushOptions)
			if err != nil {
				return err
			}
			prBaseBranch, err := parsePRBaseBranchPushOptions(pushOptions)
			if err != nil {
				return err
			}
			gatePath, err := normalizeNotifyGatePath(gate)
			if err != nil {
				return err
			}

			p, err := paths.New()
			if err != nil {
				return err
			}

			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()

			var result ipc.PushReceivedResult
			if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate:         gatePath,
				Ref:          ref,
				Old:          oldSHA,
				New:          newSHA,
				SkipSteps:    skipSteps,
				Intent:       intent,
				PRBaseBranch: prBaseBranch,
			}, &result); err != nil {
				return err
			}
			// Same reason as admit-push: the hook has no negotiation ahead of
			// it, so the daemon's version rides on the reply. This is
			// post-receive, so the ref already moved and admit-push is the
			// enforcing gate; reporting the skew here is a post-hoc signal that
			// a stale daemon accepted new-shaped params it may have read wrong.
			return ipc.DaemonVersionMismatch(result.ProtocolVersion)
		},
	}

	cmd.Flags().StringVar(&gate, "gate", "", "bare repo path that received the push")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref name")
	cmd.Flags().StringVar(&oldSHA, "old", "", "previous commit SHA")
	cmd.Flags().StringVar(&newSHA, "new", "", "new commit SHA")
	cmd.Flags().StringArrayVar(&pushOptions, "push-option", nil, "git push option")
	_ = cmd.MarkFlagRequired("gate")
	_ = cmd.MarkFlagRequired("ref")
	_ = cmd.MarkFlagRequired("old")
	_ = cmd.MarkFlagRequired("new")

	return cmd
}

func normalizeNotifyGatePath(gate string) (string, error) {
	if strings.TrimSpace(gate) == "" {
		return "", fmt.Errorf("gate path is required")
	}
	abs, err := filepath.Abs(gate)
	if err != nil {
		return "", fmt.Errorf("resolve gate path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func parseSkipPushOptions(options []string) ([]types.StepName, error) {
	var steps []types.StepName
	for _, option := range options {
		value, ok := strings.CutPrefix(option, "no-mistakes.skip=")
		if !ok {
			continue
		}
		parsed, err := parseSkipSteps(value)
		if err != nil {
			return nil, err
		}
		steps = append(steps, parsed...)
	}
	return dedupeSteps(steps), nil
}

func parseSkipSteps(value string) ([]types.StepName, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var steps []types.StepName
	for _, part := range strings.Split(value, ",") {
		step := types.StepName(strings.TrimSpace(part))
		if !validStep(step) {
			return nil, fmt.Errorf("unknown step %q", step)
		}
		steps = append(steps, step)
	}
	return dedupeSteps(steps), nil
}

// intentPushOptionPrefix carries an agent-supplied intent through a git push.
// The value is base64-encoded so multi-line or special-character intents
// survive the push-option transport (which is line-oriented).
const intentPushOptionPrefix = "no-mistakes.intent="

// prBaseBranchPushOptionPrefix carries a per-run PR base branch through a git push.
const prBaseBranchPushOptionPrefix = "no-mistakes.pr-base-branch="

// formatIntentPushOption encodes intent as a single push option, or returns ""
// when there is no intent to carry.
func formatIntentPushOption(intent string) string {
	if strings.TrimSpace(intent) == "" {
		return ""
	}
	return intentPushOptionPrefix + base64.StdEncoding.EncodeToString([]byte(intent))
}

// parseIntentPushOptions extracts and decodes the intent push option, if any.
// The last occurrence wins.
func parseIntentPushOptions(options []string) (string, error) {
	intent := ""
	for _, option := range options {
		encoded, ok := strings.CutPrefix(option, intentPushOptionPrefix)
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("decode intent push option: %w", err)
		}
		intent = string(decoded)
	}
	return intent, nil
}

// formatPRBaseBranchPushOption encodes a per-run PR base branch as a push
// option, or returns "" when unset.
func formatPRBaseBranchPushOption(branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	return prBaseBranchPushOptionPrefix + branch
}

// parsePRBaseBranchPushOptions extracts the per-run PR base branch push option,
// if any. The last occurrence wins.
func parsePRBaseBranchPushOptions(options []string) (string, error) {
	branch := ""
	for _, option := range options {
		value, ok := strings.CutPrefix(option, prBaseBranchPushOptionPrefix)
		if !ok {
			continue
		}
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("pr base branch push option must not be empty")
		}
		branch = value
	}
	return branch, nil
}

func formatSkipPushOptions(steps []types.StepName) []string {
	if len(steps) == 0 {
		return nil
	}
	parts := make([]string, 0, len(steps))
	for _, step := range dedupeSteps(steps) {
		parts = append(parts, string(step))
	}
	return []string{"no-mistakes.skip=" + strings.Join(parts, ",")}
}

func validStep(step types.StepName) bool {
	for _, known := range types.AllSteps() {
		if step == known {
			return true
		}
	}
	return false
}

func dedupeSteps(steps []types.StepName) []types.StepName {
	seen := make(map[types.StepName]bool, len(steps))
	out := make([]types.StepName, 0, len(steps))
	for _, step := range steps {
		if seen[step] {
			continue
		}
		seen[step] = true
		out = append(out, step)
	}
	return out
}

func newDaemonStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Install or refresh the managed daemon service and start it",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.start", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				if err := daemonStartFn(p); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon started\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
}

func newDaemonStopCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.stop", force)
			return trackCommand("daemon.stop", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				parkedNotice, err := guardDestructiveDaemonLifecycle(p, cmd.ErrOrStderr(), "daemon stop", force)
				if err != nil {
					return err
				}
				if err := daemonStopFn(p); err != nil {
					return err
				}
				fmt.Fprint(cmd.ErrOrStderr(), parkedNotice)
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon stopped\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop the daemon even when pipeline runs are active")
	return cmd
}

func newDaemonRestartCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon (stop if running, then start)",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.restart", force)
			return trackCommand("daemon.restart", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				parkedNotice, err := guardDestructiveDaemonLifecycle(p, cmd.ErrOrStderr(), "daemon restart", force)
				if err != nil {
					return err
				}
				if err := daemonStopFn(p); err != nil {
					return fmt.Errorf("stop daemon: %w", err)
				}
				if err := daemonStartFn(p); err != nil {
					return fmt.Errorf("start daemon: %w", err)
				}
				fmt.Fprint(cmd.ErrOrStderr(), parkedNotice)
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon restarted\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restart the daemon even when pipeline runs are active")
	return cmd
}

// guardDestructiveDaemonLifecycle answers whether the action may proceed and
// returns the preservation promise for the parked runs it would preserve. The
// caller prints that notice only after the daemon has actually stopped: a
// refusal or a failed stop preserves nothing.
func guardDestructiveDaemonLifecycle(p *paths.Paths, stderr io.Writer, action string, force bool) (string, error) {
	// This process is the one that starts the daemon back up, so its own step
	// plan is the layout the preserved runs will resume under. The executable
	// can have been replaced out of band since a parked run started, so the
	// drift check is not free of charge here either.
	decision, err := lifecycle.Decide(p, steps.AllSteps(), lifecycle.SameBinary)
	if err != nil {
		return "", fmt.Errorf("check active pipeline runs: %w", err)
	}
	parkedNotice := decision.ParkedNotice()
	blocking := decision.Blocking
	if len(blocking) == 0 {
		return parkedNotice, nil
	}
	runWord, verb := lifecycle.RunCountWords(len(blocking))
	if force {
		fmt.Fprintf(stderr, "FORCE: %s will stop/restart the daemon while %d active pipeline %s %s in progress\n", action, len(blocking), runWord, verb)
		fmt.Fprint(stderr, lifecycle.RunList(blocking))
		return parkedNotice, nil
	}
	return "", fmt.Errorf("refusing %s because %d active pipeline %s %s in progress; pass --force to stop/restart the daemon anyway\n%s", action, len(blocking), runWord, verb, lifecycle.RunList(blocking))
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check if the daemon is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.status", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				alive, err := daemonIsRunningFn(p)
				if err != nil {
					return err
				}
				if alive {
					pid, _ := daemon.ReadPID(p)
					if pid > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon running %s\n", sGreen.Render("●"), sDim.Render(fmt.Sprintf("(pid %d)", pid)))
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon running\n", sGreen.Render("●"))
					}
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon not running\n", sDim.Render("○"))
				}
				return nil
			})
		},
	}
}

func newDaemonRunCmd() *cobra.Command {
	var root string

	cmd := &cobra.Command{
		Use:    "run",
		Short:  "Run the daemon in the foreground",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if root != "" {
				if err := os.Setenv("NM_HOME", root); err != nil {
					return fmt.Errorf("set NM_HOME: %w", err)
				}
			}
			return daemonRun()
		},
	}

	cmd.Flags().StringVar(&root, "root", "", "override no-mistakes data directory")
	return cmd
}
