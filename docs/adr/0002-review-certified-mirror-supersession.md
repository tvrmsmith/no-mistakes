---
status: accepted
---

# A reviewed successor may replace the mirror head its own run published

Accepted Decision 41-A lets pipeline publication replace a private mirror head only when that head is exactly `Run.SubmittedHeadSHA`, and says plainly that ownership is not containment evidence. That leaves one reachable state with no path out. A run pushes, CI finds the branch conflicts with its base, the repair agent rebases to resolve them, and the rebased head cannot descend from the head Review certified, so CI revalidates from Format instead of publishing. Revalidation re-runs Format through Review and certifies the new head. Push then refuses, because the mirror still holds the head this same run published before the repair, the conflict resolutions changed the patches so patch-ID equivalence fails, and the old head is not the submitted head so 41-A does not apply. Every ingredient is deterministic: both runs that hit a base-branch merge conflict on the reorder build died this way, after roughly five hours each, with the work finished and unpublishable.

We extend the exception. Publication may archive and replace a mirror head when that head is one this run published AND `runs.review_approved_head_sha` equals the head being published. The evidence is a completed review of the successor, not ownership of the predecessor, which is what 41-A's objection actually asks for. The old head is archived under the existing `refs/tags/no-mistakes-abandoned/<branch>/<sha>` tag, so nothing is destroyed either way.

## Considered options

**Leave 41-A alone and document a manual recovery.** Keeps our contract identical to upstream's and costs no divergence. It also concedes that every merge-conflict repair on an already-pushed branch burns a full run and needs a human with `git update-ref`. The failure is deterministic, not rare, so this is a standing tax rather than an edge case.

**Merge the base instead of rebasing in the repair.** Makes the repaired head a descendant of the reviewed head, so the continuity proof passes and CI publishes without revalidating. Rejected: the merged tree contains base-branch code Review never saw, so it satisfies the continuity check by ancestry while defeating what the check is asking. It would trade a stuck run for an unreviewed publication.

**Re-stamp `Run.SubmittedHeadSHA` to the published head after a repair.** Satisfies 41-A's letter without amending any documented decision. Rejected: it routes around the sentence "ownership is not containment evidence" rather than answering it, and it overloads the submitted head to mean both what the contributor pushed and what we last published. The next reader of that field inherits the ambiguity.

**Archive the superseded mirror head inside the CI repair path.** Moves the action to where supersession happens instead of discovering it at Push. Same substance as this decision, so it needs the same extension, and it splits mirror authority across two owners.

## Consequences

- Our gate-model contract diverges from upstream `kunchenguid/no-mistakes`. Upstream issue #983 designed reconciliation for a clean rebase, where stable patch IDs prove containment, and added 41-A for the submitted head. A rebase carrying conflict resolutions fits neither path. Worth raising upstream rather than carrying the divergence silently.
- The exception is only as safe as its narrowness. It must require both conditions together: a head this run published, and a review approval recorded for the exact head being published. Either alone reopens the hole 41-A closed, so the tests pin both halves and the negative cases.
- A run whose review approval was revoked cannot use the exception. `prepareRestart` clears `review_approved_head_sha` on every restart, so a run mid-revalidation has no approval to present and the guard behaves exactly as it does today.
- Nothing is destroyed on either path. The archive tag already holds the exact superseded head before the branch ref is replaced, which is what makes this a supersession rather than a clobber.
- `docs/src/content/docs/concepts/gate-model.md` continues to own the contract text. Decision 41-A stays as written, with this extension stated beside it, since the reasoning for the narrow original still explains why the second condition exists.
