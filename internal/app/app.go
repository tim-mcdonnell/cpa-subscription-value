// Package app is the plugin's method dispatcher and lifecycle owner. The cgo
// layer hands it (method, payload) pairs; it answers with envelopes.
package app

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/api"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/config"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/ingest"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

// PluginID is the store id, route prefix and menu key.
const PluginID = "cpa-subscription-value"

// App holds the plugin state for one host process.
type App struct {
	version string
	host    *abi.Host

	mu        sync.Mutex
	cfg       config.Config
	schema    uint32
	store     *store.Store
	router    *api.Router
	pricer    ingest.Pricer
	startedAt time.Time

	// ingest is the hand-off between HandleUsage (must return fast) and the
	// single writer goroutine.
	ingest   chan domain.UsageRecord
	writerWG sync.WaitGroup
	stop     chan struct{}
	running  bool

	queued      atomic.Int64
	dropped     atomic.Int64
	lastEventNS atomic.Int64
}

// New builds an App that is idle until plugin.register arrives.
func New(version string, host *abi.Host) *App {
	return &App{version: version, host: host, pricer: ingest.NoPricer{}}
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
	a.mu.Lock()
	defer a.mu.Unlock()
	a.schema = abi.NegotiateSchema(req.SchemaVersion)
	if err := a.applyConfigLocked(cfg); err != nil {
		return nil, err
	}
	return abi.OK(abi.Registration{
		SchemaVersion: a.schema,
		Metadata:      a.metadata(),
		Capabilities:  abi.Capabilities{UsagePlugin: true, ManagementAPI: true},
	})
}

// applyConfigLocked (re)opens the store when the data dir changes and starts
// the writer. It is idempotent so reconfigure is safe.
func (a *App) applyConfigLocked(cfg config.Config) error {
	if a.store != nil && a.cfg.DataDir == cfg.DataDir {
		a.cfg = cfg
		return nil
	}
	if a.store != nil {
		a.stopWriterLocked()
		_ = a.store.Close()
		a.store = nil
	}
	st, err := store.Open(dbPath(cfg.DataDir))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	a.cfg = cfg
	a.store = st
	a.router = api.New(PluginID, storeShim{st}, a.health)
	if a.startedAt.IsZero() {
		a.startedAt = time.Now()
	}
	_ = st.SetSetting("plugin_started_at", a.startedAt)
	a.startWriterLocked()
	a.log("info", "plugin registered", map[string]any{"version": a.version, "schema": a.schema, "data_dir": cfg.DataDir})
	return nil
}

func dbPath(dir string) string { return dir + "/subscription-value.sqlite" }

func (a *App) startWriterLocked() {
	if a.running {
		return
	}
	a.ingest = make(chan domain.UsageRecord, a.cfg.IngestBuffer)
	a.stop = make(chan struct{})
	a.running = true
	a.writerWG.Add(1)
	go a.writer(a.ingest, a.stop, a.store)
}

func (a *App) stopWriterLocked() {
	if !a.running {
		return
	}
	close(a.stop)
	a.running = false
	a.mu.Unlock()
	a.writerWG.Wait()
	a.mu.Lock()
}

// Shutdown drains the ingest queue and closes the store. Safe to call twice.
func (a *App) Shutdown() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopWriterLocked()
	if a.store != nil {
		_ = a.store.Close()
		a.store = nil
		a.router = nil
	}
}

// usage answers usage.handle. It must not block: the host calls every usage
// sink serially on one goroutine.
func (a *App) usage(payload []byte) ([]byte, error) {
	var rec domain.UsageRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		return nil, fmt.Errorf("decode usage record: %w", err)
	}
	a.mu.Lock()
	ch, running := a.ingest, a.running
	a.mu.Unlock()
	if !running {
		a.dropped.Add(1)
		return abi.OK(struct{}{})
	}
	select {
	case ch <- rec:
		a.queued.Add(1)
	default:
		a.dropped.Add(1)
	}
	return abi.OK(struct{}{})
}

// writer is the only goroutine that touches the store from the ingest path.
func (a *App) writer(ch <-chan domain.UsageRecord, stop <-chan struct{}, st *store.Store) {
	defer a.writerWG.Done()
	for {
		select {
		case rec := <-ch:
			a.persist(st, rec)
		case <-stop:
			for {
				select {
				case rec := <-ch:
					a.persist(st, rec)
				default:
					return
				}
			}
		}
	}
}

func (a *App) persist(st *store.Store, rec domain.UsageRecord) {
	a.queued.Add(-1)
	res, ok := ingest.Normalize(rec, a.pricer)
	if !ok {
		return
	}
	acct, err := st.UpsertAccount(domain.Account{
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
	inserted, err := st.InsertEvent(&res.Event, res.Readings)
	if err != nil {
		a.log("warn", "insert event failed", map[string]any{"error": err.Error(), "dedup_key": res.Event.DedupKey})
		return
	}
	if inserted {
		a.lastEventNS.Store(res.Event.ObservedAt.UnixNano())
		if a.cfg.LogLevel == "debug" {
			a.log("debug", "event recorded", map[string]any{"provider": res.Provider, "model": res.Event.Model, "meters": len(res.Readings)})
		}
	}
}

func (a *App) registration() abi.ManagementRegistration {
	a.mu.Lock()
	r := a.router
	a.mu.Unlock()
	if r == nil {
		return abi.ManagementRegistration{}
	}
	return r.Registration()
}

func (a *App) management(payload []byte) ([]byte, error) {
	var req abi.ManagementRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("decode management request: %w", err)
	}
	a.mu.Lock()
	r := a.router
	a.mu.Unlock()
	if r == nil {
		return abi.OK(abi.ErrorResponse(503, "plugin not registered"))
	}
	return abi.OK(r.Handle(req))
}

func (a *App) health() api.Health {
	a.mu.Lock()
	cfg, schema, started := a.cfg, a.schema, a.startedAt
	a.mu.Unlock()
	var last time.Time
	if ns := a.lastEventNS.Load(); ns > 0 {
		last = time.Unix(0, ns)
	}
	return api.Health{
		PluginVersion: a.version,
		SchemaVersion: schema,
		StartedAt:     started,
		IngestQueued:  a.queued.Load(),
		IngestDropped: a.dropped.Load(),
		LastEventAt:   last,
		Config:        cfg,
	}
}

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

// storeShim adapts store.Store to api.Store; the query structs are
// field-compatible by construction.
type storeShim struct{ s *store.Store }

func (s storeShim) ListAccounts() ([]domain.Account, error) { return s.s.ListAccounts() }
func (s storeShim) GetAccountByAuthIndex(idx string) (domain.Account, bool, error) {
	return s.s.GetAccountByAuthIndex(idx)
}
func (s storeShim) ListEvents(q api.EventQuery) ([]domain.UsageEvent, error) {
	return s.s.ListEvents(store.EventQuery(q))
}
func (s storeShim) ListReadings(q api.ReadingQuery) ([]domain.MeterReading, error) {
	return s.s.ListReadings(store.ReadingQuery(q))
}
func (s storeShim) Stats() (api.Stats, error) {
	st, err := s.s.Stats()
	return api.Stats(st), err
}
