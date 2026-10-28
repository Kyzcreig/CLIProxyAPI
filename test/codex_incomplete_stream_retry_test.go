package test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// TestCodexIncompleteStreamFailsOverOnExecute is the fleet's 2026-10-27 Prism outage in miniature
// (fleet card t_c9bd528e): chatgpt.com/backend-api/codex closed the SSE body before
// response.completed on ~1/3 of gpt-6.1-sol requests for 20+ hours. The non-stream Execute path
// reads the WHOLE upstream body before returning, so nothing has reached the client when the
// truncation is detected and the request is safe to replay on another credential. The error used
// to be request-scoped, which made the conductor return it at once: no second credential, no
// request-retry, and no `upstream execution failed` log line (Prism saw hundreds per hour while the
// cpa log showed a handful). A truncated stream is a transport fault, not a fault of the request.
func TestCodexIncompleteStreamFailsOverOnExecute(t *testing.T) {
	const model = "gpt-5.4"
	const created = `{"type":"response.created","response":{"id":"resp_cut"}}`
	const completed = `{"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	var cutHits, okHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if account == "cut" {
			cutHits.Add(1)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", created) // body ends: no response.completed
			return
		}
		okHits.Add(1)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", created, completed)
	}))
	defer server.Close()

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0) // no same-credential retry: the fix must come from credential failover
	cfg := &config.Config{}
	manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))
	for i, key := range []string{"cut", "ok"} {
		id := "codex-incomplete-" + key
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
			ID: id, Provider: "codex", Status: cliproxyauth.StatusActive,
			Attributes: map[string]string{"priority": fmt.Sprint(4 - i), "base_url": server.URL, "api_key": key},
			Metadata:   map[string]any{"disable_cooling": true},
		}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}

	resp, errExec := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model: model, Payload: []byte(fmt.Sprintf(`{"model":%q,"input":"hello"}`, model)),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if errExec != nil {
		t.Fatalf("a truncated codex stream on one credential must fail over, got: %v (cut=%d ok=%d)",
			errExec, cutHits.Load(), okHits.Load())
	}
	if !strings.Contains(string(resp.Payload), "resp_ok") {
		t.Fatalf("expected the healthy credential's response, got %s", resp.Payload)
	}
	if cutHits.Load() != 1 || okHits.Load() != 1 {
		t.Fatalf("upstream attempts cut=%d ok=%d, want 1 and 1", cutHits.Load(), okHits.Load())
	}
}
