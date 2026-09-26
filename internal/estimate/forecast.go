package estimate

import (
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/meter"
)

// Forecast statuses.
const (
	StatusExhausted = "exhausted"
	StatusAhead     = "ahead"
	StatusOnTrack   = "on_track"
	StatusUnder     = "under"
)

// Forecast is §Remaining $, burn, forecast for one open cycle.
type Forecast struct {
	UNow         float64    `json:"u_now"`
	PTime        float64    `json:"p_time"`
	RemainingUSD float64    `json:"remaining_usd"`
	RemainingLo  float64    `json:"remaining_lo"`
	RemainingHi  float64    `json:"remaining_hi"`
	SpentUSD     float64    `json:"spent_usd"`   // u_now·V̂
	ProxiedUSD   float64    `json:"proxied_usd"` // Σ proxied $ this cycle
	BurnPerHour  float64    `json:"burn_per_hour"`
	PaceWindowH  float64    `json:"pace_window_h"`
	USDPerDay    float64    `json:"usd_per_day"`
	TExhaust     *time.Time `json:"t_exhaust,omitempty"`
	UEnd         float64    `json:"u_end"`
	UnusedUSD    float64    `json:"unused_usd"`
	Status       string     `json:"status"`
}

// ForecastCycle projects the cycle from its readings (any source, failed
// and quota_drop excluded) and V̂.
//
// u_now is the running max of those readings, which equals the latest
// reading except when the latest is a stale replica. Pace b_r = Δu over the
// trailing 6 h, widened to 24 h when that moved < 3 ticks; before the cycle
// started u was 0.
func ForecastCycle(c meter.CycleAssignment, v Result, proxiedUSD float64, now time.Time) Forecast {
	f := Forecast{ProxiedUSD: proxiedUSD}
	var pts []meter.AssignedReading
	var status string
	for _, r := range c.Readings {
		if r.Failed || r.QuotaDrop || r.ObservedAt.After(now) {
			continue
		}
		pts = append(pts, r)
		if r.Source == domain.SourceHeader {
			status = r.Status
		}
	}
	uAt := func(t time.Time) float64 {
		u := 0.0
		for _, r := range pts {
			if !r.ObservedAt.After(t) {
				u = max(u, r.UsedFraction)
			}
		}
		return u
	}
	f.UNow = uAt(now)
	win := time.Duration(c.WindowSec) * time.Second
	if win > 0 {
		f.PTime = now.Sub(c.StartsAt).Seconds() / win.Seconds()
	}
	pace := func(w time.Duration) (float64, time.Duration) {
		from := now.Add(-w)
		if from.Before(c.StartsAt) {
			from = c.StartsAt
		}
		dur := now.Sub(from)
		if dur <= 0 {
			return 0, 0
		}
		return (f.UNow - uAt(from)) / dur.Seconds(), dur
	}
	tick := c.Tick
	if tick == 0 {
		tick = 0.01
	}
	br, dur := pace(6 * time.Hour)
	if br*dur.Seconds() < 3*tick-1e-12 {
		br, dur = pace(24 * time.Hour)
	}
	f.BurnPerHour = br * 3600
	f.PaceWindowH = dur.Hours()

	rem := max(1-f.UNow, 0)
	f.RemainingUSD, f.RemainingLo, f.RemainingHi = rem*v.VHat, rem*v.CILo, rem*v.CIHi
	f.SpentUSD = f.UNow * v.VHat
	f.USDPerDay = br * v.VHat * 86400
	left := c.ResetAnchor.Sub(now).Seconds()
	f.UEnd = f.UNow
	if br > 0 {
		te := now.Add(time.Duration(rem / br * float64(time.Second)))
		f.TExhaust = &te
		f.UEnd = min(1, f.UNow+br*max(left, 0))
	}
	f.UnusedUSD = (1 - max(f.UEnd, f.UNow)) * v.VHat
	if f.UnusedUSD < 0 {
		f.UnusedUSD = 0
	}
	switch {
	case f.UNow >= 1 || status == "rejected":
		f.Status = StatusExhausted
	case f.UNow > f.PTime+0.10 && f.TExhaust != nil && f.TExhaust.Before(c.ResetAnchor):
		f.Status = StatusAhead
	case f.UNow < f.PTime-0.10:
		f.Status = StatusUnder
	default:
		// Within ±10% of linear pace, or ahead of it but slowing enough not
		// to exhaust before the reset.
		f.Status = StatusOnTrack
	}
	return f
}
