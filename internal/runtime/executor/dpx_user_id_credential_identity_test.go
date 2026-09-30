package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// PS-7 oracle for INV-M1's metadata.user_id clause under Line R (t_5c5ebc62,
// Apollo ruling 2026-09-30 08:29 PT, option A: keep CPA's OAuth identity).
//
// On a Claude OAuth credential CPA v8.0.4 rebuilds metadata.user_id
// (claude_fingerprint_policy.go:99-107 forces ApplyCLIIdentity for OAuth;
// helps/claude_credential_identity.go:288-305 ApplyClaudeCredentialMetadata):
//   - device_id    = the credential's device pool[0] (auth/claude/identity.go:267-276), constant per sub
//   - account_uuid = the credential's account
//   - session_id   = the caller's, unchanged
//   - every other key in the caller's user_id object passes through unchanged
// A change to any of these (a per-request device_id, a lost session_id, a dropped
// extra key) is a regression. The negative control is
// dpx_user_id_passthrough_negative_control_test.go (build tag ps7_negative_control),
// which asserts the pre-ruling byte-identical rule and is RED on this build by design.
func TestDPXOAuthUserIDIsCredentialIdentity(t *testing.T) {
	const (
		callerDevice  = "0000000000000000000000000000000000000000000000000000000000000000"
		callerAccount = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		session       = "11111111-2222-4333-8444-555555555555"
		block         = "x-anthropic-billing-header: cc_version=2.1.283.a1b; cc_entrypoint=sdk-ts;"
	)
	callerUserID := `{"device_id":"` + callerDevice + `","account_uuid":"` + callerAccount + `","session_id":"` + session + `","x_extra":{"k":[1,"v"]}}`
	subDevices := map[string]string{}
	for _, sub := range []struct{ name, account string }{
		{"same-account", callerAccount},
		{"other-account", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"},
	} {
		t.Run(sub.name, func(t *testing.T) {
			auth := &authpkg.Auth{ID: "oauth-" + sub.name, Provider: "claude",
				Attributes: map[string]string{"api_key": "sk-ant-oat01-synthetic", "cloak_mode": "never"},
				Metadata:   map[string]any{"account_uuid": sub.account}}
			var devices []string
			for i, stream := range []bool{false, true, false} {
				e, req, opts := aliasFixture(t)
				system := `"system":[{"type":"text","text":` + dpxJSON(t, block) + `},{"type":"text","text":"Hermes"}]`
				payload := strings.Replace(string(req.Payload), `"system":"Hermes"`, system, 1)
				payload = setCallerUserID(t, payload, callerUserID)
				if stream {
					payload = `{"stream":true,` + payload[1:]
					opts.Stream = true
				}
				req.Payload = []byte(payload)
				opts.OriginalRequest = req.Payload
				opts.Headers.Set("User-Agent", "claude-cli/2.1.283 (external, sdk-ts, agent-sdk/0.3.283)")
				opts.Headers.Set("X-Claude-Code-Session-Id", session)
				var seen []byte
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen, _ = io.ReadAll(r.Body)
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						w.Write(aliasSSE(gjson.GetBytes(seen, "tools.0.name").String(), "k", req.Model))
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
				}))
				auth.Attributes["base_url"] = upstream.URL
				if stream {
					res, err := e.ExecuteStream(context.Background(), auth, req, opts)
					if err != nil {
						upstream.Close()
						t.Fatalf("request %d ExecuteStream: %v", i, err)
					}
					for range res.Chunks {
					}
				} else if _, err := e.Execute(context.Background(), auth, req, opts); err != nil {
					upstream.Close()
					t.Fatalf("request %d Execute: %v", i, err)
				}
				upstream.Close()
				raw := gjson.GetBytes(seen, "metadata.user_id").String()
				var got map[string]json.RawMessage
				if err := json.Unmarshal([]byte(raw), &got); err != nil {
					t.Fatalf("request %d upstream metadata.user_id not a JSON object: %q", i, raw)
				}
				str := func(k string) string {
					var s string
					_ = json.Unmarshal(got[k], &s)
					return s
				}
				pool := claudeauth.NormalizeDeviceIDPool(claudeauth.ReadDeviceIDPool(&auth.Metadata))
				if len(pool) == 0 {
					t.Fatalf("request %d: credential has no device pool after dispatch", i)
				}
				if d := str("device_id"); d != pool[0] || !claudeauth.ValidDeviceID(d) {
					t.Fatalf("request %d device_id = %q, want credential pool[0] %q", i, d, pool[0])
				}
				if a := str("account_uuid"); a != sub.account {
					t.Fatalf("request %d account_uuid = %q, want credential account %q", i, a, sub.account)
				}
				if s := str("session_id"); s != session {
					t.Fatalf("request %d session_id = %q, want caller's %q", i, s, session)
				}
				if x := string(got["x_extra"]); x != `{"k":[1,"v"]}` {
					t.Fatalf("request %d extra key changed: %q", i, x)
				}
				if len(got) != 4 {
					t.Fatalf("request %d user_id keys = %d, want 4 (device_id, account_uuid, session_id, x_extra): %s", i, len(got), raw)
				}
				devices = append(devices, str("device_id"))
			}
			for i, d := range devices {
				if d != devices[0] {
					t.Fatalf("device_id not constant per sub: request 0 %q, request %d %q", devices[0], i, d)
				}
			}
			subDevices[sub.name] = devices[0]
		})
	}
	if len(subDevices) == 2 && subDevices["same-account"] == subDevices["other-account"] {
		t.Fatalf("two subs share one device_id %q; want one per credential", subDevices["same-account"])
	}
}

func setCallerUserID(t *testing.T, payload, userID string) string {
	t.Helper()
	enc, err := json.Marshal(userID)
	if err != nil {
		t.Fatal(err)
	}
	old := gjson.Get(payload, "metadata.user_id").Raw
	if old == "" {
		t.Fatal("fixture payload has no metadata.user_id")
	}
	return strings.Replace(payload, old, string(enc), 1)
}
