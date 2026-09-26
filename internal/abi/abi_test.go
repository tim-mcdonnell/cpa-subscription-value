package abi

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestNegotiateSchema(t *testing.T) {
	cases := map[uint32]uint32{0: 1, 1: 1, 2: 2, 6: MaxSchemaVersion}
	for host, want := range cases {
		if got := NegotiateSchema(host); got != want {
			t.Errorf("host %d: got %d want %d", host, got, want)
		}
	}
}

type fakeCaller struct {
	method  string
	payload []byte
	reply   []byte
	err     error
}

func (f *fakeCaller) Call(method string, payload []byte) ([]byte, error) {
	f.method, f.payload = method, payload
	return f.reply, f.err
}

func TestHostCallDecodesEnvelope(t *testing.T) {
	fc := &fakeCaller{reply: []byte(`{"ok":true,"result":{"auth_index":"abc","json":{"access_token":"t"}}}`)}
	h := NewHost(fc)
	resp, err := h.AuthGet("abc")
	if err != nil {
		t.Fatal(err)
	}
	if fc.method != MethodHostAuthGet {
		t.Fatalf("method %q", fc.method)
	}
	var req map[string]string
	if err := json.Unmarshal(fc.payload, &req); err != nil || req["auth_index"] != "abc" {
		t.Fatalf("payload %s", fc.payload)
	}
	if resp.AuthIndex != "abc" || string(resp.JSON) != `{"access_token":"t"}` {
		t.Fatalf("resp %+v", resp)
	}
}

func TestHostCallSurfacesEnvelopeError(t *testing.T) {
	h := NewHost(&fakeCaller{reply: []byte(`{"ok":false,"error":{"code":"nope","message":"denied"}}`)})
	_, err := h.AuthList()
	var envErr *EnvelopeError
	if !errors.As(err, &envErr) || envErr.Code != "nope" {
		t.Fatalf("err %v", err)
	}
}

func TestNoHost(t *testing.T) {
	var h *Host
	if _, err := h.AuthList(); !errors.Is(err, ErrNoHost) {
		t.Fatalf("err %v", err)
	}
	h.Log("info", "ignored", nil) // must not panic
}

func TestManagementBodyIsBase64OnTheWire(t *testing.T) {
	// The host decodes ManagementResponse with encoding/json into a Go []byte
	// field, so we must produce base64, which json.Marshal does by default.
	raw, err := json.Marshal(JSONResponse(200, map[string]int{"a": 1}))
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		StatusCode int
		Body       []byte
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.StatusCode != 200 || string(back.Body) != `{"a":1}` {
		t.Fatalf("round trip %s", raw)
	}
}
