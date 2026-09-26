package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/pricing"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/weights"
)

// ---- weights ----

type factorView struct {
	Key string `json:"key"`
	weights.Factor
	Kind string `json:"kind"` // model | type | fast | long
}

type modelValue struct {
	Family      string  `json:"family"`
	Factor      float64 `json:"factor"`
	Status      string  `json:"status"`
	ValuePer100 float64 `json:"value_per_100"`
	CILo        float64 `json:"ci_lo"`
	CIHi        float64 `json:"ci_hi"`
}

func (r *Router) handleWeights(q url.Values, _ []byte) abi.ManagementResponse {
	acct, resp := r.resolveAccount(q.Get("auth_index"), true)
	if resp != nil {
		return *resp
	}
	m := r.meterParam(q)
	w, ok, err := r.d.Store.LatestWeightFit(acct.ID, m)
	if err != nil {
		return serverError("weight fit", err)
	}
	if !ok {
		return abi.ErrorResponse(http.StatusNotFound, "no weight fit yet for "+acct.AuthIndex+"/"+m)
	}
	fit, err := weights.FromStore(w)
	if err != nil {
		return serverError("decode weight fit", err)
	}
	factors := make([]factorView, 0, len(fit.Factors))
	identified := false
	for k, f := range fit.Factors {
		kind, name, _ := strings.Cut(k, ":")
		if name == "" {
			name = kind
		}
		f.Name = name
		factors = append(factors, factorView{Key: k, Factor: f, Kind: kind})
		identified = identified || f.Status == weights.StatusIdentified
	}
	order := map[string]int{"model": 0, "type": 1, "fast": 2, "long": 3}
	sort.Slice(factors, func(i, j int) bool {
		if a, b := order[factors[i].Kind], order[factors[j].Kind]; a != b {
			return a < b
		}
		return factors[i].Key < factors[j].Key
	})
	models := []modelValue{}
	for _, f := range factors {
		if f.Kind != "model" {
			continue
		}
		v, ok := fit.ModelValuePer100(f.Name)
		if !ok {
			continue
		}
		lo, hi, _ := fit.ModelValueCI(f.Name)
		models = append(models, modelValue{Family: f.Name, Factor: f.Estimate, Status: f.Status, ValuePer100: v, CILo: lo, CIHi: hi})
	}
	return abi.JSONResponse(http.StatusOK, map[string]any{
		"auth_index": acct.AuthIndex, "provider": acct.Provider, "meter": m,
		"computed_at": fit.ComputedAt.UTC(), "anchor": fit.Anchor, "lag": fit.Lag, "loss": fit.Loss,
		"segments": fit.Segments, "eligible": fit.Eligible, "unknown_families": nonNil(fit.UnknownFamilies),
		"backtest": fit.Backtest, "lag_ambiguous": fit.LagAmbiguous, "price_hash": w.PriceHash,
		"identified_any": identified, "factors": factors, "scales": nonNil(fit.Scales), "models": models,
	})
}

// ---- prices ----

type priceRow struct {
	Provider string `json:"provider"`
	Family   string `json:"model_family"`
	pricing.Rates
}

// priceInfo summarizes the active table for /health.
func (r *Router) priceInfo() (map[string]any, bool) {
	if r.d.Catalog == nil {
		return nil, false
	}
	t, h := r.d.Catalog.Active()
	info := map[string]any{"hash": h, "families": len(t), "source": nil, "synced_at": nil}
	if snap, _, ok, err := r.d.Store.GetPriceSnapshot(h); err == nil && ok {
		info["source"], info["synced_at"] = snap.Source, snap.CreatedAt.UTC()
	}
	return info, true
}

func (r *Router) handlePrices(url.Values, []byte) abi.ManagementResponse {
	if r.d.Catalog == nil {
		return unavailable("price catalog not ready")
	}
	info, _ := r.priceInfo()
	t, _ := r.d.Catalog.Active()
	rows := make([]priceRow, 0, len(t))
	for k, v := range t {
		rows = append(rows, priceRow{Provider: string(k.Provider), Family: k.Family, Rates: v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		return rows[i].Family < rows[j].Family
	})
	info["unit"] = "usd_per_mtok"
	info["rows"] = rows
	return abi.JSONResponse(http.StatusOK, info)
}

func (r *Router) handlePriceSync(url.Values, []byte) abi.ManagementResponse {
	if r.d.Catalog == nil || r.d.Actions == nil {
		return unavailable("price sync not available")
	}
	ctx, cancel := jobContext()
	defer cancel()
	changed, err := r.d.Actions.SyncPrices(ctx)
	if err != nil {
		return abi.ErrorResponse(http.StatusBadGateway, "price sync: "+err.Error())
	}
	_, h := r.d.Catalog.Active()
	return abi.JSONResponse(http.StatusOK, map[string]any{"changed": changed, "hash": h})
}

func (r *Router) handleReprice(url.Values, []byte) abi.ManagementResponse {
	if r.d.Catalog == nil || r.d.Actions == nil {
		return unavailable("reprice not available")
	}
	ctx, cancel := jobContext()
	defer cancel()
	n, err := r.d.Actions.Reprice(ctx)
	if err != nil {
		return abi.JSONResponse(http.StatusInternalServerError, map[string]any{"error": "reprice: " + err.Error(), "updated": n})
	}
	_, h := r.d.Catalog.Active()
	return abi.JSONResponse(http.StatusAccepted, map[string]any{"updated": n, "hash": h})
}

// ---- settings ----

// displayPrefs are dashboard preferences, stored under displayKey.
type displayPrefs struct {
	WowView        string `json:"wow_view"`     // both | claude | codex
	CyclesShown    int    `json:"cycles_shown"` // week-over-week window
	ShowCW1h       bool   `json:"show_cw1h"`    // show the 1-hour cache-write band
	RefreshSeconds int    `json:"refresh_s"`
}

const displayKey = "display_prefs"

func defaultDisplay() displayPrefs {
	return displayPrefs{WowView: "both", CyclesShown: 12, RefreshSeconds: 60}
}

func (p displayPrefs) validate() string {
	switch {
	case !slices.Contains([]string{"both", "claude", "codex"}, p.WowView):
		return "display.wow_view: want both, claude or codex"
	case p.CyclesShown < 2 || p.CyclesShown > 104:
		return "display.cycles_shown: want 2..104"
	case p.RefreshSeconds < 15 || p.RefreshSeconds > 3600:
		return "display.refresh_s: want 15..3600"
	}
	return ""
}

func (r *Router) display() displayPrefs {
	p := defaultDisplay()
	if ok, err := r.d.Store.GetSetting(displayKey, &p); err != nil || !ok || p.validate() != "" {
		return defaultDisplay()
	}
	return p
}

func (r *Router) settingsBody() (map[string]any, error) {
	accounts, err := r.d.Store.ListAccounts()
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(accounts))
	for _, a := range accounts {
		mode := r.coverageMode(a.AuthIndex)
		rows = append(rows, map[string]any{
			"auth_index": a.AuthIndex, "provider": a.Provider, "email": a.Email,
			"coverage_mode": mode, "capacity_disabled_reason": capacityDisabledReason(mode),
		})
	}
	return map[string]any{"accounts": rows, "coverage_modes": coverageModes, "display": r.display()}, nil
}

func (r *Router) handleSettings(url.Values, []byte) abi.ManagementResponse {
	body, err := r.settingsBody()
	if err != nil {
		return serverError("settings", err)
	}
	return abi.JSONResponse(http.StatusOK, body)
}

// settingsUpdate is the POST /settings body; both parts are optional.
// coverage maps auth_index → mode; display fields not given keep their value.
type settingsUpdate struct {
	Coverage map[string]string `json:"coverage"`
	Display  json.RawMessage   `json:"display"`
}

func (r *Router) handleSettingsPost(_ url.Values, body []byte) abi.ManagementResponse {
	var u settingsUpdate
	if resp := decodeBody(body, &u); resp != nil {
		return *resp
	}
	for idx, mode := range u.Coverage {
		if !slices.Contains(coverageModes, mode) {
			return *bad("coverage[" + idx + "]: want one of " + strings.Join(coverageModes, ", "))
		}
		if _, resp := r.resolveAccount(idx, true); resp != nil {
			return *resp
		}
	}
	prefs := r.display()
	if len(u.Display) > 0 && string(u.Display) != "null" {
		dec := json.NewDecoder(strings.NewReader(string(u.Display)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&prefs); err != nil {
			return *bad("display: " + err.Error())
		}
		if msg := prefs.validate(); msg != "" {
			return *bad(msg)
		}
	}
	for idx, mode := range u.Coverage {
		if err := r.d.Store.SetSetting(coverageKey(idx), mode); err != nil {
			return serverError("save coverage", err)
		}
	}
	if len(u.Display) > 0 {
		if err := r.d.Store.SetSetting(displayKey, prefs); err != nil {
			return serverError("save display", err)
		}
	}
	if r.d.Actions != nil {
		r.d.Actions.Reconfigured()
	}
	return r.handleSettings(nil, nil)
}

// ---- jobs ----

func (r *Router) handleRecompute(q url.Values, _ []byte) abi.ManagementResponse {
	if r.d.Actions == nil {
		return unavailable("recompute not available")
	}
	acct, resp := r.resolveAccount(q.Get("auth_index"), true)
	if resp != nil {
		return *resp
	}
	keys, err := r.d.Store.MeterKeys(acct.ID)
	if err != nil {
		return serverError("meters", err)
	}
	meters := summaryMeters(keys)
	if m := strings.TrimSpace(q.Get("meter")); m != "" {
		if !slices.Contains(keys, m) {
			return abi.ErrorResponse(http.StatusNotFound, "no readings for meter "+m)
		}
		meters = []string{m}
	}
	for _, m := range meters {
		if err := r.d.Actions.Recompute(acct.ID, m); err != nil {
			return serverError("recompute "+m, err)
		}
	}
	s, err := r.summarize(acct, r.now())
	if err != nil {
		return serverError("summary", err)
	}
	return abi.JSONResponse(http.StatusOK, s)
}

func (r *Router) handlePoll(url.Values, []byte) abi.ManagementResponse {
	if r.d.Actions == nil {
		return unavailable("poll not available")
	}
	ctx, cancel := jobContext()
	defer cancel()
	started := time.Now()
	res, err := r.d.Actions.PollNow(ctx)
	if err != nil {
		return abi.ErrorResponse(http.StatusBadGateway, "poll: "+err.Error())
	}
	return abi.JSONResponse(http.StatusOK, map[string]any{"took_ms": time.Since(started).Milliseconds(), "result": res})
}
