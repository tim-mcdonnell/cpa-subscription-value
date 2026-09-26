package api

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

var exportColumns = []string{"event_id", "requested_at", "observed_at", "model", "model_family", "failed", "status_code",
	"uncached_input", "cache_read", "cache_write", "output", "reasoning", "is_fast", "is_long",
	"api_usd", "api_usd_cw1h", "price_hash"}

// handleExport writes one CSV row per event in the cycle (or the from/to
// window, default everything) with one used_<meter> column per meter whose
// header reading rode on that event's response.
func (r *Router) handleExport(q url.Values, _ []byte) abi.ManagementResponse {
	acct, resp := r.resolveAccount(q.Get("auth_index"), true)
	if resp != nil {
		return *resp
	}
	id, resp := parseID("cycle_id", q.Get("cycle_id"))
	if resp != nil {
		return *resp
	}
	w, resp := parseWindow(url.Values{"from": q["from"], "to": q["to"]})
	if resp != nil {
		return *resp
	}
	from, to := w.from, w.to
	name := acct.AuthIndex
	if id > 0 {
		c, resp := r.pickCycle(acct, "", id)
		if resp != nil {
			return *resp
		}
		from, to = c.StartsAt, cycleEnd(c, r.now())
		name += "-cycle" + strconv.FormatInt(c.ID, 10)
	}
	events, err := r.loadEvents(acct.ID, from, to)
	if err != nil {
		return serverError("events", err)
	}
	// Header readings are joined by event_id; a minute of slack past the
	// window keeps the join independent of how their timestamps round.
	rto := to
	if !rto.IsZero() {
		rto = rto.Add(time.Minute)
	}
	readings, err := r.loadReadings(acct.ID, "", domain.SourceHeader, from, rto)
	if err != nil {
		return serverError("readings", err)
	}
	byEvent := map[int64]map[string]float64{}
	seen := map[string]bool{}
	var meters []string
	for _, m := range readings {
		if m.EventID == nil {
			continue
		}
		if byEvent[*m.EventID] == nil {
			byEvent[*m.EventID] = map[string]float64{}
		}
		byEvent[*m.EventID][m.MeterKey] = m.UsedFraction
		if !seen[m.MeterKey] {
			seen[m.MeterKey] = true
			meters = append(meters, m.MeterKey)
		}
	}
	meters = sortMeters(meters)

	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	header := append([]string{}, exportColumns...)
	for _, m := range meters {
		header = append(header, "used_"+m)
	}
	_ = cw.Write(header)
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	i64 := func(v int64) string { return strconv.FormatInt(v, 10) }
	for _, e := range events {
		row := []string{i64(e.ID), ts(e.RequestedAt), ts(e.ObservedAt), e.Model, e.ModelFamily, strconv.FormatBool(e.Failed),
			strconv.Itoa(e.StatusCode), i64(e.UncachedInput), i64(e.CacheRead), i64(e.CacheWrite), i64(e.Output),
			i64(e.Reasoning), strconv.FormatBool(e.IsFast), strconv.FormatBool(e.IsLong),
			num(e.APIUSD), num(e.APIUSDCacheWrite1h), e.PriceHash}
		for _, m := range meters {
			if u, ok := byEvent[e.ID][m]; ok {
				row = append(row, num(u))
			} else {
				row = append(row, "")
			}
		}
		_ = cw.Write(row)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return serverError("write csv", err)
	}
	return abi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":        {"text/csv; charset=utf-8"},
			"Content-Disposition": {fmt.Sprintf(`attachment; filename="subscription-value-%s.csv"`, name)},
			"Cache-Control":       {"no-store"},
		},
		Body: buf.Bytes(),
	}
}
