package sim

import "github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"

// Price is $ per million tokens. CacheWrite is the 5-minute rate (1.25×
// input), CacheWrite1h the 1-hour rate (2×); cache reads are 0.1× input.
type Price struct {
	In, Out, CacheRead, CacheWrite, CacheWrite1h float64
}

func anthropic(in, out float64) Price {
	return Price{In: in, Out: out, CacheRead: 0.1 * in, CacheWrite: 1.25 * in, CacheWrite1h: 2 * in}
}

func openai(in, out float64) Price {
	return Price{In: in, Out: out, CacheRead: 0.1 * in}
}

// Prices is the built-in table keyed by model family. Only consistency
// matters: the generator and Pricer read the same numbers.
var Prices = map[string]Price{
	"claude-opus-5":    anthropic(4, 20),
	"claude-sonnet-5":  anthropic(2, 10),
	"claude-fable-5":   anthropic(10, 50),
	"gpt-5.6-sol":      openai(1.25, 10),
	"gpt-5.6-sol-mini": openai(0.25, 2),
}

// PriceHash identifies the table on every event.
const PriceHash = "sim-v1"

// Token types, in the order used by typeUSD.
const (
	TypeUncachedInput = "uncached_input"
	TypeCacheRead     = "cache_read"
	TypeCacheWrite    = "cache_write"
	TypeOutput        = "output"
)

// typeUSD prices each token bucket: [uncached, cache_read, cache_write, output].
func typeUSD(family string, un, cr, cw, out int64) [4]float64 {
	p := Prices[family]
	return [4]float64{
		float64(un) * p.In / 1e6,
		float64(cr) * p.CacheRead / 1e6,
		float64(cw) * p.CacheWrite / 1e6,
		float64(out) * p.Out / 1e6,
	}
}

// Pricer implements ingest.Pricer over Prices.
type Pricer struct{}

// Price freezes API $ onto an event from its normalized token buckets.
func (Pricer) Price(ev *domain.UsageEvent) (float64, float64, string, bool) {
	p, ok := Prices[ev.ModelFamily]
	if !ok {
		return 0, 0, "", false
	}
	t := typeUSD(ev.ModelFamily, ev.UncachedInput, ev.CacheRead, ev.CacheWrite, ev.Output)
	usd := t[0] + t[1] + t[2] + t[3]
	usd1h := usd - t[2] + float64(ev.CacheWrite)*p.CacheWrite1h/1e6
	return usd, usd1h, PriceHash, true
}
