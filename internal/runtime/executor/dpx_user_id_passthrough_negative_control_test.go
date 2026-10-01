//go:build ps7_negative_control

// Negative control for the PS-7 oracle (t_5c5ebc62). It asserts the PRE-ruling INV-M1
// rule (caller metadata.user_id byte-identical upstream) and is RED on CPA v8.0.4 by
// design: OAuth credentials rebuild device_id/account_uuid. Excluded from CI by the
// build tag. Run: go test -tags ps7_negative_control ./internal/runtime/executor -run TestDPXOAuthSDKTSUserIDPassthrough

package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// PS-7 / INV-M1 (t_5c5ebc62): a caller-authored metadata.user_id on a native
// sdk-ts request must reach upstream byte-identical when the credential is a
// Claude OAuth token (the live DPX unit's shape). Zero spend: stub upstream.
func TestDPXOAuthSDKTSUserIDPassthrough(t *testing.T) {
	const callerUserID = `{"device_id":"0000000000000000000000000000000000000000000000000000000000000000","account_uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","session_id":"11111111-2222-4333-8444-555555555555"}`
	const block = "x-anthropic-billing-header: cc_version=2.1.283.a1b; cc_entrypoint=sdk-ts;"
	for _, tc := range []struct {
		name, account string
		stream        bool
	}{
		{"same-account", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", false},
		{"same-account-stream", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", true},
		{"other-account", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, req, opts := aliasFixture(t)
			system := `"system":[{"type":"text","text":` + dpxJSON(t, block) + `},{"type":"text","text":"Hermes"}]`
			payload := strings.Replace(string(req.Payload), `"system":"Hermes"`, system, 1)
			if tc.stream {
				payload = `{"stream":true,` + payload[1:]
				opts.Stream = true
			}
			req.Payload = []byte(payload)
			opts.OriginalRequest = req.Payload
			opts.Headers.Set("User-Agent", "claude-cli/2.1.283 (external, sdk-ts, agent-sdk/0.3.283)")
			opts.Headers.Set("X-Claude-Code-Session-Id", "11111111-2222-4333-8444-555555555555")
			var seen []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen, _ = io.ReadAll(r.Body)
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Write(aliasSSE(gjson.GetBytes(seen, "tools.0.name").String(), "k", req.Model))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer upstream.Close()
			auth := &authpkg.Auth{ID: "oauth-sandbox", Provider: "claude",
				Attributes: map[string]string{"api_key": "sk-ant-oat01-offline-synthetic", "base_url": upstream.URL, "cloak_mode": "never"},
				Metadata:   map[string]any{"account_uuid": tc.account}}
			if tc.stream {
				res, err := e.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatalf("ExecuteStream: %v", err)
				}
				for range res.Chunks {
				}
			} else if _, err := e.Execute(context.Background(), auth, req, opts); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got := gjson.GetBytes(seen, "metadata.user_id").String(); got != callerUserID {
				t.Fatalf("metadata.user_id rewritten\n in: %s\nout: %s", callerUserID, got)
			}
		})
	}
}
