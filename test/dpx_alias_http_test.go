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

func TestDPXAliasExecutorHTTP(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binding := contentalias.Binding{Principal: "offline-http", Session: "11111111-2222-4333-8444-555555555555", Version: "v1"}
	if _, err := contentalias.Create(dir, binding, contentalias.DefaultManifest()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{MaxRetryCredentials: 1, Routing: config.RoutingConfig{PromptCachePolicy: "off"}, DPXContentAlias: config.DPXContentAlias{Enabled: true, StoreDirectory: dir, Principal: binding.Principal, SessionID: binding.Session, Version: "v1"}}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		name := gjson.GetBytes(raw, "tools.0.name").String()
		key := ""
		gjson.GetBytes(raw, "tools.0.input_schema.properties").ForEach(func(k, v gjson.Result) bool { key = k.String(); return false })
		if !strings.HasPrefix(name, "dpx_v1_t_") || !strings.HasPrefix(key, "dpx_v1_p_") {
			t.Error("forward missing")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "c", "name": name, "input": map[string]string{key: "/tmp/Hermes"}}}, "usage": map[string]int{"input_tokens": 5, "output_tokens": 7}})
	}))
	defer upstream.Close()
	e := executor.NewClaudeExecutor(cfg)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "offline-client" {
			w.WriteHeader(401)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		req := execpkg.Request{Model: gjson.GetBytes(raw, "model").String(), Payload: raw}
		opts := execpkg.Options{SourceFormat: translator.FromString("claude"), OriginalRequest: raw, Headers: r.Header.Clone()}
		response, err := e.Execute(context.Background(), &authpkg.Auth{ID: "offline", Provider: "claude", Attributes: map[string]string{"api_key": "offline-upstream", "base_url": upstream.URL, "cloak_mode": "never"}}, req, opts)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(response.Payload)
	}))
	defer daemon.Close()
	identity, _ := json.Marshal(`{"device_id":"0000000000000000000000000000000000000000000000000000000000000000","account_uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","session_id":"11111111-2222-4333-8444-555555555555"}`)
	raw := []byte(`{"model":"claude-sonnet-4-6","max_tokens":128,"system":"Hermes","metadata":{"user_id":` + string(identity) + `},"messages":[{"role":"user","content":"read"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]}`)
	request, _ := http.NewRequest(http.MethodPost, daemon.URL+"/v1/messages", bytes.NewReader(raw))
	request.Header = http.Header{"X-Api-Key": {"offline-client"}, "User-Agent": {"claude-cli/2.1.284 (external, cli)"}, "X-App": {"cli"}, "Anthropic-Beta": {"claude-code-20250219"}}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	out, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || calls != 1 || gjson.GetBytes(out, "content.0.name").String() != "Read" || gjson.GetBytes(out, "content.0.input.file_path").String() != "/tmp/Hermes" {
		t.Fatal("real HTTP inverse failed")
	}
}
