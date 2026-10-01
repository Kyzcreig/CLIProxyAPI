package test

// Hermetic upstream-wire recorder for the Claude executor (spec one-cliproxyapi-lineage
// §3 I2, §7 Phase 2, AC2: TestFleetAliasOffClaudeWire). Like fleet_wire_recorder_test.go
// this file uses only APIs that exist on pristine upstream <BASE> (v8.0.4 d33f63f8), so the
// same recorder runs there to produce testdata/fleet_wire/pristine-claude-*.json
// (FLEET_CLAUDE_WIRE_RECORD_DIR, TestFleetClaudeWireRecord) and on the fleet tree to
// produce the records the gate compares. Bodies are kept as raw bytes: the gate is
// byte-for-byte.

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
)

// fleetClaudeWireRecord is one upstream /v1/messages request as the fake vendor received it.
type fleetClaudeWireRecord struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

// fleetClaudeWireArm is one replayed client request.
type fleetClaudeWireArm struct {
	Name    string
	Model   string
	Stream  bool
	Payload string
	Headers http.Header
}

const fleetClaudeWireIdentity = `{"device_id":"0000000000000000000000000000000000000000000000000000000000000000","account_uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","session_id":"11111111-2222-4333-8444-555555555555"}`

// fleetClaudeWireArms are the 50 replayed Claude requests: five client header shapes
// (Claude Code CLI, Agent SDK, sdk-cli, a plain Anthropic SDK, a bare client) crossed with
// ten body shapes (system string / array, tools, tool_use+tool_result history, thinking,
// cache_control, metadata.user_id, stream on/off, stop sequences, temperature).
func fleetClaudeWireArms() []fleetClaudeWireArm {
	type headerShape struct {
		name string
		h    http.Header
	}
	shapes := []headerShape{
		{"cli", http.Header{"User-Agent": {"claude-cli/2.1.283 (external, cli)"}, "X-App": {"cli"}, "Anthropic-Beta": {"claude-code-20250219,oauth-2025-04-20"}, "X-Stainless-Package-Version": {"0.52.0"}, "X-Stainless-Lang": {"js"}, "X-Stainless-Runtime": {"node"}}},
		{"sdk-ts", http.Header{"User-Agent": {"claude-cli/2.1.283 (external, sdk-ts)"}, "X-App": {"cli"}, "Anthropic-Beta": {"claude-code-20250219"}}},
		{"sdk-cli", http.Header{"User-Agent": {"claude-cli/2.1.283 (external, sdk-cli)"}, "X-App": {"cli"}}},
		{"anthropic-sdk", http.Header{"User-Agent": {"anthropic-sdk-python/0.49.0"}, "X-Stainless-Lang": {"python"}, "Anthropic-Version": {"2023-06-01"}}},
		{"bare", http.Header{"User-Agent": {"curl/8.7.1"}}},
	}
	identity, _ := json.Marshal(fleetClaudeWireIdentity)
	tools := `[{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}},{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"integer"}}}}]`
	bodies := []struct {
		name   string
		model  string
		stream bool
		body   string
	}{
		{"plain", "claude-sonnet-4-6", false, `{"model":"%s","max_tokens":128,"messages":[{"role":"user","content":"fleet wire probe: say ok"}]}`},
		{"system-string", "claude-sonnet-4-6", true, `{"model":"%s","max_tokens":256,"stream":true,"system":"You are Hermes, a terse assistant.","messages":[{"role":"user","content":"summarise the release notes"}]}`},
		{"system-array-cache", "claude-opus-5-5", true, `{"model":"%s","max_tokens":512,"stream":true,"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Hermes sandbox"}],"messages":[{"role":"user","content":[{"type":"text","text":"read sandbox"}]}]}`},
		{"tools", "claude-sonnet-4-6", false, `{"model":"%s","max_tokens":128,"system":"Hermes","metadata":{"user_id":` + string(identity) + `},"messages":[{"role":"user","content":"Read sandbox"}],"tools":` + tools + `}`},
		{"tools-stream", "claude-haiku-4-5-20251001", true, `{"model":"%s","max_tokens":128,"stream":true,"system":"Hermes","metadata":{"user_id":` + string(identity) + `},"messages":[{"role":"user","content":"Read sandbox"}],"tools":` + tools + `,"tool_choice":{"type":"auto"}}`},
		{"tool-history", "claude-sonnet-4-6", true, `{"model":"%s","max_tokens":128,"stream":true,"system":"Hermes","messages":[{"role":"user","content":"read it"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Read","input":{"file_path":"/tmp/Hermes"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"hello"}]}],"tools":` + tools + `}`},
		{"thinking", "claude-opus-5-5", true, `{"model":"%s","max_tokens":4096,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"think about the release notes"}]}`},
		{"metadata-only", "claude-haiku-4-5-20251001", false, `{"model":"%s","max_tokens":64,"metadata":{"user_id":` + string(identity) + `},"messages":[{"role":"user","content":"ok"}]}`},
		{"stop-temperature", "claude-sonnet-4-6", false, `{"model":"%s","max_tokens":128,"temperature":0.2,"top_p":0.9,"stop_sequences":["\n\n"],"messages":[{"role":"user","content":"count to three"}]}`},
		{"multi-turn", "claude-sonnet-4-6", true, `{"model":"%s","max_tokens":128,"stream":true,"system":"Hermes","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":[{"type":"text","text":"and now?","cache_control":{"type":"ephemeral"}}]}]}`},
	}
	var arms []fleetClaudeWireArm
	for _, hs := range shapes {
		for _, b := range bodies {
			arms = append(arms, fleetClaudeWireArm{
				Name:    "claude-" + hs.name + "-" + b.name,
				Model:   b.model,
				Stream:  b.stream,
				Payload: fmt.Sprintf(b.body, b.model),
				Headers: hs.h.Clone(),
			})
		}
	}
	return arms
}

// fleetClaudeFakeUpstream records every request and answers a minimal Anthropic
// response (SSE for stream, JSON otherwise).
func fleetClaudeFakeUpstream(t *testing.T, records chan fleetClaudeWireRecord) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := fleetClaudeWireRecord{Method: r.Method, Path: r.URL.Path, Headers: map[string][]string{}, Body: string(raw)}
		for k, v := range r.Header {
			rec.Headers[k] = append([]string(nil), v...)
		}
		records <- rec
		model := "claude-sonnet-4-6"
		var probe struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(raw, &probe)
		if probe.Model != "" {
			model = probe.Model
		}
		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			events := []string{
				`{"type":"message_start","message":{"id":"msg_fleet","type":"message","role":"assistant","content":[],"model":"` + model + `","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			}
			for _, ev := range events {
				var typ struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal([]byte(ev), &typ)
				_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ.Type, ev)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_fleet","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}`, model)
	}))
}

// fleetClaudeMetaCapture wraps the Claude executor and records the Options.Metadata of
// each Execute/ExecuteStream call (what the conductor's policy hook wrote).
type fleetClaudeMetaCapture struct {
	cliproxyauth.ProviderExecutor
	seen chan map[string]any
}

func (c fleetClaudeMetaCapture) record(opts cliproxyexecutor.Options) {
	cp := make(map[string]any, len(opts.Metadata))
	for k, v := range opts.Metadata {
		cp[k] = v
	}
	select {
	case c.seen <- cp:
	default:
	}
}

func (c fleetClaudeMetaCapture) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	c.record(opts)
	return c.ProviderExecutor.Execute(ctx, auth, req, opts)
}

func (c fleetClaudeMetaCapture) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	c.record(opts)
	return c.ProviderExecutor.ExecuteStream(ctx, auth, req, opts)
}

// fleetRecordClaudeWire drives one arm through the real Manager + Claude executor built
// from cfg against a loopback fake vendor. It returns what the vendor received (nil when
// the request was refused before dispatch), the metadata the executor was called with,
// and the Manager error.
func fleetRecordClaudeWire(t *testing.T, cfg *config.Config, arm fleetClaudeWireArm) (*fleetClaudeWireRecord, map[string]any, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	records := make(chan fleetClaudeWireRecord, 4)
	server := fleetClaudeFakeUpstream(t, records)
	defer server.Close()

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.SetConfig(cfg)
	seen := make(chan map[string]any, 4)
	manager.RegisterExecutor(fleetClaudeMetaCapture{ProviderExecutor: runtimeexecutor.NewClaudeExecutor(cfg), seen: seen})
	authID := "fleet-claude-wire-" + arm.Name
	registry.GetGlobalRegistry().RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: arm.Model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
		ID: authID, Provider: "claude", Status: cliproxyauth.StatusActive,
		Attributes: map[string]string{"base_url": server.URL, "api_key": "fleet-claude-wire-upstream-key", "cloak_mode": "never"},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(arm.Payload))
	for k, v := range arm.Headers {
		ginCtx.Request.Header[k] = v
	}
	ginCtx.Set("userApiKey", fleetWireClientAPIKey)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	opts := cliproxyexecutor.Options{
		Stream:          arm.Stream,
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: []byte(arm.Payload),
		Headers:         arm.Headers.Clone(),
		Metadata:        map[string]any{},
	}
	req := cliproxyexecutor.Request{Model: arm.Model, Payload: []byte(arm.Payload)}
	var errExec error
	if arm.Stream {
		result, err := manager.ExecuteStream(ctx, []string{"claude"}, req, opts)
		errExec = err
		if err == nil {
			for range result.Chunks {
			}
		}
	} else {
		_, errExec = manager.Execute(ctx, []string{"claude"}, req, opts)
	}
	var meta map[string]any
	select {
	case meta = <-seen:
	default:
	}
	select {
	case rec := <-records:
		return &rec, meta, errExec
	default:
		return nil, meta, errExec
	}
}

// fleetClaudeWireVolatileHeaders are per-process random values upstream generates (not
// keyed by anything the fleet code touches; measured by recording the same tree twice —
// the only differing bytes). Masked before comparison.
var fleetClaudeWireVolatileHeaders = map[string]bool{
	"X-Claude-Code-Session-Id": true,
}

func fleetClaudeWireNormalize(rec fleetClaudeWireRecord) fleetClaudeWireRecord {
	out := fleetClaudeWireRecord{Method: rec.Method, Path: rec.Path, Headers: map[string][]string{}, Body: rec.Body}
	for k, v := range rec.Headers {
		if fleetClaudeWireVolatileHeaders[http.CanonicalHeaderKey(k)] {
			out.Headers[k] = []string{"<volatile>"}
			continue
		}
		vv := append([]string(nil), v...)
		sort.Strings(vv)
		out.Headers[k] = vv
	}
	return out
}

// TestFleetClaudeWireRecord writes the normalized record of every arm (alias absent) to
// $FLEET_CLAUDE_WIRE_RECORD_DIR. Run it on pristine <BASE> to (re)generate
// testdata/fleet_wire/pristine-claude-*.json.
func TestFleetClaudeWireRecord(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("FLEET_CLAUDE_WIRE_RECORD_DIR"))
	if dir == "" {
		t.Skip("FLEET_CLAUDE_WIRE_RECORD_DIR not set")
	}
	if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	for _, arm := range fleetClaudeWireArms() {
		rec, _, err := fleetRecordClaudeWire(t, &config.Config{}, arm)
		if err != nil || rec == nil {
			t.Fatalf("%s: err=%v rec=%v", arm.Name, err, rec != nil)
		}
		raw, _ := json.MarshalIndent(fleetClaudeWireNormalize(*rec), "", "  ")
		if errWrite := os.WriteFile(filepath.Join(dir, arm.Name+".json"), append(raw, '\n'), 0o644); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
}
