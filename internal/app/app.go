// Package app is the plugin's method dispatcher and lifecycle owner. The cgo
// layer hands it (method, payload) pairs; it answers with envelopes.
//
// Lifecycle model: everything that depends on configuration lives in one
// immutable runtime that register/reconfigure builds whole and swaps in
// atomically. Request-path handlers read the current runtime pointer and
// never take the lifecycle lock, so they cannot block behind a store open
// or a reconfigure, and they never observe a half-applied config.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/api"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/config"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/engine"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/ingest"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/poll"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/pricing"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

// PluginID is the store id, route prefix and menu key.
const PluginID = "cpa-subscription-value"

// App holds the plugin state for one host process.
type App struct {
	version string
	host    *abi.Host

	// lifecycle serializes register, reconfigure and Shutdown.
	lifecycle sync.Mutex
	rt        atomic.Pointer[runtime]
	startedAt time.Time
	schema    atomic.Uint32

	dropped     atomic.Int64
	lastEventNS atomic.Int64
}

// runtime is one configured instance: built whole, swapped in, torn down whole.
type runtime struct {
	cfg     config.Config
	store   *store.Store
	catalog *pricing.Catalog
	engine  *engine.Engine
	poller  *poll.Poller
	router  *api.Router
	pricer  ingest.Pricer

	ingest chan domain.UsageRecord
	// stop closes to tell the writer to drain and exit; writerDone closes
	// when it has.
	stop       chan struct{}
	writerDone chan struct{}

	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup
}

// New builds an App that is idle until plugin.register arrives.
func New(version string, host *abi.Host) *App {
	return &App{version: version, host: host}
}

// Handle dispatches one ABI method.
func (a *App) Handle(method string, payload []byte) ([]byte, error) {
	switch method {
	case abi.MethodPluginRegister, abi.MethodPluginReconfigure:
		return a.register(payload)
	case abi.MethodPluginQuiesce, abi.MethodPluginShutdown:
		a.Shutdown()
		return abi.OK(struct{}{})
	case abi.MethodUsageHandle:
		return a.usage(payload)
	case abi.MethodManagementRegister:
		return abi.OK(a.registration())
	case abi.MethodManagementHandle:
		return a.management(payload)
	default:
		return abi.Fail("unknown_method", "unknown method: "+method), nil
	}
}

func (a *App) metadata() abi.Metadata {
	return abi.Metadata{
		Name:             "Subscription Value",
		Version:          a.version,
		Author:           "tim-mcdonnell",
		GitHubRepository: "https://github.com/tim-mcdonnell/cpa-subscription-value",
		ConfigFields: []abi.ConfigField{
			{Name: "data_dir", Type: "string", Description: "Directory for the SQLite database (mount it in Docker). Default /CLIProxyAPI/data/cpa-subscription-value"},
			{Name: "poll_interval_minutes", Type: "integer", Description: "Minutes between provider usage-endpoint polls per account; 0 disables. Default 20, minimum 5"},
			{Name: "retention_days", Type: "integer", Description: "Days of per-request history to keep. Default 365"},
			{Name: "claude_cache_write_multiplier", Type: "number", Description: "Price of Claude cache writes relative to input: 1.25 (5m cache) or 2.0 (1h cache). Default 1.25"},
			{Name: "price_sync_hours", Type: "integer", Description: "Hours between models.dev price syncs; 0 disables. Default 24"},
			{Name: "log_level", Type: "enum", EnumValues: []string{"info", "debug"}, Description: "Verbosity of plugin log lines"},
		},
	}
}

func (a *App) register(payload []byte) ([]byte, error) {
	var req abi.LifecycleRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	cfg, err := config.Parse(req.ConfigYAML)
	if err != nil {
		return nil, err
	}
	a.schema.Store(abi.NegotiateSchema(req.SchemaVersion))
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()
	if a.startedAt.IsZero() {
		a.startedAt = time.Now()
	}
	old := a.rt.Load()
	if old != nil && old.cfg == cfg {
		return a.registrationResponse()
	}
	// The old runtime is stopped before the new store opens so two runtimes
	// never hold the same database, and so a reconfigure is all-or-nothing.
	a.rt.Store(nil)
	a.teardown(old)
	rt, err := a.build(cfg, old == nil)
	if err != nil {
		return nil, err
	}
	a.rt.Store(rt)
	a.log("info", "plugin registered", map[string]any{"version": a.version, "schema": a.schema.Load(), "data_dir": cfg.DataDir})
	return a.registrationResponse()
}

func (a *App) registrationResponse() ([]byte, error) {
	return abi.OK(abi.Registration{
		SchemaVersion: a.schema.Load(),
		Metadata:      a.metadata(),
		Capabilities:  abi.Capabilities{UsagePlugin: true, ManagementAPI: true},
	})
}

// build opens the store and starts the writer and background work. It is
// called with the lifecycle lock held and no runtime installed.
func (a *App) build(cfg config.Config, firstStart bool) (*runtime, error) {
	st, err := store.Open(dbPath(cfg.DataDir))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	rt := &runtime{cfg: cfg, store: st}
	rt.catalog = pricing.NewCatalog(st, cfg, a.host, a.log)
	if err := rt.catalog.Load(); err != nil {
		a.log("warn", "load price snapshot failed; using built-in table", map[string]any{"error": err.Error()})
	}
	rt.pricer = rt.catalog
	rt.engine = engine.New(st, rt.catalog, a.log, engine.Options{Retention: time.Duration(cfg.RetentionDays) * 24 * time.Hour})
	if firstStart {
		// Only the process start is a restart from the estimator's point of
		// view; a reconfigure keeps the same process and loses nothing.
		_ = st.SetSetting("plugin_started_at", a.startedAt)
		rt.engine.RecordRestart(a.startedAt)
	}
	rt.router = api.New(PluginID, api.Deps{Store: st, Health: a.health, Catalog: rt.catalog, Actions: a})

	rt.ingest = make(chan domain.UsageRecord, cfg.IngestBuffer)
	rt.stop = make(chan struct{})
	rt.writerDone = make(chan struct{})
	go a.writer(rt)

	ctx, cancel := context.WithCancel(context.Background())
	rt.bgCancel = cancel
	if a.host != nil && cfg.PollInterval() > 0 {
		rt.poller = poll.New(st, a.host, a.log, cfg.PollInterval())
		rt.bgWG.Add(1)
		go func() { defer rt.bgWG.Done(); rt.poller.Run(ctx) }()
	}
	rt.bgWG.Add(1)
	go func() { defer rt.bgWG.Done(); rt.engine.Run(ctx) }()
	if cfg.PriceSyncHours > 0 {
		rt.bgWG.Add(1)
		go func() { defer rt.bgWG.Done(); rt.catalog.RunSync(ctx, time.Duration(cfg.PriceSyncHours)*time.Hour) }()
	}
	return rt, nil
}

// teardown stops background work, drains the writer and closes the store.
// Called with the lifecycle lock held after the runtime was uninstalled, so
// no new usage records can reach it.
func (a *App) teardown(rt *runtime) {
	if rt == nil {
		return
	}
	rt.bgCancel()
	rt.bgWG.Wait()
	close(rt.stop)
	<-rt.writerDone
	_ = rt.store.Close()
}

func dbPath(dir string) string { return dir + "/subscription-value.db" }

// Shutdown drains the ingest queue and closes the store. Safe to call twice.
func (a *App) Shutdown() {
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()
	a.teardown(a.rt.Swap(nil))
}

// usage answers usage.handle. It must not block: the host calls every usage
// sink serially on one goroutine.
func (a *App) usage(payload []byte) ([]byte, error) {
	var rec domain.UsageRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		return nil, fmt.Errorf("decode usage record: %w", err)
	}
	rt := a.rt.Load()
	if rt == nil {
		a.dropped.Add(1)
		return abi.OK(struct{}{})
	}
	select {
	case rt.ingest <- rec:
	default:
		a.dropped.Add(1)
	}
	return abi.OK(struct{}{})
}

// writer is the only goroutine that touches the store from the ingest path.
// After stop it drains whatever is queued, so nothing accepted is lost.
func (a *App) writer(rt *runtime) {
	defer close(rt.writerDone)
	for {
		select {
		case rec := <-rt.ingest:
			a.persist(rt, rec)
		case <-rt.stop:
			for {
				select {
				case rec := <-rt.ingest:
					a.persist(rt, rec)
				default:
					return
				}
			}
		}
	}
}

func (a *App) persist(rt *runtime, rec domain.UsageRecord) {
	res, ok := ingest.Normalize(rec, rt.pricer)
	if !ok {
		return
	}
	acct, err := rt.store.UpsertAccount(domain.Account{
		Provider:  res.Provider,
		AuthIndex: res.Event.AuthIndex,
		AuthID:    res.Event.AuthID,
		PlanType:  res.Extras.PlanType,
		LastSeen:  res.Event.ObservedAt,
	})
	if err != nil {
		a.log("warn", "upsert account failed", map[string]any{"error": err.Error(), "auth_index": res.Event.AuthIndex})
		return
	}
	res.Event.AccountID = acct.ID
	for i := range res.Readings {
		res.Readings[i].AccountID = acct.ID
	}
	inserted, err := rt.store.InsertEvent(&res.Event, res.Readings)
	if err != nil {
		a.log("warn", "insert event failed", map[string]any{"error": err.Error(), "dedup_key": res.Event.DedupKey})
		return
	}
	if !inserted {
		return
	}
	a.lastEventNS.Store(res.Event.ObservedAt.UnixNano())
	rt.engine.NoteEvent(res.Event.ObservedAt)
	for _, r := range res.Readings {
		rt.engine.MarkDirty(acct.ID, r.MeterKey)
	}
	if rt.cfg.LogLevel == "debug" {
		a.log("debug", "event recorded", map[string]any{"provider": res.Provider, "model": res.Event.Model, "meters": len(res.Readings)})
	}
}

func (a *App) registration() abi.ManagementRegistration {
	rt := a.rt.Load()
	if rt == nil {
		return abi.ManagementRegistration{}
	}
	return rt.router.Registration()
}

func (a *App) management(payload []byte) ([]byte, error) {
	var req abi.ManagementRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("decode management request: %w", err)
	}
	rt := a.rt.Load()
	if rt == nil {
		return abi.OK(abi.ErrorResponse(503, "plugin not registered"))
	}
	return abi.OK(rt.router.Handle(req))
}

func (a *App) health() api.Health {
	var cfg any
	var queued int64
	if rt := a.rt.Load(); rt != nil {
		cfg = rt.cfg
		queued = int64(len(rt.ingest))
	}
	var last time.Time
	if ns := a.lastEventNS.Load(); ns > 0 {
		last = time.Unix(0, ns)
	}
	return api.Health{
		PluginVersion: a.version,
		SchemaVersion: a.schema.Load(),
		StartedAt:     a.startedAt,
		IngestQueued:  queued,
		IngestDropped: a.dropped.Load(),
		LastEventAt:   last,
		Config:        cfg,
	}
}

// Actions exposed to the management API. Each works on the runtime current
// at the call. A reconfigure during a long call tears the old runtime down
// underneath it; the call then fails with a closed-store error, which the
// API reports, rather than the host crashing.

var errNotRegistered = fmt.Errorf("plugin not registered")

// Recompute runs the estimator for one meter now.
func (a *App) Recompute(accountID int64, meterKey string) error {
	rt := a.rt.Load()
	if rt == nil {
		return errNotRegistered
	}
	return rt.engine.Recompute(accountID, meterKey)
}

// PollNow runs one poll round and returns its summary.
func (a *App) PollNow(ctx context.Context) (any, error) {
	rt := a.rt.Load()
	if rt == nil {
		return nil, errNotRegistered
	}
	if rt.poller == nil {
		return nil, fmt.Errorf("polling is disabled or no host is attached")
	}
	return rt.poller.PollOnce(ctx), nil
}

// SyncPrices fetches models.dev now.
func (a *App) SyncPrices(ctx context.Context) (bool, error) {
	rt := a.rt.Load()
	if rt == nil {
		return false, errNotRegistered
	}
	return rt.catalog.SyncNow(ctx)
}

// Reprice recomputes stored costs under the active price table.
func (a *App) Reprice(ctx context.Context) (int64, error) {
	rt := a.rt.Load()
	if rt == nil {
		return 0, errNotRegistered
	}
	return rt.catalog.Reprice(ctx, rt.store, pricing.DefaultRepriceBatch)
}

// Reconfigured is called after a settings change through the API. Settings
// live in the store and are read on use, so nothing needs restarting.
func (a *App) Reconfigured() {}

func (a *App) log(level, msg string, fields map[string]any) {
	if a.host == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["plugin"] = PluginID
	a.host.Log(level, msg, fields)
}
