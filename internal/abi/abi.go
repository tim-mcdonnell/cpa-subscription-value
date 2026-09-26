// Package abi is the JSON-over-C-ABI contract with CLIProxyAPI's plugin host.
// It has no dependency on CPA modules: shapes are hand-rolled from
// sdk/pluginabi and sdk/pluginapi so one binary loads on every host version
// whose schema_version we answer with.
package abi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ABIVersion is pluginabi.ABIVersion; unchanged since v7.2.8.
const ABIVersion uint32 = 1

// MaxSchemaVersion is the highest plugin schema we implement. Hosts reject a
// register response whose schema_version exceeds theirs; we answer
// min(host, MaxSchemaVersion). 2 is what v7.2.122 speaks.
const MaxSchemaVersion uint32 = 2

// Method names (pluginabi).
const (
	MethodPluginRegister     = "plugin.register"
	MethodPluginReconfigure  = "plugin.reconfigure"
	MethodPluginQuiesce      = "plugin.quiesce"
	MethodPluginShutdown     = "plugin.shutdown"
	MethodUsageHandle        = "usage.handle"
	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"

	MethodHostLog            = "host.log"
	MethodHostHTTPDo         = "host.http.do"
	MethodHostAuthList       = "host.auth.list"
	MethodHostAuthGet        = "host.auth.get"
	MethodHostAuthGetRuntime = "host.auth.get_runtime"
)

// Envelope is the response wrapper both directions.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError carries a failed call.
type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *EnvelopeError) Error() string {
	if e == nil {
		return "plugin call failed"
	}
	return e.Code + ": " + e.Message
}

// OK wraps a result value.
func OK(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{OK: true, Result: result})
}

// Fail builds an error envelope. It never fails.
func Fail(code, message string) []byte {
	raw, _ := json.Marshal(Envelope{OK: false, Error: &EnvelopeError{Code: code, Message: message}})
	return raw
}

// LifecycleRequest is sent with plugin.register and plugin.reconfigure.
type LifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// Registration answers plugin.register / plugin.reconfigure.
type Registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      Metadata     `json:"metadata"`
	Capabilities  Capabilities `json:"capabilities"`
}

// Metadata mirrors pluginapi.Metadata (Go field names on the wire).
type Metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []ConfigField
}

// ConfigField mirrors pluginapi.ConfigField.
type ConfigField struct {
	Name        string
	Type        string
	EnumValues  []string `json:",omitempty"`
	Description string
}

// Capabilities lists what we implement; omitted capabilities default to false.
type Capabilities struct {
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

// NegotiateSchema picks the schema version to answer with.
func NegotiateSchema(host uint32) uint32 {
	if host == 0 {
		return 1
	}
	if host < MaxSchemaVersion {
		return host
	}
	return MaxSchemaVersion
}

// ManagementRegistration answers management.register.
type ManagementRegistration struct {
	Routes    []ManagementRoute `json:"routes,omitempty"`
	Resources []ResourceRoute   `json:"resources,omitempty"`
}

// ManagementRoute is an exact path under /v0/management/, management-key protected.
type ManagementRoute struct {
	Method string
	Path   string
}

// ResourceRoute is an exact GET path under /v0/resource/plugins/<id>/,
// unauthenticated; a non-empty Menu makes it a CPAMC menu entry (iframe).
type ResourceRoute struct {
	Path        string
	Menu        string
	Description string
}

// ManagementRequest mirrors pluginapi.ManagementRequest (+ host_callback_id).
// Body is base64 on the wire because it is a Go []byte without tags.
type ManagementRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// ManagementResponse mirrors pluginapi.ManagementResponse.
type ManagementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// JSONResponse builds a ManagementResponse with a JSON body.
func JSONResponse(status int, v any) ManagementResponse {
	body, err := json.Marshal(v)
	if err != nil {
		body = []byte(`{"error":"encode response: ` + strings.ReplaceAll(err.Error(), `"`, `'`) + `"}`)
		status = http.StatusInternalServerError
	}
	return ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}}, Body: body}
}

// ErrorResponse builds a JSON error body.
func ErrorResponse(status int, msg string) ManagementResponse {
	return JSONResponse(status, map[string]string{"error": msg})
}

// HTTPRequest / HTTPResponse mirror pluginapi for host.http.do.
type HTTPRequest struct {
	Method         string
	URL            string
	Headers        http.Header
	Body           []byte
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// HTTPResponse is the host.http.do result.
type HTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// AuthGetResponse is host.auth.get's result: the physical auth-file JSON.
type AuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

// AuthFileEntry is one row of host.auth.list.
type AuthFileEntry struct {
	ID        string `json:"id,omitempty"`
	AuthIndex string `json:"auth_index,omitempty"`
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Label     string `json:"label,omitempty"`
	Email     string `json:"email,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
}

// AuthListResponse is host.auth.list's result.
type AuthListResponse struct {
	Files []AuthFileEntry `json:"files"`
}

// HostCaller performs a host callback. The cgo layer implements it; tests
// substitute a fake.
type HostCaller interface {
	Call(method string, payload []byte) ([]byte, error)
}

// Host wraps HostCaller with typed helpers.
type Host struct {
	caller HostCaller
}

// NewHost wraps a caller.
func NewHost(c HostCaller) *Host { return &Host{caller: c} }

// ErrNoHost is returned when the plugin runs without a host (tests, dry runs).
var ErrNoHost = errors.New("no plugin host attached")

func (h *Host) call(method string, req any, out any) error {
	if h == nil || h.caller == nil {
		return ErrNoHost
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode %s: %w", method, err)
	}
	raw, err := h.caller.Call(method, payload)
	if err != nil {
		return err
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode %s envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error == nil {
			return fmt.Errorf("%s failed", method)
		}
		return env.Error
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// Log writes to CPA's logger. Level is trace|debug|info|warn|error.
func (h *Host) Log(level, message string, fields map[string]any) {
	_ = h.call(MethodHostLog, map[string]any{"level": level, "message": message, "fields": fields}, nil)
}

// HTTPDo performs an upstream request through CPA's transport (no credentials injected).
func (h *Host) HTTPDo(req HTTPRequest) (HTTPResponse, error) {
	var resp HTTPResponse
	err := h.call(MethodHostHTTPDo, req, &resp)
	return resp, err
}

// AuthList lists credentials.
func (h *Host) AuthList() ([]AuthFileEntry, error) {
	var resp AuthListResponse
	err := h.call(MethodHostAuthList, map[string]any{}, &resp)
	return resp.Files, err
}

// AuthGet returns the physical auth-file JSON for an auth index. Read-only:
// callers must never refresh or save tokens.
func (h *Host) AuthGet(authIndex string) (AuthGetResponse, error) {
	var resp AuthGetResponse
	err := h.call(MethodHostAuthGet, map[string]string{"auth_index": authIndex}, &resp)
	return resp, err
}
