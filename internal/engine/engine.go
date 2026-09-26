// Package engine schedules the derived work: recomputing cycles and
// estimates when new readings arrive, fitting per-model weights hourly,
// pruning old rows, and recording restart gaps. It owns no statistics; it
// calls estimate and weights.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
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
	settingRestartGaps   = "restart_gaps"
	settingLastEventAt   = "last_event_at"
	settingLastRecompute = "last_recompute_" // + "<account>_<meter>"
	settingLastWeights   = "last_weights_"   // + "<account>_<meter>"
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
			e.flush(ctx, true)
			return
		case <-e.wake:
		case <-tick.C:
		}
		e.flush(ctx, false)
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

// flush recomputes every dirty meter whose last mark is older than the
// debounce; force recomputes all of them.
func (e *Engine) flush(ctx context.Context, force bool) {
	e.mu.Lock()
	var due []dirtyKey
	for k, t := range e.dirty {
		if force || time.Since(t) >= e.opts.Debounce {
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
		if ctx.Err() != nil && !force {
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
	if acct, ok, _ := e.st.GetAccount(accountID); ok {
		o.Series = acct.AuthIndex
	}
	if err := estimate.RecomputeWith(e.st, accountID, meterKey, time.Now(), o); err != nil {
		return err
	}
	return e.st.SetSetting(fmt.Sprintf("%s%d_%s", settingLastRecompute, accountID, meterKey), time.Now())
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
	lag := latestLag(e.st, acct.ID, meterKey)
	var segs []weights.Segment
	for _, c := range cycles {
		rows, err := e.st.ListSegments(c.ID, lag)
		if err != nil {
			return err
		}
		segs = append(segs, adaptSegments(c, rows)...)
	}
	if len(segs) == 0 {
		return fmt.Errorf("no segments")
	}
	table, hash := e.catalog.Active()
	params := weights.DefaultParams(acct.Provider, table)
	fit, err := weights.FitWeights(segs, params, time.Now())
	if err != nil {
		return err
	}
	fit.Lag = lag
	row := fit.ToStore(acct.ID, meterKey, hash)
	if err := e.st.InsertWeightFit(&row); err != nil {
		return err
	}
	return e.st.SetSetting(fmt.Sprintf("%s%d_%s", settingLastWeights, acct.ID, meterKey), time.Now())
}

// latestLag is the lag the newest estimate for the meter used; "time" when none.
func latestLag(st *store.Store, accountID int64, meterKey string) string {
	ests, err := st.LatestEstimatesPerCycle(accountID, meterKey)
	if err != nil || len(ests) == 0 {
		return string(meter.LagTime)
	}
	newest := ests[0]
	for _, x := range ests[1:] {
		if x.ComputedAt.After(newest.ComputedAt) {
			newest = x
		}
	}
	if newest.Lag == "" {
		return string(meter.LagTime)
	}
	return newest.Lag
}

// segmentFeatures mirrors estimate's features_json.
type segmentFeatures struct {
	Tokens map[string]meter.TokenCounts `json:"tokens"`
}

// adaptSegments turns stored segments into learner input. Segments with
// exclusion flags are dropped, except low_usd ones, which the learner
// down-weights. Fast/long are aggregate counts on the segment, not per
// family, so features carry neither flag; the learner's fast/long factors
// stay at their priors until per-family counts exist.
func adaptSegments(c store.Cycle, rows []store.Segment) []weights.Segment {
	out := make([]weights.Segment, 0, len(rows))
	for _, r := range rows {
		weight := 1.0
		skip := false
		for _, f := range r.Flags {
			if f == "low_usd" {
				weight = 0.5
			} else {
				skip = true
			}
		}
		if skip || r.DeltaTicks <= 0 {
			continue
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
