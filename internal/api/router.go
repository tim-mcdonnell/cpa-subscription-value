// Package api serves the management routes (JSON, management-key protected by
// the host) and the unauthenticated dashboard resource. It speaks only to the
// Store interface below so it can be tested without SQLite.
package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/web"
)

// Stats is the store's row counts and size, surfaced by /health.
type Stats struct {
	Accounts, Events, Readings int64
	OldestEvent, NewestEvent   time.Time
	DBBytes                    int64
}

// EventQuery selects usage events. AccountID 0 means all accounts; zero From/To
// mean unbounded; Limit <= 0 means the store's default.
type EventQuery struct {
	AccountID int64
	From, To  time.Time
	Limit     int
	Desc      bool
}

// ReadingQuery selects meter readings. Empty MeterKey/Source mean any.
type ReadingQuery struct {
	AccountID        int64
	MeterKey, Source string
	From, To         time.Time
	Limit            int
	Desc             bool
}

// Store is the read side of internal/store that the API needs.
type Store interface {
	ListAccounts() ([]domain.Account, error)
	GetAccountByAuthIndex(string) (domain.Account, bool, error)
	ListEvents(EventQuery) ([]domain.UsageEvent, error)
	ListReadings(ReadingQuery) ([]domain.MeterReading, error)
	Stats() (Stats, error)
}

// Health is the process-level state the app reports; the store adds its own.
type Health struct {
	PluginVersion string
	SchemaVersion uint32
	StartedAt     time.Time
	IngestQueued  int64
	IngestDropped int64
	LastEventAt   time.Time
	Config        any
}

// HealthFunc snapshots Health at request time.
type HealthFunc func() Health

// Router dispatches management and resource requests.
type Router struct {
	pluginID string
	store    Store
	health   HealthFunc
}

// New builds a Router. health may be nil.
func New(pluginID string, store Store, health HealthFunc) *Router {
	return &Router{pluginID: pluginID, store: store, health: health}
}

const (
	defaultLimit = 100
	maxLimit     = 5000
)

var managementPaths = []string{"/health", "/accounts", "/events", "/readings"}

// Registration answers management.register.
func (r *Router) Registration() abi.ManagementRegistration {
	routes := make([]abi.ManagementRoute, 0, len(managementPaths))
	for _, p := range managementPaths {
		routes = append(routes, abi.ManagementRoute{Method: http.MethodGet, Path: "/" + r.pluginID + p})
	}
	return abi.ManagementRegistration{
		Routes: routes,
		Resources: []abi.ResourceRoute{{
			Path:        "/dashboard",
			Menu:        "Subscription Value",
			Description: "API-equivalent $ value of Claude and Codex subscriptions",
		}},
	}
}

// Handle answers management.handle for both route kinds.
func (r *Router) Handle(req abi.ManagementRequest) abi.ManagementResponse {
	path, query := r.normalize(req)
	if path == "/dashboard" {
		return abi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}},
			Body:       web.Dashboard,
		}
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method != "" && method != http.MethodGet && method != http.MethodHead {
		for _, p := range managementPaths {
			if p == path {
				return abi.ErrorResponse(http.StatusMethodNotAllowed, "method not allowed")
			}
		}
	}
	switch path {
	case "/health":
		return r.handleHealth()
	case "/accounts":
		return r.handleAccounts()
	case "/events":
		return r.handleEvents(query)
	case "/readings":
		return r.handleReadings(query)
	}
	return abi.ErrorResponse(http.StatusNotFound, "not found")
}

// normalize strips the management/resource prefixes and the plugin id, and
// merges any query string that leaked into Path. Paths are matched by the
// suffix after "/<pluginID>" so both host path styles work.
func (r *Router) normalize(req abi.ManagementRequest) (string, url.Values) {
	q := url.Values{}
	for k, v := range req.Query {
		q[k] = v
	}
	p := req.Path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		if extra, err := url.ParseQuery(p[i+1:]); err == nil {
			for k, v := range extra {
				if _, ok := q[k]; !ok {
					q[k] = v
				}
			}
		}
		p = p[:i]
	}
	p = strings.TrimPrefix(p, "/v0/management")
	marker := "/" + r.pluginID
	if i := strings.Index(p, marker+"/"); i >= 0 {
		p = p[i+len(marker):]
	} else if strings.HasSuffix(p, marker) {
		p = "/"
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	if p == "" {
		p = "/"
	}
	return p, q
}

func (r *Router) handleHealth() abi.ManagementResponse {
	now := time.Now()
	var h Health
	if r.health != nil {
		h = r.health()
	}
	body := map[string]any{
		"ok":             true,
		"plugin_id":      r.pluginID,
		"plugin_version": h.PluginVersion,
		"schema_version": h.SchemaVersion,
		"started_at":     tsPtr(h.StartedAt),
		"uptime_s":       uptimeSeconds(h.StartedAt, now),
		"now":            now.UTC(),
		"ingest": map[string]any{
			"queued":        h.IngestQueued,
			"dropped":       h.IngestDropped,
			"last_event_at": tsPtr(h.LastEventAt),
		},
		"config": h.Config,
	}
	status := http.StatusOK
	if r.store == nil {
		body["ok"] = false
		body["error"] = "store not ready"
		status = http.StatusServiceUnavailable
	} else if st, err := r.store.Stats(); err != nil {
		body["ok"] = false
		body["error"] = "store stats: " + err.Error()
		status = http.StatusServiceUnavailable
	} else {
		body["store"] = map[string]any{
			"accounts":     st.Accounts,
			"events":       st.Events,
			"readings":     st.Readings,
			"oldest_event": tsPtr(st.OldestEvent),
			"newest_event": tsPtr(st.NewestEvent),
			"db_bytes":     st.DBBytes,
		}
	}
	return abi.JSONResponse(status, body)
}

func (r *Router) handleAccounts() abi.ManagementResponse {
	if r.store == nil {
		return abi.ErrorResponse(http.StatusServiceUnavailable, "store not ready")
	}
	accounts, err := r.store.ListAccounts()
	if err != nil {
		return abi.ErrorResponse(http.StatusInternalServerError, "list accounts: "+err.Error())
	}
	if accounts == nil {
		accounts = []domain.Account{}
	}
	return abi.JSONResponse(http.StatusOK, map[string]any{"accounts": accounts})
}

func (r *Router) handleEvents(q url.Values) abi.ManagementResponse {
	if r.store == nil {
		return abi.ErrorResponse(http.StatusServiceUnavailable, "store not ready")
	}
	acct, resp := r.resolveAccount(q.Get("auth_index"))
	if resp != nil {
		return *resp
	}
	window, resp := parseWindow(q)
	if resp != nil {
		return *resp
	}
	events, err := r.store.ListEvents(EventQuery{
		AccountID: acct.ID,
		From:      window.from,
		To:        window.to,
		Limit:     window.limit,
		Desc:      window.desc,
	})
	if err != nil {
		return abi.ErrorResponse(http.StatusInternalServerError, "list events: "+err.Error())
	}
	if events == nil {
		events = []domain.UsageEvent{}
	}
	return abi.JSONResponse(http.StatusOK, map[string]any{
		"account_id": acct.ID,
		"auth_index": acct.AuthIndex,
		"count":      len(events),
		"events":     events,
	})
}

func (r *Router) handleReadings(q url.Values) abi.ManagementResponse {
	if r.store == nil {
		return abi.ErrorResponse(http.StatusServiceUnavailable, "store not ready")
	}
	acct, resp := r.resolveAccount(q.Get("auth_index"))
	if resp != nil {
		return *resp
	}
	window, resp := parseWindow(q)
	if resp != nil {
		return *resp
	}
	readings, err := r.store.ListReadings(ReadingQuery{
		AccountID: acct.ID,
		MeterKey:  strings.TrimSpace(q.Get("meter")),
		Source:    strings.TrimSpace(q.Get("source")),
		From:      window.from,
		To:        window.to,
		Limit:     window.limit,
		Desc:      window.desc,
	})
	if err != nil {
		return abi.ErrorResponse(http.StatusInternalServerError, "list readings: "+err.Error())
	}
	if readings == nil {
		readings = []domain.MeterReading{}
	}
	return abi.JSONResponse(http.StatusOK, map[string]any{
		"account_id": acct.ID,
		"auth_index": acct.AuthIndex,
		"count":      len(readings),
		"readings":   readings,
	})
}

// resolveAccount maps auth_index to an account; empty means all accounts
// (zero Account). Unknown auth_index is a 404.
func (r *Router) resolveAccount(authIndex string) (domain.Account, *abi.ManagementResponse) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return domain.Account{}, nil
	}
	acct, ok, err := r.store.GetAccountByAuthIndex(authIndex)
	if err != nil {
		resp := abi.ErrorResponse(http.StatusInternalServerError, "lookup account: "+err.Error())
		return domain.Account{}, &resp
	}
	if !ok {
		resp := abi.ErrorResponse(http.StatusNotFound, "unknown auth_index")
		return domain.Account{}, &resp
	}
	return acct, nil
}

type window struct {
	from, to time.Time
	limit    int
	desc     bool
}

func parseWindow(q url.Values) (window, *abi.ManagementResponse) {
	var w window
	var err error
	if w.from, err = parseTime(q.Get("from")); err != nil {
		return w, bad("from: " + err.Error())
	}
	if w.to, err = parseTime(q.Get("to")); err != nil {
		return w, bad("to: " + err.Error())
	}
	if !w.from.IsZero() && !w.to.IsZero() && w.to.Before(w.from) {
		return w, bad("to is before from")
	}
	if w.limit, err = parseLimit(q.Get("limit")); err != nil {
		return w, bad("limit: " + err.Error())
	}
	switch strings.ToLower(strings.TrimSpace(q.Get("order"))) {
	case "", "desc":
		w.desc = true
	case "asc":
		w.desc = false
	default:
		return w, bad("order: want asc or desc")
	}
	return w, nil
}

func bad(msg string) *abi.ManagementResponse {
	resp := abi.ErrorResponse(http.StatusBadRequest, msg)
	return &resp
}

// parseTime accepts RFC3339 or unix seconds (milliseconds when > 1e12).
func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q (want RFC3339 or unix seconds)", s)
}

func parseLimit(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", s)
	}
	if n < 1 {
		return defaultLimit, nil
	}
	if n > maxLimit {
		return maxLimit, nil
	}
	return n, nil
}

func tsPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func uptimeSeconds(started, now time.Time) int64 {
	if started.IsZero() || now.Before(started) {
		return 0
	}
	return int64(now.Sub(started) / time.Second)
}
