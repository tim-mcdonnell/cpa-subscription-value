package app

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
)

type recordingHost struct{ calls []string }

func (r *recordingHost) Call(method string, payload []byte) ([]byte, error) {
	r.calls = append(r.calls, method)
	return []byte(`{"ok":true,"result":{}}`), nil
}

func mustEnvelope(t *testing.T, a *App, method string, payload []byte) json.RawMessage {
	t.Helper()
	raw, err := a.Handle(method, payload)
	if err != nil {
		t.Fatal(err)
	}
	var env abi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	return env.Result
}

// The full lifecycle as the host drives it: register with a v7.2.122-style
// schema, receive a usage record in wire shape, then query it back through
// the management API.
func TestLifecycleRoundTrip(t *testing.T) {
	host := &recordingHost{}
	a := New("test", abi.NewHost(host))
	dir := t.TempDir()

	regReq, _ := json.Marshal(abi.LifecycleRequest{SchemaVersion: 2, ConfigYAML: []byte("data_dir: " + dir + "\nlog_level: debug\n")})
	res := mustEnvelope(t, a, abi.MethodPluginRegister, regReq)
	var reg abi.Registration
	if err := json.Unmarshal(res, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.SchemaVersion != 2 || !reg.Capabilities.UsagePlugin || !reg.Capabilities.ManagementAPI {
		t.Fatalf("registration %+v", reg)
	}

	// Wire shape: Go field names, RFC3339 time, nanosecond durations.
	usage := map[string]any{
		"Provider": "claude", "Model": "claude-opus-5-5", "AuthID": "claude-1.json", "AuthIndex": "0a1b2c3d", "AuthType": "oauth",
		"RequestedAt": time.Now().Add(-time.Minute).Format(time.RFC3339Nano), "Latency": int64(3 * time.Second), "TTFT": int64(400 * time.Millisecond),
		"Detail": map[string]int64{"InputTokens": 1200, "OutputTokens": 300, "CacheReadTokens": 8000, "CacheCreationTokens": 500, "CachedTokens": 8000, "TotalTokens": 10000},
		"ResponseHeaders": http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.63"},
			"Anthropic-Ratelimit-Unified-7d-Reset":       {"1790000000"},
			"Anthropic-Ratelimit-Unified-7d-Status":      {"allowed"},
			"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.09"},
			"Anthropic-Ratelimit-Unified-5h-Reset":       {"1789000000"},
		},
	}
	payload, _ := json.Marshal(usage)
	mustEnvelope(t, a, abi.MethodUsageHandle, payload)
	// Replay must be a no-op.
	mustEnvelope(t, a, abi.MethodUsageHandle, payload)

	deadline := time.Now().Add(5 * time.Second)
	for len(a.rt.Load().ingest) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the writer finish the last record

	mreq, _ := json.Marshal(abi.ManagementRequest{Method: "GET", Path: "/v0/management/" + PluginID + "/events", Query: map[string][]string{"auth_index": {"0a1b2c3d"}}})
	res = mustEnvelope(t, a, abi.MethodManagementHandle, mreq)
	var mresp abi.ManagementResponse
	if err := json.Unmarshal(res, &mresp); err != nil {
		t.Fatal(err)
	}
	if mresp.StatusCode != 200 {
		t.Fatalf("status %d body %s", mresp.StatusCode, mresp.Body)
	}
	var body struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(mresp.Body, &body); err != nil {
		t.Fatalf("body %s: %v", mresp.Body, err)
	}
	if len(body.Events) != 1 {
		t.Fatalf("want 1 event, got %d: %s", len(body.Events), mresp.Body)
	}
	if got := body.Events[0]["uncached_input"]; got != float64(1200) {
		t.Fatalf("uncached_input %v", got)
	}
	// The built-in price table knows Opus 5.5, so ingest froze a real cost.
	if usd, _ := body.Events[0]["api_usd"].(float64); usd <= 0 {
		t.Fatalf("api_usd %v; event %v", usd, body.Events[0])
	}
	if hash, _ := body.Events[0]["price_hash"].(string); hash == "" {
		t.Fatalf("price_hash empty")
	}

	rreq, _ := json.Marshal(abi.ManagementRequest{Method: "GET", Path: "/" + PluginID + "/readings", Query: map[string][]string{"auth_index": {"0a1b2c3d"}, "meter": {"7d"}}})
	res = mustEnvelope(t, a, abi.MethodManagementHandle, rreq)
	_ = json.Unmarshal(res, &mresp)
	var rbody struct {
		Readings []map[string]any `json:"readings"`
	}
	if err := json.Unmarshal(mresp.Body, &rbody); err != nil || len(rbody.Readings) != 1 {
		t.Fatalf("readings %s (%v)", mresp.Body, err)
	}
	if rbody.Readings[0]["used_fraction"] != 0.63 {
		t.Fatalf("reading %v", rbody.Readings[0])
	}

	res = mustEnvelope(t, a, abi.MethodManagementRegister, nil)
	var mreg abi.ManagementRegistration
	_ = json.Unmarshal(res, &mreg)
	if len(mreg.Resources) != 1 || mreg.Resources[0].Menu == "" {
		t.Fatalf("resources %+v", mreg.Resources)
	}

	mustEnvelope(t, a, abi.MethodPluginQuiesce, nil)
	a.Shutdown() // idempotent
}

func TestUsageBeforeRegisterIsDropped(t *testing.T) {
	a := New("test", abi.NewHost(&recordingHost{}))
	mustEnvelope(t, a, abi.MethodUsageHandle, []byte(`{"Provider":"claude"}`))
	if a.dropped.Load() != 1 {
		t.Fatalf("dropped %d", a.dropped.Load())
	}
}

// Lifecycle calls interleaved with usage and management traffic must neither
// race nor deadlock nor panic: the host runs all of them concurrently.
func TestLifecycleConcurrency(t *testing.T) {
	a := New("test", abi.NewHost(&recordingHost{}))
	dirs := []string{t.TempDir(), t.TempDir()}
	reg := func(i int) []byte {
		b, _ := json.Marshal(abi.LifecycleRequest{SchemaVersion: 2, ConfigYAML: []byte("data_dir: " + dirs[i%2] + "\npoll_interval_minutes: 0\nprice_sync_hours: 0\n")})
		return b
	}
	usage, _ := json.Marshal(map[string]any{"Provider": "codex", "Model": "gpt-5.6-sol", "AuthIndex": "c0dex", "RequestedAt": time.Now().Format(time.RFC3339Nano),
		"Detail": map[string]int64{"InputTokens": 100, "OutputTokens": 10}, "ResponseHeaders": http.Header{"X-Codex-Primary-Used-Percent": {"12"}, "X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-At": {"1790000000"}}})
	mreq, _ := json.Marshal(abi.ManagementRequest{Method: "GET", Path: "/" + PluginID + "/health"})

	stop := make(chan struct{})
	time.AfterFunc(1500*time.Millisecond, func() { close(stop) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for w := 0; w < 3; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					switch (i + w) % 5 {
					case 0:
						_, _ = a.Handle(abi.MethodPluginRegister, reg(i))
					case 1:
						_, _ = a.Handle(abi.MethodUsageHandle, usage)
					case 2:
						_, _ = a.Handle(abi.MethodManagementHandle, mreq)
					case 3:
						_ = a.Recompute(1, "7d")
					case 4:
						a.Shutdown()
					}
				}
			}(w)
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("lifecycle deadlocked")
	}
	a.Shutdown()
}
