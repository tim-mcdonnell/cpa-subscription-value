# Estimator design (Phase 2, as implemented)

This file describes what `internal/meter`, `internal/estimate` and `internal/sim` actually do. The spec is `docs/plan.md` §Normalization, §Storage and §Estimator. Where the code refines or departs from the plan, the entry says so and gives the reason. The measured corpus results are at the end.

## Pipeline

```
readings (one meter) ─┐
                      ├─ meter.AssignCycles ─► []CycleAssignment (per anchor × regime)
events (account) ─────┤        │
   meter.ScopeEvents ─┘        ├─ meter.Crossings (header, or poll for poll-only)
   meter.NewEventIndex         ├─ meter.BuildSegments × {time, idx1, idx0, idx2}
                               ├─ estimate.CoverageGaps (polls) ─► coverage_gap on segments
                               ├─ estimate.ChooseLag (pooled over the last 4 cycles)
                               ├─ estimate.Primary (guard, V̂, SE_q, SE_b, CI, mix)
                               ├─ grade, level_shift / v_blend (chained to the previous final)
                               └─ estimate.ForecastCycle (open cycle only)
estimate.Analyze = all of the above, pure.  estimate.Recompute = load → Analyze → persist.
```

## Exported API

**meter**:
- `AssignCycles(readings, events, now, Options) []CycleAssignment`
- `Crossings([]AssignedReading, tick, source) []Crossing`
- `Tick([]MeterReading) (tick, precisionChange)`
- `NewEventIndex(events, SplitFunc) *EventIndex`
- `BuildSegments(cycle, crossings, *EventIndex, Lag, SegmentOptions) []Segment`
- `ScopeEvents`, `IsScoped`, `DefaultScopes` (the `7d_oi` scope is `(?i)fable`)
- `RatioSplit`, `Level`, `SortEvents`
- Lags: `LagTime`, `LagIdx0`, `LagIdx1`, `LagIdx2`

**estimate**:
- `Primary([]Segment, Options) Result`
- `ChooseLag(history []map[Lag][]Segment, prev Lag, minCrossings) LagChoice`
- `Grade(GradeInput) string`
- `ForecastCycle(cycle, Result, proxiedUSD, now) Forecast`
- `PollOnly(...)`, `CoverageGaps(...)`, `CUSUM([]float64)`, `LevelShift`, `Blend`
- `Analyze(meterKey, readings, events, now, AnalyzeOptions) []CycleResult`
- `Recompute(st, accountID, meterKey, now)` and `RecomputeWith`

**sim**:
- `DefaultScenario(seed, provider)`, `Generate(Scenario) Corpus`
- `Corpus{Records, Events, Readings, End, Truth}` with `ReadingsFor` and `TrueU7`
- `Pricer`, `Prices`

## Algorithms and refinements

### Tick (`meter.Tick`)

Plan: `tick = 10^-min(precision_dp)`, floored at 1e-4, and flagged `precision_change` when readings mix precisions.

Refinement: upstream trims trailing zeros. A captured real header shows `"0.0"`, and 50% arrives as `"0.5"`. Taking the plain minimum would therefore turn a whole-percent cycle into tick 0.1. Instead:

- Each reading's dp is clamped to [2, 4]. No provider reports coarser than whole percent, and the plan's floor is 1e-4.
- A coarser precision `d` only counts when at least 10 consecutive readings all have dp ≤ d.

Trimming alone almost never produces such a run (about 1% of 4-dp values end in "00"), while a genuine precision era always does. When the cycle mixes eras, the tick is the coarser one and the cycle carries `precision_change`.

### Cycle assignment (`meter.AssignCycles`)

This follows the plan with tol = 120 s. Some details the plan leaves open:

**Close times**
- `reset` closes at the anchor.
- `early_reset` with a new anchor closes at the first new-anchor reading.
- A same-anchor `early_reset` closes at the first pending reading.
- `regime` closes at the revert-detection reading.
- `expired` closes at the anchor.

**Starts**
- `starts_at = anchor − window`, except for a same-anchor regime cycle, which starts at its first pending reading.

**Stale anchor versus revert.** A reading whose older anchor is still in the future is routed to that cycle as a late reading. Two conditions together make it a genuine A→B→A revert:
- It arrives more than `RevertAfter` (120 s, a new option) after the switch.
- It is before the anchor minus tol.

On a revert, both cycles get `regime_change` plus an ambiguous interval [switch, revert]. Segments overlapping that interval are flagged `regime_change` and excluded. The older cycle reopens; the newer one closes with reason `regime`. A later flip back uses the same rule.

**Pending early reset.**
- A pending state starts only on a successful header reading with u ≤ 0.05 and u ≤ P − 0.01.
- It confirms on ≥3 successful, non-decreasing header readings spanning ≥60 s, all at or below P − 0.01.
- Refinement: a single high reading at or below the old peak's level is set aside as a stale replica rather than killing the pending state. The simulated stale replicas (3%) otherwise defeat detection right after a reset.
- Two highs in a row, or any progress past the peak, reject the reset: the pending readings become `quota_drop`.
- Pending that is still open at the end of the data is also flushed as `quota_drop`. It re-evaluates on the next recompute.

**Poll readings** provide anchor evidence only. They can open cycles, but they never start or confirm a pending state and are never crossings.

### Crossings and segments

- **Crossings.** The first eligible reading of a cycle is the baseline, not a crossing. Segments run between consecutive crossings, so both endpoints of every run are crossings, and spend before the first crossing is not used.
- **Index lags.** L ∈ {0,1,2} sum events with index in (i_{k−1}−L, i_k−L], with events ordered by (observed_at, id). A crossing whose event is not in the scoped event list is skipped for index lags, so its movement folds into the next segment.
- **Time lag.** Input-side $ registers at `requested_at`, output $ at `requested_at + latency`. The split comes from `RatioSplit`, which apportions `api_usd` by list-price ratios relative to uncached input: cache read 0.1×, cache write 1.25×, output 5× for Claude and 8× for Codex. These ratios hold for every current family of each vendor, so no absolute price table is needed. `SplitFunc` is swappable once `internal/pricing` lands. The cw1h band is split the same way.

**Segment flags**

| Flag | Meaning |
|---|---|
| `quota_drop` | A quota-drop reading falls inside the segment. |
| `regime_change` | The segment overlaps an ambiguous interval. |
| `restart_gap` | The segment overlaps `SegmentOptions.RestartGaps`. **Not yet fed from settings.** |
| `lag_incomplete` | TEnd is within 10 min of `now`, for every lag, because in-flight requests are missing. |
| `long_gap` | The longest silence between events inside the segment exceeds 2 h (12 h for scoped meters). Informational. |
| `failed_tokens` | The segment includes failed attempts that carried tokens. Informational. |
| `low_usd` | Set by the guard. |
| `coverage_gap` | Set by the poll detector (see below). |

**Segment features.** `features_json` is:

```
{tokens:{family:{uncached_input,cache_read,cache_write,output}}, fast_tokens, long_tokens,
 usd_by_family, usd_by_type, usd_failed}
```

### Primary estimate, variance, guard (`estimate.Primary`)

**Exclusions.**
- Segments flagged `quota_drop`, `regime_change`, `restart_gap`, `lag_incomplete` or `coverage_gap` are excluded, and so are segments the guard marks `low_usd`.
- R is the number of maximal contiguous runs of used segments.

**Estimates.**
- `V̂ = Σ$ / D` with `D = ΣΔ·tick`.
- The same ratio gives `v_hat_cw1h`, `v_hat_excl_failed` (subtracting failed-attempt $), and `v_hat_all` (every segment, nothing excluded; a floor).

**Uncertainty.**
- **Quantization:** `SE_q = V̂·tick·√(R/6)/D`. For one run over 60 ticks this gives 0.680%, the plan's number (unit-tested).
- **Blocks:** used segments are merged until a block spans ≥5 ticks or ≥1 h. Blocks never cross a run boundary.
- **Bootstrap:** moving-block bootstrap with block length ⌈B^{1/3}⌉ over the B blocks, 1000 resamples of Σ$*/ΣD*.
- **Seed:** FNV-1a-64 of `"<series>|<meter>|<anchor unix>|<regime>|<lag>"` seeds PCG(s, s ⊕ 0x9e3779b97f4a7c15). Here `series` is the account ID in `Recompute`.
- **CI:** `CI95 = V̂·exp(±1.96·√((SE_q/V̂)² + (SE_b/V̂)²))`.

**Coverage guard.**
- Up to 3 passes: flag `low_usd` where `$_k < 0.2·V̂·Δ_k·tick`, recompute V̂, and stop early when the set is unchanged.
- `unexplained_frac = ΣΔ(low_usd ∪ coverage_gap) / ΣΔ(all segments)`.

**Refinement: the poll detector also excludes.**
- The plan names the poll detector ("a poll rise with zero proxied $ in between → `coverage_gap`") but excludes only on the ratio test. Here, consecutive polls whose level rose with no proxied registration in (p₁ − 5 min, p₂] are a coverage gap. Every segment overlapping that span is flagged `coverage_gap` and excluded.
- The 5-minute margin keeps a provider that books spend slightly late from faking a gap. The result also carries `coverage_gap_ticks` and `coverage_gap_usd` (ticks × tick × V̂).
- Why: a burst of a few ticks inside a silence usually shares its segment with up to one tick of legitimate spend. It then passes the φ = 0.2 ratio test. The sensitivity table below shows the ratio guard alone misses most bursts smaller than about 5 ticks.

### Grade

The grade follows the plan's bands:

| Grade | Condition |
|---|---|
| `insufficient` | Fewer than 5 ticks, or fewer than 3 used segments. |
| `low` | `regime_change`, or anything that is neither medium nor high. |
| `high` | ≥30 ticks, CI width ≤15%, unexplained ≤0.05, no precision change, lag stable, not poll-only. |
| `medium` | ≥15 ticks, CI width ≤30%, unexplained ≤0.15. |

The plan's bands leave a hole: ≥15 ticks and width ≤30% but 0.15 < unexplained ≤ 0.30. Such cycles are graded `low`. Poll-only estimates are capped at `medium`.

### Lag choice (`estimate.ChooseLag`)

- **Score.** Prequential, per the plan: walking a cycle's eligible segments, `û_k = $_k / V̂_{k−1}`, and the score is `S(L) = median_k |û_k − Δ_k·tick|` in ticks.
- **Warm-up.** Scoring starts once 5 ticks have accumulated.
- **Pooling.** The walk restarts per cycle. Errors are pooled over the last 4 cycles of the (account, meter), because the plan chooses the lag per (account, meter).
- **Decision.** Take the argmin when at least 15 crossings are scored. Otherwise fall back to the previous cycle's lag, then to `time`. Ties resolve in the order time, idx1, idx0, idx2.
- **Stability.** `lag_stable` requires the winner to beat the runner-up by ≥5%. Otherwise the result carries `lag_unstable`, which blocks `high`. The plan's actual criterion is disagreement with the weight learner's backtest, which is Phase 4.

### Running vs final, blend, shifts

- **Kinds.** Closed cycles get `final`; the open one gets `running`. Each cycle chains to the previous final that isn't `insufficient`.
- **Blend.** `v_blend` (running only) is the log-space precision-weighted blend with that final. The prior variance is `(SE/V̂)² + k·0.35²`, where k is the number of cycles elapsed.
- **Level shift.** `level_shift` is set on a final when `|V̂₂ − V̂₁| > 3·√(SE₁² + SE₂²)`, with `SE = √(SE_q² + SE_b²)`.
- **CUSUM.** Runs on the per-block `log(Σ$/ΣD)` values:
  - SE_block is the sample SD of those values.
  - The reference mean is that of the first quarter of the blocks (at least 3).
  - k = SE_block/2, h = 4·SE_block.
  - Change point = the last zero of the alarming side, plus one.
  - Needs at least 6 blocks.
- **CUSUM output: refinement.** A split is reported as `sub_regimes` (V̂ on each side) with the flag `regime_split`. It does **not** create new cycles or a new `regime_seq`, because `regime_seq` is part of the cycle key and is reserved for early resets. The flag is informational: it changes neither V̂ nor the grade (see the measured false-positive rate below).

### Poll-only method

- **When.** A cycle switches to poll-only when header coverage is below 50%. Coverage = the share of the cycle's successful scoped events whose response carried a reading of this meter.
- **How.** Crossings are taken over the cycle's polls, with the tick taken from the polls. Segments use the time lag, with the same `Primary`, so `Var_q = R·tick²/6`. The grade is capped at `medium`.
- **Separation.** Header and poll readings never mix in one crossing sequence.

### Forecast (`estimate.ForecastCycle`)

Per the plan, with these specifics:

- **u_now** is the running max of eligible readings, which equals the latest reading unless that one is a stale replica.
- **Pace** is taken over the trailing 6 h, widened to 24 h when the 6 h window moved less than 3 ticks. It is clipped at `starts_at`, with u = 0 before the cycle start.
- **t_exhaust** is set only when the burn rate is positive.
- **Status:** a meter that is ahead of linear pace but will not exhaust before the reset is reported `on_track`. The plan's four statuses don't cover that case.
- **Rejection:** a `rejected` status header means `exhausted`.

### Persistence (`estimate.Recompute`)

**Loading.** Readings and events are paged in batches of 5000, the store's cap. Events start one window before the first reading.

**Writes.** Recompute calls:
- `UpsertCycle`
- `SetReadingCycles` (dropped readings go back to NULL)
- `ReplaceSegments` with all four lags; the chosen lag's segments carry `low_usd`
- `InsertEstimate`

**No schema change** (other agents are editing migrations). `mix_json` is `{by_family, by_type, diag}`. The `diag` object holds the result fields that have no column:
- `v_hat_all`, `v_hat_excl_failed`, `tick`, `crossings`, `blocks`
- `lag_scores`, `lag_stable`
- `coverage_gap_ticks`, `coverage_gap_usd`, `sub_regimes`
- `forecast`, `proxied_usd`, `header_coverage`

**Frozen cycles.** A closed cycle that already has a `final` under the same price hash is left untouched. A new hash writes a new final and the old rows stay. `pricing_changed` is set when a cycle's events span two hashes.

**Running rows** are appended only when something other than the forecast changed. `/summary` should call `ForecastCycle` live rather than trust a stored forecast. The result: recomputing unchanged data, or recomputing after a replayed ingest, leaves every table byte-identical (tested).

**Non-estimable meters.** Meters that are neither `5h`/`7d` nor scoped get cycles and reading assignments only, with no segments or estimates.

## Thresholds

| Parameter | Value | Source |
|---|---|---|
| Anchor tolerance | 120 s | plan |
| Revert window | 120 s | new |
| Pending start | u ≤ 0.05, u ≤ P − 0.01 | plan |
| Pending confirm | ≥3 readings, ≥60 s | plan |
| Crossing epsilon | 1e-9 | plan |
| `long_gap` | 2 h (12 h scoped) | plan |
| `lag_incomplete` window | 10 min | new |
| Guard | φ 0.2, ≤3 passes | plan |
| Coverage-gap margin | 5 min | new |
| Blocks | ≥5 ticks or ≥1 h | plan |
| Bootstrap | 1000 resamples, block ⌈B^{1/3}⌉, FNV-1a → PCG | plan |
| σ_rw | 0.35 | plan |
| Lag | ≥15 crossings, 4-cycle pool, 5-tick warm-up, 5% stability margin | plan / new / new / new |
| CUSUM | k = σ/2, h = 4σ, reference = first quarter | plan / new |
| Poll-only | header coverage < 50% | plan |
| Pace | 6 h, 24 h when < 3 ticks | plan |
| Tick | dp clamped to [2, 4], coarse run ≥ 10 | new |

## Synthetic corpus (`internal/sim`)

`DefaultScenario(seed, provider)` builds 4 weekly cycles whose peak targets are drawn uniformly from [0.6, 0.9]. Every scenario includes:

- **Hidden allowance.** V₇d is log-uniform in [$200, $600] for Claude and [$60, $180] for Codex, whose requests are about 4× cheaper. V₅h = 0.15·V₇d.
- **Hidden multipliers.** cache_read 0.5×, Fable 1.2×, gpt-5.6-sol-mini 0.8×.
- **One early reset** in cycle 1, 2.5–4 days in, same-anchor by default.
- **One shift:** V × 1.3 from cycle 2 on.
- **One precision change:** Claude headers carry 4 dp for the first 3–4 days, then 2 dp.

Traffic:

- **Sessions.** Starts are exponential between 07:00 and 24:00; days are weighted 1.2 on weekdays and 0.5 on weekends; each day's spend stops at its budget. Each session has 1–4 lockstep streams.
- **Latency.** TTFT is lognormal with median 1.5 s; generation runs at 40–90 tok/s.
- **Contexts.** Claude uses a cached prefix plus a fresh cache write, compacting past 160k; Codex has no cache writes.
- **Mix.** Family shares drift daily (lognormal σ 0.35).
- **Failures.** 3%, half of them partial (the prompt is billed, the output is cut).
- **Stale headers.** 3% of headers come from a replica 10–90 s stale.
- **Polls.** Every 20 min, integer percent.

Header rendering:

- **Claude** floors to 2 dp (or 4 dp) and spells the value like the real header, with trailing zeros trimmed (`"0.5"`, `"0.0"`).
- **Codex** sends an integer percent.
- **Lag modes:** `time` (input at request start, output at completion) or `idx1` (a response is booked just after its own header).

**Bypass β:**
- `gaps` mode: `BurstsPerWeek` (3, scaled to the period length) bursts inside silences of at least 1 h between proxied requests.
- `diffuse` mode: bypass is spread over random proxied requests.

**Output.** Records go through `ingest.Normalize` with the built-in price table (Opus $4/$20, Sonnet $2/$10, Fable $10/$50, gpt-5.6-sol $1.25/$10, per MTok; cache read 0.1×, cache write 1.25×). IDs are assigned in ingest order: events at completion, polls when taken.

**Truth.** `Truth.Periods[i].VEff` is the recovery target: the API $ of proxied usage per 100% of the movement it caused, for that period's actual mix.

## Measured results

Final run: `go test -count=1 ./internal/estimate/...` on 2026-09-26. The full suite takes 16 s wall-clock (≈126 s CPU, 8 cores); `-short` takes about 5 s. Qualifying cycles are weekly, closed, header-method, with ≥40 ticks, matched to a truth period.

The β = 0 pool has 200 seeds:
- providers alternate Claude and Codex,
- the hidden lag alternates `time` and `idx1` in pairs,
- a quarter of the seeds use a new-anchor early reset.

| Assertion (plan) | Measured | Status |
|---|---|---|
| β=0: \|V̂−V\|/V ≤ 3% | 780 cycles: bias −0.003%, sd 0.155%, max 1.73% | met |
| β=0: CI covers V in ≥ 90% | 780/780 (100%); grades high 313, medium 467 | met, but see limitation 2 |
| Wrong lag moves V̂ < 2% | max 1.19% over all lags and cycles with ≥30 crossings | met |
| Lag recovered ≥ 80% (≥30 crossings) | time truth 62.6% (n=454); idx1 truth exact 44.4%, time-vs-index 94.5% (n=477) | **not met**, see limitation 1 |
| (same, meter weighted at API price) | time 95.7% (n=139); idx1 exact 60.7%, idx1-or-idx2 100% (n=145) | met up to idx1 ≡ idx2 |
| Concentrated β=0.2: unexplained ∈ [0.15, 0.30] | 154 cycles: 0.213 ± 0.011, all in band | met, using the poll detector |
| Concentrated β=0.2: `coverage_gap` flagged | 154/154 | met |
| Concentrated β=0.2: guarded V̂ within 5% | mean −1.57%, max 4.10% | met, using the poll detector |
| Concentrated β=0.2: V̂_all ≈ V(1−β) | mean −0.52%, max 3.61% | met |
| Diffuse β=0.2: undetectable, V̂ is a floor | 75 cycles: V̂/V 0.800 ± 0.001; unexplained 0.000 | met (V̂ ≤ 1.02·V) |
| Early reset detected within 3 readings, no segment spans it | 200/200 detected; boundary at the first post-reset reading in every seed (0 readings missed); no spanning segment in any lag | met |
| Level shift flagged, each cycle within 5% | 200/200 flagged; false positives on 8/800 unshifted finals | met |
| Poll-only within 6% (≥30 polls, peak ≥50%) | 67 cycles: bias +0.41%, sd 1.03%, max 3.35% | met |
| Monotone crossings under concurrency | 62,933 crossings, 0 violations, 14,885 raw readings out of order | met |
| Determinism | byte-identical JSON for the same seed | met |
| Double ingest → identical tables | replay plus recompute leaves the tables byte-identical | met (`run_test.go`) |
| Reprice scales V̂ consistently | ×1.1 $ → ×1.1 V̂ and CI, same grade | met (unit test) |
| Identified weight factors ≥90%, false-unlock ≤10% | — | Phase 4 (learner) |

Early-reset detection: "within 3 readings" is read as "the detected boundary (`closed_at`) lies within the first 3 successful header readings after the true reset". In every seed it is the first. Confirming the reset still needs ≥3 readings spanning ≥60 s. Until then, the low readings sit in a pending state.

### Bypass sensitivity

40 seeds per row; "within 5%" is the share of cycles whose guarded V̂ is within 5%.

| Scenario | Unexplained | In band* | Guarded mean / max | Within 5% | `coverage_gap` |
|---|---|---|---|---|---|
| β 0.2, 3 bursts/wk, polls | 0.213 ± 0.011 | 1.00 | −1.57% / 4.10% | 1.00 | 1.00 |
| β 0.2, 3 bursts/wk, no polls (plan's ratio guard only) | 0.195 ± 0.040 | 0.88 | −3.12% / 22.2% | 0.82 | 0 |
| β 0.2, 6 bursts/wk, polls | 0.222 ± 0.016 | 1.00 | −2.91% / 6.18% | 0.95 | 1.00 |
| β 0.2, 6 bursts/wk, no polls | 0.105 ± 0.046 | 0.16 | −11.95% / 21.0% | 0.06 | 0 |
| β 0.05, 3 bursts/wk, polls | 0.060 ± 0.011 | 0.92 | −1.24% / 2.91% | 1.00 | 1.00 |
| β 0.05, 3 bursts/wk, no polls | 0.017 ± 0.013 | 0.06 | −3.61% / 5.57% | 0.78 | 0 |

\*In band means unexplained ∈ [0.75β, 1.5β].

The ratio guard reliably catches bursts of about 6 ticks or more. Smaller bursts share a segment with up to one tick of legitimate spend and slip past φ = 0.2. Without a poller (Phase 3), expect V̂ to understate by roughly the undetected share.

### 5h meter lag recovery

This was measured during development over 60 seeds and all 5h cycles, with default multipliers. Events there are about 20% of a tick, so the lag is far more visible than on 7d:

- time truth: 85% exact;
- idx1 truth: 54% exact and 99% time-vs-index.

## Known limitations and open items

1. **Lag identifiability on the weekly meter (plan assertion not met).** On 7d a typical request is 2–4% of a tick. Moving one event across a segment boundary changes a segment's $ by about that much. The per-segment mismatch between API-price weighting and the meter's hidden weighting (cache reads at 0.5×, Fable at 1.2×, daily mix drift) is 4–9% of a tick. So even with the true V, the median errors of the four lags differ by less than their noise.
   - With the multipliers set to 1, time-lag recovery rises to 96%. The weight learner (Phase 4), which removes that mix noise, should therefore lift recovery; the plan already cross-checks the lag against the learner.
   - Separately, idx1 and idx2 are observationally equivalent under the plan's score. Under floor rounding, the threshold is crossed *inside* the pushing event, so assigning that event to either side gives errors with the same distribution (overshoot versus undershoot).
   - The lag therefore only matters, and is only identifiable, to ±1 index, and it moves V̂ by less than 2% (measured max 1.19%).
   - The corpus test asserts the plan's 80% at API weights and holds regression floors at the measured level for default weights.
2. **The CI is conservative.** Coverage is 100% because the CI half-width (about 2–3%) is roughly 10× the realized error (sd 0.16%). SE_b measures how block ratios disperse with the mix, but the target V_eff is mix-specific for the same events, so that dispersion never becomes error. The CI is honest for "what would 100% be worth at a different mix". A tighter, calibrated interval would need the learner's mix model.
3. **CUSUM `regime_split` fires on 60% of unshifted weekly cycles** (468/780); with API-weighted meters, 8%. It is detecting real, mix-driven V_eff swings between days. It stays informational until Phase 4 can separate a mix change from a limit change.
4. **`restart_gap` is plumbed but not wired.** `AnalyzeOptions.RestartGaps` exists, but nothing derives it yet from the `last_event_at` / `plugin_started_at` settings.
5. **Weekly `v_blend` is only set on running estimates.** Finals store 0.
6. **4-dp cycles.** The 5h cycles that fall entirely in the 4-dp era run at tick 1e-4, which yields thousands of one-event segments. V̂ stays correct and the lag becomes very identifiable, but block counts and grades behave differently from whole-percent cycles. No assertion covers this.
