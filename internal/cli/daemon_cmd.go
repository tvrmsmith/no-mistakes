package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

var (
	daemonRun         = daemon.Run
	daemonStartFn     = daemon.Start
	daemonStopFn      = daemon.StopWithOptions
	daemonIsRunningFn = daemon.IsRunning
)

// defaultDrainTimeout is the CLI-side default for `--drain-timeout`, applied
// when `--drain` is passed without an explicit timeout.
const defaultDrainTimeout = 10 * time.Minute

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
			return client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate:         gatePath,
				Ref:          ref,
				Old:          oldSHA,
				New:          newSHA,
				SkipSteps:    skipSteps,
				Intent:       intent,
				PRBaseBranch: prBaseBranch,
			}, &result)
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
	var drain bool
	var drainTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.stop", force)
			opts, err := drainStopOptions(drain, drainTimeout, force)
			if err != nil {
				return err
			}
			return trackCommand("daemon.stop", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := guardDestructiveDaemonLifecycle(p, cmd.ErrOrStderr(), "daemon stop", lifecycleGuardMode(force, drain)); err != nil {
					return err
				}
				outcome, err := daemonStopFn(p, opts)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon stopped\n", sGreen.Render("✓"))
				if opts.Drain {
					printDrainOutcome(cmd.OutOrStdout(), outcome)
				}
				return deadlineDrainError(outcome)
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop the daemon even when pipeline runs are active")
	cmd.Flags().BoolVar(&drain, "drain", false, "refuse new runs, let in-flight runs finish, then stop the daemon")
	cmd.Flags().DurationVar(&drainTimeout, "drain-timeout", defaultDrainTimeout, "how long to wait for in-flight runs to finish before forcibly stopping them (only with --drain)")
	return cmd
}

func newDaemonRestartCmd() *cobra.Command {
	var force bool
	var drain bool
	var drainTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon (stop if running, then start)",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.restart", force)
			opts, err := drainStopOptions(drain, drainTimeout, force)
			if err != nil {
				return err
			}
			return trackCommand("daemon.restart", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				if err := guardDestructiveDaemonLifecycle(p, cmd.ErrOrStderr(), "daemon restart", lifecycleGuardMode(force, drain)); err != nil {
					return err
				}
				outcome, err := daemonStopFn(p, opts)
				if err != nil {
					return fmt.Errorf("stop daemon: %w", err)
				}
				if opts.Drain {
					printDrainOutcome(cmd.OutOrStdout(), outcome)
				}
				if err := daemonStartFn(p); err != nil {
					return fmt.Errorf("start daemon: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon restarted\n", sGreen.Render("✓"))
				return deadlineDrainError(outcome)
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restart the daemon even when pipeline runs are active")
	cmd.Flags().BoolVar(&drain, "drain", false, "refuse new runs, let in-flight runs finish, then restart the daemon")
	cmd.Flags().DurationVar(&drainTimeout, "drain-timeout", defaultDrainTimeout, "how long to wait for in-flight runs to finish before forcibly stopping them (only with --drain)")
	return cmd
}

// drainStopOptions validates the --drain/--force/--drain-timeout combination
// and builds the StopOptions to send to the daemon. --drain and --force say
// opposite things about active runs, so combining them is rejected before
// anything else happens.
func drainStopOptions(drain bool, drainTimeout time.Duration, force bool) (daemon.StopOptions, error) {
	if drain && force {
		return daemon.StopOptions{}, fmt.Errorf("--drain and --force cannot be used together")
	}
	if !drain {
		return daemon.StopOptions{}, nil
	}
	if drainTimeout <= 0 {
		return daemon.StopOptions{}, fmt.Errorf("--drain-timeout must be positive, got %s", drainTimeout)
	}
	return daemon.StopOptions{Drain: true, DrainTimeout: drainTimeout}, nil
}

// printDrainOutcome reports what a drain actually did: how many runs finished
// on their own, and one line per run the drain cut short. A CI-monitor cut is
// worded as expected behavior (the PR is left open, CI keeps running); a
// deadline cut is worded as work that was forcibly stopped.
func printDrainOutcome(w io.Writer, outcome daemon.StopOutcome) {
	fmt.Fprintf(w, "  %s %d run(s) finished before the daemon stopped\n", sGreen.Render("✓"), len(outcome.Finished))
	for _, run := range outcome.Interrupted {
		switch run.Reason {
		case ipc.DrainInterruptedCIMonitor:
			fmt.Fprintf(w, "  %s %s (%s): CI monitor cut by drain, PR remains open and CI is still running\n", sDim.Render("-"), run.RunID, run.Branch)
		case ipc.DrainInterruptedDeadline:
			fmt.Fprintf(w, "  %s %s (%s): forcibly stopped at the drain deadline\n", sDim.Render("-"), run.RunID, run.Branch)
		default:
			fmt.Fprintf(w, "  %s %s (%s): interrupted by drain (%s)\n", sDim.Render("-"), run.RunID, run.Branch, run.Reason)
		}
	}
}

// deadlineDrainError is the only nonzero case a drain introduces: a run the
// drain forcibly stopped at its deadline. A ci_monitor interruption alone is
// the drain's designed behavior and exits 0.
func deadlineDrainError(outcome daemon.StopOutcome) error {
	var ids []string
	for _, run := range outcome.Interrupted {
		if run.Reason == ipc.DrainInterruptedDeadline {
			ids = append(ids, run.RunID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return fmt.Errorf("drain deadline forcibly stopped %d run(s): %s", len(ids), strings.Join(ids, ", "))
}

// lifecycleGuardMode maps the stop/restart flags onto the guard's mode. An
// explicit mode reads better here than a (force, drain bool) pair: the guard
// has exactly three mutually exclusive behaviors (refuse, force through
// loudly, wait quietly), and --drain with --force is already rejected before
// this point, so the mapping is total and unambiguous.
func lifecycleGuardMode(force, drain bool) daemonLifecycleGuardMode {
	switch {
	case force:
		return lifecycleGuardForce
	case drain:
		return lifecycleGuardDrain
	default:
		return lifecycleGuardNormal
	}
}

// daemonLifecycleGuardMode selects guardDestructiveDaemonLifecycle's
// behavior when active pipeline runs exist.
type daemonLifecycleGuardMode int

const (
	// lifecycleGuardNormal refuses the command and lists the active runs.
	lifecycleGuardNormal daemonLifecycleGuardMode = iota
	// lifecycleGuardForce proceeds, loudly warning that active runs may fail.
	lifecycleGuardForce
	// lifecycleGuardDrain proceeds quietly: the daemon will wait for these
	// runs to finish rather than cutting them off.
	lifecycleGuardDrain
)

func guardDestructiveDaemonLifecycle(p *paths.Paths, stderr io.Writer, action string, mode daemonLifecycleGuardMode) error {
	runs, err := lifecycle.ActiveRuns(p)
	if err != nil {
		return fmt.Errorf("check active pipeline runs: %w", err)
	}
	if len(runs) == 0 {
		return nil
	}
	switch mode {
	case lifecycleGuardForce:
		fmt.Fprintf(stderr, "FORCE: %s will stop/restart the daemon while %d active pipeline runs are in progress\n", action, len(runs))
		fmt.Fprint(stderr, lifecycle.RunList(runs))
		return nil
	case lifecycleGuardDrain:
		fmt.Fprintf(stderr, "%s will wait on %d active pipeline runs before stopping the daemon\n", action, len(runs))
		fmt.Fprint(stderr, lifecycle.RunList(runs))
		return nil
	default:
		return fmt.Errorf("refusing %s because %d active pipeline runs are in progress; pass --force to stop/restart the daemon anyway\n%s", action, len(runs), lifecycle.RunList(runs))
	}
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
