package executor

import (
	"context"
	"fmt"
	authpkg "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewToolReferenceActualHTTP(t *testing.T) {
	e, req, opts := aliasFixture(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		name := gjson.GetBytes(raw, "tools.0.name").String()
		if !strings.HasPrefix(name, "dpx_v1_t_") {
			t.Error("forward control absent")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-review\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_reference\",\"tool_name\":%q}}\n\n", name)
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()
	auth := &authpkg.Auth{ID: "review", Provider: "claude", Attributes: map[string]string{"api_key": "offline-synthetic", "base_url": server.URL, "cloak_mode": "never"}}
	response, err := e.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for chunk := range response.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		out.Write(chunk.Payload)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	if strings.Contains(out.String(), "dpx_v1_t_") {
		t.Fatal("actual CPA native HTTP/SSE emitted allocated tool_reference alias without restoration")
	}
	if !strings.Contains(out.String(), `"tool_name":"Read"`) {
		t.Fatal("original reference absent")
	}
}
