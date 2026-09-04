# Pre-reorder pipeline baseline

Frozen 2026-09-04, before any step of ADR 0001 reached a real run. It exists so the reorder can
be judged against numbers nobody can retro-fit, and so a later reader can tell a genuine cost
change from a change in what we were measuring.

Source is the local daemon database, `<NM_HOME>/state.sqlite`, tables `runs`,
`step_results`, and `agent_invocations`. Every query is reproduced below so the same numbers can
be recomputed against a post-reorder population.

## Population

| field | value |
| --- | --- |
| runs | 391 |
| window | 2026-07-09 to 2026-09-04 |
| agent invocations | 2229 |
| total agent time | 443.2 h |
| run status | 234 failed, 116 completed, 40 cancelled, 1 running |

Step layout for this population is `intent, rebase, review, test, document, lint, push, pr, ci`.
Only 63 rows record it in `runs.step_plan`; the other 328 predate the column and are legacy rows
whose layout is inferred from the window rather than proven.

## Cost by step

| step | invocations | mean | total | share |
| --- | --- | --- | --- | --- |
| review | 1703 | 810.7 s | 383.52 h | **86.5%** |
| test | 207 | 610.8 s | 35.12 h | 7.9% |
| document | 165 | 266.7 s | 12.23 h | 2.8% |
| ci | 39 | 706.9 s | 7.66 h | 1.7% |
| rebase | 42 | 322.2 s | 3.76 h | 0.8% |
| pr | 49 | 38.7 s | 0.53 h | 0.1% |
| lint | 5 | 168.9 s | 0.23 h | 0.1% |
| intent | 19 | 32.6 s | 0.17 h | 0.0% |

Review median is 761.6 s and p90 is 1421.7 s, so the mean is not being dragged by a small tail.
The step is uniformly expensive.

## Invocations per run

Review is not one slow pass. It is many.

| step | mean per run | worst |
| --- | --- | --- |
| review | **5.44** | 27 |
| ci | 1.86 | 3 |
| intent | 1.58 | 2 |
| test | 1.25 | 4 |
| lint | 1.25 | 2 |
| document | 1.13 | 4 |
| rebase | 1.08 | 2 |
| pr | 1.04 | 2 |

Mean agent time per run is 4894.5 s across the 326 runs that invoked an agent at all. The worst
single run spent 502.7 minutes.

## Where the first blocking signal comes from

For the 235 runs that produced one, the first step to fail or park was:

| step | runs |
| --- | --- |
| review | **153** |
| ci | 23 |
| test | 20 |
| push | 20 |
| rebase | 13 |
| document | 5 |
| pr | 1 |

Review is the first blocking signal in 65% of runs. That is the number the reorder is aimed at.
Under the new layout, a formatting, lint, test, or metrics breach should claim most of those,
and Review's share should fall toward the design findings only it can see.

## Findings produced

| step | invocations reporting | mean findings | total |
| --- | --- | --- | --- |
| review | 914 | 8.84 | 8079 |
| document | 162 | 0.65 | 106 |
| test | 170 | 0.38 | 65 |
| lint | 5 | 0.60 | 3 |

Review produces 98% of all findings. How many belong to the cheap-gate classes is the question
the eval corpus tag answers, not this table.

## Why wall-clock run time is not the metric

Park time dwarfs execution. Across the 269 runs that parked, mean park is 154.7 minutes, worst
is 38.4 hours, and the total is 693.6 h against 443.2 h of agent time. Wall clock therefore
measures how quickly a human answered a gate. Compare `agent_invocations.duration_ms` instead.

Mean time to first blocking signal is 20859 s per run for the same reason. It is a park-latency
figure, not a pipeline-speed figure, and it is recorded here only so a post-reorder reading of
the same query is not mistaken for progress.

## Confounders

**Model mix.** 1833 invocations on `claude-opus-5` (401.64 h), 238 on `claude-opus-4-8`
(14.82 h), 158 unrecorded (26.76 h). Stratify by `model` or restrict to opus-5, otherwise a model
change reads as a pipeline change.

**Workload size.** `workload_lines` is populated for review only, at 0.176 agent seconds per
changed line over 7.86 M lines. No other step can be normalized this way today, so per-step
totals must be read against comparable branch sizes.

**Run outcome mix.** 60% of this population failed. A reorder that changes which runs fail also
changes which runs accumulate cost, so compare within an outcome rather than across the whole
population.

## Rerunning this

```sql
-- cost by step
SELECT step_name, COUNT(*) n,
       ROUND(AVG(duration_ms)/1000.0, 1) avg_s,
       ROUND(SUM(duration_ms)/3600000.0, 2) total_h
FROM agent_invocations GROUP BY step_name ORDER BY total_h DESC;

-- invocations per run, by step
SELECT step_name, ROUND(AVG(c), 2) avg_per_run, MAX(c) worst
FROM (SELECT step_name, run_id, COUNT(*) c FROM agent_invocations GROUP BY step_name, run_id)
GROUP BY step_name ORDER BY avg_per_run DESC;

-- first blocking step
SELECT step_name, COUNT(*) n FROM (
  SELECT sr.run_id, sr.step_name,
         ROW_NUMBER() OVER (PARTITION BY sr.run_id ORDER BY sr.step_order) rn
  FROM step_results sr WHERE sr.status IN ('failed', 'awaiting_approval')
) WHERE rn = 1 GROUP BY step_name ORDER BY n DESC;

-- split the two populations once the reorder has run
SELECT COALESCE(NULLIF(step_plan, ''), '(legacy)') plan, COUNT(*) n
FROM runs GROUP BY plan ORDER BY n DESC;
```

The last query is the discriminator. Post-reorder runs record
`intent,rebase,format,lint,test,metrics,document,review,push,pr,ci` in `runs.step_plan`, so the
two populations separate without new instrumentation. Join `agent_invocations` to `runs` on
`run_id` and group by that column to compare cost directly.

## What would count as success

- Review's share of total agent time falls well below 86.5%.
- Review invocations per run falls from 5.44 toward 1 or 2.
- Review stops being the first blocking signal in the majority of runs.
- Total agent time per run falls, after subtracting whatever the restart rule adds back. This is
  the one that can go the wrong way, and the restart counter on the run row is what makes it
  measurable.
