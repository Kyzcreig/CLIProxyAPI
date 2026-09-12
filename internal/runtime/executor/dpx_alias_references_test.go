package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func nativeReferenceBlocks(name string) []any {
	ref := map[string]any{"type": "tool_reference", "tool_name": name, "name": "ordinary-value"}
	return []any{ref, map[string]any{"type": "tool_result", "tool_use_id": "call", "content": []any{ref, map[string]any{"type": "text", "text": "Read Hermes /tmp/Hermes"}}}, map[string]any{"type": "tool_search_tool_result", "tool_use_id": "search", "content": map[string]any{"type": "tool_search_tool_search_result", "tool_references": []any{ref}}}}
}
func nativeReferenceSSE(blocks []any, model string) []byte {
	values := []any{map[string]any{"type": "message_start", "message": map[string]any{"id": "refs", "type": "message", "role": "assistant", "model": model, "content": []any{}, "usage": map[string]int{"input_tokens": 5, "output_tokens": 0}}}}
	for i, block := range blocks {
		values = append(values, map[string]any{"type": "content_block_start", "index": i, "content_block": block}, map[string]any{"type": "content_block_stop", "index": i})
	}
	values = append(values, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": 7}}, map[string]any{"type": "message_stop"})
	var out []byte
	for _, value := range values {
		raw, _ := json.Marshal(value)
		out = append(out, []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", value.(map[string]any)["type"], raw))...)
	}
	return out
}
func TestDPXAliasToolReferencesHTTP(t *testing.T) {
	for _, mode := range []string{"json", "aggregated-sse", "native-sse"} {
		t.Run(mode, func(t *testing.T) {
			e, req, opts := aliasFixture(t)
			if mode == "aggregated-sse" {
				// Select Execute's actual upstream-SSE branch. An unregistered
				// response format observes the restored bytes before translation;
				// OpenAI has no native tool_reference representation to assert.
				opts.ResponseFormat = translator.FromString("dpx-reference-observer")
			}
			var err error
			req.Payload, err = sjson.SetBytes(req.Payload, "messages.0.content", nativeReferenceBlocks("Read"))
			if err != nil {
				t.Fatal(err)
			}
			opts.OriginalRequest = req.Payload
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				raw, _ := io.ReadAll(r.Body)
				name := gjson.GetBytes(raw, "tools.0.name").String()
				if !strings.HasPrefix(name, "dpx_v1_t_") {
					t.Error("forward tool control missing")
				}
				for _, path := range []string{"messages.0.content.0.tool_name", "messages.0.content.1.content.0.tool_name", "messages.0.content.2.content.tool_references.0.tool_name"} {
					if gjson.GetBytes(raw, path).String() != name {
						t.Errorf("reference mismatch at %s", path)
					}
				}
				if gjson.GetBytes(raw, "messages.0.content.1.content.1.text").String() != "Read Hermes /tmp/Hermes" {
					t.Error("ordinary result changed")
				}
				blocks := nativeReferenceBlocks(name)
				if mode == "json" {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{"id": "refs", "type": "message", "role": "assistant", "model": req.Model, "content": blocks, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 5, "output_tokens": 7}})
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Write(nativeReferenceSSE(blocks, req.Model))
				}
			}))
			defer upstream.Close()
			auth := &authpkg.Auth{ID: "reference-test", Provider: "claude", Attributes: map[string]string{"api_key": "offline-synthetic", "base_url": upstream.URL, "cloak_mode": "never"}}
			var out []byte
			if mode == "native-sse" {
				response, e := e.ExecuteStream(context.Background(), auth, req, opts)
				if e != nil {
					t.Fatal(e)
				}
				for chunk := range response.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					out = append(out, chunk.Payload...)
				}
			} else {
				response, e := e.Execute(context.Background(), auth, req, opts)
				if e != nil {
					t.Fatal(e)
				}
				out = response.Payload
			}
			if calls != 1 || bytes.Contains(out, []byte("dpx_v1_t_")) {
				t.Fatalf("inverse/call count failed: calls=%d out=%s", calls, out)
			}
			if bytes.Count(out, []byte(`"tool_name":"Read"`)) != 3 || !bytes.Contains(out, []byte("Read Hermes /tmp/Hermes")) {
				t.Fatalf("references or ordinary value lost: %s", out)
			}
		})
	}
}
