# Measuring the API-equivalent value of Claude Max and ChatGPT/Codex subscriptions through CLIProxyAPI

Research date: 2026-09-26. Repos were read at HEAD on that date; permalinks pin the commit.
Labels: **[V]** means I verified it in source code or an official doc. **[I]** means inferred. **[C]** means a community claim I did not reproduce.

Commit refs used below:
- CLIProxyAPI (CPA) `ed980be` (v7.3.18)
- Cli-Proxy-API-Management-Center (CPAMC) `4530da2`
- cpa-quota-estimator `f793608`
- claude-ratelimit-proxy `28ab496`
- ccusage `25ca71b`
- Claude-Code-Usage-Monitor `c59a83b`
- CodexBar `9c4bd43`
- ccstatusline `35440e4`
- openai/codex `a6bd192`

---

## TL;DR

- **Your regression idea already exists, twice.**
  - For **Codex**, it exists as a CPA plugin: [Autsunset/cpa-quota-estimator](https://github.com/Autsunset/cpa-quota-estimator) (v0.4.6 in the [official plugin store](https://raw.githubusercontent.com/router-for-me/CLIProxyAPI-Plugins-Store/main/registry.json)). It listens to CPA's `usage.handle`, pairs each request's tokens with that request's `X-Codex-Primary/Secondary-*` headers, and converts Δ% into Token and USD/Credits "full-cycle capacity". It adds a per-model weight learner (Huber loss, 21-day decay, a random-walk capacity scale per cycle) and a "guided calibration" mode **[V]**.
  - For **Claude**, it exists outside CPA: [victor-bajanov/ratelimit-proxy](https://github.com/victor-bajanov/ratelimit-proxy) `optime.py` fits `ticks = b_in·input + b_out·output + b_cw·cache_write + b_cr·cache_read` over "operational time" (one unit per 1% tick) **[V]**.
  - **No CPA plugin does this for Claude yet** **[V: grep of the estimator finds no anthropic or claude code; the store lists no other plugin with this purpose]**.
- **The verdict on the idea: sound, if you (a) use the per-request rate-limit headers rather than polling the endpoint and (b) treat % as a quantized, lagged, account-wide meter.**
  - The headers are rounded to whole percent today: 2 decimals as a fraction (`0.09`, `0.63`). Earlier builds sent full floats (`0.018416969696969696` in Dec 2025).
  - Quantization error telescopes. So a **ratio of sums between integer-crossing boundaries**, with a CI from the ±1-tick endpoint error, is the robust primary estimator. Per-category and per-model regression is a secondary step, useful only where your traffic mix varies.
- **CPA already captures everything you need, per request.** With `usage-statistics-enabled: true` and a management key set, each usage-queue record carries the full upstream `response_headers` alongside `tokens` and a canonical `token_breakdown`. That includes `anthropic-ratelimit-unified-{5h,7d,7d_oi,…}-utilization` and the `x-codex-*` headers. CPA also keeps a passive per-credential quota snapshot, exposed on `GET /v0/management/auth-files` **[V]**.
- **CPA itself will not do this.** Built-in usage statistics (the old `/v0/management/usage`) were removed in v6.10.0. The maintainer explicitly declined a "subscription value" feature in [#4647](https://github.com/router-for-me/CLIProxyAPI/issues/4647) (2026-07-29) and pointed to plugins and external tools **[V]**.
- **Recommended architecture:**
  1. Install cpa-quota-estimator for Codex now.
  2. Build a small **external sidecar**. It uses the RESP `SUBSCRIBE usage` stream, which is non-destructive and coexists with CPA Usage Keeper. It adds a low-rate `oauth/usage` poll through CPA's `/v0/management/api-call` with `$TOKEN$`, so CPA handles the token and you avoid refresh races. It stores to SQLite and runs the estimator.
  3. Optionally add [cpa-prometheus](https://github.com/giovannirco/cpa-prometheus-plugin) for Grafana gauges.
- **Biggest threats to validity:**
  - Usage not routed through the proxy: claude.ai web/desktop/mobile share the Claude pool, and Codex cloud tasks share the Codex pool.
  - Per-model sub-limits: Fable `7d_oi`, and `weekly_scoped` limits in `limits[]`.
  - Undisclosed internal weighting. Anthropic says only that cached content "counts less".
  - Promos and limit changes, and irregular or early resets (Codex Reset Bank credits; Claude weekly-reset anomalies).
- **CPA does not record the cache-write TTL split** (5m vs 1h), so the Claude "API $" value of cache writes is only bounded, between 1.25× and 2× input **[V: no `ephemeral_1h` in the CPA tree]**.

---

## 1. Landscape

| Project | Provider | Method | Output | Link |
|---|---|---|---|---|
| **cpa-quota-estimator** (Autsunset, 19★, pushed 2026-09-24) | Codex only | (b) passive headers from the CPA `usage.handle` stream + (c) Δ%-vs-$ fit + weight learner | Full-cycle and remaining Token/USD/Credits capacity with P25–P75, per-model allowances, burn forecast, monthly ledger | [repo](https://github.com/Autsunset/cpa-quota-estimator) |
| **claude-ratelimit-proxy** (victor-bajanov, 0★, 2026-09-23) | Claude | TLS-intercepting localhost proxy; logs `anthropic-ratelimit-unified-*` + tokens to SQLite; (c) no-intercept OLS per tick | Per-counter coefficients (ticks per token type), implied cap as a floor | [repo](https://github.com/victor-bajanov/ratelimit-proxy) |
| **codex-token-usage** (zhumengling, CPA plugin) | Codex (+ xAI) | (b) headers + naive ratio `tokens·100/used%` | "7d/month quota estimates", cost via LiteLLM, "suspicious external consumption" detection | [summary.go#L3718](https://github.com/zhumengling/codex-token-usage/blob/8dffcb71ce8d99ce1a45fcf39d1dcb63879e44b6/summary.go#L3718-L3732) |
| **cpa-prometheus** (giovannirco, CPA plugin) | Claude, Codex, Kimi, Antigravity, Gemini CLI, xAI | `usage.handle` token counters + (b) polls quota URLs every 5 min via `host.auth`/`host.http.do` | Prometheus `cliproxy_tokens_total{type=…}`, `cliproxy_quota_used_ratio{window}`, Grafana JSON | [README](https://github.com/giovannirco/cpa-prometheus-plugin) |
| **CPA Usage Keeper** (Willxup, 1.2k★) | all CPA traffic | Consumes the CPA usage queue (RESP subscribe), SQLite, price sync | Usage/cost dashboards, quota refresh; recommended by the CPA README | [repo](https://github.com/Willxup/cpa-usage-keeper) |
| **CPA-Manager-Plus** (seakee, 3.6k★) | all CPA traffic | Usage queue → SQLite; prices from models.dev → LiteLLM → OpenRouter | Cost analytics, quota windows, "reset evidence" | [repo](https://github.com/seakee/CPA-Manager-Plus) |
| CPAMC (official web UI) | Claude, Codex, Kimi, xAI, … | (b) client-side fetch of `oauth/usage` / `wham/usage` through CPA `/api-call` | Quota bars incl. `seven_day_opus/sonnet/oauth_apps/cowork`, Fable | [constants.ts](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/4530da271ba2e89810d4dccebc57f3091afa590a/src/utils/quota/constants.ts#L106-L133) |
| Other CPA store plugins: usage-statistics (Fwindy), cap-token-usage-tracker, cpa-usage, quota-router, quota-pacer, cpa-quota-api-extension, codex-5h-quota-warmer, … | various | Persistence or routing only; none computes $/% | — | [registry.json](https://raw.githubusercontent.com/router-for-me/CLIProxyAPI-Plugins-Store/main/registry.json) |
| **ccusage** (now `ccusage/ccusage`, 18.7k★; Rust rewrite; `ccusage codex` is built in) | Claude Code, Codex + ~15 CLIs | (a) local JSONL × LiteLLM (+ models.dev) prices | Daily/weekly/session/5h-block API-equivalent $ | [repo](https://github.com/ccusage/ccusage) |
| **tokscale** (junhoyeo, 5.5k★) | 50+ clients | (a) local logs × LiteLLM → OpenRouter; `tokscale usage` shows vendor quota | $ + leaderboard; **no** value estimate | [repo](https://github.com/junhoyeo/tokscale) |
| **Claude-Code-Usage-Monitor** / `claude-monitor` (v4.0.0) | Claude | (a) JSONL + (d) hardcoded plan limits / P90; now prefers the official statusline `rate_limits`, opt-in `oauth/usage` | Burn rate, predictions | [plans.py](https://github.com/Maciek-roboblog/Claude-Code-Usage-Monitor/blob/c59a83bf943f329f0e61f1a29c760353ee1860a5/src/claude_monitor/core/plans.py#L51-L102) |
| **CodexBar** (steipete, 21.9k★) | Codex, Claude, many more | (b) `wham/usage`, `oauth/usage`; (a) local cost scan (ccusage-derived); buckets cost into quota weeks | Menu-bar % + $/quota-week side by side; no $/% ratio | [QuotaWindows](https://github.com/steipete/CodexBar/blob/9c4bd43003b33832ef797652d5a2a5c64210ef75/Sources/CodexBarCore/CostUsageModels%2BQuotaWindows.swift) |
| **ccstatusline** | Claude | (b) `oauth/usage` + statusline `rate_limits`; parses `limits[]` `weekly_scoped` | Status-line widgets | [usage-fetch.ts](https://github.com/sirmalloc/ccstatusline/blob/35440e4a93ac8aba7e57973ac004a68adcc51089/src/utils/usage-fetch.ts#L124-L150) |
| ccflare / better-ccflare | Claude (+ many) | Proxy with per-request token/cost tracking, rate-limit audit trail | Load balancing, analytics; no value estimate | [better-ccflare](https://github.com/tombii/better-ccflare) |
| verkyyi/tokenledger | Claude team pools | Headers + `oauth/usage`; documents the `7d_oi` Fable header | Headroom per account | [issue #155](https://github.com/verkyyi/tokenledger/issues/155) |
| Blog "Max gives $X" estimates | Claude | Anecdotal repricing | "$600–1,500/mo for $200", "12–40×" | [finout](https://www.finout.io/blog/claude-code-pricing-2026), [cloudzero](https://www.cloudzero.com/blog/claude-pricing/) **[C, low quality]** |

"claude-monitor" is the PyPI name of Claude-Code-Usage-Monitor **[I]**. `@ccusage/codex` has been folded into the unified Rust `ccusage` CLI (README source table: `ccusage codex daily`) **[V]**.

---

## 2. Methodology deep-dives

### (a) Local JSONL × price table: ccusage, tokscale, CodexBar cost scan
- **Prices.** ccusage embeds a build-time LiteLLM snapshot and fetches `model_prices_and_context_window.json` plus models.dev at runtime ([pricing.rs L54-56](https://github.com/ccusage/ccusage/blob/25ca71b1a029a88356406f0c53323975adbecd01/rust/crates/ccusage-core/src/pricing.rs#L54-L56)). It handles `*_above_200k` tiers (L200-218), a 5m/1h cache-creation split (`cost.rs`), and a fast-mode multiplier ([cost.rs L36-45, L101-120](https://github.com/ccusage/ccusage/blob/25ca71b1a029a88356406f0c53323975adbecd01/rust/crates/ccusage-core/src/cost.rs#L36-L45)). Cost modes: `display` uses the logged `costUSD`, `calculate` uses tokens × prices, `auto` falls back.
- **Dedup.** It hashes `message.id + requestId`, adding session+timestamp when `requestId` is absent ([claude/lib.rs L348-362](https://github.com/ccusage/ccusage/blob/25ca71b1a029a88356406f0c53323975adbecd01/rust/adapters/claude/src/lib.rs#L348-L362)). This works around streaming writing multiple JSONL rows per message.
- **Multiple machines.** Not solved. Every tool reads only the local `~/.claude/projects` and `~/.codex/sessions`.
- **Good:** exact per-request tokens, API $ is well defined, and it works offline. **Bad:** it knows nothing about the allowance. JSONL formats drift. It misses web/desktop usage and other machines.

### (b) Polling the provider utilization endpoint
- **Claude.** `GET https://api.anthropic.com/api/oauth/usage` with `anthropic-beta: oauth-2025-04-20`.
  - It returns `five_hour`, `seven_day`, legacy `seven_day_opus/sonnet`, `extra_usage`, and (since about 2026-07) a `limits[]` array with `kind: "weekly_scoped"` per-model entries. The legacy per-model keys return null ([ccstatusline usage-types.ts L50-60](https://github.com/sirmalloc/ccstatusline/blob/35440e4a93ac8aba7e57973ac004a68adcc51089/src/utils/usage-types.ts#L50-L60), [usage-fetch.ts L124-150](https://github.com/sirmalloc/ccstatusline/blob/35440e4a93ac8aba7e57973ac004a68adcc51089/src/utils/usage-fetch.ts#L124-L150)).
  - CPAMC also reads `seven_day_oauth_apps`, `seven_day_cowork`, and `iguana_necktie` (a Fable alias) ([constants.ts](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/4530da271ba2e89810d4dccebc57f3091afa590a/src/utils/quota/constants.ts#L106-L126)).
  - The endpoint is undocumented and "aggressively rate-limited": cpa-quota-api-extension defaults to a 30-minute cache ([README L530](https://github.com/dinhkarate/cpa-quota-api-extension)). Claude-Code-Usage-Monitor uses a 180 s TTL and labels it "experimental" ([api_usage.py L1-30](https://github.com/Maciek-roboblog/Claude-Code-Usage-Monitor/blob/c59a83bf943f329f0e61f1a29c760353ee1860a5/src/claude_monitor/output/api_usage.py#L1-L30)).
  - `utilization` is an integer percent per the claude.ai page parser ([ratelimit-proxy proxy.py L427-431](https://github.com/victor-bajanov/ratelimit-proxy/blob/28ab496866819fb08fab432f999a862e6008faad/proxy.py#L420-L450)).
- **Claude, per request.** Every `/v1/messages` response carries `anthropic-ratelimit-unified-{status,5h-*,7d-*,representative-claim,overage-*,fallback-percentage}`. A request for a capped model also carries a model-scoped claim such as `7d_oi-*` for Fable, which is absent on Opus requests ([tokenledger #155](https://github.com/verkyyi/tokenledger/issues/155), [ratelimit-proxy proxy.py L195-213](https://github.com/victor-bajanov/ratelimit-proxy/blob/28ab496866819fb08fab432f999a862e6008faad/proxy.py#L195-L213)).
- **Codex.** `GET https://chatgpt.com/backend-api/wham/usage`. Its `used_percent` is **`i32`** in OpenAI's own generated model ([rate_limit_window_snapshot.rs L15-16](https://github.com/openai/codex/blob/a6bd19261c30ce0a0225fe90e646822d29916f11/codex-rs/codex-backend-openapi-models/src/models/rate_limit_window_snapshot.rs#L15-L16)) **[V: integer precision]**.
  - Per-request headers are `x-codex-{primary,secondary}-{used-percent,window-minutes,reset-at}`, plus credits and additional namespaced limits (`x-codex-<limit>-…`). Codex CLI parses these as f64 ([rate_limits.rs L55-80](https://github.com/openai/codex/blob/a6bd19261c30ce0a0225fe90e646822d29916f11/codex-rs/codex-api/src/rate_limits.rs#L55-L80)).
  - On the websocket path they arrive as a `codex.rate_limits` event ([CPA codex_quota.go](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/runtime/executor/helps/codex_quota.go#L16-L28)).

### (c) Fitting token spend to Δ%
- **cpa-quota-estimator.**
  - It reads `X-Codex-Primary-Used-Percent` from `r.ResponseHeaders` ([app.go L450](https://github.com/Autsunset/cpa-quota-estimator/blob/f793608a21c21fa3258dc5fe50cfef9f56d10ab4/app.go#L450)).
  - It keeps only the **first crossing of each new all-time high**, because concurrent responses carry stale percentages ([estimator.go L246-277](https://github.com/Autsunset/cpa-quota-estimator/blob/f793608a21c21fa3258dc5fe50cfef9f56d10ab4/estimator.go#L246-L277)).
  - It computes `Δ$·100/Δ%` per adjacent milestone and reports the **median** and P25–P75 ([estimator.go L101-167](https://github.com/Autsunset/cpa-quota-estimator/blob/f793608a21c21fa3258dc5fe50cfef9f56d10ab4/estimator.go#L101-L167)).
  - It builds segments at lag 0/1/2 ([quota_segments.go L435](https://github.com/Autsunset/cpa-quota-estimator/blob/f793608a21c21fa3258dc5fe50cfef9f56d10ab4/quota_segments.go#L435)).
  - A multiplicative model (per-cycle log scale × model factor × token-type factor × fast/long-context factors) is fitted with Huber loss. Factors are unlocked only when Fisher information allows ([weight_learner.go L284-357](https://github.com/Autsunset/cpa-quota-estimator/blob/f793608a21c21fa3258dc5fe50cfef9f56d10ab4/weight_learner.go#L284-L357)).
  - Pricing comes from models.dev, with "official API USD", "Codex Credits", or custom rates.
  - It has explicit **coverage modes** (`cpa_only`/`mixed`/`unknown`). Capacity is disabled when not all traffic goes through CPA.
  - It detects early resets and regime changes.
- **claude-ratelimit-proxy `optime.py`.** Boundary = the first request carrying a new running-max level. It sums every counter between boundaries and fits no-intercept OLS with classical SEs ([optime.py L253-265](https://github.com/victor-bajanov/ratelimit-proxy/blob/28ab496866819fb08fab432f999a862e6008faad/optime.py#L253-L265)). It drops censored intervals: window starts, tails, and ticks with no local traffic. It states that the implied cap is a **floor**, because other machines also move the meter.
- **codex-token-usage.** A single cumulative ratio `usedTokens·100/usedPercent` ([summary.go L3718-3732](https://github.com/zhumengling/codex-token-usage/blob/8dffcb71ce8d99ce1a45fcf39d1dcb63879e44b6/summary.go#L3718-L3732)).

### (d) Hardcoded or guessed plan limits
- **Claude-Code-Usage-Monitor.** `PLAN_LIMITS` sets Pro 19k, Max5 88k, and Max20 220k tokens per 5 h, with cost limits of $18, $35, and $140 ([plans.py L51-88](https://github.com/Maciek-roboblog/Claude-Code-Usage-Monitor/blob/c59a83bf943f329f0e61f1a29c760353ee1860a5/src/claude_monitor/core/plans.py#L51-L102)). "Custom" uses the P90 of past blocks that hit ≥95% of a "common limit" ([p90_calculator.py L32-49](https://github.com/Maciek-roboblog/Claude-Code-Usage-Monitor/blob/c59a83bf943f329f0e61f1a29c760353ee1860a5/src/claude_monitor/core/p90_calculator.py#L32-L49)). Its own TEAM entry is labeled "unverified".
- **ccusage `blocks --token-limit max`.** Uses the historical max block ([blocks.rs L605](https://github.com/ccusage/ccusage/blob/25ca71b1a029a88356406f0c53323975adbecd01/rust/crates/ccusage/src/blocks.rs#L605)).

---

## 3. Critique

| Method | Good | Bad |
|---|---|---|
| (a) JSONL × prices | Exact tokens; defensible "API list price" | No allowance info; blind to web/desktop, other machines, and proxied clients that don't write JSONL; dedup is heuristic |
| (b) endpoint polling | Authoritative, account-wide, includes per-model scoped limits (`limits[]`) | Integer %; undocumented; rate-limited; polling adds timing error; no attribution |
| (b′) per-request headers | Free, per request, naturally paired with tokens; headers now expose model-scoped claims | Also whole-percent today; stale on concurrent streams; set at response start, so they likely exclude the current request **[I]** |
| (c) Δ%-fit | The only way to get "$ per 100%"; cpa-quota-estimator is careful (first crossings, resets, coverage modes, learner) | Median-of-adjacent-ratios is noisy at 1% ticks (each ratio has ±100% endpoint error on Δ=1); leaks under mixed routing; per-category weights are weakly identified unless the mix varies |
| (d) hardcoded limits | Zero setup | Stale by construction (limits change, per-model caps, weekly caps); P90 heuristics say nothing about $ value |

---

## 4. The regression approach: pitfalls and the recommended estimator

### Pitfalls, with evidence
1. **Quantization.**
   - Header utilization currently reads `0.01`, `0.63`, `0.09`, `0.80` ([CodexBar #1894](https://github.com/steipete/CodexBar/issues/1894), [tokenledger #155](https://github.com/verkyyi/tokenledger/issues/155)). ratelimit-proxy states "the meters are rounded to 1%" ([README](https://github.com/victor-bajanov/ratelimit-proxy)).
   - In Dec 2025 the same header was a full float, `"0.018416969696969696"` ([claude-code #12829](https://github.com/anthropics/claude-code/issues/12829)). Precision has changed at least once **[V]**. Store the raw string.
   - `oauth/usage` gives integer percent. Codex `wham/usage` is `i32`.
   - For Max 20x at an assumed weekly value of V dollars, one tick is V/100. At 5h granularity the tick is coarser in $ terms relative to request size **[I]**.
2. **Lag and staleness.** Headers arrive with the first response bytes, so they cannot include that request's own output **[I]**. Long concurrent streams report older levels. Use the "first request carrying a new running max" boundary, as both existing tools do, and test lag 0/1/2 as cpa-quota-estimator does.
3. **Internal weighting ≠ API price.**
   - Anthropic says limits depend on "Model choice, Effort level… Tool usage" and that cached content "counts less against your limits" ([Usage limit best practices](https://support.claude.com/en/articles/9797557-usage-limit-best-practices)). The ratios are not disclosed.
   - API list prices: cache read 0.1× input (0.025× on Fable 5.1), cache write 1.25× (5m) or 2× (1h). Opus 5.5 is $4/$20 per MTok, Sonnet 5 $2/$10, Fable 5.1 $10/$50 ([pricing docs](https://platform.claude.com/docs/en/about-claude/pricing)).
   - So "$ per %" is **mix-dependent by construction**. Report it together with the mix, or per model.
   - For Codex, the published Credits card gives per-model input/cached/output credit rates and a Fast multiplier of 2.5× ([learn.chatgpt.com/docs/pricing](https://learn.chatgpt.com/docs/pricing)). cpa-quota-estimator claims the Credits and API rates coincide up to a ×25 factor **[C]**.
4. **Separate per-model limits.** Fable has its own `7d_oi` claim, which appears only on Fable requests. `limits[].weekly_scoped` covers models by display name, and the legacy `seven_day_opus/sonnet` keys now return null. Fit each meter only against the tokens that feed it. Opus/Sonnet tokens drive `7d`; Fable tokens drive both `7d` and `7d_oi` **[I: whether Fable also counts toward `7d` is unverified]**.
5. **Overlapping 5h and 7d windows.** They are two meters on one token stream. Fit them independently; do not difference one against the other. The 5h window "rolls" from first use. The 7d `resets_at` has been observed to mislead: [monperrus gist](https://gist.github.com/monperrus/3ac4b303a84946bbeaf2b1123ee99491) saw 72 h resets from Jun 9–20 2026, and commenters report that later patterns differ **[C]**. Codex has early resets through Reset Bank credits (see the `codex-auto-reset` and `quota-activation` plugins in the store) and "regime reverts" that cpa-quota-estimator has to repair.
6. **Unrouted usage.**
   - The Claude pool is shared: "all activity in both tools counts against the same usage limits" ([support 11145838](https://support.claude.com/en/articles/11145838-using-claude-code-with-your-pro-or-max-plan)).
   - Codex: "Local messages and cloud chats share your plan's usage allowance" ([pricing](https://learn.chatgpt.com/docs/pricing)).
   - Any unrouted tick biases $/% **downward**, because it looks like a cheap %. ratelimit-proxy drops ticks with zero local traffic, which only partly helps.
   - The claude.ai usage page payload has a `seven_day_breakdown.rows` "usage by surface" ([proxy.py L427-465](https://github.com/victor-bajanov/ratelimit-proxy/blob/28ab496866819fb08fab432f999a862e6008faad/proxy.py#L420-L466)). That could directly measure the non-Claude-Code share **[V exists; format unverified]**.
7. **Multiple accounts in the proxy.** Key everything by credential (`auth_index`/`auth_id`). CPA's usage record carries `auth_index`, `auth_type`, and `access_token_sha256` ([plugin.go L116-133](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/redisqueue/plugin.go#L116-L133)). Never pool the meters of different accounts. Only pool $/% estimates, and weight them.
8. **Promos and limit changes.** For example, Fable 5 was "up to 50% of weekly usage through July 7, 2026" ([quota-router README](https://github.com/Smarty-Pants-Inc/cpa-plugin-quota-router), citing [Anthropic](https://www.anthropic.com/news/redeploying-fable-5)). Treat each reset cycle as its own regime, and detect breaks.
9. **Failed and interrupted requests** may still consume quota. cpa-quota-estimator keeps 0/408/499/502 counts for sensitivity analysis. Also see CPA issue [#5866](https://github.com/router-for-me/CLIProxyAPI/issues/5866) (records marked failed on client disconnect).
10. **Cache-write TTL is unknown in CPA** (no `ephemeral_1h` field), so compute Claude $ at both 1.25× and 2× for writes.

### Recommended estimator
- **Unit of observation.** For each (account, meter, cycle), find boundary requests: the first successful response whose header shows a level above the running max. Interval k runs from boundary b_{k-1} to b_k. Let Δu_k be the level difference (usually 1) and let x_k be the vector of API-$ by (model × {uncached-in, cache-read, cache-write, output}) summed over requests *started* in [b_{k-1}, b_k). Lag-shift by one request as a sensitivity check.
- **Primary estimate (robust, mix-specific).** Use a ratio of sums over a long run: V̂ = 100 · Σ_k $_k / Σ_k Δu_k = 100 · $(b_0→b_K) / (u_K − u_0).
  - Because each boundary is a *crossing*, the endpoint error is at most about 1 tick total, not 1 tick per interval. The relative SE is roughly (1/√12 … 1)/(u_K − u_0).
  - Over a 60-point weekly span that is ≲2%. This beats the median of per-tick ratios.
  - Report V̂ per cycle. A 95% CI comes from a block bootstrap over ticks (blocks = sessions or hours) plus a ±1 endpoint band.
- **Secondary (weights).** Use no-intercept WLS or NNLS of Δu_k on x_k with log-scale per-model and per-token-type factors: the cpa-quota-estimator model, or ratelimit-proxy's linear version.
  - Weight intervals by 1/Var(Δu_k). A crossing interval carries about ±0.5 tick of timing error.
  - Merge adjacent ticks into ≥3–5 point intervals before fitting, to cut quantization noise.
  - Report coefficients relative to API price (e.g. "cache read counts 0.4× its API price"). Lock factors to 1 unless the design matrix has enough variation: check its condition number or Fisher information.
  - Planned "calibration phases" (same model only, same cycle, ≥5 points each) are the cleanest identification. cpa-quota-estimator has this built in.
- **Contamination guard.**
  - Flag ticks where proxied $ in the interval is below a floor, e.g. under 20% of the current V̂/100. Report the flagged-tick share as "estimated unrouted fraction". Exclude those ticks, or model a latent unrouted term.
  - Better: periodically read the claude.ai `seven_day_breakdown` by surface.
- **Week-over-week tracking.** Log V̂_cycle with CI and a random walk on log V (cpa-quota-estimator uses σ=0.35), or a CUSUM change-point test on log V̂. Always annotate the mix: share of Opus/Sonnet/Fable, cache-read share, output share.

---

## 5. CLIProxyAPI capabilities (v7.3.18, `ed980be`)

- **Usage stats.**
  - Built-in aggregation was removed in v6.10.0 ([README L141-151](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/README.md#L141-L151); commit `18bb9c31` per [#3129](https://github.com/router-for-me/CLIProxyAPI/issues/3129)).
  - Today, `usage-statistics-enabled` gates an **in-memory queue** with `redis-usage-queue-retention-seconds` (default 60, max 3600) ([config.example.yaml L152-158](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/config.example.yaml#L152-L158)).
  - The queue is enabled only when a management secret is set or Home is enabled ([server.go L245](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/api/server.go#L245)).
  - **Stats do not persist across restarts.**
- **Consumption paths.**
  - `GET /v0/management/usage-queue?count=N` is a destructive pop ([usage.go](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/api/handlers/management/usage.go#L23-L43)).
  - Or RESP on the **same port** (protocol-sniffed, [protocol_multiplexer.go L105](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/api/protocol_multiplexer.go#L105)): `AUTH <mgmt-key>`, `SUBSCRIBE usage`/`errors`, `LPOP`/`RPOP usage` ([redis_queue_protocol.go L108-175](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/api/redis_queue_protocol.go#L108-L175)).
  - When any subscriber exists, records fan out to subscribers and are **not** queued. A subscriber whose 256-deep buffer fills is dropped ([queue.go L65-76, L141-160](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/redisqueue/queue.go#L65-L160)).
  - Keeper warns that all collectors must use subscription mode.
- **Per-request fields** ([plugin.go L93-188](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/redisqueue/plugin.go#L93-L188)):
  - `tokens{input,output,reasoning,cached,cache_read,cache_creation,total}`
  - `token_breakdown` (schema v2: mutually exclusive uncached / cache_read / cache_write input, plus reasoning / non-reasoning output; [accounting.go L92](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/sdk/cliproxy/usage/accounting.go#L25-L120))
  - `model`, `alias`, `response_model`, `provider`, `auth_index`, `auth_type`, `access_token_sha256`, `service_tier`, `reasoning_effort`, `session_id`, `latency`/`ttft`, `failed`/`fail`
  - **`response_headers`**: the full upstream headers, set on every attempt regardless of request-log ([logging_helpers.go L190-192](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/runtime/executor/helps/logging_helpers.go#L190-L192), [usage_helpers.go L594-597](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/runtime/executor/helps/usage_helpers.go#L594-L597)). Codex websocket quota events are merged in.
- **Passive quota snapshot.**
  - For `claude`, `codex`, and `devin`, CPA stores `anthropic-ratelimit-unified-*`, `x-codex-*`, and `retry-after` per credential and per model ([quota_signals.go L15-215](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/sdk/cliproxy/auth/quota_signals.go#L15-L215)).
  - It is exposed as `quota{observed_at,signals}` and `model_quotas` on `GET /v0/management/auth-files` ([auth_files.go L673-676, L801-818](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/api/handlers/management/auth_files.go#L673-L676)).
  - It is used for routing and cooldown only, not history.
- **Active quota fetch.**
  - CPA itself does none for Claude/Codex. CPAMC does it in the browser via `POST /v0/management/api-call` with `Authorization: Bearer $TOKEN$` ([data.ts L157-176](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/4530da271ba2e89810d4dccebc57f3091afa590a/src/features/quota/providers/claude/data.ts#L157-L176)). CPA substitutes the in-memory token server-side ([api_tools.go L49-103, L272-293](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/api/handlers/management/api_tools.go#L49-L103)).
  - `/v0/management/quota/fetch` delegates to plugin `QuotaProvider`s or a declarative `quota_probe` ([plugin_quota.go](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/api/handlers/management/plugin_quota.go#L58-L130)).
  - The Claude OAuth scope includes `user:profile`, so `oauth/usage` works with CPA's tokens ([anthropic_auth.go L35](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/auth/claude/anthropic_auth.go#L35)).
- **Plugins.** Plugins are in-process C-ABI shared libraries. They are "trusted in-process code" and disabled by default. Interfaces include `UsagePlugin.HandleUsage` (with `ResponseHeaders`), `QuotaProvider`, `ManagementHandler`, and request/response interceptors ([pluginapi/types.go L1250, L1492](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/sdk/pluginapi/types.go#L1250-L1253)). There is a store with about 96 plugins.
- **Request logging.** `request-log` produces per-request files, and `GET /v0/management/request-log-by-id/:id` retrieves them.
- **Token storage.**
  - Auth-dir JSON (default `~/.cli-proxy-api`) holds `access_token`, `refresh_token`, `id_token`, `expired`, `last_refresh`, `email`, and `type` ([claude/token.go L21-51](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/internal/auth/claude/token.go#L21-L51)). CPA runs its own auto-refresh loop.
  - **Race risk:** if a sidecar refreshes a token itself, the refresh token rotates. Claude Code rotates refresh tokens ([CodexBar comment](https://github.com/steipete/CodexBar/blob/9c4bd43003b33832ef797652d5a2a5c64210ef75/Sources/CodexBarCore/Providers/Claude/ClaudeOAuth/ClaudeOAuthCredentials.swift#L738)). CPA's next refresh would then hit `invalid_grant`, and CPA disables the auth on that error ([conductor_cooldown.go L1710](https://github.com/router-for-me/CLIProxyAPI/blob/ed980be34b9981735eaa16941956b1e8d9abfb7c/sdk/cliproxy/auth/conductor_cooldown.go#L1710)).
  - Reading the file read-only is safe but can hand you an expired token. **Use `/api-call` instead.**

### Build options

| Option | Pros | Cons |
|---|---|---|
| A. **External sidecar** (recommended for Claude) | Crash-isolated; any language; survives CPA upgrades; coexists with Keeper via SUBSCRIBE | Must reconnect fast or lose events (no replay if unsubscribed and retention is 60 s); needs the management key |
| B. Native plugin (like cpa-quota-estimator/cpa-prometheus) | Sees every `usage.handle`; ships a dashboard in CPAMC | CGO/ABI coupling to CPA versions; runs in-process |
| C. Fork CPA | — | Maintainers refused this feature (#4647); constant rebase churn |
| D. Extend cpa-quota-estimator to parse `anthropic-ratelimit-unified-*` | Reuses mature cycle/reset/learner logic | Codex-specific assumptions (Spark, credits, `reset_at` semantics) are threaded throughout; sizable port |

**Recommended architecture (A + install the estimator for Codex):**
1. Set `usage-statistics-enabled: true` and `redis-usage-queue-retention-seconds: 3600` (a buffer for sidecar restarts, drained with `LPOP` on reconnect), and set the management key.
2. The sidecar holds `SUBSCRIBE usage` (e.g. `redis-py` against `cpa:8317`). For each record it stores the raw JSON, `token_breakdown`, model, `auth_index`, and every `anthropic-ratelimit-unified-*` / `x-codex-*` header as raw strings, in SQLite (WAL).
3. Every 10–30 min per Claude credential, call `POST /v0/management/api-call {auth_index, GET oauth/usage, Bearer $TOKEN$}` to capture `limits[]` weekly_scoped, `extra_usage`, and `seven_day_oauth_apps`. Treat a 429 as backoff. Optionally poll `wham/usage` for Codex too.
4. Pull prices nightly from LiteLLM or models.dev, with an Anthropic pricing override table. Keep price versions so historical $ can be recomputed.
5. Run the estimator from section 4 per (account, meter, cycle). Expose `/metrics` (V̂, CI, unrouted fraction, mix) for Prometheus/Grafana, next to cpa-prometheus's `cliproxy_quota_used_ratio`.
6. Route all Claude clients through CPA, and minimise claude.ai web use on that account, or measure it via the usage page breakdown.

---

## 6. Open questions / not verified

- Whether `anthropic-ratelimit-unified-*-utilization` is rounded (2 dp) for all accounts or only some. The Dec 2025 sample was a full float. Capture raw strings and check.
- Whether headers on response N include request N's own consumption (lag). Only inferred. Test empirically with isolated large requests.
- Whether Fable/`7d_oi` usage also counts toward `7d`, and how `seven_day_oauth_apps` (seen in CPAMC keys) interacts with CPA traffic. CPA uses a Claude Code OAuth client, so its traffic may be classified as Claude Code rather than an "OAuth app" **[I]**.
- The exact claude.ai `seven_day_breakdown` schema and its endpoint (`/api/organizations/<org>/usage`, reached via a browser session, not the CPA OAuth token).
- The `oauth/usage` rate limit. Only community statements ("aggressive"; a 30-min cache recommended).
- cpa-quota-estimator's claim that Codex Credits = API $ × 25. I did not cross-check it against the OpenAI API price page.
- I did not run any of these tools or observe live headers. Everything above comes from source, docs, and issue threads. The GitHub API rate limit cut off further issue-thread reading; for example, the full CPA #4647 proposal body is truncated.
