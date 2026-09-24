---
name: upstream-sync
description: Use when updating from upstream: merging kunchenguid main into main, then main into personal-build.
---

# Upstream sync

`personal-build` is Trevor's fork line: upstream `main` plus his own merged PRs. A sync folds the latest upstream into it without losing either side. Remotes: `upstream` is kunchenguid, `origin` is the personal fork.

## Steps

1. **Fetch.** `git fetch upstream` and `git fetch origin`. Done when `main..upstream/main` and `personal-build..origin/personal-build` are listed; report both, since commits that other sessions merged to `origin/personal-build` are part of the sync.
2. **Catch up locally.** Fast-forward `personal-build` to `origin/personal-build`. Done when `personal-build..origin/personal-build` is empty.
3. **Merge upstream into main.** Check out `main`, `git merge --no-edit upstream/main`. `main` carries earlier merge commits, so it cannot fast-forward; a merge is the expected shape. Done when the merge commits cleanly.
4. **Merge main into personal-build.** Check out `personal-build`, `git merge --no-edit main`. Resolve with the `mattpocock-skills:resolving-merge-conflicts` skill, holding each hunk against the **divergence ledger** below: every hunk keeps upstream's intent AND personal-build's. Done when `git diff --name-only --diff-filter=U` is empty and no conflict marker is left.
5. **Fix fallout.** `gofmt -l .`, then `make lint`. When `internal/skill/skill.go` changed, run `go run ./cmd/genskill` and stage `skills/no-mistakes/`. Done when both are clean.
6. **Verify.** `go test -race ./...`. A package that times out under the full parallel run is **load-suspect**: rerun it alone with `-timeout 20m`, and pin a hang with `scripts/run-each-test.sh <package dir>`. Only a failure that reproduces alone is real. Done when every package passes, alone if load-suspect.
7. **Commit.** A merge commit under 150 words naming each behaviour reconciliation (a hunk where both intents needed new code) and each class of adapted test.
8. **Re-fetch, then ask.** Fetch both remotes again; if anything landed, return to step 2. Then report and ask Trevor before pushing `main` and `personal-build`.

## Divergence ledger

The places personal-build deliberately differs from upstream, and what an upstream change touching them needs. When a sync meets a divergence not listed here, add it.

- **Step order.** Personal-build runs intent → rebase → format → lint → test → metrics → document → review → push → pr → ci, and `pipeline.RestartBoundary` is Format. Upstream tests that assume Test after Review, or a restart from Review, get retargeted (a step that still sits after Review, `pipeline.RestartBoundary` in assertions).
- **Personal-only steps.** Format and Metrics exist only here. When upstream changes behaviour for "every step" (for example, a fail-closed base fetch), apply it to them too and update the pipeline-steps reference list.
- **Base branch.** Steps resolve their base through `runBranchBaseSHA` / `effectivePRBaseBranch`, never `Repo.DefaultBranch`. An upstream change to base resolution keeps upstream's mechanics and personal-build's branch choice.
- **Shared step exit.** `runValidationStep` commits a dirty worktree at every validation step's exit. When upstream wants a round's leftovers kept uncommitted, add the exception there, keyed on the outcome's findings.
- **Test discovery and coverage.** The Test step discovers units and requires coverage artifacts. Upstream Test tests switch to `newTestContextWithCoverage` with `config.Commands{Test: oneRepositoryUnitCommand}`, which skips the discovery turn and keeps agent call counts upstream's.
- **Review coverage.** A clean review turn certifies only with `reviewed_paths` covering the diff; a test double that returns clean findings sets `ReviewedPaths: fullReviewCoverage(...)`. A test that hangs inside `waitForApprovalOrReconcile` is usually this.
- **CI monitor clock.** A CI test with a seconds-scale `ci_timeout` calls `pinCIMonitorClock(step)`; `TestEveryShortTimeoutTestPinsTheClock` names the offenders.
- **Deleted helpers.** Personal-build removed dead code upstream still has. An upstream test calling a removed helper asserts through the surviving one.
- **Signatures.** Tests use `t.Context()`, since many test files dropped the `context` import. Personal-build changed signatures upstream calls with the old shape (for example `startRun`'s omit-intent argument, `runCancel func(error) error`, fake daemons returning `ipc.ProtocolVersion`). Match the personal-build signature at the upstream call site.
- **Deleted test files.** A file personal-build deleted that upstream modified stays deleted (`git rm`), unless the upstream change is a test personal-build lacks; then port that test to where its subject now lives.
