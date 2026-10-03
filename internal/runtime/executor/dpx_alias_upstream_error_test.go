package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// t_d27c2365: a non-2xx upstream answer on an aliased request used to surface
// only "contentalias:upstream_failure", hiding the real reason (the P5 Opus
// bracket aborted on a server-side-fallback 400 nobody could read). The status
// and the upstream body, with aliases restored, must reach the caller.
func TestDPXAliasUpstreamErrorBodyRestored(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
			e, req, opts := aliasFixture(t)
			opts.Stream = stream
			var sentFallback string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				sentFallback = gjson.GetBytes(raw, "fallback_credit_token").String()
				tool := gjson.GetBytes(raw, "tools.0.name").String()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"tools.0 ` + tool + `: fallback rejected"}}`))
			}))
			defer upstream.Close()
			req.Model = "claude-opus-4-8"
			req.Payload = []byte(strings.Replace(string(req.Payload), `"model":"claude-sonnet-4-6"`, `"model":"claude-opus-4-8","fallback_credit_token":"fct-opaque"`, 1))
			opts.OriginalRequest = req.Payload
			auth := &authpkg.Auth{ID: "sandbox", Provider: "claude", Attributes: map[string]string{"api_key": "offline-synthetic", "base_url": upstream.URL, "cloak_mode": "never"}}
			var err error
			if stream {
				_, err = e.ExecuteStream(context.Background(), auth, req, opts)
			} else {
				_, err = e.Execute(context.Background(), auth, req, opts)
			}
			if sentFallback != "fct-opaque" {
				t.Fatalf("fallback_credit_token not forwarded: %q", sentFallback)
			}
			var statusErr interface{ StatusCode() int }
			if err == nil || !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest {
				t.Fatalf("want 400 status error, got %v", err)
			}
			if got := gjson.Get(err.Error(), "error.message").String(); got != "tools.0 Read: fallback rejected" {
				t.Fatalf("upstream reason not restored: %q (raw %q)", got, err.Error())
			}
		})
	}
}
