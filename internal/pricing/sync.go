package pricing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/ingest"
)

// ModelsDevURL is the price source of truth at runtime.
const ModelsDevURL = "https://models.dev/api.json"

// HTTPDoer is satisfied by *abi.Host, whose HTTPDo goes out through CPA's
// transport (proxy settings included). StdHTTP is the fallback without a host.
type HTTPDoer interface {
	HTTPDo(abi.HTTPRequest) (abi.HTTPResponse, error)
}

// StdHTTP performs requests with net/http.
type StdHTTP struct{ Client *http.Client }

func (s StdHTTP) HTTPDo(req abi.HTTPRequest) (abi.HTTPResponse, error) {
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	hr, err := http.NewRequest(req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return abi.HTTPResponse{}, err
	}
	for k, vs := range req.Headers {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	resp, err := c.Do(hr)
	if err != nil {
		return abi.HTTPResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return abi.HTTPResponse{}, err
	}
	return abi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}, nil
}

// models.dev shape: {"<provider>": {"models": {"<id>": {"id", "cost": {...}}}}}.
// cost.tiers holds context-length tiers (OpenAI gpt-5.4+: size 272000).
type mdProvider struct {
	Models map[string]mdModel `json:"models"`
}

type mdModel struct {
	ID   string  `json:"id"`
	Cost *mdCost `json:"cost"`
}

type mdCost struct {
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  float64  `json:"cache_read"`
	CacheWrite float64  `json:"cache_write"`
	Tiers      []mdTier `json:"tiers"`
}

type mdTier struct {
	Input float64 `json:"input"`
	Tier  struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
	} `json:"tier"`
}

var mdProviders = map[string]domain.Provider{
	"anthropic": domain.ProviderClaude,
	"openai":    domain.ProviderCodex,
}

// Sync fetches models.dev and builds a list-price table from its anthropic
// and openai providers, filled by Builtin. raw is the two providers' JSON
// only, for the snapshot's raw_json (the full file is ~5 MB).
func Sync(ctx context.Context, doer HTTPDoer) (Table, []byte, error) {
	if doer == nil {
		doer = StdHTTP{}
	}
	type result struct {
		resp abi.HTTPResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := doer.HTTPDo(abi.HTTPRequest{
			Method:  http.MethodGet,
			URL:     ModelsDevURL,
			Headers: http.Header{"Accept": {"application/json"}},
		})
		ch <- result{resp, err}
	}()
	var res result
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case res = <-ch:
	}
	if res.err != nil {
		return nil, nil, fmt.Errorf("pricing: fetch models.dev: %w", res.err)
	}
	if res.resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("pricing: fetch models.dev: status %d", res.resp.StatusCode)
	}
	return parseModelsDev(res.resp.Body)
}

func parseModelsDev(body []byte) (Table, []byte, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, nil, fmt.Errorf("pricing: parse models.dev: %w", err)
	}
	kept := map[string]json.RawMessage{}
	builtin := Builtin()
	t := Table{}
	for name, p := range mdProviders {
		raw, ok := all[name]
		if !ok {
			return nil, nil, fmt.Errorf("pricing: models.dev has no %q provider", name)
		}
		kept[name] = raw
		var prov mdProvider
		if err := json.Unmarshal(raw, &prov); err != nil {
			return nil, nil, fmt.Errorf("pricing: parse models.dev %s: %w", name, err)
		}
		for k, r := range collapse(p, prov) {
			t[k] = withPolicy(p, k.Family, r, builtin)
		}
	}
	if len(t) == 0 {
		return nil, nil, fmt.Errorf("pricing: models.dev listed no priced models")
	}
	for k, r := range builtin {
		if _, ok := t[k]; !ok {
			t[k] = r
		}
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return nil, nil, err
	}
	return t, raw, nil
}

// collapse groups models by ingest.ModelFamily. The family row is the model
// whose id is the family itself when listed, else the highest-priced member.
// Any member priced differently from that row also gets its own id as a key,
// so Lookup can price it exactly (Opus 5.5 is cheaper than Opus 5).
func collapse(p domain.Provider, prov mdProvider) Table {
	type member struct {
		id string
		r  Rates
	}
	groups := map[string][]member{}
	for id, m := range prov.Models {
		if m.Cost == nil || (m.Cost.Input <= 0 && m.Cost.Output <= 0) {
			continue
		}
		if m.ID != "" {
			id = m.ID
		}
		id = normalizeModel(id)
		c := m.Cost
		r := Rates{Input: c.Input, Output: c.Output, CacheRead: c.CacheRead, CacheWrite5m: c.CacheWrite,
			LongCtxMult: 1, Source: "models.dev"}
		for _, tier := range c.Tiers {
			if tier.Tier.Type == "context" && c.Input > 0 && tier.Input > c.Input {
				r.LongCtxMult = round9(tier.Input / c.Input)
			}
		}
		fam := ingest.ModelFamily(p, id)
		groups[fam] = append(groups[fam], member{id, r})
	}
	t := Table{}
	for fam, ms := range groups {
		sort.Slice(ms, func(i, j int) bool {
			si, sj := score(ms[i].r), score(ms[j].r)
			if si != sj {
				return si > sj
			}
			return ms[i].id < ms[j].id
		})
		head := ms[0]
		for _, m := range ms {
			if m.id == fam {
				head = m
			}
		}
		t[Key{p, fam}] = head.r
		for _, m := range ms {
			if m.id != fam && !sameRates(m.r, head.r) {
				t[Key{p, m.id}] = m.r
			}
		}
	}
	return t
}

func score(r Rates) float64 { return r.Input + r.Output + r.CacheRead + r.CacheWrite5m }

func sameRates(a, b Rates) bool {
	a.Source, b.Source = "", ""
	return Table{{}: a}.Hash() == Table{{}: b}.Hash()
}
