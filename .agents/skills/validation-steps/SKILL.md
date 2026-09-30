---
name: validation-steps
description: Use when changing the Format, Lint, Test, or Metrics steps, test unit discovery, coverage artifacts, operator extra linters, or how an agent-authored commit restarts validation.
user-invocable: false
metadata:
  internal: true
---

User-facing semantics live in `docs/src/content/docs/reference/pipeline-steps.md`, `reference/repo-config.md` (`test.units`, `metrics`, `restart`), `reference/global-config.md` (`lint.extra_linters`), and `reference/environment.md` (the `NO_MISTAKES_*` variables).

**Validation restart (`runValidationStep`, `internal/pipeline/steps/common_fix.go`)**

- Format, Lint, Test, Metrics, Document, and Review exit through `runValidationStep`. It commits a dirty worktree and attributes the commit by `StepContext.AgentInvocations()`, which `runAgent` counts before each turn. An agent-authored commit sets `RestartFrom = pipeline.RestartBoundary` (Format); a tool-authored one restarts nothing.
- `restartExemptCommit` skips the restart when every changed path matches trusted `restart.exempt_paths`; `AGENTS.md` and `CLAUDE.md` never qualify. `commitPathsSinceHead` diffs `--no-renames`, so a code file moved into `docs/` still counts as code.
- The certifier never modifies what it certifies. A round that returns `ReviewApprovedHeadSHA` with a dirty tree commits nothing and parks (`residueGateOutcome`). Approve discards exactly the paths recorded on `RunShared.ValidationResidue` through `pipeline.ApprovalResidueDiscarder`; fix commits them and re-reviews. Discard runs `restore --source=HEAD --staged --worktree` and `clean -ffd` on `:(literal)` pathspecs, then re-reads to prove every recorded path is gone. Keep it scoped to the record: `reset --hard` plus `clean -fd` destroys work no gate ruled on.
- The churn and residue gates both clear `outcome.AutoFixable`, so neither resolves silently inside an auto-fix round.
- `prepareRestart` revokes review authority in the same write as the head (`db.UpdateRunHeadSHAForRevalidation`). `Executor.honourRestart` is the only place a restart is acted on. A skipped boundary never rewinds: with Push live the run fails with `ErrRestartBoundarySkipped`, and with Push skipped `declineRestart` drops the request before the round acts. The skip set comes from `runs.skipped_steps`, never from step rows.
- Restarts are uncapped on purpose. `RunShared.LastRestartTree` parks a step that re-commits its own last restart tree (`churnGateOutcome`), and `runs.restart_count` against `db.RestartSoftCap` only annotates.
- A re-entry is not a fix round, so a step reads `sctx.PreviousFindings` on its non-fix path only. A validation step never implements `pipeline.ApprovalGateReconciler`, whose copied context would misattribute the round's commit.
- A parked gate's diff falls back to the round's exit commit only for a `pipeline.CommitsOwnWorkAtExit` step, and only when `step_rounds.starting_head_sha` proves the round moved the head.
- Decision record: `docs/adr/0001-cheap-gates-before-review-and-restart-on-agent-commits.md`. Regressions: `internal/pipeline/restart_test.go`, `internal/pipeline/executor_residue_test.go`, `internal/pipeline/steps/restart_test.go`, `internal/pipeline/steps/restart_exemption_test.go`.

**Test discovery and execution (`internal/pipeline/steps/test_discovery.go`, `test.go`)**

- Precedence: trusted `test.units`, then `commands.test` as one `repository` unit, then an agent inference pass that runs only when neither is configured. The agent pass reads the pushed worktree, which grants nothing the unconditional evidence pass lacks.
- `RunShared` caches the layout keyed by `changedFilesFingerprint` (NUL-joined) and writes it through to `runs.test_discovery`, so recovery reuses it. Persistence is best effort.
- An unusable discovery answer parks for a maintainer through `parkForMaintainer`; an agent invocation failure still fails the run.
- `config.NormalizeUnitPath` and `config.ValidateUnitPath` own a unit path's form and rule at every site. `validateDiscovery` also deduplicates `Selected`.
- A changed path belongs to its most specific owners (`mostSpecificOwners`, with `.` shortest). `selectUnitsForPaths` and `underSelectedUnits` share that predicate, so under-selection can fire only on the agent source. It expands once, and a second fault in the run parks with finding ID `test-scope-fault`. Approving that park accepts the gap for that changed-file fingerprint (`RunShared.AcceptParkedTestScopeGap`, called from `Executor.applyApprovalOverride`), so a re-test of the same set does not park again. A fix selecting it calls `RunShared.ResetTestDiscovery` and rediscovers with no repair turn. Every Test round clears the parked gap first, so approving any other Test gate accepts nothing.
- Fix mode adds untracked files to the changed set, because a new test file is the common repair.
- Unit commands receive `NO_MISTAKES_BASE_SHA`, `NO_MISTAKES_CHANGED_FILES`, and `NO_MISTAKES_CHANGED_FILE_COUNT`. `changedFilesEnvValue` drops paths containing a newline and empties a list over 96 KiB instead of truncating it; either omission emits a warning finding on every outcome.
- The discovery prompt carries the targeted-validation boundary (never the full suite, even for `.`), and `test.instructions` feeds both discovery and the evidence turn.
- Regressions: `internal/pipeline/steps/test_discovery_test.go`, `internal/pipeline/steps/test_execution_test.go`, `internal/pipeline/steps/test_scope_gap_test.go` (the scope-fault park's approve and fix answers, and the runbook-template prompt rule), `internal/pipeline/shared_test.go`, `internal/pipeline/test_scope_gap_test.go` (approval wiring on the live and recovered gate), `internal/pipeline/executor_shared_restore_test.go`.

**Vacuous-green guard (`guardVacuousGreen`, `internal/pipeline/steps/coverage_*.go`)**

- An exit code certifies nothing alone. Every unit command gets `NO_MISTAKES_COVERAGE_DIR` and writes a coverage profile (LCOV or Cobertura) plus a test report (JUnit or TRX) there. Content decides the format, and reads are bounded.
- Missing or unparseable artifacts park for the maintainer. Zero executed tests, or no changed function covered, parks auto-fixable with finding ID `vacuous-green`, which selects the write-the-missing-test fix prompt.
- Only trusted `ignore_patterns` (`Config.TrustedIgnorePatterns`) exempt a path.
- The per-unit directory is wiped before each command. The guard runs after under-selection expansion and before the evidence pass, and not at all when no unit command ran. A change touching no profiled extension skips only the changed-function check.
- Coverage never reaches the branch: `RunManager.cleanupRunCoverage` drops it at run end and `sweepOrphanCoverageDirs` after a crash, sweeping nothing when active runs cannot be listed.
- Regressions: `internal/pipeline/steps/test_coverage_guard_test.go`, `internal/pipeline/steps/coverage_artifacts_test.go`, `internal/pipeline/steps/coverage_changed_test.go`, `internal/daemon/coverage_cleanup_test.go`, `internal/daemon/coverage_sweep_test.go`.

**Metrics step (`internal/pipeline/steps/metrics.go`, `metrics_verdict.go`)**

- The step runs trusted `commands.metrics`, reads a verdict, and gates. The CRAP command lives in the `coding-standards` repository, and `metrics_verdict.go` owns the contract it must meet. There is no agent fallback, because numbers produced by inspection are fiction.
- The verdict comes from stdout alone (`runStepShellCommandEnvSplit`). Parsing tries the whole output, then one linear brace-stack pass that records balanced objects at every depth containing `metricsFunctionsKey`, newest first, bounded by `maxMetricsReportCandidates`. `functions: null` means zero measured functions.
- `evaluateMetricsOutput` normalises each reported file to repo-relative once (`metricsWorkDirRoots` covers the `/private/var` alias), because `metrics.json` is published.
- Every `metrics.exempt_paths` entry passes `validatePathInstructionGlob`. Every finding carries an explicit ID, and a breach ID comes from file and function (`metricsBreachID`).
- `Breached` fails closed on both sides: any breach, or a nonzero exit. Only a named breach is auto-fixable (`metricsMeasuredABreach`); an exit-only breach is ask-user. A pass whose output did not parse carries a warning.
- A configured command with no coverage (the agent-evidence Test path, or Test skipped) parks for the maintainer. The step reads `NO_MISTAKES_COVERAGE_ROOT` and never sets `NO_MISTAKES_COVERAGE_DIR`.
- `changedFilesEnvAdvisory` is the one changed-file omission advisory, shared with Test.
- Regressions: `internal/pipeline/steps/metrics_test.go`, `internal/pipeline/steps/metrics_verdict_test.go`, `internal/config/config_metrics_test.go`.

**Operator extra linters (`lint.extra_linters`, `internal/pipeline/steps/lint_extra.go`)**

- `applyExtraLinters` runs after `lintDuty` on every path, including fix rounds. It only appends findings and widens `NeedsApproval`. The list is global-only; `RepoConfig` has no `lint` field.
- `findings_pattern` (named groups `file`, `line`, `message`) is required at load and matched against stdout alone. A nonzero exit parks as a warning. Severity defaults to `info`.
- Findings are bounded per linter, per description, and across the whole list, because they ride the IPC event stream.
- Each finding carries an explicit `Action` (`extraLinterAction`) and a stable `ID` (`extraLinterFindingID`). `config.ExtraLinterIDSlug` owns the slug and rejects names that collide at load. Extras never run `commands.prepare`.
- Commands receive the `NO_MISTAKES_*` run variables plus `NO_MISTAKES_REPO_PATH`, because the gate worktree hides the real checkout.
- Regressions: `internal/config/lint_test.go`, `internal/pipeline/steps/lint_extra_test.go`, `internal/e2e/extra_linters_test.go`.
