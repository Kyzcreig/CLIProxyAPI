package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	execpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func offlineAuth(base string) *authpkg.Auth {
	return &authpkg.Auth{ID: "offline", Provider: "claude", Attributes: map[string]string{"api_key": "offline-synthetic", "base_url": base, "cloak_mode": "never"}}
}

func TestDPXAliasCountParity(t *testing.T) {
	e, req, opts := aliasFixture(t)
	cfg := e.cfg.DPXContentAlias
	session, err := contentalias.Open(cfg.StoreDirectory, contentalias.Binding{Principal: cfg.Principal, Session: cfg.SessionID, Version: cfg.Version}, contentalias.DefaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	wire, _, err := contentalias.Prepare(req.Payload, session)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := helps.CountClaudeInputTokens(wire)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Error("wrong count route")
		}
		for _, key := range []string{"tools", "system", "messages"} {
			if gjson.GetBytes(raw, key).Raw != gjson.GetBytes(wire, key).Raw {
				t.Errorf("count mismatch in %s", key)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":123}`)
	}))
	defer server.Close()
	response, err := e.CountTokens(context.Background(), offlineAuth(server.URL), req, opts)
	if err != nil || gjson.GetBytes(response.Payload, "input_tokens").Int() != expected || calls != 0 {
		t.Fatalf("local count: %v got=%s expected=%d calls=%d", err, response.Payload, expected, calls)
	}
	loopback, _ := url.Parse(server.URL)
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.anthropic.com" {
			return nil, fmt.Errorf("unexpected_destination")
		}
		clone := r.Clone(r.Context())
		u := *r.URL
		u.Scheme = loopback.Scheme
		u.Host = loopback.Host
		clone.URL = &u
		clone.Host = loopback.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	response, err = e.CountTokens(ctx, offlineAuth("https://api.anthropic.com"), req, opts)
	if err != nil || calls != 1 || gjson.GetBytes(response.Payload, "input_tokens").Int() != 123 {
		t.Fatalf("HTTP count: %v %s calls=%d", err, response.Payload, calls)
	}
	if err := os.Remove(filepath.Join(cfg.StoreDirectory, "map.json")); err != nil {
		t.Fatal(err)
	}
	for _, auth := range []*authpkg.Auth{offlineAuth(server.URL), offlineAuth("https://api.anthropic.com")} {
		if _, err := e.CountTokens(ctx, auth, req, opts); err == nil || calls != 1 {
			t.Fatal("missing map count dispatched")
		}
	}
}

func TestNativeCompatibilityDelta(t *testing.T) {
	e, req, opts := aliasFixture(t)
	var enriched map[string]any
	if err := json.Unmarshal(req.Payload, &enriched); err != nil {
		t.Fatal(err)
	}
	enriched["temperature"] = 0.37
	enriched["top_p"] = 0.8
	enriched["max_tokens"] = 4096
	enriched["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
	enriched["system"] = []any{map[string]any{"type": "text", "text": "Hermes", "cache_control": map[string]string{"type": "ephemeral"}}}
	enriched["messages"] = []any{
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "name": "Read", "id": "previous-call", "input": map[string]string{"file_path": "/tmp/Hermes"}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "previous-call", "content": "Hermes opaque result"}}},
	}
	req.Payload, _ = json.Marshal(enriched)
	opts.OriginalRequest = req.Payload
	var bodies [][]byte
	var headers []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		headers = append(headers, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`)
	}))
	defer server.Close()
	var responses [][]byte
	for _, on := range []bool{false, false, true} {
		e.cfg.DPXContentAlias.Enabled = on
		resp, err := e.Execute(context.Background(), offlineAuth(server.URL), req, opts)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, resp.Payload)
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatal("disabled wire not deterministic")
	}
	cfg := e.cfg.DPXContentAlias
	s, err := contentalias.Open(cfg.StoreDirectory, contentalias.Binding{cfg.Principal, cfg.SessionID, cfg.Version}, contentalias.DefaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	expected, _, err := contentalias.Prepare(bodies[0], s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(expected, bodies[2]) {
		t.Fatal("executor adds changes beyond typed aliases")
	}
	for i := 1; i < len(responses); i++ {
		if !bytes.Equal(responses[0], responses[i]) {
			t.Fatal("usage/cache response changed")
		}
	}
	for _, h := range headers {
		h.Del("Content-Length")
	}
	if !reflect.DeepEqual(headers[0], headers[2]) {
		t.Fatal("auth/header identity delta")
	}
	if !strings.HasPrefix(gjson.GetBytes(bodies[2], "system.0.text").String(), "dpx_v1_w_") || !strings.HasPrefix(gjson.GetBytes(bodies[2], "tools.0.name").String(), "dpx_v1_t_") {
		t.Fatal("forward missing")
	}
	// tool_result text is aliased like any other text the model reads (t_cb095320).
	if result := gjson.GetBytes(bodies[2], "messages.1.content.0.content").String(); strings.Contains(result, "Hermes") || !strings.HasSuffix(result, " opaque result") || !strings.HasPrefix(result, "dpx_v1_w_") {
		t.Fatal("tool-result text not aliased: " + result)
	}
}
func TestNoAuthIdentityOrUsageMutation(t *testing.T)         { TestNativeCompatibilityDelta(t) }
func TestCapturedSelectedFieldPseudonymization(t *testing.T) { TestNativeCompatibilityDelta(t) }

func TestDPXAliasCompressedErrorsDoNotEcho(t *testing.T) {
	for _, route := range []string{"execute", "stream", "count"} {
		t.Run(route, func(t *testing.T) {
			e, req, opts := aliasFixture(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Encoding", "gzip")
				w.WriteHeader(429)
				z := gzip.NewWriter(w)
				io.WriteString(z, `{"error":{"message":"private-sentinel-command"}}`)
				z.Close()
			}))
			defer server.Close()
			var err error
			switch route {
			case "execute":
				_, err = e.Execute(context.Background(), offlineAuth(server.URL), req, opts)
			case "stream":
				_, err = e.ExecuteStream(context.Background(), offlineAuth(server.URL), req, opts)
			case "count":
				_, err = e.countTokensUpstream(context.Background(), offlineAuth(server.URL), req, opts)
			}
			if err == nil || calls != 1 || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatalf("unsafe error: %v calls=%d", err, calls)
			}
			scoped, ok := err.(interface{ IsRequestScoped() bool })
			if !ok || !scoped.IsRequestScoped() {
				t.Fatalf("retryable error %T", err)
			}
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != 429 {
				t.Fatal("upstream status lost")
			}
		})
	}
}

func TestDPXAliasMissingBindingAllPaths(t *testing.T) {
	e, req, opts := aliasFixture(t)
	e.cfg.DPXContentAlias.SessionID = ""
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer server.Close()
	for _, run := range []func() (execpkg.Response, error){func() (execpkg.Response, error) {
		return e.Execute(context.Background(), offlineAuth(server.URL), req, opts)
	}, func() (execpkg.Response, error) {
		return e.CountTokens(context.Background(), offlineAuth(server.URL), req, opts)
	}, func() (execpkg.Response, error) {
		return e.countTokensUpstream(context.Background(), offlineAuth(server.URL), req, opts)
	}} {
		if _, err := run(); err == nil {
			t.Fatal("missing binding")
		}
	}
	if _, err := e.ExecuteStream(context.Background(), offlineAuth(server.URL), req, opts); err == nil || calls != 0 {
		t.Fatal("unbound dispatch")
	}
}

func TestDPXAliasTypedDiagnostics(t *testing.T) {
	e, req, opts := aliasFixture(t)
	before := append([]byte(nil), req.Payload...)
	for _, raw := range []string{`{"system":"private-sentinel","tools":[{"name":"x","input_schema":{"$ref":"https://private-sentinel"}}]}`, `{"system":"private-sentinel","system":"duplicate"}`} {
		cfg := e.cfg.DPXContentAlias
		s, err := contentalias.Open(cfg.StoreDirectory, contentalias.Binding{cfg.Principal, cfg.SessionID, cfg.Version}, contentalias.DefaultManifest())
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = contentalias.Prepare([]byte(raw), s)
		if err == nil || strings.Contains(err.Error(), "private-sentinel") {
			t.Fatal("diagnostic echo")
		}
	}
	if !bytes.Equal(req.Payload, before) || !bytes.Equal(opts.OriginalRequest, before) {
		t.Fatal("diagnostic injected into history")
	}
	var value any
	if json.Unmarshal(before, &value) != nil {
		t.Fatal("fixture invalid")
	}
}
