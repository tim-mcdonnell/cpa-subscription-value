# cpa-subscription-value — a CLIProxyAPI plugin that measures what your Claude Max and ChatGPT/Codex subscriptions are worth

## Context

Tim pays $200/mo for Claude Max and also holds a ChatGPT/Codex subscription. All agent traffic flows through one CLIProxyAPI (CPA) instance in the homelab (`homelab-compose/stacks/cliproxyapi`, image `eceasy/cli-proxy-api:v7.2.122`, plugins dir mounted at `/CLIProxyAPI/plugins`). He wants to know, continuously and not only at the end of a usage period, how much API-equivalent dollar value each subscription yields: "100% of this week's allowance ≈ $X", tracked week over week, plus per-model weights (how Opus vs Sonnet vs Fable, cache reads, output move the meter relative to API list price) and burn rate / forecast.

Research (`research/subscription-value-measurement.md` in this session's workspace) established:
- CPA already hands plugins everything needed per request: token counts and the **full upstream response headers**, which carry the provider's quota utilization for that account (`anthropic-ratelimit-unified-{5h,7d,7d_oi}-utilization` as a 0–1 fraction; `x-codex-{primary,secondary}-used-percent` as 0–100). Pairing each request's API-$ cost with the meter's movement, then taking a ratio of sums between integer-% crossings, gives "$ per 100%" with a well-understood quantization error.
- The idea exists twice already, unevenly: `Autsunset/cpa-quota-estimator` (MIT, Codex-only CPA plugin, sophisticated but its cost formula misprices Claude tokens) and Tim's own never-deployed Keeper fork (`~/projects/cpa-usage-keeper`, `internal/quota/estimate`, both providers, OLS + bootstrap). Neither gives one consistent measurement across both providers.
- **Decisions taken with Tim:** build a standalone CPA plugin named **`cpa-subscription-value`**; write a **fresh estimator** (borrow ideas, not code); **port the MIT weight learner** from cpa-quota-estimator for per-model weights (with attribution); one account per provider; near-zero unrouted usage; headline = weekly $ value, plus weights and burn/forecast; 5h window recorded but secondary. Keeper stays as-is for general usage dashboards.

## Hard constraints discovered (verified in source)

| Constraint | Consequence |
|---|---|
| Plugin = C-ABI shared library (`cliproxy_plugin_init`, JSON envelopes), built `CGO_ENABLED=1 go build -buildmode=c-shared` | Go, cgo, one `.so` per GOOS/GOARCH; copy the export shim from `CLIProxyAPI/examples/plugin/usage/go/main.go` |
| Loader rejects `plugin.register` responses with `schema_version` > host's. v7.2.122 host = **2**, HEAD = 6 | Answer `min(host_schema_version, 2)`; hand-roll the JSON (no CPA module dependency, like cpa-prometheus) |
| `HandleUsage` is invoked synchronously over the ABI, serially with every other usage sink, no timeout | Push to a buffered channel and return; one writer goroutine does SQLite |
| `UsageRecord` JSON keys are Go field names; `Detail` has only `InputTokens, OutputTokens, ReasoningTokens, CachedTokens, CacheReadTokens, CacheCreationTokens, TotalTokens`; **no** canonical `TokenBreakdown` | Normalize per provider (see §Normalization). Claude: `InputTokens` excludes cache; `CachedTokens` mirrors `CacheReadTokens`, falling back to `CacheCreationTokens` when reads are 0 (`usage_helpers.go:1170-1176`). Codex: `InputTokens` includes cache; `CachedTokens == CacheReadTokens == cached_tokens` (`usage_helpers.go:1036-1042`); reasoning ⊂ output |
| On v7.2.122 a **reused Codex websocket delivers no `x-codex-*` headers**; the merge of `codex.rate_limits` events into headers arrived in v7.2.142 | Recommend upgrading CPA to current v7.3.x (§Phase 0). Design keeps a **poll fallback** (`wham/usage`, `oauth/usage`) so it degrades, never breaks |
| `QuotaProvider` capability only exists ≥ v7.2.159 | Don't depend on it; poll via `host.auth.get` (returns the auth-file JSON incl. `access_token`) + `host.http.do`, read-only — never refresh tokens ourselves (refresh-token rotation would get the credential disabled) |
| UI = unauthenticated same-origin iframe of a GET resource route `/v0/resource/plugins/<id>/<path>` with a non-empty `Menu`; no wildcards, so one self-contained HTML file | Single `//go:embed web/dashboard.html`; page reads the management key from CPAMC's `cli-proxy-auth` localStorage entry (XOR `enc::v1::` scheme, key `'cli-proxy-api-webui::secure-storage|'+host+'|'+UA`) and falls back to a prompt; inline SVG charts, no CDN |
| No host data-dir convention; Docker mounts only `plugins`, auths, `static`, `logs` | Config `data_dir`, default `/CLIProxyAPI/data/cpa-subscription-value`; add a compose volume for it |
| Account identity: `AuthIndex` = sha256(type:abs path)[:8] — stable unless the auth file is renamed/moved | Key everything by `AuthIndex`; store `AuthID` (file name) and email (from `host.auth.list`) for relinking |
| Management routes: exact paths under `/v0/management/`, management-key auth, no `:`/`*`; avoid `/plugins/<id>/{config,quota}` | Routes under `/v0/management/cpa-subscription-value/...` |
| Lifecycle: `plugin.register` → `plugin.reconfigure` on config change → `plugin.quiesce` (≥7.2.142) → C `shutdown` | Idempotent register/reconfigure; flush + WAL checkpoint on quiesce/shutdown |

## Repository layout (`~/projects/cpa-subscription-value`, new git repo, Go 1.27)

```
cmd/plugin/            main.go (cgo exports, envelope), only file with import "C"
internal/abi/          host call helper (host.log, host.http.do, host.auth.list/get), envelopes, schema negotiation
internal/config/       YAML config (data_dir, poll_interval, pricing assumptions, retention, weight learner knobs)
internal/ingest/       UsageRecord → normalized UsageEvent + []MeterReading; provider adapters claude.go, codex.go
internal/store/        SQLite (modernc.org/sqlite, WAL), migrations, queries
internal/meter/        meter keys, cycle assignment, crossing detection
internal/estimate/     primary estimator (ratio of sums), CI, lag selection, coverage guard, burn/forecast
internal/weights/      ported weight learner (MIT; THIRD_PARTY_NOTICES.md), provider parameterization
internal/pricing/      models.dev sync (anthropic + openai), overrides, snapshot hash, frozen $ per event, reprice job
internal/poll/         oauth/usage + wham/usage pollers → MeterReading(source=poll)
internal/api/          management routes (JSON) + resource route serving the dashboard
internal/sim/          synthetic corpus generator used by tests (hidden weights, rounding, lag, concurrency, bypass)
web/dashboard.html     single-file UI (vanilla JS + inline SVG), embedded
Makefile, Dockerfile.build (golang:1.27-bookworm → glibc-compatible .so), .github/workflows/release.yml
docs/design.md         the estimator design (from §Estimator below), kept current
```

Reference implementations to copy patterns from (all in the research scratchpad clones, cite when porting):
- ABI shim + Makefile: `Autsunset_cpa-quota-estimator/main.go`, `Makefile`, `.github/workflows/release.yml`
- Register/management registration shape: `Autsunset_cpa-quota-estimator/app.go:60-123`
- Host auth + upstream fetch for polling: `giovannirco_cpa-prometheus-plugin/internal/quota/fetch.go:153,203-224`
- CPAMC key decoding: `Autsunset_cpa-quota-estimator/web/dashboard.html:150-200`
- Weight learner to port: `Autsunset_cpa-quota-estimator/{weight_learner,weight_identifiability,weight_cycle_scales,weight_backtest}.go`
- Claude header list/format: `router-for-me_CLIProxyAPI/sdk/cliproxy/auth/quota_signals_test.go:70-83`, `internal/runtime/executor/helps/claude_ratelimit.go:259-276` (reset parsing)

## Normalization (provider adapters)

Output of every adapter is the same struct: `UsageEvent{ingest_id, auth_index, auth_id, provider, model, alias, service_tier, reasoning_effort, requested_at, observed_at = requested_at + TTFT (or Latency), failed, status_code, uncached_input, cache_read, cache_write, output, reasoning, api_usd, price_hash}` plus `[]MeterReading`.

- **Claude** (`Provider` contains `claude`/`anthropic`): `uncached_input = InputTokens`, `cache_read = CacheReadTokens`, `cache_write = CacheCreationTokens`, `output = OutputTokens` (reasoning ⊂ output, reported separately as `ReasoningTokens`). Ignore `CachedTokens`. Headers → readings for each claim `c ∈ {5h, 7d, 7d_oi, …}` present as `anthropic-ratelimit-unified-<c>-utilization`: `used_fraction = parseFloat(raw)`, `raw` kept, `reset_at = <c>-reset` (unix s; also accept RFC3339/HTTP-date), `status = <c>-status`, `window_seconds` from claim (`5h`→18000, `7d*`→604800). Also store `representative-claim`, `overage-status`.
- **Codex** (`codex`/`openai`): `cache_read = max(CacheReadTokens, CachedTokens)`, `cache_write = CacheCreationTokens` (normally 0), `uncached_input = max(InputTokens − cache_read − cache_write, 0)`, `output = OutputTokens`, `reasoning = ReasoningTokens` (⊂ output). Headers: for each of `primary`, `secondary`, and `additional-<name>`: `used_fraction = used-percent/100`, `window_seconds = window-minutes·60`, `reset_at = reset-at`. **Meter key is derived from window length, not role**: 18000→`5h`, 604800→`7d`, otherwise `additional:<name>` or `win:<seconds>`; `plan_type` stored on the account.
- Shared meter-key vocabulary: `5h`, `7d`, `7d_oi` (Claude Fable-scoped, utilization may exceed 1.0), `weekly_scoped:<model>` (from `oauth/usage.limits[]`), `additional:<name>` (Codex). **Estimable** (metered by all of the account's proxied traffic): `5h`, `7d` for both providers. Everything else is stored and charted, never estimated (feature-scoped meters would give confident-looking garbage).
- Failed attempts (`Failed=true`) are stored with `status_code` and excluded from crossing detection; their $ is tracked separately so the estimator can report sensitivity.

## Storage (SQLite, `internal/store`)

Stored = source of truth; derived = recomputed from scratch for open cycles, frozen when a cycle closes.

| Table | Key columns |
|---|---|
| `accounts` (stored) | `id, provider, auth_index, auth_id, email, plan_type, first_seen, last_seen`; UNIQUE(provider, auth_index) |
| `usage_events` (stored) | `id, account_id, dedup_key` (sha1 of AuthID+RequestedAt+Model+token tuple; UNIQUE, makes replays no-ops), `requested_at_ns, observed_at_ns, latency_ns, model, model_family, alias, service_tier, reasoning_effort, failed, status_code, uncached_input, cache_read, cache_write, output, reasoning, is_fast, is_long, api_usd, api_usd_cw1h, price_hash, headers_json, flags`; index (account_id, observed_at_ns) |
| `meter_readings` (stored) | `id, account_id, meter_key, source (header/poll), event_id, observed_at_ns, used_fraction, raw, precision_dp, reset_at, status, window_s, cycle_id`; index (account_id, meter_key, observed_at_ns) |
| `cycles` (derived, stable ids) | `id, account_id, meter_key, window_s, reset_anchor, starts_at, first_reading_at, closed_at, end_reason (reset/early_reset/regime/expired), regime_seq, tick, peak_fraction, flags_json`; UNIQUE(account_id, meter_key, reset_anchor, regime_seq) |
| `segments` (derived) | `id, cycle_id, lag (0/1/2/time), seq, from_level, to_level, delta_ticks, from_reading_id, to_reading_id, t_start_ns, t_end_ns, usd, usd_cw1h, features_json ({family:{type:tokens}}, fast/long), n_events, n_failed, flags`; UNIQUE(cycle_id, lag, seq). Crossings are the `to_*` endpoints |
| `estimates` (stored history) | `id, cycle_id, computed_at, kind (running/final/reprice), method (header/poll), lag, v_hat, v_hat_cw1h, ci_lo, ci_hi, se_quant, se_boot, ticks_used, ticks_excluded, runs, unexplained_frac, grade, mix_json, v_blend, flags_json, price_hash`; one `final` per (cycle, price_hash) |
| `weight_fits` (stored history) | `id, account_id, meter_key, computed_at, lag, anchor_family, factors_json, scales_json, loss, backtest_json, price_hash` |
| `prices` / `price_snapshots` (stored) | `snapshot_hash, provider, model_family, input, output, cache_read, cache_write_5m, cache_write_1h, long_ctx_mult, fast_mult, source`; snapshots keep `raw_json` |
| `settings` (stored) | `key, value_json` — incl. `last_event_at`, `plugin_started_at`, dropped counter, per-account coverage mode, meter scope regexes, tolerances, anchor overrides |

Additions to the normalization above: `precision_dp` is derived from the raw header string per reading (never hardcode 2 dp — it was a full float in Dec 2025); a cycle's `tick = 10^-min(precision_dp)` over its header readings (floor 1e-4), mixed precision → flag `precision_change`. `is_long = context > 200k`, `is_fast` from `ServiceTier`. `model_family` = model id with version/date suffix stripped by a per-provider regex table. Claude `reasoning` is informational only (thinking is inside `output_tokens`), never priced.

## Estimator (`internal/meter`, `internal/estimate`) — fresh design

Notation: `u` = utilization fraction; `tick` = quantum (0.01 for whole percent); `V` = API-list $ that 100% of a meter is worth; `$` = frozen API-$ of an event.

**Cycle assignment** (per account × meter, readings in `observed_at` order, `tol = 120 s`): same anchor within `tol` → assign, update peak `P`. `reset_at > R+tol` → close (`reset`; `early_reset` if `now < R−tol`), open new cycle. `reset_at < R−tol` (stale concurrent stream) → assign to the existing cycle with that anchor if its anchor is still in the future (A→B→A reverts: both flagged `regime_change`, grade capped `low`), else drop as `reset_ambiguous`. Same anchor but `u ≤ 0.05` and `u ≤ P−0.01` → pending; confirm early reset on ≥3 successful non-decreasing header readings spanning ≥60 s, all `≤ P−0.01` → close (`early_reset`), reopen with `regime_seq+1`; otherwise flush as `quota_drop`. Cycles with `now > R+tol` and no newer reading close lazily as `expired`. Poll readings give anchor evidence only; never crossings, never early-reset confirmation.

**Crossings** (header readings only, successful events, not `quota_drop`): `level = floor(u/tick + 1e-9)`; running max `M`; a reading with `level > M` is crossing `k` with `Δ_k = L_k − L_{k−1}` (may exceed 1). Equal/lower later readings are stale and ignored.

**Lag attribution** — segment k's spend under candidate lag: index lags `L ∈ {0,1,2}` sum events with index `i ∈ (i_{k−1}−L, i_k−L]` (events sorted by `observed_at`); candidate `time` registers input-side $ at `requested_at` and output $ at `requested_at+latency` and sums registrations in `(obs_{k−1}, obs_k]` — expected winner under concurrency. Lag is chosen per (account, meter) by a prequential score: walking crossings with `V̂_{k−1}` from earlier segments, `S(L) = median_k |û_k − L_k·tick|`; `argmin` when ≥15 crossings, else previous cycle's lag, else `time`. Cross-checked with the learner's backtest → `lag_unstable` on disagreement. Codex lag may differ by transport (websocket `codex.rate_limits` events likely lag 0, HTTP lag 1) — record and display it.

**Primary estimate**: exclude segments flagged `quota_drop, regime_change, restart_gap, lag_incomplete, low_usd`; the rest form `R` contiguous runs. `D = Σ Δ_k·tick`, **`V̂ = Σ $_k / D`** (and `V̂_cw1h` for the 2× cache-write band).

**Variance**: overshoot at each run endpoint ~U(0, tick), interior endpoints telescope → `Var_q(D) = R·tick²/6`, `SE_q/V̂ = tick·√(R/6)/D` (one run over 60 ticks at 0.01: 0.68%). Mix/timing dispersion: merge consecutive segments into blocks of ≥5 ticks or ≥1 h, moving-block bootstrap (block length `⌈B^{1/3}⌉`, 1000 resamples, fixed seed derived from the series identity) of `V* = Σ$*/ΣD*` → `SE_b`. `CI95 = V̂·exp(±1.96·√((SE_q/V̂)² + (SE_b/V̂)²))`; report both SEs separately (measurement vs mix).

**Coverage guard**: iterate ≤3×: flag `low_usd` on segments with `$_k < 0.2·V̂·Δ_k·tick`, recompute; `unexplained_frac = ΣΔ_flagged/ΣΔ_all`; also report `V̂_all` (nothing excluded) as the floor — unrouted usage always biases downward. Polls add a second detector: a poll rise with zero proxied $ in between → `coverage_gap` with its $ equivalent.

**Grade**: `insufficient` < 5 ticks or < 3 crossings; `low` 5–14 ticks, or rel. CI width > 30%, or unexplained > 0.30, or `regime_change`; `medium` ≥ 15 ticks, width ≤ 30%, unexplained ≤ 0.15; `high` ≥ 30 ticks, width ≤ 15%, unexplained ≤ 0.05, no regime/precision flags, lag stable.

**Running vs final**: recompute the open cycle after each crossing (debounced 30 s) as `running`; `v_blend` = precision-weighted log-space blend with the previous `final` (prior variance inflated by σ_rw = 0.35 per cycle), shown as "blended", never replacing `v_hat`. On close write `final` frozen with `price_hash`. Week-over-week = the `final` rows; flag `level_shift` when consecutive finals differ by > 3·√(SE₁²+SE₂²); within a cycle a CUSUM on log per-block ratios (h = 4·SE_block) splits sub-regimes (`regime_seq`). Reprice writes a new `final` under the new hash; old rows stay.

**Poll-only method** (old CPA websocket case): when header coverage < 50% of a cycle's events, segments run between consecutive polls, same ratio of sums, `Var_q = R·tick²/6`, grade capped `medium`. Never mix header and poll readings within one crossing sequence.

**Remaining $, burn, forecast** (per meter, headline 7d): `u_now` = latest reading any source; `P_time = (now−starts_at)/window_s`; `remaining_$ = (1−u_now)·V̂` (CI scaled); `spent_$ = u_now·V̂` vs Σ proxied $ (coverage check); recent pace `b_r` = Δu over trailing 6 h (≥3 ticks, else 24 h); `t_exhaust = now + (1−u_now)/b_r`; `û_end = min(1, u_now + b_r·(R−now))`; `$/day = b_r·V̂·86400`; projected unused $ = `(1−û_end)·V̂`. Status: `exhausted` (u ≥ 1 or rejecting status), `ahead` (u_now > P_time+0.10 and t_exhaust < R), `on_track` (|u_now−P_time| ≤ 0.10), `under`.

**Pitfall rules**: `7d_oi` scope = Fable-family events only, only Fable responses carry it, `long_gap` threshold 12 h for scoped meters, levels > 100 ticks allowed; `fable_feeds_7d: auto|yes|no` (auto includes Fable in 7d features and lets the learner's `f_fable` on 7d decide). `-overage-status` active → `overage_active`, overage ticks excluded (billed usage). Codex `additional:*` and `weekly_scoped:*` stored-only unless a scope regex is configured. Failures with tokens included and flagged, plus `v_hat_excl_failed` sensitivity. Restart: `last_event_at` vs `plugin_started_at` defines `restart_gap`; spanning segments excluded, a new run starts at the next crossing. Ingest: 4096-slot channel, overflow → dropped counter, drain on quiesce/shutdown.

**Synthetic corpus** (`internal/sim`, deterministic seeds): hidden `V_7d, V_5h`, per-(family,type) true multipliers of API price (e.g. cache_read 0.5, output 1.0, fable 1.2), lag mode (`time`/`idx1`), rounding (floor 2 dp; Codex integer %), Poisson sessions with night gaps, ≤4 concurrent streams with lognormal latency/TTFT, daily mix drift, 3% failures (half with partial tokens), polls every 20 min, unrouted β ∈ {0, 0.05, 0.2} concentrated-in-gaps or diffuse, one early reset, one 30% V shift at a cycle boundary, one precision change; emits `UsageRecord` JSON + headers in exact CPA form, 4 cycles reaching 60–90%. Assertions (200 seeds): β=0 → `|V̂_final−V|/V ≤ 3%` on 7d cycles with ≥40 ticks and CI covers V ≥ 90%; lag recovered ≥ 80% with ≥30 crossings and wrong lag moves V̂ < 2%; identified weight factors cover truth ≥ 90%, false-unlock ≤ 10%; concentrated β=0.2 → `unexplained_frac ∈ [0.15,0.30]`, `coverage_gap` flagged, guarded V̂ within 5%, `V̂_all ≈ V(1−β)`; diffuse bypass documented as undetectable (V̂ is a floor); early reset detected within 3 readings; level shift flagged with each cycle within 5%; poll-only within 6% given ≥30 polls over ≥50%; monotone crossings under concurrency; double ingest → byte-identical tables; reprice scales V̂ consistently.

## Weight learner (ported, MIT)

Port `weight_learner.go`, `weight_identifiability.go`, `weight_cycle_scales.go`, `weight_backtest.go` from cpa-quota-estimator into `internal/weights`, keeping the math (Huber δ=0.6, Levenberg–Marquardt, priors N(0, 0.5), random walk σ=0.35 on the per-cycle scale, factor unlock only when within-cycle $-share SD ≥ 0.05 and conditional Fisher information passes, 21-day decay, prequential backtest per lag) and replacing every Codex constant with `ProviderParams`. Model:

`Δu_k ≈ exp(s_c) · Σ_{m,t} tok_{k,m,t} · refRate_{m,t} · exp(f_m) · exp(g_t) · exp(h_fast·fast_k) · exp(h_long·long_k)`, with `refRate` = API list price per token from `prices` (cache write at 5 m), `f_anchor = 0`, `g_uncached_input = 0`; `exp(−s_c)` is the learner's own V for cycle c, cross-checked with the primary V̂ (`learner_divergence` if outside 2·SE).

Inputs: chosen-lag eligible segments (plus `low_usd` ones down-weighted by `BoundaryWeight = 1/(1+2·boundaryTokens/totalTokens)` — the only place that weight is used), features collapsed to (model_family, type ∈ {uncached_input, cache_read, cache_write, output}) plus fast/long indicators; `unknown_model` segments excluded. Per-provider defaults: Claude anchor = highest-$ family in the last 21 days (expected `claude-opus`), type priors N(0, 0.5) for cache_read and cache_write (a fitted cache_write factor ≈ 1.6 indicates 1 h TTL), `h_fast` N(0, 0.5), `h_long` N(0, 0.3) with refRate already doubled; Codex anchor = highest-$ `gpt-*-codex` family, types {uncached_input, cache_read, output}, `h_fast` N(0, 0.5) with refRate already 2.5×, no cache_write. Report per factor: `exp(est)` as "counts × its API price" with CI from the LM Hessian, `identified | prior_locked`, segments contributing, share range; per-model allowance `V_m = exp(−s_c − f_m)` with delta-method CI. Fallback when nothing is identified: all factors 1.0, only the mix-specific V̂ shown, per-model table captioned "assumes API-price weighting". Attribution: `THIRD_PARTY_NOTICES.md` with the MIT text and file provenance (commit `f793608`).

## Pricing

- Source: `https://models.dev/api.json`, providers `anthropic` and `openai`, synced daily; hard override table for models not listed (Opus 5.5 $4/$20, Sonnet 5 $2/$10, Fable 5.1 $10/$50 per MTok per current pricing docs — verify at build time via the `claude-api` skill reference, do not trust memory).
- Claude cache write TTL is unknown from CPA: config `claude_cache_write_multiplier` default 1.25 (5-minute); the API also reports `api_usd_hi` at 2.0× so the UI can show a band.
- Codex: API USD basis by default; a `codex_credits_card` override table is optional (v2).
- Every event stores `api_usd` frozen at ingest with `price_hash` (canonical hash of the compiled price table, same idea as the fork's `ContentHash`). A `POST /prices/reprice` job recomputes `api_usd` in 500-row batches when prices change; estimates carry `pricing_changed` when a cycle spans two hashes.

## Management API (all JSON, under `/v0/management/cpa-subscription-value`)

| Route | Returns |
|---|---|
| `GET /health` | plugin version, host schema, CPA version seen, ingest lag, dropped events, last poll per account, DB size |
| `GET /accounts` | accounts (auth_index, provider, email, plan_type, coverage mode, meters seen) |
| `GET /summary?auth_index` | current cycle per estimable meter: used %, V̂ with CI, confidence, $ spent, $ remaining, burn/forecast, unexplained-tick share, mix (share of $ by model / token type) |
| `GET /cycles?auth_index&meter` | per-cycle history: start, reset_at, final/running V̂ + CI, confidence, $ spent, tokens, mix, flags — the week-over-week series |
| `GET /series?auth_index&meter&cycle_id` | within-cycle timeline: readings, crossings, cumulative $, running V̂ |
| `GET /weights?auth_index` | learner output per provider |
| `GET /events?auth_index&from&to&limit` | recent normalized events (debug) |
| `GET /prices`, `POST /prices/sync`, `POST /prices/reprice` | pricing |
| `GET/POST /settings` | coverage mode per account, poll interval, multipliers, retention |
| `GET /export?auth_index&cycle_id` | CSV of events+readings for offline analysis |

Resource: `GET /v0/resource/plugins/cpa-subscription-value/dashboard`, `Menu: "Subscription Value"`.

## Dashboard (single HTML, same layout for both providers)

1. **Header cards, one per account, identical fields**: provider/email/plan; current weekly used %; **V̂ this cycle** ($ per 100%, with CI and confidence badge); $ spent so far; $ remaining; projected exhaustion / status (on track / fast / slow); unexplained-tick share.
2. **Week-over-week chart**: V̂ per cycle with CI bars, both providers on the same axis (toggle), annotated with mix shares and flags (regime change, pricing changed, low coverage).
3. **Current cycle pace chart**: used % vs time with the sustainable line and the two projections; second axis cumulative API-$.
4. **$ vs %** scatter/line: cumulative $ against cumulative %, crossings marked, fitted slope = V̂/100.
5. **Per-model weights table**: factor vs API price with CI, identified/locked, data share; per provider.
6. **Mix panel**: $ share by model and token type this cycle vs last.
7. **Settings**: coverage mode, cache-write multiplier, poll interval, price sync/reprice, export.

Chart primitives: hand-written SVG helpers (line, bars, band, scatter) in one `<script>`; dark/light via `prefers-color-scheme` and CPAMC's theme attribute if present. Auto-refresh every 60 s while visible.

## Phases

**Phase 0 — Environment (in `homelab-compose`, one PR)**
- Bump `stacks/cliproxyapi/compose.yaml` image to the current v7.3.x tag (fixes Codex websocket headers; brings `plugin.quiesce`). Add volume `${APPDATA_ROOT}/cliproxyapi/data:/CLIProxyAPI/data`.
- `config.yaml` (appdata, not committed): `plugins.enabled: true`, `plugins.configs.cpa-subscription-value: {enabled: true, data_dir: /CLIProxyAPI/data/cpa-subscription-value}`. Document in `config.yaml.example` and the stack README. Confirm Docker VM arch with `uname -m` (expected `x86_64` → build `linux/amd64`).

**Phase 1 — Skeleton that records truth**
- Create the repo; copy `research/subscription-value-measurement.md` from this session's scratch workspace into `docs/research.md` (the workspace is deleted with the session).
- ABI shim, schema negotiation, config parsing, SQLite store + migrations, `HandleUsage` → channel → writer, provider adapters, raw header persistence, `/health`, `/events`, `/accounts`; `management.register`; dashboard stub showing raw readings.
- Build in Docker, drop into `${APPDATA_ROOT}/cliproxyapi/plugins/linux/amd64/cpa-subscription-value-v0.1.0.so`, verify in CPA logs and `GET /v0/management/plugins`.
- **Run for 2–3 days** to settle the open empirical questions: header precision (raw strings), lag (does response N's header include request N's input/output?), whether Fable tokens move `7d` as well as `7d_oi`, Codex header availability on websocket. Record answers in `docs/design.md`.

**Phase 2 — Meters, cycles, primary estimator, burn/forecast**
- `internal/meter` (cycle assignment, crossings, lag variants), `internal/estimate` (V̂, CI, confidence, coverage guard, running vs final), `/summary`, `/cycles`, `/series`; dashboard panels 1–4. Golden tests on the synthetic corpus.

**Phase 3 — Pricing hygiene + poll fallback**
- models.dev sync, overrides, price hash, frozen $, reprice job; pollers for `oauth/usage` (captures `limits[]` weekly_scoped) and `wham/usage` at `poll_interval` (default 20 min) with 429 backoff; poll readings enter the same tables with `source=poll` and are used per the estimator rules.

**Phase 4 — Weight learner**
- Port + parameterize; hourly refit; `/weights`; dashboard panel 5–6; synthetic tests asserting recovery of hidden weights within CI.

**Phase 5 — Polish**
- Retention job, CSV export, `THIRD_PARTY_NOTICES.md`, release workflow (`<id>_<ver>_<goos>_<goarch>.zip` + `checksums.txt`, matching the store's expected asset names so it can be added to a custom `store-sources` registry later), README with the methodology and its limits (unrouted usage, feature-scoped meters, undisclosed internal weighting).

## Verification

- **Unit/golden**: `go test ./...` — adapters (table tests against captured real `UsageRecord` JSON from Phase 1 for both providers, incl. the Claude `CachedTokens` quirk and failed attempts); meter/cycle assignment against scripted reset sequences (scheduled, early reset, regime revert); estimator against `internal/sim` corpora (assertions from §Estimator: V recovered within tolerance, lag identified, bypass flagged, CI coverage ≈ 95% over many seeds); weights recovery.
- **ABI smoke**: a Go test that `dlopen`s the built `.so` via a tiny cgo harness (or drives `internal/api` directly) and replays a recorded `plugin.register` → `usage.handle` → `management.*` sequence; asserts `schema_version` negotiation for host values 2 and 6.
- **Integration on the homelab**: after each phase, build with `Dockerfile.build`, copy to the plugins dir, `docker compose restart cliproxyapi`, check `docker logs` for `plugin loaded`, hit `GET /v0/management/cpa-subscription-value/health` with the management key, open CPAMC → Plugins → Subscription Value, and cross-check a cycle's cumulative $ against Keeper's window cost for the same interval (they price the same events, so they should agree to within the cache-write assumption).
- **Sanity against reality**: after one full weekly cycle on each provider, compare V̂ to the ad-hoc single-point estimate Keeper already shows (`quotaWindowUsageEstimate`) and to community anecdotes ($600–1,500/mo for Max 20x); the estimate should sit inside the CI and the CI should narrow as the cycle progresses.
