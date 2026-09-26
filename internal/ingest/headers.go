package ingest

import (
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

const (
	claudePrefix = "anthropic-ratelimit-unified-"
	codexPrefix  = "x-codex-"
)

var claimRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// lowerHeaders flattens a header map to lowercase keys with the first
// non-empty value, so lookups ignore whatever canonicalization the host did.
func lowerHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		for _, v := range vs {
			if v = strings.TrimSpace(v); v != "" {
				out[strings.ToLower(k)] = v
				break
			}
		}
	}
	return out
}

// claudeReadings reads one meter per `<claim>-utilization` header. The claim
// (5h, 7d, 7d_oi, ...) is the meter key verbatim; utilization is a fraction.
func claudeReadings(h http.Header, observed time.Time) ([]domain.MeterReading, Extras) {
	lh := lowerHeaders(h)
	ex := Extras{
		RepresentativeClaim:   lh[claudePrefix+"representative-claim"],
		OverageStatus:         lh[claudePrefix+"overage-status"],
		OverageDisabledReason: lh[claudePrefix+"overage-disabled-reason"],
		UnifiedStatus:         lh[claudePrefix+"status"],
	}
	var out []domain.MeterReading
	for k, raw := range lh {
		claim, ok := strings.CutPrefix(k, claudePrefix)
		if !ok {
			continue
		}
		claim, ok = strings.CutSuffix(claim, "-utilization")
		if !ok || !claimRe.MatchString(claim) {
			continue
		}
		frac, ok := parseFraction(raw)
		if !ok {
			continue
		}
		r := domain.MeterReading{
			MeterKey:     claim,
			Source:       domain.SourceHeader,
			ObservedAt:   observed,
			UsedFraction: frac,
			Raw:          raw,
			PrecisionDP:  decimals(raw),
			Status:       lh[claudePrefix+claim+"-status"],
			WindowSec:    windowFromClaim(claim),
		}
		r.ResetAt, _ = parseTime(lh[claudePrefix+claim+"-reset"])
		out = append(out, r)
	}
	sortReadings(out)
	return out, ex
}

// codexLimit is one `x-codex-<prefix>-used-percent` header decomposed.
type codexLimit struct {
	prefix string // header segment between x-codex- and -used-percent
	name   string // "" for the account-wide limit, else the additional limit
	role   string // primary, secondary, or "" when the header carried none
}

// parseCodexPrefix handles the shapes CPA emits: `primary`, `secondary`,
// `additional-<name>[-<role>]` (websocket path), and legacy HTTP
// `<name>-<role>` (e.g. bengalfox-secondary, code-review-primary).
func parseCodexPrefix(p string) codexLimit {
	l := codexLimit{prefix: p}
	rest := strings.TrimPrefix(p, "additional-")
	switch rest {
	case "primary", "secondary":
		l.role = rest
		return l
	}
	for _, role := range []string{"primary", "secondary"} {
		if n, ok := strings.CutSuffix(rest, "-"+role); ok {
			l.name, l.role = n, role
			return l
		}
	}
	l.name = rest
	return l
}

// codexReadings reads one meter per `-used-percent` header. Account-wide
// meters are keyed by window length, not by primary/secondary role, because
// weekly can occupy primary when a plan has no 5h limit. Additional (feature
// scoped) limits keep their name and are never mapped onto 5h/7d.
func codexReadings(h http.Header, observed time.Time) ([]domain.MeterReading, Extras) {
	lh := lowerHeaders(h)
	ex := Extras{PlanType: lh[codexPrefix+"plan-type"]}

	type entry struct {
		lim codexLimit
		r   domain.MeterReading
		win int64
	}
	var entries []entry
	perName := map[string]int{}
	for k, raw := range lh {
		p, ok := strings.CutPrefix(k, codexPrefix)
		if !ok {
			continue
		}
		p, ok = strings.CutSuffix(p, "-used-percent")
		if !ok || p == "" {
			continue
		}
		pct, ok := parseFraction(raw)
		if !ok {
			continue
		}
		lim := parseCodexPrefix(p)
		base := codexPrefix + p
		var minutes int64
		if f, err := strconv.ParseFloat(lh[base+"-window-minutes"], 64); err == nil && f > 0 && !math.IsInf(f, 0) {
			minutes = int64(f)
		}
		r := domain.MeterReading{
			Source:       domain.SourceHeader,
			ObservedAt:   observed,
			UsedFraction: pct / 100,
			Raw:          raw,
			PrecisionDP:  decimals(raw) + 2,
			WindowSec:    minutes * 60,
		}
		if t, ok := parseTime(lh[base+"-reset-at"]); ok {
			r.ResetAt = t
		} else if s, err := strconv.ParseFloat(lh[base+"-reset-after-seconds"], 64); err == nil && s >= 0 && !math.IsInf(s, 0) {
			r.ResetAt = observed.Add(time.Duration(s * float64(time.Second)))
		}
		entries = append(entries, entry{lim: lim, r: r, win: minutes})
		perName[lim.name]++
	}

	out := make([]domain.MeterReading, 0, len(entries))
	for _, e := range entries {
		label := windowLabel(e.win)
		switch {
		case e.lim.name == "":
			e.r.MeterKey = label
		case perName[e.lim.name] > 1:
			// Same additional limit with two windows: keep them distinct.
			e.r.MeterKey = "additional:" + e.lim.name + ":" + label
		default:
			e.r.MeterKey = "additional:" + e.lim.name
		}
		out = append(out, e.r)
	}
	sortReadings(out)
	return out, ex
}

func windowLabel(minutes int64) string {
	switch minutes {
	case 300:
		return domain.MeterFiveHour
	case 10080:
		return domain.MeterSevenDay
	}
	return "win:" + strconv.FormatInt(minutes*60, 10)
}

var claimWindowRe = regexp.MustCompile(`^(\d+)([hdm])`)

// windowFromClaim reads the leading <n>h / <n>d / <n>m of a Claude claim;
// suffixes such as _oi do not change the window.
func windowFromClaim(claim string) int64 {
	m := claimWindowRe.FindStringSubmatch(claim)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	switch m[2] {
	case "m":
		return n * 60
	case "h":
		return n * 3600
	}
	return n * 86400
}

func parseFraction(raw string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, false
	}
	return f, true
}

// decimals counts digits after the point in the header's own spelling; that
// is the only evidence of how coarsely upstream rounded.
func decimals(raw string) int {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		raw = raw[:i]
	}
	i := strings.IndexByte(raw, '.')
	if i < 0 {
		return 0
	}
	return len(raw) - i - 1
}

// parseTime accepts unix seconds (or milliseconds when implausibly large),
// RFC3339 and HTTP-date; anything else yields false.
func parseTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		if f <= 0 || math.IsInf(f, 0) {
			return time.Time{}, false
		}
		if f > 1e12 {
			f /= 1000
		}
		sec := math.Floor(f)
		return time.Unix(int64(sec), int64((f-sec)*1e9)).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, true
	}
	if t, err := http.ParseTime(raw); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func sortReadings(rs []domain.MeterReading) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].MeterKey < rs[j].MeterKey })
}
