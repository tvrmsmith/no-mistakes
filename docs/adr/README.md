# ADRs

One decision per file, named `NNNN-<kebab-slug>.md`, numbered in the order they were decided. Each carries a `status` in its frontmatter and the sections ADR 0001 shows: a lead paragraph stating the decision and why, `## Considered options`, and `## Consequences`.

`docs/agents/domain.md` owns when to read these while exploring. This file owns how to change one.

## Status

- **proposed**: the decision is written but the code does not yet honour it. Edit it in place, including for style, right up until it lands.
- **accepted**: the code honours it. The rules below apply from here on.
- **superseded by NNNN**: a later ADR replaced the decision. Leave the body intact so the reasoning stays readable.

## Changing an accepted ADR

The question is whether the **decision** changed, not how large the edit is.

- **The decision stands and the record is wrong or incomplete.** Amend it. Add or correct the affected line in place, most often a `## Consequences` bullet. A fact the ADR was silent on is an amendment, not a new decision.
- **The decision changed.** Write the next numbered ADR, state what it replaces, and set the old one's status to `superseded by NNNN`. Leave the old body alone.

An accepted ADR is never reworded for style, and never rewritten in place to say something it did not say. Its value is that it records what was decided at the time.

## Where a fact belongs

An ADR records **why** a decision was made and what it costs. `AGENTS.md` records **what the code does** and the traps in it, and `CONTEXT.md` defines vocabulary. Keep a fact in one of the three. When a consequence is also a live trap for future agents, the ADR states the reasoning and `AGENTS.md` points at it.
