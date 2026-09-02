---
status: proposed
---

# Cheap gates run before Review, and any agent-authored commit restarts validation

Review is the slowest step in the pipeline and, running third, it spent that time reporting problems that a formatter, a linter, a test run, or a metrics gate would have caught for a fraction of the cost. Worse, every step after it could commit code that Review had never seen, so its verdict described a tree that no longer existed by the time the branch was pushed. We reordered to `Intent, Rebase, Format, Lint, Test, Metrics, Document, Review, Push, PR, CI` and made Review the last step of the validation region, so it certifies the tree that actually ships. To keep that true under fix rounds, any agent-authored commit inside the region restarts validation at Format, while deterministic tool output does not.

## Considered options

**Adopt Archon instead.** Archon's YAML DAG engine offers the ordering flexibility we wanted, but no-mistakes' value is not its ordering. It is the push-triggered gate, the trust boundary that keeps a contributor's branch from selecting the commands that run with maintainer credentials, and the park-and-resume semantics that survive a daemon stop. Rebuilding those on a general workflow engine costs more than reordering an array.

**Restart on any HEAD movement.** Simpler to state, and it makes the formatter restart the pipeline on every whitespace change. Keying on authorship costs one attribution decision per step and stops deterministic tools from paying for a full revalidation cycle.

**Leave Review early and accept the stale verdict.** This is what we had. Certification then means "Review saw an ancestor of this tree", which is not a claim worth making.

## Consequences

- `runs.review_approved_head_sha` must be cleared by `prepareRestart`, or a restart whose re-review parks would leave a stale approval authorizing the push.
- Restart terminates without an explicit cap, because `ResetStepsFrom` leaves `step_rounds` intact so per-step fix budgets never refill.
- Commits touching only documentation paths are exempt from restart, defined by a trusted-only glob list that deliberately excludes `AGENTS.md` and `CLAUDE.md`, since those steer the agents in later steps.
- The combined document+lint housekeeping pass dies with the reorder. Lint now precedes Document, so there is nothing left to stash. Measured cost is roughly 80 seconds per run against a Review step averaging 822 seconds.
- Existing eval corpus entries need a pipeline-version tag. Review now sees a tree that four gates already cleared, so a share of the gold-labelled findings can no longer occur, and scoring new runs against untagged entries would read as a quality regression that is really a scope change.
- `step_order` values change, so runs are drained before the update rather than migrated.
