package meter

import (
	"slices"
	"sort"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

// Crossing is the first reading at a new running-max level (§Estimator
// "Crossings"). Delta is the level jump from the previous crossing, or from
// the baseline (the cycle's first eligible reading) for the first one.
type Crossing struct {
	ReadingID int64     `json:"reading_id"`
	EventID   int64     `json:"event_id,omitempty"`
	At        time.Time `json:"at"`
	Fraction  float64   `json:"fraction"`
	Level     int64     `json:"level"`
	Delta     int64     `json:"delta"`
}

// Crossings walks a cycle's readings of one source (header, or poll for the
// poll-only method), skipping failed attempts and quota_drop readings.
// level = floor(u/tick); a reading above the running max M is a crossing;
// equal or lower readings are stale and ignored, so levels are strictly
// increasing whatever order concurrent responses arrive in.
func Crossings(rs []AssignedReading, tick float64, source string) []Crossing {
	var out []Crossing
	m := int64(-1)
	for _, r := range rs {
		if r.Source != source || r.Failed || r.QuotaDrop {
			continue
		}
		lvl := Level(r.UsedFraction, tick)
		if m < 0 {
			m = lvl
			continue
		}
		if lvl <= m {
			continue
		}
		x := Crossing{ReadingID: r.ID, At: r.ObservedAt, Fraction: r.UsedFraction, Level: lvl, Delta: lvl - m}
		if r.EventID != nil {
			x.EventID = *r.EventID
		}
		out = append(out, x)
		m = lvl
	}
	return out
}

// TokenCounts are one family's tokens by type.
type TokenCounts struct {
	UncachedInput int64 `json:"uncached_input"`
	CacheRead     int64 `json:"cache_read"`
	CacheWrite    int64 `json:"cache_write"`
	Output        int64 `json:"output"`
}

// TypeUSD is API $ by token type.
type TypeUSD struct {
	UncachedInput float64 `json:"uncached_input"`
	CacheRead     float64 `json:"cache_read"`
	CacheWrite    float64 `json:"cache_write"`
	Output        float64 `json:"output"`
}

// Input is the input-side (registered at requested_at) share.
func (t TypeUSD) Input() float64 { return t.UncachedInput + t.CacheRead + t.CacheWrite }

// Add returns the element-wise sum.
func (t TypeUSD) Add(o TypeUSD) TypeUSD {
	return TypeUSD{t.UncachedInput + o.UncachedInput, t.CacheRead + o.CacheRead, t.CacheWrite + o.CacheWrite, t.Output + o.Output}
}

// SplitFunc apportions an event's frozen APIUSD over token types.
type SplitFunc func(ev domain.UsageEvent) TypeUSD

// RatioSplit apportions APIUSD by list-price ratios relative to uncached
// input: cache read 0.1×, cache write 1.25× (5-minute TTL), output 5× for
// Claude and 8× for Codex. These ratios hold across every current family of
// each vendor, so the split needs no absolute price table.
func RatioSplit(ev domain.UsageEvent) TypeUSD {
	outRatio := 5.0
	if ev.Provider == domain.ProviderCodex {
		outRatio = 8
	}
	u := float64(ev.UncachedInput)
	cr := 0.1 * float64(ev.CacheRead)
	cw := 1.25 * float64(ev.CacheWrite)
	o := outRatio * float64(ev.Output)
	sum := u + cr + cw + o
	if sum == 0 {
		return TypeUSD{UncachedInput: ev.APIUSD}
	}
	f := ev.APIUSD / sum
	return TypeUSD{u * f, cr * f, cw * f, o * f}
}

// Segment is the spend between two consecutive crossings under one lag.
// Its fields map onto store.Segment; Tokens/USDBy* become features_json.
type Segment struct {
	Lag             Lag                    `json:"lag"`
	Seq             int                    `json:"seq"`
	FromLevel       int64                  `json:"from_level"`
	ToLevel         int64                  `json:"to_level"`
	DeltaTicks      int64                  `json:"delta_ticks"`
	Tick            float64                `json:"tick"`
	FromReadingID   int64                  `json:"from_reading_id"`
	ToReadingID     int64                  `json:"to_reading_id"`
	TStart          time.Time              `json:"t_start"`
	TEnd            time.Time              `json:"t_end"`
	USD             float64                `json:"usd"`
	USDCacheWrite1h float64                `json:"usd_cw1h"`
	USDFailed       float64                `json:"usd_failed"`
	Tokens          map[string]TokenCounts `json:"tokens"`
	FastTokens      int64                  `json:"fast_tokens"`
	LongTokens      int64                  `json:"long_tokens"`
	USDByFamily     map[string]float64     `json:"usd_by_family"`
	USDByType       TypeUSD                `json:"usd_by_type"`
	NEvents         int                    `json:"n_events"`
	NFailed         int                    `json:"n_failed"`
	Flags           []string               `json:"flags"`
}

// D is the segment's meter movement Δ·tick.
func (s Segment) D() float64 { return float64(s.DeltaTicks) * s.Tick }

// Has reports whether the segment carries flag f.
func (s Segment) Has(f string) bool { return slices.Contains(s.Flags, f) }

// EventIndex is the scoped event list prepared once per meter: sorted by
// observed_at, split by type, and exploded into time-lag registrations.
type EventIndex struct {
	Events []domain.UsageEvent
	split  []TypeUSD
	pos    map[int64]int
	regs   []registration
}

type registration struct {
	t      time.Time
	ev     int
	output bool
}

// NewEventIndex sorts evs and precomputes the split (RatioSplit when nil).
func NewEventIndex(evs []domain.UsageEvent, split SplitFunc) *EventIndex {
	if split == nil {
		split = RatioSplit
	}
	ix := &EventIndex{Events: SortEvents(evs), pos: make(map[int64]int, len(evs))}
	ix.split = make([]TypeUSD, len(ix.Events))
	ix.regs = make([]registration, 0, 2*len(ix.Events))
	for i, e := range ix.Events {
		ix.pos[e.ID] = i
		ix.split[i] = split(e)
		ix.regs = append(ix.regs,
			registration{t: e.RequestedAt, ev: i},
			registration{t: e.RequestedAt.Add(e.Latency), ev: i, output: true})
	}
	slices.SortStableFunc(ix.regs, func(a, b registration) int { return a.t.Compare(b.t) })
	return ix
}

// Window returns the events observed in [from, to).
func (ix *EventIndex) Window(from, to time.Time) []domain.UsageEvent {
	lo := sort.Search(len(ix.Events), func(i int) bool { return !ix.Events[i].ObservedAt.Before(from) })
	hi := sort.Search(len(ix.Events), func(i int) bool { return !ix.Events[i].ObservedAt.Before(to) })
	return ix.Events[lo:hi]
}

// Active reports whether any registration falls in (from, to].
func (ix *EventIndex) Active(from, to time.Time) bool {
	i := sort.Search(len(ix.regs), func(i int) bool { return ix.regs[i].t.After(from) })
	return i < len(ix.regs) && !ix.regs[i].t.After(to)
}

// SegmentOptions tunes BuildSegments.
type SegmentOptions struct {
	// Horizon is when the data was read (now); segments ending within
	// IncompleteWindow of it may miss in-flight requests → lag_incomplete.
	Horizon          time.Time
	IncompleteWindow time.Duration
	// LongGap flags segments with a silence longer than this (2 h; 12 h for
	// scoped meters, whose events are sparse by construction).
	LongGap     time.Duration
	RestartGaps []Interval
}

// DefaultSegmentOptions are the plan's thresholds.
func DefaultSegmentOptions(scoped bool, horizon time.Time) SegmentOptions {
	o := SegmentOptions{Horizon: horizon, IncompleteWindow: 10 * time.Minute, LongGap: 2 * time.Hour}
	if scoped {
		o.LongGap = 12 * time.Hour
	}
	return o
}

type part int

const (
	partAll part = iota
	partInput
	partOutput
)

// BuildSegments attributes spend to each pair of consecutive crossings
// (§Estimator "Lag attribution"). Index lag L sums events with index in
// (i_{k−1}−L, i_k−L]; crossings whose event is not in ix are skipped for
// index lags, so their movement folds into the next segment. The time lag
// sums registrations in (obs_{k−1}, obs_k].
func BuildSegments(c CycleAssignment, xs []Crossing, ix *EventIndex, lag Lag, o SegmentOptions) []Segment {
	type point struct {
		Crossing
		idx int
	}
	L := lag.index()
	var pts []point
	for _, x := range xs {
		p := point{Crossing: x, idx: -1}
		if L >= 0 {
			i, ok := ix.pos[x.EventID]
			if x.EventID == 0 || !ok {
				continue
			}
			p.idx = i
		}
		pts = append(pts, p)
	}
	var drops []time.Time
	for _, r := range c.Readings {
		if r.QuotaDrop {
			drops = append(drops, r.ObservedAt)
		}
	}
	var out []Segment
	for k := 1; k < len(pts); k++ {
		prev, cur := pts[k-1], pts[k]
		s := Segment{
			Lag: lag, Seq: k - 1, FromLevel: prev.Level, ToLevel: cur.Level,
			DeltaTicks: cur.Level - prev.Level, Tick: c.Tick,
			FromReadingID: prev.ReadingID, ToReadingID: cur.ReadingID,
			TStart: prev.At, TEnd: cur.At,
			Tokens: map[string]TokenCounts{}, USDByFamily: map[string]float64{}, Flags: []string{},
		}
		if L >= 0 {
			for i := max(prev.idx-L+1, 0); i <= cur.idx-L; i++ {
				ix.accumulate(&s, i, partAll)
			}
		} else {
			j := sort.Search(len(ix.regs), func(j int) bool { return ix.regs[j].t.After(s.TStart) })
			for ; j < len(ix.regs) && !ix.regs[j].t.After(s.TEnd); j++ {
				p := partInput
				if ix.regs[j].output {
					p = partOutput
				}
				ix.accumulate(&s, ix.regs[j].ev, p)
			}
		}
		s.Flags = segmentFlags(c, drops, s, ix, o)
		out = append(out, s)
	}
	return out
}

func (ix *EventIndex) accumulate(s *Segment, i int, p part) {
	e := ix.Events[i]
	sp := ix.split[i]
	fam := e.ModelFamily
	if fam == "" {
		fam = "unknown"
	}
	tc := s.Tokens[fam]
	var usd, cw1h float64
	var toks int64
	cw1hTotal := e.APIUSDCacheWrite1h
	if cw1hTotal == 0 {
		cw1hTotal = e.APIUSD
	}
	if p != partOutput {
		tc.UncachedInput += e.UncachedInput
		tc.CacheRead += e.CacheRead
		tc.CacheWrite += e.CacheWrite
		toks += e.UncachedInput + e.CacheRead + e.CacheWrite
		usd += sp.Input()
		cw1h += max(cw1hTotal-sp.Output, 0)
		s.USDByType = s.USDByType.Add(TypeUSD{UncachedInput: sp.UncachedInput, CacheRead: sp.CacheRead, CacheWrite: sp.CacheWrite})
		s.NEvents++
		if e.Failed {
			s.NFailed++
		}
	}
	if p != partInput {
		tc.Output += e.Output
		toks += e.Output
		usd += sp.Output
		cw1h += sp.Output
		s.USDByType.Output += sp.Output
	}
	s.Tokens[fam] = tc
	s.USD += usd
	s.USDCacheWrite1h += cw1h
	s.USDByFamily[fam] += usd
	if e.Failed {
		s.USDFailed += usd
	}
	if e.IsFast {
		s.FastTokens += toks
	}
	if e.IsLong {
		s.LongTokens += toks
	}
}

func segmentFlags(c CycleAssignment, drops []time.Time, s Segment, ix *EventIndex, o SegmentOptions) []string {
	flags := []string{}
	for _, t := range drops {
		if t.After(s.TStart) && !t.After(s.TEnd) {
			flags = append(flags, FlagQuotaDrop)
			break
		}
	}
	for _, iv := range c.Ambiguous {
		if iv.overlaps(s.TStart, s.TEnd) {
			flags = addFlag(flags, FlagRegimeChange)
		}
	}
	for _, iv := range o.RestartGaps {
		if iv.overlaps(s.TStart, s.TEnd) {
			flags = addFlag(flags, FlagRestartGap)
		}
	}
	if !o.Horizon.IsZero() && s.TEnd.After(o.Horizon.Add(-o.IncompleteWindow)) {
		flags = append(flags, FlagLagIncomplete)
	}
	if o.LongGap > 0 && maxGap(ix.Window(s.TStart, s.TEnd), s.TStart, s.TEnd) > o.LongGap {
		flags = append(flags, FlagLongGap)
	}
	if s.USDFailed > 0 {
		flags = append(flags, FlagFailedTokens)
	}
	return flags
}

// maxGap is the longest silence between from, the events' observations, and to.
func maxGap(evs []domain.UsageEvent, from, to time.Time) time.Duration {
	last, best := from, time.Duration(0)
	for _, e := range evs {
		best = max(best, e.ObservedAt.Sub(last))
		last = e.ObservedAt
	}
	return max(best, to.Sub(last))
}
