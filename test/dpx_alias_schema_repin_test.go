package test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	execpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// TestDPXAliasToolSchemaChangeAcrossRequests is the t_dbf31d31 regression on
// the offline wire lane: a d-l unit's alias map persists across client
// builds, so a caller that edits one tool's input_schema (Hermes a1c31d3a79
// reworded kanban_create.wake) must still be served. Before the fix the
// second tool-bearing request never reached upstream: 400
// contentalias:schema_changed, every turn, until the map was wiped.
func TestDPXAliasToolSchemaChangeAcrossRequests(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binding := contentalias.Binding{Principal: "offline-repin", Session: "11111111-2222-4333-8444-666666666666", Version: "v1"}
	if _, err := contentalias.Create(dir, binding, contentalias.DefaultManifest()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{MaxRetryCredentials: 1, Routing: config.RoutingConfig{PromptCachePolicy: "off"}, DPXContentAlias: config.DPXContentAlias{Enabled: true, StoreDirectory: dir, Principal: binding.Principal, SessionID: binding.Session, Version: "v1"}}
	var upstreamBodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		upstreamBodies = append(upstreamBodies, raw)
		name := gjson.GetBytes(raw, "tools.0.name").String()
		key := ""
		gjson.GetBytes(raw, "tools.0.input_schema.properties").ForEach(func(k, v gjson.Result) bool { key = k.String(); return false })
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "c", "name": name, "input": map[string]string{key: "card"}}}, "usage": map[string]int{"input_tokens": 5, "output_tokens": 7}})
	}))
	defer upstream.Close()
	e := executor.NewClaudeExecutor(cfg)
	send := func(wakeDescription string) (int, []byte) {
		t.Helper()
		identity, _ := json.Marshal(`{"device_id":"0000000000000000000000000000000000000000000000000000000000000000","account_uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","session_id":"11111111-2222-4333-8444-666666666666"}`)
		raw := []byte(`{"model":"claude-sonnet-4-6","max_tokens":128,"system":"Hermes","metadata":{"user_id":` + string(identity) + `},"messages":[{"role":"user","content":"make a card"}],"tools":[{"name":"kanban_create","input_schema":{"type":"object","properties":{"title":{"type":"string"},"wake":{"type":"boolean","description":"` + wakeDescription + `"}},"required":["title"]}}]}`)
		req := execpkg.Request{Model: "claude-sonnet-4-6", Payload: raw}
		opts := execpkg.Options{SourceFormat: translator.FromString("claude"), OriginalRequest: raw, Headers: http.Header{"User-Agent": {"claude-cli/2.1.284 (external, cli)"}, "X-App": {"cli"}, "Anthropic-Beta": {"claude-code-20250219"}}}
		response, err := e.Execute(context.Background(), &authpkg.Auth{ID: "offline", Provider: "claude", Attributes: map[string]string{"api_key": "offline-upstream", "base_url": upstream.URL, "cloak_mode": "never"}}, req, opts)
		if err != nil {
			return 400, []byte(err.Error())
		}
		return 200, response.Payload
	}
	for i, desc := range []string{"Defaults to false", "Default: wake (kanban.auto_subscribe_wake)"} {
		code, out := send(desc)
		if code != 200 {
			t.Fatalf("request %d (wake description %q): %s", i+1, desc, out)
		}
		if got := gjson.GetBytes(out, "content.0.name").String(); got != "kanban_create" {
			t.Fatalf("request %d: tool name not restored: %s", i+1, out)
		}
		if got := gjson.GetBytes(out, "content.0.input.title").String(); got != "card" {
			t.Fatalf("request %d: argument key not restored: %s", i+1, out)
		}
	}
	if len(upstreamBodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(upstreamBodies))
	}
	if !strings.Contains(string(upstreamBodies[1]), "Default: wake") {
		t.Fatal("the second request must carry its own (new) schema upstream")
	}
	if a, b := gjson.GetBytes(upstreamBodies[0], "tools.0.name").String(), gjson.GetBytes(upstreamBodies[1], "tools.0.name").String(); a != b || !strings.HasPrefix(a, "dpx_v1_t_") {
		t.Fatalf("tool alias must be stable across the schema change: %q vs %q", a, b)
	}
	if bytes.Contains(upstreamBodies[1], []byte(`"kanban_create"`)) {
		t.Fatal("original tool name leaked upstream")
	}
}
