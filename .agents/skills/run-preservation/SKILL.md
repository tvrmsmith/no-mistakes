---
name: run-preservation
description: Use when changing what a daemon stop preserves, startup recovery of parked runs or CI monitors, branch contention between preserved runs, or a run's persisted skip scope.
user-invocable: false
metadata:
  internal: true
---

The drain and destructive-guard rules live in the `daemon-runtime` skill; this skill owns what survives the stop and how the next start resumes it.

**Resume points (`pipeline.resumePoint`)**

- A run holds exactly one: an approval/fix-review gate (`lifecycle.ParkedAtGate`) or a live CI monitor (`lifecycle.ResumableCIMonitor`: the only active step is `ci`, `running`, no `agent_pid`, and the run has a PR URL). The pid clause matters because an inline CI repair keeps the row `running`.
- `runs.awaiting_agent_since` is evidence, not proof, so `ParkedAtGate` corroborates it against a gate step row. It never changes gate resolution.

**Clean stop**

- `RunManager.Shutdown` cancels with `pipeline.ErrDaemonShutdown`. The gate wait in both `Executor.executeStep` and `Executor.Resume` translates that into `pipeline.ErrParkPreserved` before it fails anything, and `RunManager.finishRunGoroutine` then keeps the worktree. Every path that can complete a gate re-checks `ctx.Err()` right before acting. A run cancelled mid-step still fails.
- `Executor.ciMonitorPreservable` owns every CI-monitor condition: a running `ci` row with no pid, a PR URL, a clean worktree, and a head equal to the recorded head. Every refusal routes through its single wrap; a refused run that holds a PR URL ends `types.RunCIMonitorInterrupted` and keeps its checkout, since it may hold an unpublished repair commit.
- `pipeline.CIMonitorWorktreeClean` is the one cleanliness rule, read by the stop path, the destructive guard, recovery, the drain, and the orphan sweep. An incomplete read returns `ErrRecoveryEvidenceUnavailable` and never counts as clean.
- The drain classifies through `RunManager.atAPreservedResumePoint`, so it exempts only what the stop will actually preserve.

**Recovery (`prepareRecoveredRun`, `internal/daemon/manager.go`)**

- Deferral is the default. A run fails terminally only for an error classified `unresumable` at the point the adverse fact is established: missing worktree, head mismatch, live `agent_pid`, drifted step plan, unparseable trusted config. Every incomplete read defers, leaving the row `running` and its worktree intact, because failing a transient case deletes unpushed pipeline commits.
- A deferred run stays owned: `registerDeferredRun` lets `axi abort --run` end it.
- When `GetActiveRuns` fails, startup runs no sweep and no worktree cleanup.
- `preservedBranchRuns` settles branch contention. Startup admits gates and CI monitors, the live push path admits gates only, and a branch with more than one candidate resumes none of them.
- `resumeRecoveredRun` defers on `ErrRecoveryEvidenceUnavailable` from `Executor.Resume`, even after planning succeeded. The gate pin is read before the resume preconditions are validated.

**Scope**

- `runs.skipped_steps` persists the effective skip set, `cfg.SkippedSteps(skipSteps)` (run `--skip` plus trusted `skip_steps`), and `startRun` fails rather than start a run whose scope cannot survive a stop. `ValidateRecoveredRun` accepts a skipped row only when that set explains it. `runs.step_plan` is guard evidence and never selects steps.

**Tests**

- Stop a test daemon through `testDaemonInstance` (`internal/daemon/helpers_test.go`) and close every client first.
- Regressions: `internal/pipeline/executor_shutdown_park_test.go`, `internal/pipeline/executor_ci_resume_test.go`, `internal/daemon/shutdown_park_test.go`, `internal/daemon/ci_monitor_preservation_test.go`, `internal/daemon/ci_monitor_resume_test.go`, `internal/daemon/recovery_reads_test.go`, `internal/daemon/manager_drain_test.go`, `internal/lifecycle/cimonitor_test.go`, `internal/lifecycle/resumable_test.go`.
