package test

// AC-M12 (SPEC-modes §9 Phase 2, Line R): the same token maps to the same alias
// across 3 turns and 2 client sessions through one DPX daemon, and again after
// the daemon restarts over the same store. Fake upstream; no network. Also
// proves the W1 wirelog wiring: one digest row per upstream request, capture
// "dpx", 0 brand tokens in the body that left.

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestDPXAliasDeterminismGolden(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	binding := contentalias.Binding{Principal: "acm12", Session: "22222222-3333-4444-8555-666666666666", Version: "v1"}
	if _, err := contentalias.Create(dir, binding, contentalias.DefaultManifest()); err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(t.TempDir(), "wirelog.spool.jsonl")
	cfg := &config.Config{MaxRetryCredentials: 1, DPXContentAlias: config.DPXContentAlias{
		Enabled: true, StoreDirectory: dir, Principal: binding.Principal, SessionID: binding.Session, Version: "v1",
		WirelogSpool: spool, WirelogLane: "dtlx", WirelogSub: "25",
	}}
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen = append(seen, string(raw))
		name := gjson.GetBytes(raw, "tools.0.name").String()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip") // as api.anthropic.com answers; the row must still carry usage
		zw := gzip.NewWriter(w)
		json.NewEncoder(zw).Encode(map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "c", "name": name, "input": map[string]string{}}}, "usage": map[string]int{"input_tokens": 5, "output_tokens": 7}})
		zw.Close()
	}))
	defer upstream.Close()
	auth := &authpkg.Auth{ID: "acm12", Provider: "claude", Attributes: map[string]string{"api_key": "offline-upstream", "base_url": upstream.URL, "cloak_mode": "never"}}
	headers := http.Header{"X-Api-Key": {"offline-client"}, "User-Agent": {"claude-cli/2.1.284 (external, cli)"}, "X-App": {"cli"}, "Anthropic-Beta": {"claude-code-20250219"}}

	send := func(e *executor.ClaudeExecutor, session string, turns int) {
		identity, _ := json.Marshal(fmt.Sprintf(`{"device_id":"%s","account_uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","session_id":"%s"}`, strings.Repeat("0", 64), session))
		msgs := []string{`{"role":"user","content":"Hermes asks: read the Hermes notes"}`}
		for i := 0; i < turns; i++ {
			raw := []byte(`{"model":"claude-sonnet-4-6","max_tokens":128,"system":"You are Hermes.","metadata":{"user_id":` + string(identity) + `},"messages":[` + strings.Join(msgs, ",") + `],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]}`)
			req := execpkg.Request{Model: "claude-sonnet-4-6", Payload: raw}
			opts := execpkg.Options{SourceFormat: translator.FromString("claude"), OriginalRequest: raw, Headers: headers.Clone()}
			resp, err := e.Execute(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatalf("session %s turn %d: %v", session, i, err)
			}
			if got := gjson.GetBytes(resp.Payload, "content.0.name").String(); got != "Read" {
				t.Fatalf("inverse failed: %q", got)
			}
			msgs = append(msgs, `{"role":"assistant","content":"ok"}`, fmt.Sprintf(`{"role":"user","content":"Hermes turn %d"}`, i+2))
		}
	}
	e := executor.NewClaudeExecutor(cfg)
	send(e, "11111111-1111-4111-8111-111111111111", 3)
	send(e, "33333333-3333-4333-8333-333333333333", 3)
	send(executor.NewClaudeExecutor(cfg), "44444444-4444-4444-8444-444444444444", 1) // daemon restart, same store

	if len(seen) != 7 {
		t.Fatalf("upstream requests = %d, want 7", len(seen))
	}
	golden := map[string]string{}
	for i, body := range seen {
		if strings.Contains(strings.ToLower(body), "hermes") {
			t.Fatalf("request %d leaked a manifest word upstream", i)
		}
		for _, path := range []string{"system", "tools.0.name", "tools.0.input_schema.properties", "messages.0.content"} {
			v := gjson.Get(body, path).Raw
			if want, ok := golden[path]; !ok {
				golden[path] = v
			} else if v != want {
				t.Fatalf("request %d: alias drift at %s: %s != %s", i, path, v, want)
			}
		}
	}
	if !strings.HasPrefix(gjson.Get(seen[0], "tools.0.name").String(), "dpx_v1_t_") {
		t.Fatal("tool name not aliased")
	}

	f, err := os.Open(spool)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		row := gjson.Parse(sc.Text())
		rows++
		if row.Get("v").Int() != 2 || row.Get("capture").String() != "dpx" || row.Get("lane").String() != "dtlx" || row.Get("sub").String() != "25" {
			t.Fatalf("row labels: %s", sc.Text())
		}
		if row.Get("req.body.brandTokens").Int() != 0 || row.Get("res.status").Int() != 200 || row.Get("res.body.usage.input_tokens").Int() != 5 {
			t.Fatalf("row digest: %s", sc.Text())
		}
		if strings.Contains(sc.Text(), "offline-upstream") || strings.Contains(strings.ToLower(sc.Text()), "hermes") {
			t.Fatal("row carries a credential or content")
		}
	}
	if rows != 7 {
		t.Fatalf("wirelog rows = %d, want 7", rows)
	}
}
