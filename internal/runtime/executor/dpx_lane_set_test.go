package executor

import (
	"context"
	"errors"
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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func dpxUA(mode string) string { return "claude-cli/2.1.284 (external, " + mode + ")" }

// TestDPXAliasLaneSetResolve pins the lane-set gate (t_bf75897d): membership
// is any-lane-admits, the label is the first admitting lane in configured
// order, and a bad set or an unmatched request is refused.
func TestDPXAliasLaneSetResolve(t *testing.T) {
	set := []string{"dtlx", "dslx", "dlx"}
	cases := []struct {
		lanes                  []string
		entrypoint, origin, ua string
		lane, reason           string
	}{
		{set, "cli", "human", dpxUA("cli"), "dtlx", ""},
		{set, "sdk-ts", "sdk", dpxUA("sdk-ts"), "dslx", ""},
		{set, "sdk-cli", "sdk", dpxUA("sdk-cli"), "dlx", ""}, // only the open lane admits it
		{[]string{"dlx", "dtlx"}, "cli", "human", dpxUA("cli"), "dlx", ""},
		{[]string{"dtlx", "dslx"}, "sdk-ts", "sdk", dpxUA("sdk-ts"), "dslx", ""},
		{[]string{"dtlx", "dslx"}, "sdk-cli", "sdk", dpxUA("sdk-cli"), "", "lane_entrypoint_mismatch"},
		{[]string{"dtlx", "dslx"}, "sdk-ts", "sdk", dpxUA("cli"), "", "lane_entrypoint_mismatch"},
		{[]string{"dslx", "dhlx"}, "cli", "human", dpxUA("cli"), "", "lane_entrypoint_mismatch"},
		{[]string{"dtlx", "bogus"}, "cli", "human", dpxUA("cli"), "", "unknown_lane"},
		{[]string{"dslx"}, "sdk-ts", "sdk", dpxUA("sdk-ts"), "dslx", ""},
		{nil, "cli", "human", dpxUA("cli"), "", ""},
	}
	for _, c := range cases {
		lane, reason := helps.ResolveDPXRequestLane(c.lanes, dpxLaneBody(dpxBlock(c.entrypoint, c.origin)), c.ua)
		if lane != c.lane || reason != c.reason {
			t.Errorf("%v %s/%s ua=%q: got (%q,%q) want (%q,%q)", c.lanes, c.entrypoint, c.origin, c.ua, lane, reason, c.lane, c.reason)
		}
	}
	if got := (config.DPXContentAlias{Lane: "dslx"}).EffectiveLanes(); len(got) != 1 || got[0] != "dslx" {
		t.Errorf("singular lane must be a one-element set, got %v", got)
	}
	if got := (config.DPXContentAlias{Lane: "dslx", Lanes: []string{"dtlx", "dlx"}}).EffectiveLanes(); strings.Join(got, ",") != "dtlx,dlx" {
		t.Errorf("lanes must replace lane, got %v", got)
	}
	if got := (config.DPXContentAlias{}).EffectiveLanes(); got != nil {
		t.Errorf("unset lanes must stay ungated, got %v", got)
	}
}

// TestDPXAliasLaneSetExecute drives the real Execute path of one unit that
// serves [dtlx, dslx]: a dslx request and a dtlx request both reach upstream
// and each wirelog row carries the lane its entrypoint resolves to, not a
// static field; a request neither lane admits is refused before dispatch.
func TestDPXAliasLaneSetExecute(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "wirelog.spool.jsonl")
	run := func(t *testing.T, cfgMut func(*config.DPXContentAlias), entrypoint, origin string) (int, error) {
		t.Helper()
		e, req, opts := aliasFixture(t)
		e.cfg.DPXContentAlias.WirelogSpool = spool
		e.cfg.DPXContentAlias.WirelogLane = "static-label"
		e.cfg.DPXContentAlias.WirelogSub = "25"
		cfgMut(&e.cfg.DPXContentAlias)
		system := `"system":[{"type":"text","text":` + dpxJSON(t, dpxBlock(entrypoint, origin)) + `},{"type":"text","text":"Hermes"}]`
		req.Payload = []byte(strings.Replace(string(req.Payload), `"system":"Hermes"`, system, 1))
		opts.OriginalRequest = req.Payload
		opts.Headers.Set("User-Agent", dpxUA(entrypoint))
		calls := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, req.Model)
		}))
		defer upstream.Close()
		auth := &authpkg.Auth{ID: "probe", Provider: "claude", Attributes: map[string]string{"api_key": "lane-set-key", "base_url": upstream.URL, "cloak_mode": "never"}, Metadata: map[string]any{"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}}
		_, err := e.Execute(context.Background(), auth, req, opts)
		return calls, err
	}
	set := func(c *config.DPXContentAlias) { c.Lanes = []string{"dtlx", "dslx"} }
	for _, c := range []struct{ entrypoint, origin string }{{"sdk-ts", "sdk"}, {"cli", "human"}} {
		if calls, err := run(t, set, c.entrypoint, c.origin); err != nil || calls != 1 {
			t.Fatalf("%s through [dtlx dslx]: calls=%d err=%v, want forwarded", c.entrypoint, calls, err)
		}
	}
	calls, err := run(t, set, "sdk-cli", "sdk")
	var aliasErr contentalias.Error
	if calls != 0 || !errors.As(err, &aliasErr) || aliasErr != "lane_entrypoint_mismatch" {
		t.Errorf("sdk-cli through [dtlx dslx]: calls=%d err=%v, want refused before dispatch", calls, err)
	}
	// Singular lane: same unit shape, one-element set, label stays the static field.
	if calls, err := run(t, func(c *config.DPXContentAlias) { c.Lane = "dslx" }, "sdk-ts", "sdk"); err != nil || calls != 1 {
		t.Fatalf("sdk-ts through lane=dslx: calls=%d err=%v", calls, err)
	}
	if calls, err := run(t, func(c *config.DPXContentAlias) { c.Lane = "dslx" }, "cli", "human"); calls != 0 || err == nil {
		t.Errorf("cli through lane=dslx: calls=%d err=%v, want refused", calls, err)
	}
	raw, errRead := os.ReadFile(spool)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		row := gjson.Parse(line)
		got = append(got, row.Get("lane").String()+"/"+row.Get("req.body.billing.cc_entrypoint").String())
	}
	if want := "dslx/sdk-ts dtlx/cli static-label/sdk-ts"; strings.Join(got, " ") != want {
		t.Errorf("wirelog lanes %q, want %q", strings.Join(got, " "), want)
	}
}
