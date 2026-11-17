# Handoff, 2026-11-17

Development on this fork stopped on the machine that produced it. This file records where the open work stands so it can resume elsewhere.

## Branches pushed with this handoff

| Branch | Head | State | Next step |
|---|---|---|---|
| `personal-build` | `d69a3b00` | 2 new commits. `d69a3b00` fixes the daemon stop race: `processRunning` in `internal/daemon/proc_unix.go` treats ps's "exit status 1" as an error, so a process that is already gone is not seen as stopped. It includes a regression test. `93e3c384` extends the upstream-sync ledger. | Decide whether the `SKIP_TVRMSMITH_LINT=1` line in the upstream-sync skill stays. Then sync upstream/main (218 commits behind as of this date; the last sync conflicted in about 12 files). Install with `no-mistakes update --when-idle` (upstream #1697 replaced the old /tmp install-when-idle watcher). The 5s `daemonStopTimeout` is still too short for a daemon with many runs: scale it or file an issue. |
| `fix/59-check-discovered-commands` | see branch | Fix for #59 (check inferred unit commands before any run). PR #61. One new commit, re-signed. | Rerun the pipeline. If it passes, update PR #61. |
| `fix/60-rediscovery-scope-fault` | `d9995170` | Fix for #60 (rediscovery re-ask on scope fault). PR #62. Two new commits. The pipeline rerun at `d9995170` has no recorded outcome. | Rerun the pipeline on #62 first. Both fixes share the quote-aware placeholder check and `newShellParseCommand`. |

The PR and issue states for #61, #62, #65, #66 and #67 were not checked at handoff time.
