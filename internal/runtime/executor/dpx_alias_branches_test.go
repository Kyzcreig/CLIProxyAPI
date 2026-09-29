package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/chat-completions"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func aliasSSE(name, key, model string) []byte {
	values := []any{
		map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "type": "message", "role": "assistant", "model": model, "content": []any{}, "usage": map[string]int{"input_tokens": 5, "output_tokens": 0}}},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "c", "name": name, "input": map[string]any{}}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": `{"` + key + `":"/tmp/Hermes"}`}},
		map[string]any{"type": "content_block_stop", "index": 0},
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 7}},
		map[string]any{"type": "message_stop"},
	}
	var out []byte
	for _, v := range values {
		raw, _ := json.Marshal(v)
		out = append(out, []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", v.(map[string]any)["type"], raw))...)
	}
	return out
}
func TestAllExecutorResponseBranchesRestore(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, translated := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/translated=%t", stream, translated), func(t *testing.T) {
				e, req, opts := aliasFixture(t)
				if stream {
					req.Payload = append([]byte(`{"stream":true,`), req.Payload[1:]...)
					opts.OriginalRequest = req.Payload
					opts.Stream = true
				}
				if translated {
					opts.ResponseFormat = translator.FromString("openai")
				}
				calls := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					raw, _ := io.ReadAll(r.Body)
					t.Logf("observed upstream stream=%t; invoked ExecuteStream=%t", gjson.GetBytes(raw, "stream").Bool(), stream)
					name := gjson.GetBytes(raw, "tools.0.name").String()
					key := ""
					gjson.GetBytes(raw, "tools.0.input_schema.properties").ForEach(func(k, v gjson.Result) bool { key = k.String(); return false })
					if gjson.GetBytes(raw, "stream").Bool() {
						w.Header().Set("Content-Type", "text/event-stream")
						w.Write(aliasSSE(name, key, req.Model))
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprintf(w, `{"content":[{"type":"tool_use","id":"c","name":%q,"input":{%q:"/tmp/Hermes"}}],"usage":{"input_tokens":5,"output_tokens":7}}`, name, key)
					}
				}))
				defer upstream.Close()
				auth := &authpkg.Auth{ID: "one", Provider: "claude", Attributes: map[string]string{"api_key": "offline-synthetic", "base_url": upstream.URL, "cloak_mode": "never"}}
				var out []byte
				if stream {
					response, err := e.ExecuteStream(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range response.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						out = append(out, chunk.Payload...)
					}
				} else {
					response, err := e.Execute(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					out = response.Payload
				}
				if calls != 1 || bytes.Contains(out, []byte("dpx_v1_")) || !bytes.Contains(out, []byte("Read")) || !bytes.Contains(out, []byte("file_path")) || !bytes.Contains(out, []byte("/tmp/Hermes")) {
					t.Fatalf("unrestored: %s", out)
				}
				if translated && !bytes.Contains(out, []byte("choices")) {
					t.Fatalf("translator inactive: %s", out)
				}
			})
		}
	}
}
