package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// dpxAllLanes enumerates every grammar-v2 d lane name:
// claude-d[t|h|s|a]<p|l><x|r>[f|s][-N], bare and "claude-"-prefixed.
func dpxAllLanes() []string {
	var out []string
	for _, mode := range []string{"", "t", "h", "s", "a"} {
		for _, host := range []string{"p", "l"} {
			for _, pin := range []string{"x", "r"} {
				for _, harness := range []string{"", "f", "s"} {
					for _, n := range []string{"", "-25"} {
						name := "d" + mode + host + pin + harness + n
						out = append(out, name, "claude-"+name)
					}
				}
			}
		}
	}
	return out
}

func dpxBlock(entrypoint, origin string) string {
	block := "x-anthropic-billing-header: cc_version=2.1.284.a1b; cc_entrypoint=" + entrypoint + ";"
	if origin != "" {
		block += " cc_turn_origin=" + origin + ";"
	}
	return block
}

func dpxLaneBody(block string) []byte {
	return []byte(`{"model":"m","system":[{"type":"text","text":"` + block + `"},{"type":"text","text":"x"}],"messages":[{"role":"user","content":"hi"}]}`)
}

// TestDPXAliasLaneNeverCLIWithoutTUI is the invariant: across EVERY d lane the
// grammar can name, a cli billing block (cli/human, UA "(external, cli)") is
// admitted only on the t lanes (a genuine interactive TUI on a PTY), on the
// d-a LOCAL lanes (dalx/dalr + f/s — Ace ruling 2026-09-30 18:46 PT, kanban
// t_0feddd7e: these are Ace's own interactive lanes, the human drives them, so
// cli/human is the truthful stamp there), and on the open bare lanes, which
// pass the genuine caller's identity through untouched. If a future change lets
// h, s or the a-p lanes carry cli, this fails.
func TestDPXAliasLaneNeverCLIWithoutTUI(t *testing.T) {
	cli := dpxLaneBody(dpxBlock("cli", "human"))
	const cliUA = "claude-cli/2.1.284 (external, cli)"
	lanes := dpxAllLanes()
	if len(lanes) != 5*2*2*3*2*2 {
		t.Fatalf("lane enumeration size %d", len(lanes))
	}
	for _, lane := range lanes {
		policy, ok := helps.ResolveDPXLane(lane)
		if !ok {
			t.Fatalf("grammar-v2 lane %q not recognised", lane)
		}
		admitted := helps.CheckDPXLaneEntrypoint(lane, cli, cliUA) == ""
		genuineTUI := policy.Mode == "t"
		aceInteractiveALocal := policy.Mode == "a" && policy.Host == "l"
		cliOK := genuineTUI || policy.Open || aceInteractiveALocal
		if admitted && !cliOK {
			t.Errorf("lane %q (mode %q host %q) admits cli without a real TUI on the wire", lane, policy.Mode, policy.Host)
		}
		if !admitted && cliOK {
			t.Errorf("lane %q (mode %q host %q) refuses the genuine interactive client", lane, policy.Mode, policy.Host)
		}
		for _, ep := range policy.Allowed {
			if ep == helps.DPXEntrypointCLI && !genuineTUI && !aceInteractiveALocal {
				t.Errorf("lane %q policy lists cli but is mode %q host %q", lane, policy.Mode, policy.Host)
			}
		}
	}
}

// TestDPXAliasLaneEntrypointTable pins the honest 3-way record per mode.
func TestDPXAliasLaneEntrypointTable(t *testing.T) {
	type tc struct {
		lane, entrypoint, origin, ua string
		ok                           bool
	}
	ua := func(mode string) string { return "claude-cli/2.1.284 (external, " + mode + ")" }
	cases := []tc{
		{"dtlx", "cli", "human", ua("cli"), true},
		{"dtlx", "cli", "", ua("cli"), true},
		{"dtlx", "sdk-cli", "sdk", ua("sdk-cli"), false},
		{"dhlx", "sdk-cli", "sdk", ua("sdk-cli"), true},
		{"dhlx", "cli", "human", ua("cli"), false},
		{"dhlx", "sdk-ts", "sdk", ua("sdk-ts"), false},
		{"dslr", "sdk-ts", "sdk", ua("sdk-ts"), true},
		{"dslx", "sdk-cli", "sdk", ua("sdk-cli"), false},
		{"dalx", "cli", "human", ua("cli"), true},
		{"dalxs", "cli", "human", ua("cli"), true},
		{"dalxf", "cli", "human", ua("cli"), true},
		{"dalx", "sdk-cli", "sdk", ua("sdk-cli"), true},  // fallback override stays admissible
		{"dalr", "sdk-ts", "sdk", ua("sdk-ts"), false},
		// Ace ruling 2026-09-30 18:46 PT (t_0feddd7e): the d-a LOCAL faces are
		// Ace's own interactive lanes and stamp cli/human — cli is ADMITTED on
		// dalx/dalr (+ f/s). sdk-cli stays admissible too (the apx parity
		// fallback is a deliberate degraded-honest mode). The p host keeps the
		// honest sdk stamp (a headless emulator unit, no human).
		{"dalr", "cli", "human", ua("cli"), true},
		{"dalrs", "cli", "human", ua("cli"), true},
		{"dalrf", "cli", "human", ua("cli"), true},
		{"dalx-25", "cli", "human", ua("cli"), true},
		{"claude-dalx", "cli", "human", ua("cli"), true},
		{"dapx", "cli", "human", ua("cli"), false},
		{"dapxs", "cli", "human", ua("cli"), false},
		{"daprs", "cli", "human", ua("cli"), false},
		{"dapr", "sdk-cli", "sdk", ua("sdk-cli"), true},
		{"daprs", "sdk-ts", "sdk", ua("sdk-ts"), true},
		// turn_origin and UA must agree with the entrypoint.
		{"dtlx", "cli", "sdk", ua("cli"), false},
		{"dhlx", "sdk-cli", "human", ua("sdk-cli"), false},
		{"dtlx", "cli", "human", ua("sdk-cli"), false},
		{"dhlx", "sdk-cli", "sdk", ua("cli"), false},
		// open lanes pass the caller through.
		{"dlx", "cli", "human", ua("cli"), true},
		{"dlr", "sdk-ts", "sdk", ua("sdk-ts"), true},
		{"dpx-25", "sdk-cli", "sdk", ua("sdk-cli"), true},
	}
	for _, c := range cases {
		got := helps.CheckDPXLaneEntrypoint(c.lane, dpxLaneBody(dpxBlock(c.entrypoint, c.origin)), c.ua)
		if (got == "") != c.ok {
			t.Errorf("%s entrypoint=%s origin=%s ua=%q: reason=%q want ok=%t", c.lane, c.entrypoint, c.origin, c.ua, got, c.ok)
		}
	}
	for _, lane := range []string{"dtlx", "dhlx", "dslx", "dalx"} {
		if got := helps.CheckDPXLaneEntrypoint(lane, []byte(`{"system":"plain","messages":[]}`), ""); got != "lane_entrypoint_mismatch" {
			t.Errorf("%s block-less body: reason=%q", lane, got)
		}
	}
	for _, lane := range []string{"apx", "btlx", "dxlx", "dtlx-cli", "dtlxq", "DTLX"} {
		if got := helps.CheckDPXLaneEntrypoint(lane, dpxLaneBody(dpxBlock("cli", "human")), ""); got != "unknown_lane" {
			t.Errorf("%q: reason=%q want unknown_lane", lane, got)
		}
	}
	if got := helps.CheckDPXLaneEntrypoint("", dpxLaneBody(dpxBlock("cli", "human")), ""); got != "" {
		t.Errorf("unset lane must not gate: %q", got)
	}
}

// TestDPXAliasLaneGateBeforeDispatch drives the real Execute path: a lane
// mismatch is refused with zero upstream requests, a match is forwarded with
// the caller's entrypoint unchanged (DPX records it, never writes it).
func TestDPXAliasLaneGateBeforeDispatch(t *testing.T) {
	run := func(t *testing.T, lane, block, ua string) (int, []byte, error) {
		t.Helper()
		e, req, opts := aliasFixture(t)
		e.cfg.DPXContentAlias.Lane = lane
		system := `"system":[{"type":"text","text":` + dpxJSON(t, block) + `},{"type":"text","text":"Hermes"}]`
		req.Payload = []byte(strings.Replace(string(req.Payload), `"system":"Hermes"`, system, 1))
		opts.OriginalRequest = req.Payload
		opts.Headers.Set("User-Agent", ua)
		calls := 0
		var sent []byte
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			sent, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, req.Model)
		}))
		defer upstream.Close()
		auth := &authpkg.Auth{ID: "probe", Provider: "claude", Attributes: map[string]string{"api_key": "lane-test-key", "base_url": upstream.URL, "cloak_mode": "never"}, Metadata: map[string]any{"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}}
		_, err := e.Execute(context.Background(), auth, req, opts)
		return calls, sent, err
	}
	refused := []struct{ lane, entrypoint, origin string }{
		{"dhlx", "cli", "human"}, {"dslx", "cli", "human"}, {"dtlx", "sdk-cli", "sdk"}, {"dapx", "cli", "human"}, {"dalr", "sdk-ts", "sdk"},
	}
	for _, c := range refused {
		mode := c.entrypoint
		calls, _, err := run(t, c.lane, dpxBlock(c.entrypoint, c.origin), "claude-cli/2.1.284 (external, "+mode+")")
		var aliasErr contentalias.Error
		if calls != 0 || !errors.As(err, &aliasErr) || aliasErr != "lane_entrypoint_mismatch" {
			t.Errorf("%s %s: calls=%d err=%v, want refused before dispatch", c.lane, c.entrypoint, calls, err)
		}
	}
	admitted := []struct{ lane, entrypoint, origin string }{
		{"dtlx", "cli", "human"}, {"dhlx", "sdk-cli", "sdk"}, {"dalx", "cli", "human"}, {"dalrs", "cli", "human"}, {"dalx", "sdk-cli", "sdk"}, {"dlx", "cli", "human"},
	}
	for _, c := range admitted {
		calls, sent, err := run(t, c.lane, dpxBlock(c.entrypoint, c.origin), "claude-cli/2.1.284 (external, "+c.entrypoint+")")
		if err != nil || calls != 1 {
			t.Errorf("%s %s: calls=%d err=%v, want forwarded", c.lane, c.entrypoint, calls, err)
			continue
		}
		fields, ok := helps.DPXBillingFields(sent)
		if !ok || fields["cc_entrypoint"] != c.entrypoint || fields["cc_turn_origin"] != c.origin {
			t.Errorf("%s: upstream block %v, want the caller's entrypoint=%s origin=%s untouched", c.lane, fields, c.entrypoint, c.origin)
		}
	}
	// Agent SDK requests must retain their sdk-ts billing block through aliasing.
	calls, sent, err := run(t, "dslx", dpxBlock("sdk-ts", "sdk"), "claude-cli/2.1.284 (external, sdk-ts)")
	if err != nil || calls != 1 {
		t.Fatalf("dslx sdk-ts: calls=%d err=%v, want aliased request forwarded", calls, err)
	}
	fields, ok := helps.DPXBillingFields(sent)
	if !ok || fields["cc_entrypoint"] != "sdk-ts" || fields["cc_turn_origin"] != "sdk" {
		t.Errorf("dslx sdk-ts: upstream block %v", fields)
	}
	if strings.Contains(string(sent), "Hermes") {
		t.Error("dslx sdk-ts: body not aliased")
	}
}
