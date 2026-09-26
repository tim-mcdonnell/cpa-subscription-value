// Package api serves the management routes (JSON, management-key protected by
// the host) and the unauthenticated dashboard resource. Reads go straight to
// the store; anything that mutates state or runs a job goes through Actions,
// which the app implements.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/poll"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/pricing"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
	"github.com/tim-mcdonnell/cpa-subscription-value/web"
)

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

// Actions are the jobs the POST routes trigger. The app owns scheduling and
// locking; every method may block for the length of the job.
type Actions interface {
	// Recompute re-derives one meter of one account synchronously.
	Recompute(accountID int64, meterKey string) error
	// PollNow runs one poll round and returns its summary (JSON-encodable).
	PollNow(ctx context.Context) (any, error)
	// SyncPrices fetches models.dev and activates the table.
	SyncPrices(ctx context.Context) (changed bool, err error)
	// Reprice rewrites frozen $ of events not under the active hash.
	Reprice(ctx context.Context) (updated int64, err error)
	// Reconfigured tells the app that /settings changed.
	Reconfigured()
}

// Deps is everything the router reads from or delegates to.
type Deps struct {
	Store   *store.Store     // required
	Health  HealthFunc       // may be nil
	Catalog *pricing.Catalog // may be nil → price routes 503
	Actions Actions          // may be nil → POST routes 503 (settings still save)
}

// Router dispatches management and resource requests.
type Router struct {
	pluginID string
	d        Deps
	now      func() time.Time
}

// New builds a Router.
func New(pluginID string, d Deps) *Router {
	return &Router{pluginID: pluginID, d: d, now: time.Now}
}

// RecomputeSettingKey is where the app records when it last recomputed a
// meter (value: a JSON time). /health reports it.
func RecomputeSettingKey(authIndex, meterKey string) string {
	return "last_recompute_" + authIndex + "_" + meterKey
}

const (
	defaultLimit = 100
	maxLimit     = 5000
	jobTimeout   = 5 * time.Minute
)

type handlerFunc func(r *Router, q url.Values, body []byte) abi.ManagementResponse

type route struct {
	method, path string
	h            handlerFunc
}

// routes is the management surface, in registration order.
var routes = []route{
	{http.MethodGet, "/health", (*Router).handleHealth},
	{http.MethodGet, "/accounts", (*Router).handleAccounts},
	{http.MethodGet, "/events", (*Router).handleEvents},
	{http.MethodGet, "/readings", (*Router).handleReadings},
	{http.MethodGet, "/summary", (*Router).handleSummary},
	{http.MethodGet, "/cycles", (*Router).handleCycles},
	{http.MethodGet, "/series", (*Router).handleSeries},
	{http.MethodGet, "/weights", (*Router).handleWeights},
	{http.MethodGet, "/prices", (*Router).handlePrices},
	{http.MethodPost, "/prices/sync", (*Router).handlePriceSync},
	{http.MethodPost, "/prices/reprice", (*Router).handleReprice},
	{http.MethodGet, "/settings", (*Router).handleSettings},
	{http.MethodPost, "/settings", (*Router).handleSettingsPost},
	{http.MethodPost, "/recompute", (*Router).handleRecompute},
	{http.MethodPost, "/poll", (*Router).handlePoll},
	{http.MethodGet, "/export", (*Router).handleExport},
}

// Registration answers management.register.
func (r *Router) Registration() abi.ManagementRegistration {
	out := make([]abi.ManagementRoute, 0, len(routes))
	for _, rt := range routes {
		out = append(out, abi.ManagementRoute{Method: rt.method, Path: "/" + r.pluginID + rt.path})
	}
	return abi.ManagementRegistration{
		Routes: out,
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
	if method == "" || method == http.MethodHead {
		method = http.MethodGet
	}
	known := false
	for _, rt := range routes {
		if rt.path != path {
			continue
		}
		known = true
		if rt.method != method {
			continue
		}
		if r.d.Store == nil && path != "/health" {
			return abi.ErrorResponse(http.StatusServiceUnavailable, "store not ready")
		}
		return rt.h(r, query, req.Body)
	}
	if known {
		return abi.ErrorResponse(http.StatusMethodNotAllowed, "method not allowed")
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

func (r *Router) handleHealth(url.Values, []byte) abi.ManagementResponse {
	now := r.now()
	var h Health
	if r.d.Health != nil {
		h = r.d.Health()
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
	st := r.d.Store
	if st == nil {
		body["ok"] = false
		body["error"] = "store not ready"
		return abi.JSONResponse(http.StatusServiceUnavailable, body)
	}
	stats, err := st.Stats()
	if err != nil {
		body["ok"] = false
		body["error"] = "store stats: " + err.Error()
		return abi.JSONResponse(http.StatusServiceUnavailable, body)
	}
	body["store"] = map[string]any{
		"accounts":     stats.Accounts,
		"events":       stats.Events,
		"readings":     stats.Readings,
		"oldest_event": tsPtr(stats.OldestEvent),
		"newest_event": tsPtr(stats.NewestEvent),
		"db_bytes":     stats.DBBytes,
	}
	accounts, err := st.ListAccounts()
	if err != nil {
		body["ok"] = false
		body["error"] = "list accounts: " + err.Error()
		return abi.JSONResponse(http.StatusServiceUnavailable, body)
	}
	polls := []map[string]any{}
	var recomputes []map[string]any
	for _, a := range accounts {
		var ps poll.Status
		row := map[string]any{"auth_index": a.AuthIndex, "provider": a.Provider, "status": nil}
		if ok, err := st.GetSetting(poll.SettingKey(a.AuthIndex), &ps); err == nil && ok {
			row["status"] = ps
		}
		polls = append(polls, row)
		meters, _ := st.MeterKeys(a.ID)
		for _, m := range meters {
			if at, ok := r.lastRecompute(a, m); ok {
				recomputes = append(recomputes, map[string]any{"auth_index": a.AuthIndex, "meter": m, "last_recompute_at": at})
			}
		}
	}
	body["polls"] = polls
	if recomputes != nil {
		body["estimator"] = map[string]any{"recomputes": recomputes}
	}
	if p, ok := r.priceInfo(); ok {
		body["prices"] = p
	}
	return abi.JSONResponse(http.StatusOK, body)
}

// lastRecompute reads RecomputeSettingKey, also accepting the account id in
// place of the auth index.
func (r *Router) lastRecompute(a domain.Account, meterKey string) (time.Time, bool) {
	for _, k := range []string{RecomputeSettingKey(a.AuthIndex, meterKey), RecomputeSettingKey(strconv.FormatInt(a.ID, 10), meterKey)} {
		var at time.Time
		if ok, err := r.d.Store.GetSetting(k, &at); err == nil && ok && !at.IsZero() {
			return at.UTC(), true
		}
	}
	return time.Time{}, false
}

// accountView is an account plus the settings and meters the UI needs.
type accountView struct {
	domain.Account
	CoverageMode string   `json:"coverage_mode"`
	Meters       []string `json:"meters"`
	// WeightMeters are the meters with a stored weight fit (/weights is 404
	// for the others).
	WeightMeters []string `json:"weight_meters"`
}

func (r *Router) handleAccounts(url.Values, []byte) abi.ManagementResponse {
	accounts, err := r.d.Store.ListAccounts()
	if err != nil {
		return serverError("list accounts", err)
	}
	out := make([]accountView, 0, len(accounts))
	for _, a := range accounts {
		v, err := r.accountView(a)
		if err != nil {
			return serverError("account "+a.AuthIndex, err)
		}
		out = append(out, v)
	}
	return abi.JSONResponse(http.StatusOK, map[string]any{"accounts": out})
}

func (r *Router) accountView(a domain.Account) (accountView, error) {
	meters, err := r.d.Store.MeterKeys(a.ID)
	if err != nil {
		return accountView{}, err
	}
	if meters == nil {
		meters = []string{}
	}
	v := accountView{Account: a, CoverageMode: r.coverageMode(a.AuthIndex), Meters: sortMeters(meters), WeightMeters: []string{}}
	for _, m := range v.Meters {
		if _, ok, err := r.d.Store.LatestWeightFit(a.ID, m); err != nil {
			return accountView{}, err
		} else if ok {
			v.WeightMeters = append(v.WeightMeters, m)
		}
	}
	return v, nil
}

func (r *Router) handleEvents(q url.Values, _ []byte) abi.ManagementResponse {
	acct, resp := r.resolveAccount(q.Get("auth_index"), false)
	if resp != nil {
		return *resp
	}
	w, resp := parseWindow(q)
	if resp != nil {
		return *resp
	}
	events, err := r.d.Store.ListEvents(store.EventQuery{AccountID: acct.ID, From: w.from, To: w.to, Limit: w.limit, Desc: w.desc})
	if err != nil {
		return serverError("list events", err)
	}
	if events == nil {
		events = []domain.UsageEvent{}
	}
	return abi.JSONResponse(http.StatusOK, map[string]any{
		"account_id": acct.ID, "auth_index": acct.AuthIndex, "count": len(events), "events": events,
	})
}

func (r *Router) handleReadings(q url.Values, _ []byte) abi.ManagementResponse {
	acct, resp := r.resolveAccount(q.Get("auth_index"), false)
	if resp != nil {
		return *resp
	}
	w, resp := parseWindow(q)
	if resp != nil {
		return *resp
	}
	readings, err := r.d.Store.ListReadings(store.ReadingQuery{
		AccountID: acct.ID,
		MeterKey:  strings.TrimSpace(q.Get("meter")),
		Source:    strings.TrimSpace(q.Get("source")),
		From:      w.from, To: w.to, Limit: w.limit, Desc: w.desc,
	})
	if err != nil {
		return serverError("list readings", err)
	}
	if readings == nil {
		readings = []domain.MeterReading{}
	}
	return abi.JSONResponse(http.StatusOK, map[string]any{
		"account_id": acct.ID, "auth_index": acct.AuthIndex, "count": len(readings), "readings": readings,
	})
}

// resolveAccount maps auth_index to an account. Empty means all accounts
// (zero Account) unless required, which makes it a 400. Unknown is a 404.
func (r *Router) resolveAccount(authIndex string, required bool) (domain.Account, *abi.ManagementResponse) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		if required {
			return domain.Account{}, bad("auth_index is required")
		}
		return domain.Account{}, nil
	}
	acct, ok, err := r.d.Store.GetAccountByAuthIndex(authIndex)
	if err != nil {
		resp := serverError("lookup account", err)
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
	if w.limit, err = parseLimit(q.Get("limit"), defaultLimit, maxLimit); err != nil {
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

func serverError(what string, err error) abi.ManagementResponse {
	return abi.ErrorResponse(http.StatusInternalServerError, what+": "+err.Error())
}

func unavailable(msg string) abi.ManagementResponse {
	return abi.ErrorResponse(http.StatusServiceUnavailable, msg)
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

// parseLimit reads a positive integer, def when empty or < 1, capped at max.
func parseLimit(s string, def, max int) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", s)
	}
	if n < 1 {
		return def, nil
	}
	return min(n, max), nil
}

// parseID reads an optional positive int64 parameter; 0 when empty.
func parseID(name, s string) (int64, *abi.ManagementResponse) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, bad(name + ": want a positive integer")
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

func jobContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), jobTimeout)
}

// decodeBody unmarshals a JSON request body; an empty body is an error.
func decodeBody(body []byte, v any) *abi.ManagementResponse {
	if len(strings.TrimSpace(string(body))) == 0 {
		return bad("request body is empty")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return bad("invalid JSON body: " + err.Error())
	}
	return nil
}
