package test

// Hermetic upstream-wire recorder for the fleet re-port gates (spec one-cliproxyapi-lineage
// §7 Phase 0, TestFleetPolicyOffCodexWire). This file uses only APIs that exist on the
// pristine upstream base as well, so the same recorder runs on <BASE> to produce the
// testdata/fleet_wire/pristine-*.json goldens (FLEET_WIRE_RECORD_DIR, see
// TestFleetWireRecord) and on the fleet tree to produce the records the gate compares.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

// fleetWireClientAPIKey is the proxy-side (inbound) API key of the recorded client. It is a
// test literal, never a real credential.
const fleetWireClientAPIKey = "fleet-wire-test-client-key"

// fleetWireRecord is one upstream request as the fake vendor received it.
type fleetWireRecord struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    map[string]any      `json:"body"`
}

// fleetWireArm names one recorded request shape.
type fleetWireArm struct {
	Name     string // file stem
	Provider string // codex | xai
	Model    string
	Policy   string // routing.prompt-cache-policy; "" = key absent
	Payload  string // client body (OpenAI Responses)
	Headers  http.Header
}

func fleetWireArms() []fleetWireArm {
	keyless := `{"model":"%s","input":[{"role":"user","content":"fleet wire probe: summarise the release notes"}],"instructions":"You are a terse assistant."}`
	return []fleetWireArm{
		{Name: "codex-keyless-shadow", Provider: "codex", Model: "gpt-5.4", Policy: "shadow", Payload: fmt.Sprintf(keyless, "gpt-5.4")},
		{Name: "codex-keyless-enforce", Provider: "codex", Model: "gpt-5.4", Policy: "enforce", Payload: fmt.Sprintf(keyless, "gpt-5.4")},
		{Name: "codex-keyless-absent", Provider: "codex", Model: "gpt-5.4", Policy: "", Payload: fmt.Sprintf(keyless, "gpt-5.4")},
		{Name: "xai-keyless-shadow", Provider: "xai", Model: "grok-4.6", Policy: "shadow", Payload: fmt.Sprintf(keyless, "grok-4.6")},
		{Name: "xai-keyless-enforce", Provider: "xai", Model: "grok-4.6", Policy: "enforce", Payload: fmt.Sprintf(keyless, "grok-4.6")},
		// Phase 1 `off` arms: the new zero value, explicit and absent.
		{Name: "codex-keyless-off", Provider: "codex", Model: "gpt-5.4", Policy: "off", Payload: fmt.Sprintf(keyless, "gpt-5.4")},
		{Name: "xai-keyless-off", Provider: "xai", Model: "grok-4.6", Policy: "off", Payload: fmt.Sprintf(keyless, "grok-4.6")},
		{Name: "xai-keyless-absent", Provider: "xai", Model: "grok-4.6", Policy: "", Payload: fmt.Sprintf(keyless, "grok-4.6")},
		// STRIDE: client-supplied look-alikes of the policy metadata must not short-circuit
		// the resolver (opts.Metadata is built by the handler from a fixed key set).
		{
			Name: "codex-keyless-spoofed-source", Provider: "codex", Model: "gpt-5.4", Policy: "enforce",
			Payload: `{"model":"gpt-5.4","input":[{"role":"user","content":"fleet wire probe: summarise the release notes"}],"instructions":"You are a terse assistant.","metadata":{"prompt_cache_key_source":"caller","prompt_cache_key":"spoofed"},"prompt_cache_key_source":"caller"}`,
			Headers: http.Header{"X-Prompt-Cache-Key-Source": []string{"caller"}, "Prompt-Cache-Key-Source": []string{"caller"}},
		},
	}
}

// fleetRecordWire drives one request through the real Manager + provider executor against a
// loopback fake vendor and returns what the vendor received.
func fleetRecordWire(t *testing.T, arm fleetWireArm) fleetWireRecord {
	t.Helper()
	rec, _ := fleetRecordWireWith(t, arm, false)
	return rec
}

// fleetRecordWireMeta is fleetRecordWire that also returns a copy of the Options.Metadata the
// provider executor was called with (captured by a pass-through wrapper around it).
func fleetRecordWireMeta(t *testing.T, arm fleetWireArm) (fleetWireRecord, map[string]any) {
	t.Helper()
	return fleetRecordWireWith(t, arm, true)
}

// fleetMetaCapture wraps a provider executor and records the metadata of each stream call.
type fleetMetaCapture struct {
	cliproxyauth.ProviderExecutor
	seen chan map[string]any
}

func (c fleetMetaCapture) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	cp := make(map[string]any, len(opts.Metadata))
	for k, v := range opts.Metadata {
		cp[k] = v
	}
	select {
	case c.seen <- cp:
	default:
	}
	return c.ProviderExecutor.ExecuteStream(ctx, auth, req, opts)
}

func fleetRecordWireWith(t *testing.T, arm fleetWireArm, captureMeta bool) (fleetWireRecord, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	records := make(chan fleetWireRecord, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := fleetWireRecord{Method: r.Method, Path: r.URL.Path, Headers: map[string][]string{}}
		for k, v := range r.Header {
			rec.Headers[k] = append([]string(nil), v...)
		}
		if errJSON := json.Unmarshal(raw, &rec.Body); errJSON != nil {
			rec.Body = map[string]any{"_raw": string(raw)}
		}
		records <- rec
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fleet\"}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fleet\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer server.Close()

	cfg := &config.Config{}
	if arm.Policy != "" {
		if errYAML := yaml.Unmarshal([]byte("routing:\n  prompt-cache-policy: "+arm.Policy+"\n"), cfg); errYAML != nil {
			t.Fatalf("config: %v", errYAML)
		}
	}
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.SetConfig(cfg)
	var exec cliproxyauth.ProviderExecutor
	switch arm.Provider {
	case "codex":
		exec = runtimeexecutor.NewCodexExecutor(cfg)
	case "xai":
		exec = runtimeexecutor.NewXAIExecutor(cfg)
	default:
		t.Fatalf("unknown provider %q", arm.Provider)
	}
	seen := make(chan map[string]any, 4)
	if captureMeta {
		exec = fleetMetaCapture{ProviderExecutor: exec, seen: seen}
	}
	manager.RegisterExecutor(exec)
	authID := "fleet-wire-" + arm.Name
	registry.GetGlobalRegistry().RegisterClient(authID, arm.Provider, []*registry.ModelInfo{{ID: arm.Model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
		ID: authID, Provider: arm.Provider, Status: cliproxyauth.StatusActive,
		Attributes: map[string]string{"base_url": server.URL, "api_key": "fleet-wire-upstream-key"},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(arm.Payload))
	for k, v := range arm.Headers {
		ginCtx.Request.Header[k] = v
	}
	ginCtx.Set("userApiKey", fleetWireClientAPIKey)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	result, errStream := manager.ExecuteStream(ctx, []string{arm.Provider}, cliproxyexecutor.Request{
		Model: arm.Model, Payload: []byte(arm.Payload),
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: []byte(arm.Payload),
		Headers:         arm.Headers.Clone(),
		Metadata:        map[string]any{},
	})
	if errStream != nil {
		t.Fatalf("%s: ExecuteStream: %v", arm.Name, errStream)
	}
	for range result.Chunks {
	}
	select {
	case rec := <-records:
		if !captureMeta {
			return rec, nil
		}
		select {
		case meta := <-seen:
			return rec, meta
		default:
			t.Fatalf("%s: executor wrapper saw no call", arm.Name)
		}
	default:
		t.Fatalf("%s: fake vendor received no request", arm.Name)
	}
	return fleetWireRecord{}, nil
}

// fleetWireVolatileHeaders are per-request random values upstream generates on every call
// (not keyed by anything the fleet code touches). They are masked before comparison.
var fleetWireVolatileHeaders = map[string]bool{}

func fleetWireNormalize(rec fleetWireRecord) fleetWireRecord {
	out := fleetWireRecord{Method: rec.Method, Path: rec.Path, Headers: map[string][]string{}, Body: rec.Body}
	for k, v := range rec.Headers {
		if fleetWireVolatileHeaders[http.CanonicalHeaderKey(k)] {
			out.Headers[k] = []string{"<volatile>"}
			continue
		}
		vv := append([]string(nil), v...)
		sort.Strings(vv)
		out.Headers[k] = vv
	}
	return out
}

// TestFleetWireRecord writes the normalized record of every arm to $FLEET_WIRE_RECORD_DIR.
// Run it on pristine <BASE> to (re)generate testdata/fleet_wire/pristine-*.json.
func TestFleetWireRecord(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("FLEET_WIRE_RECORD_DIR"))
	if dir == "" {
		t.Skip("FLEET_WIRE_RECORD_DIR not set")
	}
	if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	for _, arm := range fleetWireArms() {
		rec := fleetWireNormalize(fleetRecordWire(t, arm))
		raw, _ := json.MarshalIndent(rec, "", "  ")
		if errWrite := os.WriteFile(filepath.Join(dir, arm.Name+".json"), append(raw, '\n'), 0o644); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
}
