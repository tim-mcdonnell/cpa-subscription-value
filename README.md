# cpa-subscription-value

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that measures what a Claude Max or ChatGPT/Codex subscription is worth in API-equivalent dollars, continuously, with one estimator and one dashboard for both providers.

Every request that CPA proxies comes back with the provider's quota utilization for that account in the response headers (`anthropic-ratelimit-unified-7d-utilization`, `x-codex-primary-used-percent`). The plugin pairs each request's API list-price cost with the meter's movement and estimates "$ per 100% of the weekly allowance" per reset cycle, with a confidence interval, plus per-model weights (how much each model and token type moves the meter relative to its API price) and burn rate / forecast.

## Status

Phase 1: records normalized per-request tokens and meter readings for both providers and serves a raw dashboard in the management center. See [`docs/plan.md`](docs/plan.md) for the design and phases and [`docs/research.md`](docs/research.md) for the research behind it.

## Install

1. Build for the CPA host (Linux amd64 in Docker): `make build-linux-amd64`, or download a release zip.
2. Copy `cpa-subscription-value.so` to `<plugins dir>/linux/amd64/cpa-subscription-value-v<version>.so`.
3. In `config.yaml`:

```yaml
plugins:
  enabled: true
  configs:
    cpa-subscription-value:
      enabled: true
      data_dir: /CLIProxyAPI/data/cpa-subscription-value   # mount this in Docker
```

4. Restart CPA. The dashboard appears in the management center's plugin menu as **Subscription Value**; the JSON API lives under `/v0/management/cpa-subscription-value/` (management key required).

The plugin answers `schema_version` 2, so it loads on CPA ≥ v7.2.x. On hosts older than v7.2.142 a reused Codex websocket carries no quota headers; the poll fallback covers that, but per-request pairing is better on a current CPA.

## Develop

```bash
make test        # go vet + go test ./...
make build       # native c-shared build into dist/
```

`cmd/plugin` is the only cgo file. Everything else is plain Go and testable without a host.

## Licensing

MIT. The per-model weight learner (Phase 4) is ported from [Autsunset/cpa-quota-estimator](https://github.com/Autsunset/cpa-quota-estimator) (MIT); see `THIRD_PARTY_NOTICES.md`.
