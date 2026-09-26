# cpa-subscription-value

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that measures what a Claude Max or ChatGPT/Codex subscription is worth in API-equivalent dollars, continuously, with one estimator and one dashboard for both providers.

Every request CPA proxies comes back with the provider's quota utilization for that account in the response headers (`anthropic-ratelimit-unified-7d-utilization`, `x-codex-primary-used-percent`). The plugin pairs each request's API list-price cost with the meter's movement and estimates **"$ per 100% of the weekly allowance"** per reset cycle with a confidence interval, plus per-model weights (how much each model and token type moves the meter relative to its API price) and burn rate / forecast.

Design: [`docs/plan.md`](docs/plan.md). Estimator as implemented, with measured results on the synthetic corpus: [`docs/design.md`](docs/design.md). Research behind it: [`docs/research.md`](docs/research.md).

## How the number is made

1. **Record.** Each upstream attempt becomes a `usage_event` with mutually exclusive token buckets (uncached input, cache read, cache write, output) priced at API list rates and frozen with a price-snapshot hash, plus one `meter_reading` per quota meter found in its response headers (`5h`, `7d`, Claude's Fable-scoped `7d_oi`, Codex additional limits). A low-rate poll of `oauth/usage` / `wham/usage` fills gaps and captures per-model weekly limits.
2. **Cycles and crossings.** Readings are grouped into reset cycles by their `reset_at` anchor (early resets and provider regime flips are detected). Within a cycle, the first reading to show each new whole-percent level is a *crossing*; the API-$ spent between crossings is attributed under four lag candidates (time-registered, or index lags 0/1/2), and the best lag is picked by a prequential score.
3. **Estimate.** `V̂ = Σ$ / ΣΔu` over eligible spans: the ratio of sums, whose quantization error telescopes to one tick per run. The 95% CI combines that with a moving-block bootstrap over segments, in log space. Ticks whose spend is implausibly small are flagged as *unexplained* (usage that bypassed the proxy) and excluded; `V̂_all` is reported as a floor. Grades: high / medium / low / insufficient.
4. **Weights.** A ported multiplicative learner (MIT, from cpa-quota-estimator) fits per-model and per-token-type factors relative to API price with identifiability gates, so "cache reads count 0.4× their API price" is stated only when the traffic mix supports it.
5. **Forecast.** Remaining $, recent pace, projected exhaustion and on-track / ahead / under status from the latest reading and V̂.

## Limits you should know

- **Unrouted usage lowers the number.** claude.ai web/desktop and Codex cloud tasks draw on the same allowance without passing through the proxy. Bursts are detected and excluded; steady background use is invisible and makes V̂ a floor. Set an account's coverage mode to `mixed` to hide capacity figures.
- **Feature-scoped meters are not estimated.** `7d_oi`, `weekly_scoped:*` and Codex additional limits meter a subset of traffic; they are stored and charted, not regressed.
- **The provider's internal weighting is undisclosed.** "$ per 100%" is mix-specific by construction; it is reported with the mix, and the learner's factors say how far the meter departs from API pricing.
- **Whole-percent readings.** Precision is derived per reading from the raw header string; a weekly cycle needs a few dozen ticks before the CI is tight.
- **Claude cache writes** are priced at the 5-minute rate by default; the 1-hour rate is shown as a band (`v_hat_cw1h`) because CPA does not report the TTL.

## Install

1. Build for the CPA host: `make build-linux-amd64` (Docker), or download a release zip (`cpa-subscription-value_<ver>_<goos>_<goarch>.zip` + `checksums.txt`).
2. Copy `cpa-subscription-value.so` to `<plugins dir>/linux/amd64/cpa-subscription-value-v<version>.so`.
3. Mount a data directory and enable the plugin in `config.yaml`:

```yaml
plugins:
  enabled: true
  configs:
    cpa-subscription-value:
      enabled: true
      data_dir: /CLIProxyAPI/data/cpa-subscription-value   # mount this in Docker
      poll_interval_minutes: 20          # 0 disables polling; endpoints are rate-limited
      claude_cache_write_multiplier: 1.25
      retention_days: 365
      price_sync_hours: 24
      log_level: info
```

4. Restart CPA. The dashboard appears in the management center's plugin menu as **Subscription Value**.

The plugin answers `schema_version` 2, so it loads on CPA ≥ v7.2.x. On hosts older than v7.2.142 a reused Codex websocket carries no quota headers; the poll fallback covers that with a coarser, poll-only estimate.

## Management API

All routes live under `/v0/management/cpa-subscription-value/` and require the management key.

| Route | Purpose |
|---|---|
| `GET /health` | version, schema, ingest queue/dropped, store stats, poll status, price snapshot, last recompute per meter |
| `GET /accounts` | accounts with coverage mode and meters seen |
| `GET /summary[?auth_index=]` | per meter: current cycle, used %, latest estimate (V̂, CI, grade, lag, unexplained share), live forecast, $ spent, mix |
| `GET /cycles?auth_index=&meter=` | week-over-week series of finals with CIs and flags |
| `GET /series?auth_index=&meter=&cycle_id=` | readings, crossings, cumulative $, estimate history, fitted slope |
| `GET /weights?auth_index=&meter=` | learner factors, per-model implied $ per 100% |
| `GET /prices`, `POST /prices/sync`, `POST /prices/reprice` | price table, models.dev sync, batched reprice |
| `GET/POST /settings` | coverage mode per account, display preferences |
| `POST /recompute`, `POST /poll` | run the estimator / one poll round now |
| `GET /export?auth_index=&cycle_id=` | CSV of events with the readings taken on them |
| `GET /events`, `GET /readings` | raw rows (debugging) |

## Develop

```bash
make test        # go vet + go test ./...
make build       # native c-shared build into dist/
```

`cmd/plugin` is the only cgo file. Everything else is plain Go and testable without a host; `internal/sim` generates a deterministic two-provider corpus with hidden truth for the estimator tests.

## Licensing

MIT. The per-model weight learner is ported from [Autsunset/cpa-quota-estimator](https://github.com/Autsunset/cpa-quota-estimator) (MIT); see `THIRD_PARTY_NOTICES.md`.
