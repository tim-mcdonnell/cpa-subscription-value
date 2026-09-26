// Package engine schedules the derived work: recomputing cycles and
// estimates when new readings arrive, fitting per-model weights hourly,
// pruning old rows, and recording restart gaps. It owns no statistics; it
// calls estimate and weights.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/estimate"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/meter"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/pricing"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/weights"
)

// LogFunc matches abi.Host.Log.
type LogFunc func(level, msg string, fields map[string]any)

// Options tune the schedule; zero values take the defaults.
type Options struct {
	Debounce     time.Duration // wait after the last dirty mark before recomputing (30 s)
	WeightsEvery time.Duration // weights refit cadence (1 h)
	Retention    time.Duration // rows older than this are pruned daily (365 d)
	// RestartGapMin is the idle span between the last event before a restart
	// and the restart itself that counts as a gap in observation (10 min).
	RestartGapMin time.Duration
	// SweepEvery marks every account's estimable meters dirty, so readings
	// that arrive without an event (polls) still get recomputed (15 min).
	SweepEvery time.Duration
}

func (o Options) withDefaults() Options {
	if o.Debounce <= 0 {
		o.Debounce = 30 * time.Second
	}
	if o.WeightsEvery <= 0 {
		o.WeightsEvery = time.Hour
	}
	if o.Retention <= 0 {
		o.Retention = 365 * 24 * time.Hour
	}
	if o.RestartGapMin <= 0 {
		o.RestartGapMin = 10 * time.Minute
	}
	if o.SweepEvery <= 0 {
		o.SweepEvery = 15 * time.Minute
	}
	return o
}

// Settings keys.
const (
	settingRestartGaps = "restart_gaps"
	settingLastEventAt = "last_event_at"
	settingLastWeights = "last_weights_" // + "<account>_<meter>"
)

type dirtyKey struct {
	account int64
	meter   string
}

// Engine is safe for concurrent MarkDirty; Run must be called once.
type Engine struct {
	st      *store.Store
	catalog *pricing.Catalog
	log     LogFunc
	opts    Options

	mu    sync.Mutex
	dirty map[dirtyKey]time.Time
	wake  chan struct{}

	gaps []meter.Interval
}

// New builds an engine over an open store.
func New(st *store.Store, catalog *pricing.Catalog, log LogFunc, opts Options) *Engine {
	if log == nil {
		log = func(string, string, map[string]any) {}
	}
	e := &Engine{st: st, catalog: catalog, log: log, opts: opts.withDefaults(), dirty: map[dirtyKey]time.Time{}, wake: make(chan struct{}, 1)}
	e.gaps = e.loadGaps()
	return e
}

// RecordRestart compares the last event seen before this process started
// with the start time and, when the plugin was down long enough to miss
// traffic, records the interval so segments spanning it are excluded.
func (e *Engine) RecordRestart(startedAt time.Time) {
	var last time.Time
	if found, err := e.st.GetSetting(settingLastEventAt, &last); err != nil || !found || last.IsZero() {
		return
	}
	if startedAt.Sub(last) < e.opts.RestartGapMin {
		return
	}
	e.gaps = append(e.gaps, meter.Interval{From: last, To: startedAt})
	_ = e.st.SetSetting(settingRestartGaps, e.gaps)
	e.log("info", "recorded restart gap", map[string]any{"from": last, "to": startedAt})
}

// NoteEvent remembers the newest event time so a later restart can find the gap.
func (e *Engine) NoteEvent(observedAt time.Time) {
	_ = e.st.SetSetting(settingLastEventAt, observedAt)
}

func (e *Engine) loadGaps() []meter.Interval {
	var gaps []meter.Interval
	_, _ = e.st.GetSetting(settingRestartGaps, &gaps)
	return gaps
}

// MarkDirty schedules a recompute for one meter of one account.
func (e *Engine) MarkDirty(accountID int64, meterKey string) {
	e.mu.Lock()
	e.dirty[dirtyKey{accountID, meterKey}] = time.Now()
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run drives the schedule until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	weightsAt := time.Now().Add(e.opts.WeightsEvery)
	pruneAt := time.Now().Add(time.Hour)
	sweepAt := time.Now().Add(e.opts.SweepEvery)
	for {
		select {
		case <-ctx.Done():
			// Dirty marks are dropped, not flushed: a recompute has no
			// cancellation point and would hold up quiesce. The sweep on
			// the next start marks every meter again.
			return
		case <-e.wake:
		case <-tick.C:
		}
		e.flush(ctx)
		now := time.Now()
		if now.After(weightsAt) {
			e.fitAll(ctx)
			weightsAt = now.Add(e.opts.WeightsEvery)
		}
		if now.After(pruneAt) {
			e.prune()
			pruneAt = now.Add(24 * time.Hour)
		}
		if now.After(sweepAt) {
			e.sweep()
			sweepAt = now.Add(e.opts.SweepEvery)
		}
	}
}

func (e *Engine) sweep() {
	accounts, err := e.st.ListAccounts()
	if err != nil {
		return
	}
	for _, a := range accounts {
		e.MarkDirty(a.ID, domain.MeterSevenDay)
		e.MarkDirty(a.ID, domain.MeterFiveHour)
	}
}

// flush recomputes every dirty meter whose last mark is older than the debounce.
func (e *Engine) flush(ctx context.Context) {
	e.mu.Lock()
	var due []dirtyKey
	for k, t := range e.dirty {
		if time.Since(t) >= e.opts.Debounce {
			due = append(due, k)
			delete(e.dirty, k)
		}
	}
	e.mu.Unlock()
	sort.Slice(due, func(i, j int) bool {
		if due[i].account != due[j].account {
			return due[i].account < due[j].account
		}
		return due[i].meter < due[j].meter
	})
	for _, k := range due {
		if ctx.Err() != nil {
			return
		}
		if err := e.Recompute(k.account, k.meter); err != nil {
			e.log("warn", "recompute failed", map[string]any{"account_id": k.account, "meter": k.meter, "error": err.Error()})
		}
	}
}

// Recompute runs the estimator for one meter synchronously.
func (e *Engine) Recompute(accountID int64, meterKey string) error {
	o := estimate.DefaultAnalyzeOptions()
	o.RestartGaps = e.gaps
	if e.catalog != nil {
		table, _ := e.catalog.Active()
		o.Split = priceSplit(table)
	}
	key := fmt.Sprint(accountID)
	if acct, ok, _ := e.st.GetAccount(accountID); ok && acct.AuthIndex != "" {
		o.Series = acct.AuthIndex
		key = acct.AuthIndex
	}
	if err := estimate.RecomputeWith(e.st, accountID, meterKey, time.Now(), o); err != nil {
		return err
	}
	return e.st.SetSetting(store.RecomputeSettingKey(key, meterKey), time.Now())
}

func (e *Engine) fitAll(ctx context.Context) {
	accounts, err := e.st.ListAccounts()
	if err != nil {
		return
	}
	for _, a := range accounts {
		for _, m := range []string{domain.MeterSevenDay, domain.MeterFiveHour} {
			if ctx.Err() != nil {
				return
			}
			if err := e.FitWeights(a, m); err != nil {
				e.log("debug", "weights fit skipped", map[string]any{"account_id": a.ID, "meter": m, "reason": err.Error()})
			}
		}
	}
}

// FitWeights refits the per-model learner for one meter from stored
// segments at the lag the latest estimate chose, and persists the fit.
func (e *Engine) FitWeights(acct domain.Account, meterKey string) error {
	if e.catalog == nil {
		return fmt.Errorf("no price catalog")
	}
	cycles, err := e.st.ListCycles(acct.ID, meterKey, 200)
	if err != nil {
		return err
	}
	if len(cycles) == 0 {
		return fmt.Errorf("no cycles")
	}
	est, lag := latestEstimate(e.st, acct.ID, meterKey)
	byLag := map[string][]weights.Segment{}
	for _, c := range cycles {
		for _, l := range meter.Lags {
			rows, err := e.st.ListSegments(c.ID, string(l))
			if err != nil {
				return err
			}
			byLag[string(l)] = append(byLag[string(l)], adaptSegments(c, rows)...)
		}
	}
	segs := byLag[lag]
	if len(segs) == 0 {
		return fmt.Errorf("no segments")
	}
	table, hash := e.catalog.Active()
	params := weights.DefaultParams(acct.Provider, table)
	now := time.Now()
	fit, err := weights.FitWeights(segs, params, now)
	if err != nil {
		return err
	}
	fit.Lag = lag
	best, mae, ambiguous := weights.Backtest(byLag, params, now)
	fit.Backtest, fit.LagAmbiguous = mae, ambiguous
	row := fit.ToStore(acct.ID, meterKey, hash)
	if err := e.st.InsertWeightFit(&row); err != nil {
		return err
	}
	// Plan §Weight learner: the learner's own V for the latest cycle is
	// cross-checked against V̂ (learner_divergence outside 2·SE) and its
	// backtest lag against the estimator's (lag_unstable on disagreement).
	chk := Check{ComputedAt: now, EstimateLag: lag, BacktestLag: best, BacktestMAE: mae, LagAmbiguous: ambiguous}
	if n := len(fit.Scales); n > 0 {
		chk.LearnerValue = fit.Scales[n-1].ValuePer100
	}
	if est != nil {
		chk.EstimateValue, chk.EstimateSE = est.VHat, math.Hypot(est.SEQuant, est.SEBoot)
		chk.Divergence = chk.LearnerValue > 0 && est.VHat > 0 && math.Abs(chk.LearnerValue-est.VHat) > 2*chk.EstimateSE
		chk.LagDisagreement = best != "" && !ambiguous && best != lag
	}
	if err := e.st.SetSetting(CheckSettingKey(acct.AuthIndex, meterKey), chk); err != nil {
		return err
	}
	return e.st.SetSetting(fmt.Sprintf("%s%d_%s", settingLastWeights, acct.ID, meterKey), now)
}

// priceSplit apportions an event's frozen api_usd across token types by the
// active price table, so the time-lag registration and the by-type mix use
// the same prices ingest did. Unknown models fall back to list-price ratios.
func priceSplit(table pricing.Table) meter.SplitFunc {
	return func(ev domain.UsageEvent) meter.TypeUSD {
		r, _, ok := table.Lookup(ev.Provider, ev.Model)
		if !ok {
			return meter.RatioSplit(ev)
		}
		u := float64(ev.UncachedInput) * r.Input
		cr := float64(ev.CacheRead) * r.CacheRead
		cw := float64(ev.CacheWrite) * r.CacheWrite5m
		o := float64(ev.Output) * r.Output
		sum := u + cr + cw + o
		if sum <= 0 {
			return meter.TypeUSD{UncachedInput: ev.APIUSD}
		}
		f := ev.APIUSD / sum
		return meter.TypeUSD{UncachedInput: u * f, CacheRead: cr * f, CacheWrite: cw * f, Output: o * f}
	}
}

// Check is the learner-vs-estimator cross-check stored per meter.
type Check struct {
	ComputedAt      time.Time          `json:"computed_at"`
	LearnerValue    float64            `json:"learner_value_per_100"`
	EstimateValue   float64            `json:"estimate_v_hat"`
	EstimateSE      float64            `json:"estimate_se"`
	Divergence      bool               `json:"learner_divergence"`
	EstimateLag     string             `json:"estimate_lag"`
	BacktestLag     string             `json:"backtest_lag,omitempty"`
	BacktestMAE     map[string]float64 `json:"backtest_mae,omitempty"`
	LagAmbiguous    bool               `json:"lag_ambiguous"`
	LagDisagreement bool               `json:"lag_unstable"`
}

// CheckSettingKey names the stored cross-check for a meter.
func CheckSettingKey(authIndex, meterKey string) string {
	return "weights_check_" + authIndex + "_" + meterKey
}

// latestEstimate is the newest estimate for the meter and the lag it used;
// nil and "time" when none exists yet.
func latestEstimate(st *store.Store, accountID int64, meterKey string) (*store.Estimate, string) {
	ests, err := st.LatestEstimatesPerCycle(accountID, meterKey)
	if err != nil || len(ests) == 0 {
		return nil, string(meter.LagTime)
	}
	newest := ests[0]
	for _, x := range ests[1:] {
		if x.ComputedAt.After(newest.ComputedAt) {
			newest = x
		}
	}
	if newest.Lag == "" {
		return &newest, string(meter.LagTime)
	}
	return &newest, newest.Lag
}

// segmentFeatures mirrors estimate's features_json.
type segmentFeatures struct {
	Tokens map[string]meter.TokenCounts `json:"tokens"`
}

// adaptSegments turns stored segments into learner input using the
// estimator's eligibility rule; low_usd segments stay in at half weight
// (the boundary weight is not persisted). Fast/long are aggregate counts on
// the segment, not per family, so features carry neither flag; the learner's
// fast/long factors stay at their priors until per-family counts exist.
func adaptSegments(c store.Cycle, rows []store.Segment) []weights.Segment {
	out := make([]weights.Segment, 0, len(rows))
	for _, r := range rows {
		if estimate.Excludes(r.Flags) || r.DeltaTicks <= 0 {
			continue
		}
		weight := 1.0
		if slices.Contains(r.Flags, meter.FlagLowUSD) {
			weight = 0.5
		}
		var fs segmentFeatures
		if err := json.Unmarshal(r.Features, &fs); err != nil || len(fs.Tokens) == 0 {
			continue
		}
		seg := weights.Segment{
			CycleKey:   fmt.Sprint(c.ID),
			RegimeKey:  fmt.Sprint(c.RegimeSeq),
			EndAt:      r.TEnd,
			DeltaTicks: float64(r.DeltaTicks),
			Tick:       c.Tick,
			Weight:     weight,
			Lag:        r.Lag,
		}
		for family, tc := range fs.Tokens {
			for t, n := range map[weights.TokenType]int64{
				"uncached_input": tc.UncachedInput, "cache_read": tc.CacheRead,
				"cache_write": tc.CacheWrite, "output": tc.Output,
			} {
				if n > 0 {
					seg.Features = append(seg.Features, weights.Feature{Family: family, Type: t, Tokens: n})
				}
			}
		}
		sort.Slice(seg.Features, func(i, j int) bool {
			if seg.Features[i].Family != seg.Features[j].Family {
				return seg.Features[i].Family < seg.Features[j].Family
			}
			return seg.Features[i].Type < seg.Features[j].Type
		})
		out = append(out, seg)
	}
	return out
}

func (e *Engine) prune() {
	cutoff := time.Now().Add(-e.opts.Retention)
	ev, rd, err := e.st.PruneOlderThan(cutoff)
	if err != nil {
		e.log("warn", "prune failed", map[string]any{"error": err.Error()})
		return
	}
	if ev+rd > 0 {
		e.log("info", "pruned old rows", map[string]any{"events": ev, "readings": rd, "cutoff": cutoff})
	}
}
