package pricing

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/config"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

// LogFunc receives diagnostics; level is "debug", "info", "warn" or "error".
type LogFunc func(level, msg string, fields map[string]any)

// Catalog is the live pricer (it implements ingest.Pricer). The active table
// is compiled with the configured Claude cache-write multiplier, persisted as
// a snapshot, and swapped atomically on sync.
type Catalog struct {
	st   *store.Store
	cfg  config.Config
	doer HTTPDoer
	log  LogFunc

	mu    sync.RWMutex
	table Table
	hash  string

	syncMu         sync.Mutex
	firstSyncDelay time.Duration
}

// NewCatalog starts on the built-in table; call Load to restore the stored
// one. A nil doer (or nil *abi.Host) falls back to net/http.
func NewCatalog(st *store.Store, cfg config.Config, doer HTTPDoer, log LogFunc) *Catalog {
	if h, ok := doer.(*abi.Host); doer == nil || (ok && h == nil) {
		doer = StdHTTP{}
	}
	if log == nil {
		log = func(string, string, map[string]any) {}
	}
	c := &Catalog{st: st, cfg: cfg, doer: doer, log: log, firstSyncDelay: time.Minute}
	c.table = c.compile(Builtin())
	c.hash = c.table.Hash()
	return c
}

func (c *Catalog) compile(t Table) Table {
	return t.withClaudeCacheWrite(c.cfg.ClaudeCacheWriteMultiplier)
}

// Active returns the table in use and its hash. Do not mutate the table.
func (c *Catalog) Active() (Table, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.table, c.hash
}

// Price implements ingest.Pricer under the active table.
func (c *Catalog) Price(ev *domain.UsageEvent) (usd, usdCacheWrite1h float64, hash string, ok bool) {
	t, h := c.Active()
	usd, usd1h, ok := t.Cost(ev)
	if !ok {
		return 0, 0, "", false
	}
	return usd, usd1h, h, true
}

// Load restores the latest stored snapshot, or the built-in table when none
// exists, recompiles it under the current config and persists the result if
// that produced a new hash (e.g. the cache-write multiplier changed).
func (c *Catalog) Load() error {
	base, source := Builtin(), "builtin"
	if c.st != nil {
		snap, rows, ok, err := c.st.LatestPriceSnapshot()
		if err != nil {
			return err
		}
		if ok && len(rows) > 0 {
			base, source = tableFromRows(rows), snap.Source
		}
	}
	t := c.compile(base)
	if _, err := c.persist(t, source, nil); err != nil {
		return err
	}
	c.swap(t)
	return nil
}

// SyncNow fetches models.dev, persists the compiled table (a no-op when the
// hash is already stored) and makes it active. changed reports whether the
// active hash moved.
func (c *Catalog) SyncNow(ctx context.Context) (changed bool, err error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	fetched, raw, err := Sync(ctx, c.doer)
	if err != nil {
		return false, err
	}
	t := c.compile(fetched)
	if _, err := c.persist(t, ModelsDevURL, raw); err != nil {
		return false, err
	}
	_, old := c.Active()
	c.swap(t)
	changed = t.Hash() != old
	c.log("info", "pricing: synced models.dev", map[string]any{
		"hash": t.Hash(), "previous": old, "changed": changed, "rows": len(t),
	})
	return changed, nil
}

func (c *Catalog) swap(t Table) {
	h := t.Hash()
	c.mu.Lock()
	c.table, c.hash = t, h
	c.mu.Unlock()
}

func (c *Catalog) persist(t Table, source string, raw []byte) (bool, error) {
	if c.st == nil {
		return false, nil
	}
	h := t.Hash()
	if _, _, ok, err := c.st.GetPriceSnapshot(h); err != nil || ok {
		return false, err
	}
	return c.st.InsertPriceSnapshot(store.PriceSnapshot{Hash: h, Source: source, Raw: raw}, tableToRows(t))
}

// store.Price rates are USD per token; Rates are USD per MTok.
func tableToRows(t Table) []store.Price {
	rows := make([]store.Price, 0, len(t))
	for k, r := range t {
		rows = append(rows, store.Price{
			Provider: string(k.Provider), ModelFamily: k.Family,
			Input: r.Input / 1e6, Output: r.Output / 1e6, CacheRead: r.CacheRead / 1e6,
			CacheWrite5m: r.CacheWrite5m / 1e6, CacheWrite1h: r.CacheWrite1h / 1e6,
			LongCtxMult: mult(r.LongCtxMult), FastMult: mult(r.FastMult), Source: r.Source,
		})
	}
	return rows
}

func tableFromRows(rows []store.Price) Table {
	t := make(Table, len(rows))
	for _, p := range rows {
		t[Key{domain.Provider(p.Provider), p.ModelFamily}] = Rates{
			Input: round9(p.Input * 1e6), Output: round9(p.Output * 1e6), CacheRead: round9(p.CacheRead * 1e6),
			CacheWrite5m: round9(p.CacheWrite5m * 1e6), CacheWrite1h: round9(p.CacheWrite1h * 1e6),
			LongCtxMult: p.LongCtxMult, FastMult: p.FastMult, Source: p.Source,
		}
	}
	return t
}

// DefaultRepriceBatch is the page size for Reprice.
const DefaultRepriceBatch = 500

// Reprice rewrites the frozen $ of every event not already under the active
// hash. It prices with one table for the whole run, pages by id so events it
// cannot price are skipped rather than revisited, and stops between batches
// on cancellation. A second run over the same hash updates nothing.
func (c *Catalog) Reprice(ctx context.Context, st *store.Store, batch int) (int64, error) {
	if st == nil {
		st = c.st
	}
	if st == nil {
		return 0, fmt.Errorf("pricing: reprice: no store")
	}
	if batch <= 0 {
		batch = DefaultRepriceBatch
	}
	t, h := c.Active()
	var updated int64
	var afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return updated, err
		}
		evs, err := st.ListEventsForReprice(h, afterID, batch)
		if err != nil {
			return updated, err
		}
		if len(evs) == 0 {
			return updated, nil
		}
		ids := make([]int64, 0, len(evs))
		usd := make([]float64, 0, len(evs))
		usd1h := make([]float64, 0, len(evs))
		for i := range evs {
			afterID = evs[i].ID
			u, u1, ok := t.Cost(&evs[i])
			if !ok {
				continue
			}
			ids, usd, usd1h = append(ids, evs[i].ID), append(usd, u), append(usd1h, u1)
		}
		if len(ids) > 0 {
			if err := st.UpdateEventPrices(ids, usd, usd1h, h); err != nil {
				return updated, err
			}
			updated += int64(len(ids))
		}
	}
}

// RunSync blocks until ctx ends, syncing one minute after start and then
// every interval; a sync that changes prices is followed by a reprice.
// Errors are logged, never returned. Run it in its own goroutine.
func (c *Catalog) RunSync(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	timer := time.NewTimer(c.firstSyncDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		changed, err := c.SyncNow(ctx)
		switch {
		case err != nil:
			c.log("warn", "pricing: sync failed", map[string]any{"error": err.Error()})
		case changed:
			n, err := c.Reprice(ctx, nil, DefaultRepriceBatch)
			if err != nil {
				c.log("warn", "pricing: reprice failed", map[string]any{"error": err.Error(), "updated": n})
			} else {
				c.log("info", "pricing: repriced events", map[string]any{"updated": n})
			}
		}
		timer.Reset(every)
	}
}
