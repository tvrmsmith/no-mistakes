# no-mistakes

A push-triggered gate that runs an ordered pipeline of validation steps against a contributor's branch before it reaches a pull request. This glossary defines the vocabulary; behavior lives in `AGENTS.md` and the code.

## Pipeline

**Step**:
One named stage of the pipeline, executed in a fixed order.
_Avoid_: stage, phase, task

**Validation region**:
The contiguous span of steps that judge the tree, from the restart boundary through the certifier. Steps outside it either prepare the branch or publish it.
_Avoid_: check phase, gate block

**Restart boundary**:
The step the pipeline re-enters when a restart fires. It is the first step of the validation region.
_Avoid_: rewind point, reset target

**Restart**:
Re-running the validation region from the restart boundary, so a later step's verdict always describes the tree as it now stands. Distinct from a fix round, which re-runs one step in place.
_Avoid_: rerun, replay, loop

**Fix round**:
A second or later attempt at a single step, with the previous findings supplied to the agent. Bounded per step by `auto_fix.<step>`.
_Avoid_: retry, iteration

**Agent-authored commit**:
A commit whose content an agent wrote. It triggers a restart, because no gate has judged it.
_Avoid_: AI commit, fix commit

**Tool-authored commit**:
A commit whose content a deterministic tool produced, such as a formatter. It does not trigger a restart.
_Avoid_: mechanical commit, auto commit

## Gates

**Gate**:
A step's decision point where the run stops for a human. The run survives a daemon stop while parked at one.
_Avoid_: checkpoint, prompt, approval step

**Park**:
To hold a run at a gate awaiting a response.
_Avoid_: pause, block, suspend

**Certification**:
Review's record that it judged a specific HEAD SHA. A later step may only publish a tree that certification covers.
_Avoid_: approval, sign-off

## Trust

**Trusted config**:
The `.no-mistakes.yaml` read from the default branch at a pinned SHA, rather than from the branch a contributor pushed. Exactly one file is trusted per run.
_Avoid_: base config, upstream config

**Gate strength**:
A config field that decides how hard a gate is to pass. Trusted-only, because a pushed branch weakening its own gate defeats the point of one.
_Avoid_: threshold, strictness setting

## Testing

**Unit**:
An independently testable component of a repository, such as a service in a monorepo. Units are what the Test step selects between.
_Avoid_: module, package, project, component

**Discovery**:
Deriving which units a change touches and what command tests each one. Separate from execution so the scope a run tested is auditable.
_Avoid_: detection, resolution

**Under-selection**:
Discovery omitting a unit that the changed files belong to. A scope fault, not a coverage finding.
_Avoid_: missed unit, gap

**Vacuous green**:
A test run that passes because it exercised nothing, rather than because the code is correct.
_Avoid_: false pass, empty run
