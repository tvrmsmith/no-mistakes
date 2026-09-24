package cli

import (
	"bufio"
	"fmt"
	"strings"
	"time"

	toON "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/closers"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/spf13/cobra"
)

var syncInteractive = terminalInteractive

// custodyRecoveryGuidance is the live help emitted for
// `blocked_pipeline_owned_recoverable`: a terminal run whose pipeline commits
// live only on the gate branch. The order matters. `no-mistakes rerun` stamps
// its new run with the GATE branch tip (RunManager.HandleRerun) while `axi run`
// looks up an active run by the caller's LOCAL HEAD, and this state exists
// precisely because those two differ, so `rerun` followed by `axi run` cannot
// reattach until the recovery has moved the worktree onto the preserved head.
// Recovering custody first makes the ordinary `axi run --intent` path work: it
// starts and drives the run in one command.
//
// Mirrored in the skill body and the agents guide; kept in sync by
// TestCustodyRecoveryGuidance_SyncedAcrossSurfaces.
const custodyRecoveryGuidance = "Recover custody first with `no-mistakes axi sync --recover`: it returns custody and fast-forwards a clean worktree to the preserved pipeline head. Then validate that head with `no-mistakes axi run --intent \"<what the user set out to accomplish>\"`, which starts and drives the run in one command. `no-mistakes rerun` also re-runs the selected preserved pipeline head, but it returns immediately without driving, and a following `no-mistakes axi run` reattaches only while your local HEAD equals that preserved head - so use it only after the recovery moved your worktree there. `no-mistakes rerun` also refuses a known clean caller HEAD mismatch: if the heads differ, inspect `no-mistakes axi status` and follow its exact `branch_sync.next_action.command` for custody or synchronization, then submit intended local commits with a fresh `no-mistakes axi run` once custody permits."

// refusedRecoveryRerunGuidance rides every `blocked_recover_*` refusal. The
// refusal names its own exits, and `no-mistakes rerun` is not one of them: it
// makes the run active again, after which both named exits are refused with
// blocked_recover_run_active. The refusal message itself therefore stays clear
// of rerun (pinned by TestCustodyRecoveryGuidance_SyncedAcrossSurfaces), and
// the caller still needs rerun's own clean-head rule stated somewhere, so the
// help field carries it as a caution rather than as an offer.
const refusedRecoveryRerunGuidance = "Do not reach for `no-mistakes rerun` here: it makes the run active again and the exits named above are then refused. It also refuses a known clean caller HEAD mismatch against the selected preserved head, so if the heads differ, inspect `no-mistakes axi status` and follow its exact `branch_sync.next_action.command` for custody or synchronization, then submit intended local commits with a fresh `no-mistakes axi run` once custody permits."

func newSyncCmd() *cobra.Command {
	var check, yes, recoverCustody, keepLocal, adoptPublished bool
	var bindArchiveRef string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Safely move the current branch to an exact pipeline-pushed head",
		Long: "Refreshes the current branch's persisted pipeline push binding and, after\n" +
			"confirmation, advances only a completely clean checked-out branch using one of\n" +
			"two guarded modes: a strict fast-forward for clean behind branches, or an\n" +
			"equivalent-diverged advance that first anchors the old head and then moves the\n" +
			"branch to the verified pipeline head with reset semantics. It never stashes,\n" +
			"merges genuine divergence, rebases, switches branches, or updates a remote.\n" +
			"--check performs the fresh proof without applying it.\n" +
			"--recover returns custody of a branch whose run went terminal with unpublished\n" +
			"pipeline commits: it anchors an available preserved head, then either\n" +
			"fast-forwards a clean behind worktree or adopts a diverged preserved head only\n" +
			"when proven to carry every local change. Unproven divergence refuses. A run\n" +
			"cancelled before the pipeline changed anything releases the branch by itself\n" +
			"(user_owned) and makes --recover a no-op. --recover --keep-local keeps the\n" +
			"current local head and never touches the worktree; available preserved commits\n" +
			"stay anchored, while genuinely missing preserved commits are discarded.\n" +
			"--bind-archive-ref records one exact existing refs/heads/archive/* commit as\n" +
			"evidence for the narrow keep-local recovery that stays at a required head while\n" +
			"a divergent later head remains archived; it never creates or moves a Git ref.\n" +
			"--adopt-published moves a stale custody-returned gate lane only after the\n" +
			"configured push target proves the exact divergent local head is already\n" +
			"published there.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if check && yes {
				return &exitError{code: 2, err: fmt.Errorf("--check and --yes cannot be used together")}
			}
			if (check && recoverCustody) || (check && adoptPublished) || (recoverCustody && adoptPublished) {
				return &exitError{code: 2, err: fmt.Errorf("choose only one of --check, --recoverCustody, and --adopt-published")}
			}
			if keepLocal && !recoverCustody {
				return &exitError{code: 2, err: fmt.Errorf("--keep-local requires --recover")}
			}
			if bindArchiveRef != "" && (check || yes || recoverCustody || keepLocal || adoptPublished) {
				return &exitError{code: 2, err: fmt.Errorf("--bind-archive-ref cannot be combined with synchronization or recovery flags")}
			}
			if bindArchiveRef != "" {
				return runHumanBindRecoveryArchive(cmd, bindArchiveRef)
			}
			if recoverCustody {
				return runHumanRecover(cmd, keepLocal, yes)
			}
			if adoptPublished {
				return runHumanAdoptPublished(cmd, yes)
			}
			return runHumanSync(cmd, check, yes)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "freshly verify and show the synchronization plan without changing HEAD")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "apply an eligible guarded synchronization without prompting")
	cmd.Flags().BoolVar(&recoverCustody, "recover", false, "return custody of a branch stranded by a terminal run with unpublished pipeline commits (a no-op when cancellation already released the branch)")
	cmd.Flags().BoolVar(&keepLocal, "keep-local", false, "with --recover: keep the current local head; anchor available preserved commits, discard genuinely missing ones, and make the gate follow the kept head")
	cmd.Flags().BoolVar(&adoptPublished, "adopt-published", false, "adopt a clean diverged local head into its stale gate lane only when the configured push target already has that exact head")
	cmd.Flags().StringVar(&bindArchiveRef, "bind-archive-ref", "", "bind one existing refs/heads/archive/* commit as exact keep-local recovery evidence without changing Git refs")
	return cmd
}

func newAxiSyncCmd() *cobra.Command {
	var check, recoverCustody, keepLocal, adoptPublished bool
	var bindArchiveRef string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Check or apply guarded current-branch synchronization",
		Long: "Verifies the registered invoking worktree, clean exact branch, persisted\n" +
			"pipeline push binding, configured fork or upstream target, live remote equality,\n" +
			"and either strict ancestry or content-equivalent divergence. The default applies\n" +
			"an eligible plan without a prompt: strict fast-forward for behind branches, or an\n" +
			"equivalent advance that anchors the old head before moving the branch to the\n" +
			"verified pipeline head with reset semantics.\n" +
			"--check performs the same fresh read-only plan. Blocked states change nothing.\n" +
			"--recover performs the guarded custody return offered by\n" +
			"next_action.code: recover_custody; --keep-local keeps the current local head.\n" +
			"--bind-archive-ref binds one exact existing refs/heads/archive/* commit to\n" +
			"the selected terminal run; it never creates or moves a Git ref.\n" +
			"--adopt-published performs the guarded gate-lane recovery offered by\n" +
			"next_action.code: adopt_published.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if (check && recoverCustody) || (check && adoptPublished) || (recoverCustody && adoptPublished) {
				return emitError(cmd, 2, "choose only one of --check, --recoverCustody, and --adopt-published")
			}
			if keepLocal && !recoverCustody {
				return emitError(cmd, 2, "--keep-local requires --recover")
			}
			if bindArchiveRef != "" && (check || recoverCustody || keepLocal || adoptPublished) {
				return emitError(cmd, 2, "--bind-archive-ref cannot be combined with synchronization or recovery flags")
			}
			return runAxiSync(cmd, check, recoverCustody, keepLocal, adoptPublished, bindArchiveRef)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "freshly verify and return the plan without changing HEAD")
	cmd.Flags().BoolVar(&recoverCustody, "recover", false, "return custody of a branch stranded by a terminal run with unpublished pipeline commits (a no-op when cancellation already released the branch)")
	cmd.Flags().BoolVar(&keepLocal, "keep-local", false, "with --recover: keep the current local head; anchor available preserved commits, discard genuinely missing ones, and make the gate follow the kept head")
	cmd.Flags().BoolVar(&adoptPublished, "adopt-published", false, "adopt a clean diverged local head into its stale gate lane only when the configured push target already has that exact head")
	cmd.Flags().StringVar(&bindArchiveRef, "bind-archive-ref", "", "bind one existing refs/heads/archive/* commit as exact keep-local recovery evidence without changing Git refs")
	return cmd
}

func openSyncService() (*branchsync.Service, func(), error) {
	p, d, err := openResources()
	if err != nil {
		return nil, nil, err
	}
	repo, err := findRepo(d)
	if err != nil {
		closers.Quiet(d)
		return nil, nil, err
	}
	globalCfg, cfgErr := config.LoadGlobal(p.ConfigFile())
	if cfgErr != nil {
		closers.Quiet(d)
		return nil, nil, cfgErr
	}
	return &branchsync.Service{DB: d, Repo: repo, WorkDir: ".", GateDir: p.RepoDir(repo.ID), Paths: p, RemoteTimeout: globalCfg.BranchSyncRemoteTimeout}, func() { _ = d.Close() }, nil
}

func runHumanSync(cmd *cobra.Command, check, yes bool) error {
	w := newPrinter(cmd.OutOrStdout())
	started := time.Now()
	mode := "apply"
	if check {
		mode = "check"
	}
	var observed branchsync.State
	result := "error"
	defer func() { trackSyncAttempt("sync", "human_cli", mode, observed, result, started) }()

	service, closeFn, err := openSyncService()
	if err != nil {
		return err
	}
	defer closeFn()

	state := service.Refresh(cmd.Context())
	observed = state
	printHumanSyncState(w, state)
	if check {
		if syncStateSuccessful(state, true) {
			result = "noop"
			return w.Err()
		}
		result = "refused"
		return &exitError{code: 1}
	}
	if state.State == branchsync.StateSynchronized || state.State == branchsync.StateMergedRemoteRemoved || state.State == branchsync.StateUserOwned {
		result = "noop"
		return w.Err()
	}
	if !branchsync.CanApply(state) {
		result = "refused"
		return &exitError{code: 1}
	}
	if !yes {
		if !syncInteractive() {
			w.Println("  Non-interactive input cannot confirm this plan. Re-run with `no-mistakes sync --yes`.")
			result = "refused"
			return &exitError{code: 1}
		}
		if state.Safety == branchsync.SafetySafeEquivalentAdvance {
			w.Print("  Apply this guarded synchronization? [y/N] ")
		} else {
			w.Print("  Apply this exact strict fast-forward? [y/N] ")
		}
		line, readErr := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if readErr != nil && strings.TrimSpace(line) == "" {
			return readErr
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			w.Println("  Cancelled; no files or refs were changed.")
			result = "cancelled"
			return w.Err()
		}
	}

	applyResult := service.Apply(cmd.Context())
	observed = applyResult
	printHumanSyncState(w, applyResult)
	if syncStateSuccessful(applyResult, false) {
		if applyResult.Changed {
			result = "applied"
		} else {
			result = "noop"
		}
		return w.Err()
	}
	result = "refused"
	return &exitError{code: 1}
}

func runHumanBindRecoveryArchive(cmd *cobra.Command, archiveRef string) error {
	w := newPrinter(cmd.OutOrStdout())
	started := time.Now()
	var observed branchsync.State
	result := "error"
	defer func() { trackSyncAttempt("sync", "human_cli", "bind_archive", observed, result, started) }()

	service, closeFn, err := openSyncService()
	if err != nil {
		return err
	}
	defer closeFn()

	state := service.BindRecoveryArchive(cmd.Context(), archiveRef)
	observed = state
	printHumanSyncState(w, state)
	if verifiedArchiveRecovery(state) {
		w.Println("  Archive evidence bound; follow the exact guarded recovery action shown above.")
		result = "applied"
		return w.Err()
	}
	result = "refused"
	return &exitError{code: 1}
}

func runHumanRecover(cmd *cobra.Command, keepLocal, yes bool) error {
	w := newPrinter(cmd.OutOrStdout())
	started := time.Now()
	mode := "recover"
	if keepLocal {
		mode = "recover_keep_local"
	}
	var observed branchsync.State
	result := "error"
	defer func() { trackSyncAttempt("sync", "human_cli", mode, observed, result, started) }()

	service, closeFn, err := openSyncService()
	if err != nil {
		return err
	}
	defer closeFn()

	state := service.InspectCached(cmd.Context())
	observed = state
	// A branch released by cancellation needs no confirmation: the recovery is
	// an idempotent no-op that cannot mutate anything.
	if !yes && state.State != branchsync.StateUserOwned {
		printHumanSyncState(w, state)
		if !syncInteractive() {
			retry := "no-mistakes sync --recover --yes"
			if keepLocal {
				retry = "no-mistakes sync --recover --keep-local --yes"
			}
			w.Printf("  Non-interactive input cannot confirm this recovery. Re-run with `%s`.\n", retry)
			result = "refused"
			return &exitError{code: 1}
		}
		w.Println("  Recovery returns custody of this branch from its terminal run. The only")
		if keepLocal {
			if state.Recovery != nil && state.Recovery.KeepLocal {
				w.Println("  possible Git change is moving the local gate branch to the exact required")
				w.Println("  head; the worktree and verified divergent archive are never touched.")
			} else {
				w.Println("  possible changes are anchoring available preserved pipeline commits, discarding")
				w.Println("  genuinely missing ones, and moving the local gate branch to your current head;")
				w.Println("  the worktree is never touched.")
			}
		} else {
			w.Println("  possible worktree change is a fast-forward of this clean behind branch, or")
			w.Println("  adoption of a diverged preserved head proven to carry every local change;")
			w.Println("  unproven divergence refuses, and --keep-local keeps the current head.")
		}
		w.Print("  Return custody of this branch? [y/N] ")
		line, readErr := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if readErr != nil && strings.TrimSpace(line) == "" {
			return readErr
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			w.Println("  Cancelled; no files or refs were changed.")
			result = "cancelled"
			return w.Err()
		}
	}

	recovered := service.Recover(cmd.Context(), keepLocal)
	observed = recovered
	printHumanSyncState(w, recovered)
	if recovered.Recovered {
		if recovered.State == branchsync.StateUserOwned {
			w.Println("  Nothing to recover; cancellation already released this branch to you.")
		} else {
			w.Println("  Custody returned; start a fresh run when ready.")
		}
		if recovered.Changed {
			result = "applied"
		} else {
			result = "noop"
		}
		return w.Err()
	}
	result = "refused"
	return &exitError{code: 1}
}

func runHumanAdoptPublished(cmd *cobra.Command, yes bool) error {
	w := newPrinter(cmd.OutOrStdout())
	started := time.Now()
	var observed branchsync.State
	result := "error"
	defer func() { trackSyncAttempt("sync", "human_cli", "adopt_published", observed, result, started) }()

	service, closeFn, err := openSyncService()
	if err != nil {
		return err
	}
	defer closeFn()

	observed = service.InspectCached(cmd.Context())
	if !yes {
		printHumanSyncState(w, observed)
		if !syncInteractive() {
			w.Println("  Non-interactive input cannot confirm this recovery. Re-run with `no-mistakes sync --adopt-published --yes`.")
			result = "refused"
			return &exitError{code: 1}
		}
		w.Println("  This verifies that the configured push target already has your exact rebased head,")
		w.Println("  then updates only this stale local gate lane. It never changes the target or worktree.")
		w.Print("  Adopt the published head into this gate lane? [y/N] ")
		line, readErr := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if readErr != nil && strings.TrimSpace(line) == "" {
			return readErr
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			w.Println("  Cancelled; no files or refs were changed.")
			result = "cancelled"
			return w.Err()
		}
	}

	state := service.AdoptPublished(cmd.Context())
	observed = state
	printHumanSyncState(w, state)
	if state.Changed {
		result = "applied"
		return w.Err()
	}
	result = "refused"
	return &exitError{code: 1}
}

func printHumanSyncState(w *printer, state branchsync.State) {
	w.Printf("\n  Local branch: %s\n", humanSyncSummary(state))
	if state.Local.Head != "" {
		w.Printf("  local:    %s %s\n", state.Local.Branch, state.Local.Head)
	}
	if state.Pipeline.PushedHead != "" {
		w.Printf("  pipeline: %s\n", state.Pipeline.PushedHead)
	} else if state.Pipeline.CurrentHead != "" && state.Pipeline.CurrentHead != state.Local.Head {
		w.Printf("  preserved: %s (run %s, %s)\n", state.Pipeline.CurrentHead, state.Pipeline.RunID, state.Pipeline.Status)
	}
	if state.Recovery != nil && state.Recovery.ArchiveRef != "" {
		w.Printf("  archive:  %s -> %s (%s)\n", state.Recovery.ArchiveRef, state.Recovery.PreservedHead, state.Recovery.Proof)
		w.Printf("  required: %s\n", state.Recovery.RequiredHead)
	}
	if state.Target.Ref != "" {
		w.Printf("  target:   %s %s (%s)\n", state.Target.Remote, state.Target.Ref, state.Target.Kind)
	}
	if state.Error != "" {
		w.Printf("  blocked:  %s\n", state.Error)
	}
}

func humanSyncSummary(state branchsync.State) string {
	switch state.State {
	case branchsync.StatePipelineOwned:
		if state.Safety == "blocked_pipeline_owned_recoverable" {
			if state.Recovery != nil && state.Recovery.KeepLocal {
				return "later pipeline work is preserved by a verified archive; recover custody at the exact required head with `no-mistakes sync --recover --keep-local`"
			}
			return "run ended without publishing its pipeline commits; recover custody with `no-mistakes sync --recover`. `no-mistakes rerun` resumes validating the selected preserved head, but refuses a known clean caller HEAD mismatch. If heads differ, inspect `no-mistakes axi status` and follow its exact `branch_sync.next_action.command` for custody or synchronization, then submit intended local commits with a fresh `no-mistakes axi run` once custody permits"
		}
		if state.Safety == "blocked_recover_preserved_head_missing" {
			return "run ended without a recoverable preserved head; recover custody with `no-mistakes sync --recover --keep-local` to keep the current local head"
		}
		return "pipeline fix is not pushed yet; do not make local follow-up commits"
	case branchsync.StateCustodyReturned:
		if state.Safety == "recovery_required" && state.NextAction != nil {
			return "a rebased local head needs guarded gate-lane adoption before it can start a fresh run"
		}
		if state.Safety == "gate_ready" {
			return "the published rebased head is present in this gate lane; start a fresh run when ready"
		}
		return "custody returned; the branch is yours - start a fresh run when ready"
	case branchsync.StateUserOwned:
		return "run ended before the pipeline changed anything; the branch and head are yours and immediately usable"
	case branchsync.StatePushInProgress:
		return "pipeline branch update is in progress; synchronization is unavailable"
	case branchsync.StateBehind:
		if state.Safety == branchsync.SafetySafeFastForward {
			return "clean and strictly behind; exact safe fast-forward verified"
		}
		return "behind the pipeline-pushed head; refresh required"
	case branchsync.StateDiverged:
		if state.Safety == branchsync.SafetySafeEquivalentAdvance {
			return "diverged, but local changes are represented in the pipeline head; guarded advance verified"
		}
		if state.NextAction != nil && state.NextAction.Code == branchsync.NextActionSync {
			return "diverged; refresh required to verify equivalent pipeline content"
		}
		return "diverged from the pipeline-pushed head; manual reconciliation required"
	case branchsync.StateSynchronized:
		return "already synchronized with the pipeline-pushed head"
	case branchsync.StateMergedRemoteRemoved:
		return "PR merged and remote feature branch removed; nothing to synchronize"
	case branchsync.StateMergedRemoteRetained:
		return "PR merged; feature branch is retired and local branch was not changed"
	case branchsync.StateClosed:
		return "PR closed; feature branch is retired and local branch was not changed"
	default:
		return strings.ReplaceAll(state.State, "_", " ")
	}
}

func runAxiSync(cmd *cobra.Command, check, recoverCustody, keepLocal, adoptPublished bool, bindArchiveRef string) error {
	started := time.Now()
	mode := "apply"
	switch {
	case bindArchiveRef != "":
		mode = "bind_archive"
	case check:
		mode = "check"
	case recoverCustody && keepLocal:
		mode = "recover_keep_local"
	case recoverCustody:
		mode = "recover"
	case adoptPublished:
		mode = "adopt_published"
	}
	var state branchsync.State
	result := "error"
	defer func() { trackSyncAttempt("axi-sync", "axi", mode, state, result, started) }()

	service, closeFn, err := openSyncService()
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer closeFn()

	switch {
	case bindArchiveRef != "":
		state = service.BindRecoveryArchive(cmd.Context(), bindArchiveRef)
	case check:
		state = service.Refresh(cmd.Context())
	case recoverCustody:
		state = service.Recover(cmd.Context(), keepLocal)
	case adoptPublished:
		state = service.AdoptPublished(cmd.Context())
	default:
		state = service.Apply(cmd.Context())
	}
	fields := []toON.Field{branchSyncField(state)}
	if state.Error != "" {
		fields = append(fields, toON.Field{Key: "error", Value: state.Error})
	}
	var help []string
	if state.NextAction != nil {
		help = append(help, "Run `"+state.NextAction.Command+"`")
	}
	if state.Safety == "blocked_pipeline_owned_recoverable" && (state.Recovery == nil || !state.Recovery.KeepLocal) {
		help = append(help, custodyRecoveryGuidance)
	}
	if strings.HasPrefix(state.Safety, "blocked_recover_") {
		help = append(help, refusedRecoveryRerunGuidance)
	}
	if len(help) > 0 {
		fields = append(fields, toON.Field{Key: "help", Value: help})
	}
	docErr := emitDoc(cmd, fields...)
	successful := syncStateSuccessful(state, check)
	if recoverCustody {
		successful = state.Recovered
	}
	if bindArchiveRef != "" {
		successful = verifiedArchiveRecovery(state)
	}
	if adoptPublished {
		successful = state.Changed
	}
	if successful {
		if state.Changed {
			result = "applied"
		} else {
			result = "noop"
		}
		return docErr
	}
	result = "refused"
	return &exitError{code: 1, err: docErr}
}

func verifiedArchiveRecovery(state branchsync.State) bool {
	return state.Recovery != nil && state.Recovery.Source == "bound_archive" && state.Recovery.Proof == "verified" &&
		state.Recovery.KeepLocal && state.NextAction != nil && state.NextAction.Code == "recover_custody" &&
		state.NextAction.Command == "no-mistakes axi sync --recover --keep-local"
}

func trackSyncAttempt(command, surface, mode string, state branchsync.State, result string, started time.Time) {
	telemetry.Track("command", telemetry.Fields{
		"command":      command,
		"surface":      surface,
		"mode":         mode,
		"status":       result,
		"result":       result,
		"state_before": boundedSyncValue(state.State),
		"relation":     boundedSyncValue(state.Relation),
		"target_kind":  boundedSyncValue(state.Target.Kind),
		"run_phase":    boundedSyncValue(state.Pipeline.Phase),
		"pr_state":     boundedSyncValue(state.PRState),
		"reason":       boundedSyncValue(state.Safety),
		"dirty":        !state.Local.Clean && state.Local.Head != "",
		"duration_ms":  time.Since(started).Milliseconds(),
	})
}

func boundedSyncValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	if len(value) > 64 {
		return "unknown"
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && r != '_' {
			return "unknown"
		}
	}
	return value
}

func syncStateSuccessful(state branchsync.State, check bool) bool {
	if state.State == branchsync.StateSynchronized || state.State == branchsync.StateMergedRemoteRemoved {
		return true
	}
	// A recovered branch has no pending synchronization: custody is with the
	// operator and the next step is a fresh run, not a blocked exit code.
	if state.State == branchsync.StateCustodyReturned {
		return true
	}
	// A branch released by cancellation is the operator's with nothing to
	// synchronize or recover; it must never surface as a blocked exit.
	if state.State == branchsync.StateUserOwned {
		return true
	}
	return check && branchsync.CanApply(state)
}

func branchSyncField(state branchsync.State) toON.Field {
	local := []toON.Field{
		{Key: "branch", Value: state.Local.Branch},
		{Key: "head", Value: state.Local.Head},
		{Key: "clean", Value: state.Local.Clean},
	}
	if state.Local.Reason != "" {
		local = append(local, toON.Field{Key: "reason", Value: state.Local.Reason})
	}
	pipeline := []toON.Field{
		{Key: "run", Value: state.Pipeline.RunID},
		{Key: "status", Value: state.Pipeline.Status},
		{Key: "phase", Value: state.Pipeline.Phase},
		{Key: "submitted_head", Value: state.Pipeline.SubmittedHead},
		{Key: "current_head", Value: state.Pipeline.CurrentHead},
		{Key: "pushed_head", Value: state.Pipeline.PushedHead},
		{Key: "pushed_at", Value: state.Pipeline.PushedAt},
		{Key: "push_generation", Value: state.Pipeline.PushGeneration},
	}
	target := toON.NewObject(
		toON.Field{Key: "kind", Value: state.Target.Kind},
		toON.Field{Key: "remote", Value: state.Target.Remote},
		toON.Field{Key: "url", Value: state.Target.URL},
		toON.Field{Key: "ref", Value: state.Target.Ref},
	)
	remote := toON.NewObject(
		toON.Field{Key: "observed_head", Value: state.Remote.ObservedHead},
		toON.Field{Key: "freshness", Value: state.Remote.Freshness},
		toON.Field{Key: "observed_at", Value: state.Remote.ObservedAt},
	)
	fields := []toON.Field{
		{Key: "state", Value: state.State},
		{Key: "changed", Value: state.Changed},
	}
	if state.Recovered {
		fields = append(fields, toON.Field{Key: "recovered", Value: true})
	}
	fields = append(fields,
		toON.Field{Key: "local", Value: toON.NewObject(local...)},
		toON.Field{Key: "pipeline", Value: toON.NewObject(pipeline...)},
		toON.Field{Key: "target", Value: target},
		toON.Field{Key: "remote", Value: remote},
		toON.Field{Key: "relation", Value: state.Relation},
		toON.Field{Key: "safety", Value: state.Safety},
		toON.Field{Key: "pr_state", Value: state.PRState},
	)
	if state.Recovery != nil {
		fields = append(fields, toON.Field{Key: "recovery", Value: toON.NewObject(
			toON.Field{Key: "source", Value: state.Recovery.Source},
			toON.Field{Key: "repository", Value: state.Recovery.RepositoryID},
			toON.Field{Key: "run", Value: state.Recovery.RunID},
			toON.Field{Key: "branch", Value: state.Recovery.Branch},
			toON.Field{Key: "required_head", Value: state.Recovery.RequiredHead},
			toON.Field{Key: "preserved_head", Value: state.Recovery.PreservedHead},
			toON.Field{Key: "archive_ref", Value: state.Recovery.ArchiveRef},
			toON.Field{Key: "keep_local", Value: state.Recovery.KeepLocal},
			toON.Field{Key: "proof", Value: state.Recovery.Proof},
		)})
	}
	if state.Error != "" {
		fields = append(fields, toON.Field{Key: "note", Value: state.Error})
	}
	if state.NextAction != nil {
		fields = append(fields, toON.Field{Key: "next_action", Value: toON.NewObject(
			toON.Field{Key: "code", Value: string(state.NextAction.Code)},
			toON.Field{Key: "command", Value: state.NextAction.Command},
		)})
	}
	return toON.Field{Key: "branch_sync", Value: toON.NewObject(fields...)}
}

func cachedBranchSyncField(ctxCmd *cobra.Command, runID string) *toON.Field {
	service, closeFn, err := openSyncService()
	if err != nil {
		return nil
	}
	defer closeFn()
	state := service.InspectCached(ctxCmd.Context())
	if runID != "" && state.Pipeline.RunID != runID {
		return nil
	}
	if !relevantCachedSyncState(state) {
		return nil
	}
	field := branchSyncField(state)
	return &field
}

func relevantCachedSyncState(state branchsync.State) bool {
	switch state.State {
	case branchsync.StatePipelineOwned, branchsync.StatePushInProgress, branchsync.StateBehind,
		branchsync.StateLocalAhead, branchsync.StateDiverged, branchsync.StateDirty,
		branchsync.StateRemoteAdvanced, branchsync.StateRemoteRewritten, branchsync.StateRemoteMissing,
		branchsync.StateMergedRemoteRetained, branchsync.StateMergedRemoteRemoved, branchsync.StateClosed,
		branchsync.StateTargetChanged, branchsync.StateCustodyReturned, branchsync.StateUserOwned:
		return true
	default:
		return false
	}
}
