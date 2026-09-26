# Third-party notices

## cpa-quota-estimator

`internal/weights` is a port of the weight learner from
[Autsunset/cpa-quota-estimator](https://github.com/Autsunset/cpa-quota-estimator),
commit `f793608a21c21fa3258dc5fe50cfef9f56d10ab4`.

| Source file | Ported to |
|---|---|
| `weight_learner.go` | `internal/weights/learner.go`, `internal/weights/types.go` |
| `weight_identifiability.go` | `internal/weights/identifiability.go` |
| `weight_cycle_scales.go` | `internal/weights/scales.go` |
| `weight_backtest.go` | `internal/weights/backtest.go` |

The math is kept: the multiplicative log-factor model, Huber loss (δ = 0.6,
σ_obs = 0.35), damped Gauss–Newton / Levenberg–Marquardt with IRLS, N(0, 0.5)
factor priors, per-cycle scales with a random walk (σ = 0.35) and a first-cycle
prior SD of 2.5, the two-stage fit (shared factors with free scales, then scales
with the random walk), Laplace intervals from the inverse Hessian, 21-day
half-life decay times the boundary weight, the within-cycle share-SD,
conditional-Fisher and cross-cycle ratio-range unlock gates, the online
log-scale tracker, and the prequential per-lag backtest. What changed: every
Codex-specific constant (the `gpt-5.6-sol` reference model, the credit price
table and its fixed 2.5× fast multiplier, the hard-coded list of assessed
models, the main-window filter) is replaced by `ProviderParams`, which supplies
the anchor family, a USD-per-token reference-rate function built from this
project's price table, the unlockable token types and the fast / long-context
priors, with defaults per provider. Scales are expressed as fraction of the
allowance per weighted API USD so that `exp(−s)` is the USD value of 100 % of a
meter, and the "correlated" check that withholds `identified` looks only at
factor–factor correlations (the per-cycle scale nuisance is already
marginalized in each factor's interval). The interrupted-request coefficient,
fixed model factors and the reference-mode (API vs credits) comparison scores
were not ported.

```
MIT License

Copyright (c) 2026 Autsunset

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
