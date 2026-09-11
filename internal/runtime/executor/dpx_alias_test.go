package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/contentalias"
	authpkg "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	execpkg "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func aliasFixture(t *testing.T) (*ClaudeExecutor, execpkg.Request, execpkg.Options) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binding := contentalias.Binding{Principal: "one-local-principal", Session: "11111111-2222-4333-8444-555555555555", Version: "v1"}
	if _, err := contentalias.Create(dir, binding, contentalias.DefaultManifest()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{MaxRetryCredentials: 1, DPXContentAlias: config.DPXContentAlias{Enabled: true, StoreDirectory: dir, Principal: binding.Principal, SessionID: binding.Session, Version: binding.Version}}
	identity, _ := json.Marshal(`{"device_id":"0000000000000000000000000000000000000000000000000000000000000000","account_uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","session_id":"11111111-2222-4333-8444-555555555555"}`)
	raw := []byte(`{"model":"claude-sonnet-4-6","max_tokens":128,"system":"Hermes","metadata":{"user_id":` + string(identity) + `},"messages":[{"role":"user","content":"Read sandbox"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]}`)
	headers := http.Header{"User-Agent": {"claude-cli/2.1.258 (external, cli)"}, "X-App": {"cli"}, "Anthropic-Beta": {"claude-code-20250219"}}
	return NewClaudeExecutor(cfg), execpkg.Request{Model: "claude-sonnet-4-6", Payload: raw}, execpkg.Options{SourceFormat: translator.FromString("claude"), Headers: headers, OriginalRequest: raw}
}
func TestDPXAliasExecutorHTTP(t *testing.T) {
	e, req, opts := aliasFixture(t)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		name := gjson.GetBytes(raw, "tools.0.name").String()
		props := gjson.GetBytes(raw, "tools.0.input_schema.properties")
		key := ""
		props.ForEach(func(k, v gjson.Result) bool { key = k.String(); return false })
		if name == "Read" || key == "file_path" || gjson.GetBytes(raw, "system").String() == "Hermes" {
			t.Error("forward detached")
		}
		response := map[string]any{"id": "offline-msg", "type": "message", "role": "assistant", "model": req.Model, "content": []any{map[string]any{"type": "tool_use", "id": "call1", "name": name, "input": map[string]string{key: "/tmp/Hermes-Velorin"}}}, "stop_reason": "tool_use", "usage": map[string]int{"input_tokens": 3, "output_tokens": 7}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer upstream.Close()
	auth := &authpkg.Auth{ID: "sandbox", Provider: "claude", Attributes: map[string]string{"api_key": "offline-synthetic", "base_url": upstream.URL, "cloak_mode": "never"}}
	response, err := e.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !bytes.Contains(response.Payload, []byte(`"name":"Read"`)) || !bytes.Contains(response.Payload, []byte(`"file_path":"/tmp/Hermes-Velorin"`)) {
		t.Fatalf("inverse detached: %s", response.Payload)
	}
}
func TestDPXAliasUnknownProtocol(t *testing.T) {
	e, req, opts := aliasFixture(t)
	opts.SourceFormat = translator.FromString("openai")
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer upstream.Close()
	auth := &authpkg.Auth{Attributes: map[string]string{"api_key": "offline-synthetic", "base_url": upstream.URL, "cloak_mode": "never"}}
	if _, err := e.Execute(context.Background(), auth, req, opts); err == nil || calls != 0 {
		t.Fatal("unsupported protocol dispatched")
	}
}
