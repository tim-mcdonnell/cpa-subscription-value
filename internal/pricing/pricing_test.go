package pricing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/config"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/ingest"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

var _ ingest.Pricer = (*Catalog)(nil)
var _ HTTPDoer = (*abi.Host)(nil)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestHashCanonical(t *testing.T) {
	a := Table{
		{domain.ProviderClaude, "claude-opus-5"}: {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite5m: 6.25, CacheWrite1h: 10, FastMult: 2, Source: "x"},
		{domain.ProviderCodex, "gpt-5.5"}:        {Input: 5, Output: 30, CacheRead: 0.5, LongCtxMult: 2},
	}
	// Built in the opposite order, different sources, sub-1e-9 noise, 0 vs 1 multipliers.
	b := Table{}
	b[Key{domain.ProviderCodex, "gpt-5.5"}] = Rates{Input: 5 + 1e-12, Output: 30, CacheRead: 0.5, LongCtxMult: 2, FastMult: 1, Source: "y"}
	b[Key{domain.ProviderClaude, "claude-opus-5"}] = Rates{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite5m: 6.25, CacheWrite1h: 10, FastMult: 2, LongCtxMult: 1}
	if a.Hash() != b.Hash() {
		t.Fatalf("hash not canonical: %s vs %s", a.Hash(), b.Hash())
	}
	if len(a.Hash()) != 16 {
		t.Fatalf("hash length %d", len(a.Hash()))
	}

	base := a.Hash()
	mutations := map[string]func(*Rates){
		"input":   func(r *Rates) { r.Input += 0.01 },
		"output":  func(r *Rates) { r.Output += 0.01 },
		"cr":      func(r *Rates) { r.CacheRead += 0.01 },
		"cw5m":    func(r *Rates) { r.CacheWrite5m += 0.01 },
		"cw1h":    func(r *Rates) { r.CacheWrite1h += 0.01 },
		"long":    func(r *Rates) { r.LongCtxMult = 1.5 },
		"fast":    func(r *Rates) { r.FastMult = 3 },
		"tiny":    func(r *Rates) { r.Input += 2e-9 },
		"no-fast": func(r *Rates) { r.FastMult = 0 },
	}
	for name, mut := range mutations {
		c := Table{}
		for k, r := range a {
			c[k] = r
		}
		k := Key{domain.ProviderClaude, "claude-opus-5"}
		r := c[k]
		mut(&r)
		c[k] = r
		if c.Hash() == base {
			t.Errorf("%s: hash unchanged", name)
		}
	}
	c := Table{{domain.ProviderClaude, "claude-opus-6"}: a[Key{domain.ProviderClaude, "claude-opus-5"}]}
	c[Key{domain.ProviderCodex, "gpt-5.5"}] = a[Key{domain.ProviderCodex, "gpt-5.5"}]
	if c.Hash() == base {
		t.Error("family rename: hash unchanged")
	}
}

func TestCostClaude(t *testing.T) {
	tbl := Builtin().withClaudeCacheWrite(1.25)
	ev := func(model string, mut func(*domain.UsageEvent)) *domain.UsageEvent {
		e := &domain.UsageEvent{Provider: domain.ProviderClaude, Model: model,
			UncachedInput: 1_000, CacheRead: 100_000, CacheWrite: 10_000, Output: 2_000, Reasoning: 1_500}
		if mut != nil {
			mut(e)
		}
		return e
	}
	cases := []struct {
		name       string
		ev         *domain.UsageEvent
		usd, usd1h float64
	}{
		// Opus 5: 1k·5 + 100k·0.5 + 2k·25 = 105_000; cw 10k·6.25 = 62_500 | 10k·10 = 100_000.
		{"opus-5", ev("claude-opus-5", nil), 0.1675, 0.205},
		// Opus 5.5 is its own row: 1k·4 + 100k·0.2 + 2k·20 = 64_000; cw 50_000 | 80_000.
		{"opus-5.5 dated", ev("claude-opus-5-5-20260901", nil), 0.114, 0.144},
		// Fast doubles every bucket on Opus 5.
		{"opus-5 fast", ev("claude-opus-5", func(e *domain.UsageEvent) { e.IsFast = true }), 0.335, 0.41},
		// Fable 5.1 reads at 0.25: 10k + 25k + 100k = 135_000; cw 125_000 | 200_000.
		{"fable-5.1", ev("claude-fable-5-1", nil), 0.26, 0.335},
		// Fable 5 reads at 1.0: 10k + 100k + 100k = 210_000.
		{"fable-5", ev("claude-fable-5", nil), 0.335, 0.41},
		// No long-context premium on current Claude models; fast not priced on Sonnet.
		{"sonnet-4.6 long fast", ev("claude-sonnet-4-6", func(e *domain.UsageEvent) { e.IsLong, e.IsFast = true, true }), 0.1005, 0.123},
		{"haiku alias", ev("Claude-Haiku-4-5-20251001", nil), 0.0335, 0.041},
		{"legacy opus 4.1", ev("claude-opus-4-1-20250805", nil), 0.5025, 0.615},
		{"routing prefix", ev("anthropic/claude-opus-4-8", nil), 0.1675, 0.205},
	}
	for _, c := range cases {
		usd, usd1h, ok := tbl.Cost(c.ev)
		if !ok || !near(usd, c.usd) || !near(usd1h, c.usd1h) {
			t.Errorf("%s: got (%.9f, %.9f, %v), want (%.9f, %.9f)", c.name, usd, usd1h, ok, c.usd, c.usd1h)
		}
	}

	// Configured 1h TTL: primary rate becomes 2× input, equal to the band's top.
	usd, usd1h, _ := Builtin().withClaudeCacheWrite(2.0).Cost(ev("claude-opus-5", nil))
	if !near(usd, 0.205) || !near(usd1h, 0.205) {
		t.Errorf("multiplier 2.0: got %.9f / %.9f", usd, usd1h)
	}
}

func TestCostCodex(t *testing.T) {
	tbl := Builtin()
	// Cached tokens are inside Codex input; ingest splits them into buckets.
	rec := domain.UsageRecord{Provider: "codex", Model: "gpt-5.5", Detail: domain.UsageDetail{
		InputTokens: 50_000, CachedTokens: 40_000, OutputTokens: 3_000, ReasoningTokens: 2_000}}
	res, ok := ingest.Normalize(rec, nil)
	if !ok {
		t.Fatal("normalize")
	}
	ev := res.Event
	// 10k·5 + 40k·0.5 + 3k·30 = 160_000 → $0.16; reasoning is inside output.
	if usd, usd1h, ok := tbl.Cost(&ev); !ok || !near(usd, 0.16) || !near(usd1h, 0.16) {
		t.Fatalf("gpt-5.5: %v %v %v", usd, usd1h, ok)
	}
	ev.IsFast = true
	if usd, _, _ := tbl.Cost(&ev); !near(usd, 0.4) {
		t.Errorf("gpt-5.5 fast: %v", usd)
	}
	ev.IsFast = false

	// 250k context is IsLong for ingest (>200k) but under OpenAI's 272k tier.
	ev.UncachedInput, ev.CacheRead, ev.IsLong = 50_000, 200_000, true
	if usd, _, _ := tbl.Cost(&ev); !near(usd, (50_000*5+200_000*0.5+3_000*30)/1e6) {
		t.Errorf("gpt-5.5 250k: %v", usd)
	}
	ev.UncachedInput = 100_000 // 300k context: input side doubles.
	if usd, _, _ := tbl.Cost(&ev); !near(usd, (100_000*10+200_000*1+3_000*30)/1e6) {
		t.Errorf("gpt-5.5 300k: %v", usd)
	}

	for model, want := range map[string]float64{
		"gpt-5.3-codex":             (1_000*1.75 + 1_000*14) / 1e6,
		"gpt-5.1-codex-mini":        (1_000*0.25 + 1_000*2) / 1e6,
		"gpt-5.6-sol-2026-03-01":    (1_000*4 + 1_000*20) / 1e6,
		"gpt-5.1-codex-max-xhigh":   (1_000*1.25 + 1_000*10) / 1e6,
		"codex-spark":               (1_000*1.75 + 1_000*14) / 1e6,
		"openai/gpt-6-astra-latest": (1_000*10 + 1_000*50) / 1e6,
	} {
		e := domain.UsageEvent{Provider: domain.ProviderCodex, Model: model, UncachedInput: 1_000, Output: 1_000}
		if usd, _, ok := tbl.Cost(&e); !ok || !near(usd, want) {
			t.Errorf("%s: got %v %v want %v", model, usd, ok, want)
		}
	}
}

func TestUnknownModel(t *testing.T) {
	tbl := Builtin()
	for _, e := range []domain.UsageEvent{
		{Provider: domain.ProviderClaude, Model: "claude-3-5-haiku-20241022", Output: 10},
		{Provider: domain.ProviderClaude, Model: "gpt-5.5", Output: 10},
		{Provider: domain.ProviderCodex, Model: "llama-4", Output: 10},
		{Provider: domain.ProviderCodex, Model: "", Output: 10},
	} {
		if _, _, ok := tbl.Cost(&e); ok {
			t.Errorf("%s/%s priced", e.Provider, e.Model)
		}
	}
	c := NewCatalog(nil, config.Defaults(), nil, nil)
	if _, _, h, ok := c.Price(&domain.UsageEvent{Provider: domain.ProviderCodex, Model: "llama-4"}); ok || h != "" {
		t.Errorf("catalog priced unknown model: %q %v", h, ok)
	}
}

type fakeDoer struct {
	body   []byte
	status int
	err    error
	calls  int
	url    string
}

func (f *fakeDoer) HTTPDo(req abi.HTTPRequest) (abi.HTTPResponse, error) {
	f.calls++
	f.url = req.URL
	if f.err != nil {
		return abi.HTTPResponse{}, f.err
	}
	st := f.status
	if st == 0 {
		st = 200
	}
	return abi.HTTPResponse{StatusCode: st, Body: f.body}, nil
}

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/models_dev_sample.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSyncParsesFixture(t *testing.T) {
	d := &fakeDoer{body: fixture(t)}
	tbl, raw, err := Sync(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if d.url != ModelsDevURL || len(raw) == 0 {
		t.Fatalf("url %q raw %d", d.url, len(raw))
	}
	check := func(p domain.Provider, key string, want Rates) {
		t.Helper()
		got, ok := tbl[Key{p, key}]
		if !ok {
			t.Errorf("%s/%s missing", p, key)
			return
		}
		got.Source = ""
		if !sameRates(got, want) {
			t.Errorf("%s/%s = %+v, want %+v", p, key, got, want)
		}
	}
	// Family rows come from the model whose id is the family.
	check(domain.ProviderClaude, "claude-opus-5", Rates{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite5m: 6.25, CacheWrite1h: 10, FastMult: 2})
	check(domain.ProviderClaude, "claude-fable-5", Rates{Input: 10, Output: 50, CacheRead: 1, CacheWrite5m: 12.5, CacheWrite1h: 20})
	// Differently priced checkpoints keep their own row.
	check(domain.ProviderClaude, "claude-opus-5-5", Rates{Input: 4, Output: 20, CacheRead: 0.2, CacheWrite5m: 5, CacheWrite1h: 8, FastMult: 2})
	check(domain.ProviderClaude, "claude-fable-5-1", Rates{Input: 10, Output: 50, CacheRead: 0.25, CacheWrite5m: 12.5, CacheWrite1h: 20})
	// No id equals claude-opus-4: max-priced member, all $5/$25.
	check(domain.ProviderClaude, "claude-opus-4", Rates{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite5m: 6.25, CacheWrite1h: 10})
	check(domain.ProviderClaude, "claude-sonnet-4", Rates{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite5m: 3.75, CacheWrite1h: 6})
	for _, k := range []string{"claude-opus-4-5", "claude-opus-4-5-20251101", "claude-sonnet-4-6", "claude-opus-4-8"} {
		if _, ok := tbl[Key{domain.ProviderClaude, k}]; ok {
			t.Errorf("redundant row %s", k)
		}
	}
	// OpenAI: long-context tier multiplier, fast policy, dated variant.
	check(domain.ProviderCodex, "gpt-5.5", Rates{Input: 5, Output: 30, CacheRead: 0.5, LongCtxMult: 2, FastMult: 2.5})
	check(domain.ProviderCodex, "gpt-5.6-sol", Rates{Input: 4, Output: 20, CacheRead: 0.4, CacheWrite5m: 5, CacheWrite1h: 5, LongCtxMult: 2, FastMult: 2.5})
	check(domain.ProviderCodex, "gpt-5.3-codex", Rates{Input: 1.75, Output: 14, CacheRead: 0.175, FastMult: 2})
	check(domain.ProviderCodex, "gpt-4o", Rates{Input: 2.5, Output: 10, CacheRead: 1.25, FastMult: 2})
	check(domain.ProviderCodex, "gpt-4o-2024-05-13", Rates{Input: 5, Output: 15, FastMult: 2})
	if _, ok := tbl[Key{domain.ProviderCodex, "chatgpt-image-latest"}]; ok {
		t.Error("model without cost was kept")
	}
	// Built-in fills what the fixture lacks.
	if r := tbl[Key{domain.ProviderCodex, "gpt-5.1-codex-mini"}]; r.Input != 0.25 || r.Source == "models.dev" {
		t.Errorf("fill row: %+v", r)
	}
	if r := tbl[Key{domain.ProviderClaude, "claude-opus-5"}]; r.Source != "models.dev" {
		t.Errorf("models.dev should win over builtin: %+v", r)
	}
}

func TestSyncErrors(t *testing.T) {
	for name, d := range map[string]*fakeDoer{
		"transport": {err: errors.New("boom")},
		"status":    {status: 503, body: []byte("{}")},
		"json":      {body: []byte("not json")},
		"provider":  {body: []byte(`{"openai":{"models":{}}}`)},
	} {
		if _, _, err := Sync(context.Background(), d); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Sync(ctx, blockingDoer{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: %v", err)
	}
}

type blockingDoer struct{}

func (blockingDoer) HTTPDo(abi.HTTPRequest) (abi.HTTPResponse, error) {
	time.Sleep(time.Hour)
	return abi.HTTPResponse{}, nil
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/x.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestCatalogLoadSyncReprice(t *testing.T) {
	st := openStore(t)
	cfg := config.Defaults()
	d := &fakeDoer{body: fixture(t)}
	c := NewCatalog(st, cfg, d, nil)

	if err := c.Load(); err != nil {
		t.Fatal(err)
	}
	_, hashA := c.Active()
	if snap, rows, ok, err := st.LatestPriceSnapshot(); err != nil || !ok || snap.Hash != hashA || len(rows) != len(builtinRows) {
		t.Fatalf("builtin not persisted: %v %v %s/%s %d", err, ok, snap.Hash, hashA, len(rows))
	}

	acct, err := st.UpsertAccount(domain.Account{Provider: domain.ProviderCodex, AuthIndex: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	// gpt-4o-2024-05-13 is only in models.dev, so it is unpriced under A.
	models := []string{"gpt-5.3-codex", "gpt-4o-2024-05-13", "llama-4"}
	for i := 0; i < 1203; i++ {
		ev := domain.UsageEvent{AccountID: acct.ID, Provider: domain.ProviderCodex, DedupKey: fmt.Sprint("k", i),
			Model: models[i%3], UncachedInput: 1_000, Output: 1_000, RequestedAt: time.Unix(int64(i), 0)}
		if usd, usd1h, h, ok := c.Price(&ev); ok {
			ev.APIUSD, ev.APIUSDCacheWrite1h, ev.PriceHash = usd, usd1h, h
		}
		if _, err := st.InsertEvent(&ev, nil); err != nil {
			t.Fatal(err)
		}
	}

	changed, err := c.SyncNow(context.Background())
	if err != nil || !changed {
		t.Fatalf("SyncNow: %v %v", changed, err)
	}
	_, hashB := c.Active()
	if hashB == hashA {
		t.Fatal("hash did not move")
	}
	snap, _, ok, err := st.GetPriceSnapshot(hashB)
	if err != nil || !ok || snap.Source != ModelsDevURL || len(snap.Raw) == 0 {
		t.Fatalf("snapshot B: %v %v %+v", err, ok, snap.Source)
	}

	n, err := c.Reprice(context.Background(), st, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 802 { // 401 gpt-5.3-codex + 401 gpt-4o-2024-05-13; llama stays unpriced
		t.Fatalf("repriced %d, want 802", n)
	}
	evs, err := st.ListEvents(store.EventQuery{Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		switch ev.Model {
		case "llama-4":
			if ev.PriceHash != "" {
				t.Fatalf("llama priced: %+v", ev)
			}
		case "gpt-4o-2024-05-13":
			if ev.PriceHash != hashB || !near(ev.APIUSD, 0.02) {
				t.Fatalf("gpt-4o: %s %v", ev.PriceHash, ev.APIUSD)
			}
		default:
			if ev.PriceHash != hashB || !near(ev.APIUSD, 0.01575) {
				t.Fatalf("codex: %s %v", ev.PriceHash, ev.APIUSD)
			}
		}
	}
	if n, err := c.Reprice(context.Background(), st, 100); err != nil || n != 0 {
		t.Fatalf("second reprice: %d %v", n, err)
	}

	// Same content again: nothing new stored, not changed.
	if changed, err := c.SyncNow(context.Background()); err != nil || changed {
		t.Fatalf("resync: %v %v", changed, err)
	}

	// A fresh catalog restores B from the store without fetching.
	d2 := &fakeDoer{err: errors.New("offline")}
	c2 := NewCatalog(st, cfg, d2, nil)
	if err := c2.Load(); err != nil {
		t.Fatal(err)
	}
	if _, h := c2.Active(); h != hashB || d2.calls != 0 {
		t.Fatalf("restored %s want %s (calls %d)", h, hashB, d2.calls)
	}

	// Changing the cache-write assumption recompiles to a new hash on Load.
	cfg2 := cfg
	cfg2.ClaudeCacheWriteMultiplier = 2.0
	c3 := NewCatalog(st, cfg2, d2, nil)
	if err := c3.Load(); err != nil {
		t.Fatal(err)
	}
	tbl3, h3 := c3.Active()
	if h3 == hashB || !near(tbl3[Key{domain.ProviderClaude, "claude-opus-5"}].CacheWrite5m, 10) {
		t.Fatalf("multiplier not applied: %s", h3)
	}
	if _, _, ok, _ := st.GetPriceSnapshot(h3); !ok {
		t.Fatal("recompiled table not persisted")
	}
	// Loading again under the same config converges on the same hash.
	c4 := NewCatalog(st, cfg2, d2, nil)
	if err := c4.Load(); err != nil {
		t.Fatal(err)
	}
	if _, h := c4.Active(); h != h3 {
		t.Fatalf("reload drifted: %s vs %s", h, h3)
	}
}

func TestRepriceCancelled(t *testing.T) {
	st := openStore(t)
	c := NewCatalog(st, config.Defaults(), &fakeDoer{}, nil)
	acct, _ := st.UpsertAccount(domain.Account{Provider: domain.ProviderClaude, AuthIndex: "a"})
	ev := domain.UsageEvent{AccountID: acct.ID, DedupKey: "k", Model: "claude-opus-5", Output: 1}
	if _, err := st.InsertEvent(&ev, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := c.Reprice(ctx, nil, 10); !errors.Is(err, context.Canceled) || n != 0 {
		t.Fatalf("got %d %v", n, err)
	}
}

func TestRunSync(t *testing.T) {
	st := openStore(t)
	d := &fakeDoer{body: fixture(t)}
	logs := make(chan string, 16)
	c := NewCatalog(st, config.Defaults(), d, func(level, msg string, _ map[string]any) { logs <- level + " " + msg })
	c.firstSyncDelay = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.RunSync(ctx, time.Hour); close(done) }()
	want := map[string]bool{"info pricing: synced models.dev": false, "info pricing: repriced events": false}
	for !want["info pricing: synced models.dev"] || !want["info pricing: repriced events"] {
		select {
		case m := <-logs:
			want[m] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("no sync logged: %v", want)
		}
	}
	cancel()
	<-done
}
