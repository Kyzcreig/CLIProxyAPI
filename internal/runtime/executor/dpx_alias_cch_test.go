package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// Claude Code behind a non-first-party base URL (DPX) sends the billing block
// without cch; a first-party-shaped caller sends one. Both must leave DPX with
// CPA's native re-sign over the aliased body.
const (
	dpxCLIUserAgent   = "claude-cli/2.1.284 (external, cli)"
	dpxStockBlockNoCC = "x-anthropic-billing-header: cc_version=2.1.284.a1b; cc_entrypoint=cli;"
	dpxStockBlockCCH  = "x-anthropic-billing-header: cc_version=2.1.284.a1b; cc_entrypoint=cli; cch=abcde;"
	dpxTurnOne        = `[{"role":"user","content":"Read sandbox"}]`
	dpxTurnTwo        = `[{"role":"user","content":"Read sandbox"},{"role":"assistant","content":"ok"},{"role":"user","content":"again"}]`
)

type dpxSignedRun struct {
	calls     int
	body      []byte
	userAgent string
	response  []byte
	ledger    []byte
	err       error
}

// runDPXSigned drives the alias route with a Claude OAuth credential, so CPA's
// native CCH signing is active, and returns the exact upstream capture.
func runDPXSigned(t *testing.T, block string, stream bool, messages string) dpxSignedRun {
	t.Helper()
	e, req, opts := aliasFixture(t)
	system := `"system":"Hermes"`
	if block != "" {
		system = `"system":[{"type":"text","text":` + dpxJSON(t, block) + `},{"type":"text","text":"Hermes"}]`
	}
	payload := strings.Replace(string(req.Payload), `"system":"Hermes"`, system, 1)
	payload = strings.Replace(payload, `"messages":`+dpxTurnOne, `"messages":`+messages, 1)
	if stream {
		payload = `{"stream":true,` + payload[1:]
		opts.Stream = true
	}
	req.Payload = []byte(payload)
	opts.OriginalRequest = req.Payload
	opts.Headers.Set("User-Agent", dpxCLIUserAgent)

	var run dpxSignedRun
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		run.calls++
		run.body, _ = io.ReadAll(r.Body)
		run.userAgent = r.Header.Get("User-Agent")
		name := gjson.GetBytes(run.body, "tools.0.name").String()
		key := ""
		gjson.GetBytes(run.body, "tools.0.input_schema.properties").ForEach(func(k, v gjson.Result) bool { key = k.String(); return false })
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write(aliasSSE(name, key, req.Model))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","model":%q,"content":[{"type":"tool_use","id":"c","name":%q,"input":{%q:"/tmp/Hermes"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":7}}`, req.Model, name, key)
	}))
	defer upstream.Close()
	auth := &authpkg.Auth{ID: "probe", Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-oat01-offline-synthetic", "base_url": upstream.URL, "cloak_mode": "never"}, Metadata: map[string]any{"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}}
	if stream {
		response, err := e.ExecuteStream(context.Background(), auth, req, opts)
		run.err = err
		if err == nil {
			for chunk := range response.Chunks {
				if chunk.Err != nil && run.err == nil {
					run.err = chunk.Err
				}
				run.response = append(run.response, chunk.Payload...)
			}
		}
	} else {
		response, err := e.Execute(context.Background(), auth, req, opts)
		run.err = err
		run.response = response.Payload
	}
	run.ledger, _ = os.ReadFile(filepath.Join(e.cfg.DPXContentAlias.StoreDirectory, "map.json"))
	return run
}

func dpxJSON(t *testing.T, value string) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertDPXSignedCapture(t *testing.T, run dpxSignedRun) {
	t.Helper()
	if run.err != nil || run.calls != 1 {
		t.Fatalf("alias+sign route failed: calls=%d err=%v", run.calls, run.err)
	}
	block := gjson.GetBytes(run.body, "system.0.text").String()
	if !strings.HasPrefix(block, "x-anthropic-billing-header:") || !strings.Contains(block, "cc_entrypoint=cli;") {
		t.Fatalf("billing block not forwarded: %q", block)
	}
	offset, signed := claudeBillingCCHDigitsOffset(run.body)
	if !signed {
		t.Fatalf("billing block not signed: %q", block)
	}
	resigned, err := signAnthropicMessagesBody(run.body)
	if err != nil || !bytes.Equal(resigned, run.body) {
		t.Fatalf("cch %s is not signAnthropicMessagesBody over the sent body (err=%v)", run.body[offset:offset+claudeCCHLength], err)
	}
	if run.userAgent != dpxCLIUserAgent {
		t.Fatalf("upstream UA = %q, want the CLI's %q", run.userAgent, dpxCLIUserAgent)
	}
	if bytes.Contains(bytes.ToLower(run.body), []byte("hermes")) {
		t.Fatalf("brand leaked upstream: %s", run.body)
	}
	if !bytes.Contains(run.response, []byte(`Read`)) || !bytes.Contains(run.response, []byte(`file_path`)) || !bytes.Contains(run.response, []byte(`/tmp/Hermes`)) {
		t.Fatalf("response not reverse-mapped: %s", run.response)
	}
	if path := os.Getenv("DPX_CCH_CAPTURE"); path != "" {
		record, _ := json.Marshal(map[string]any{
			"test": t.Name(), "upstream_user_agent": run.userAgent, "billing_block": block,
			"cch": string(run.body[offset : offset+claudeCCHLength]), "cch_equals_signAnthropicMessagesBody": true,
			"body_len": len(run.body), "brand_in_body": false, "response_restored": true,
		})
		f, errOpen := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if errOpen == nil {
			f.Write(append(record, '\n'))
			f.Close()
		}
	}
}

// TestDPXAliasResignsStockCLIBlock: the stock CLI block (no cch) is aliased,
// then CPA inserts and signs cch over the aliased body; the response restores.
func TestDPXAliasResignsStockCLIBlock(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, block := range []string{dpxStockBlockNoCC, dpxStockBlockCCH} {
			t.Run(fmt.Sprintf("stream=%t/caller_cch=%t", stream, strings.Contains(block, "cch=")), func(t *testing.T) {
				assertDPXSignedCapture(t, runDPXSigned(t, block, stream, dpxTurnOne))
			})
		}
	}
}

// TestDPXAliasResignLeavesLedgerAndPrefix: signing touches only the cch field,
// so the persisted alias ledger and the cacheable prefix (tools, system minus
// cch, earlier turns) are identical to the unsigned alias output.
func TestDPXAliasResignLeavesLedgerAndPrefix(t *testing.T) {
	one := runDPXSigned(t, dpxStockBlockNoCC, false, dpxTurnOne)
	two := runDPXSigned(t, dpxStockBlockNoCC, false, dpxTurnTwo)
	assertDPXSignedCapture(t, one)
	assertDPXSignedCapture(t, two)
	if !bytes.Equal(one.ledger, two.ledger) || len(one.ledger) == 0 {
		t.Fatal("alias ledger differs between turns of the same session")
	}
	for _, path := range []string{"tools", "messages.0"} {
		if gjson.GetBytes(one.body, path).Raw != gjson.GetBytes(two.body, path).Raw {
			t.Fatalf("prefix %s drifted across turns", path)
		}
	}
	strip := func(body []byte) string {
		return gjson.GetBytes(withoutDPXCCHField(body), "system").Raw
	}
	if strip(one.body) != strip(two.body) {
		t.Fatal("system prefix differs outside the cch field")
	}
}

// TestDPXAliasPostAliasMutationStillTrips: only the cch field is exempt.
func TestDPXAliasPostAliasMutationStillTrips(t *testing.T) {
	m := &contentalias.RequestMap{}
	aliased := []byte(`{"model":"m","system":[{"type":"text","text":"` + dpxStockBlockNoCC + `"},{"type":"text","text":"x"}],"messages":[{"role":"user","content":"hi"}]}`)
	signed, err := finalizeAnthropicMessagesBodyCCH(aliased, "")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(signed, aliased) {
		t.Fatal("fixture was not signed")
	}
	if err := validateDPXFinalBody(aliased, signed, m); err != nil {
		t.Fatalf("native cch insert+sign refused: %v", err)
	}
	withCCH := bytes.Replace(aliased, []byte("cc_entrypoint=cli;"), []byte("cc_entrypoint=cli; cch=abcde;"), 1)
	resigned, _ := signAnthropicMessagesBody(withCCH)
	if err := validateDPXFinalBody(withCCH, resigned, m); err != nil {
		t.Fatalf("native cch re-sign refused: %v", err)
	}
	cases := map[string][]byte{
		"message byte":    bytes.Replace(signed, []byte(`"hi"`), []byte(`"hj"`), 1),
		"entrypoint":      bytes.Replace(signed, []byte("cc_entrypoint=cli;"), []byte("cc_entrypoint=sdk;"), 1),
		"cc_version":      bytes.Replace(signed, []byte("2.1.284.a1b"), []byte("2.1.284.a1c"), 1),
		"second system":   bytes.Replace(signed, []byte(`"text":"x"`), []byte(`"text":"y"`), 1),
		"appended member": bytes.Replace(signed, []byte(`"model":"m"`), []byte(`"model":"m","x":1`), 1),
		"block dropped":   bytes.Replace(aliased, []byte(dpxStockBlockNoCC), []byte("plain"), 1),
	}
	for name, after := range cases {
		err := validateDPXFinalBody(aliased, after, m)
		var aliasErr contentalias.Error
		if !errors.As(err, &aliasErr) || aliasErr != "post_alias_mutation" {
			t.Errorf("%s: want post_alias_mutation, got %v", name, err)
		}
	}
	if err := validateDPXFinalBody(aliased, cases["message byte"], nil); err != nil {
		t.Fatalf("non-alias route must not validate: %v", err)
	}
}

// TestDPXAliasBlocklessCallerRefused: without a caller-authored block, CPA's
// fallback would prepend a whole billing block after aliasing. That is not a
// cch-field edit, so the alias route refuses before dispatch.
func TestDPXAliasBlocklessCallerRefused(t *testing.T) {
	run := runDPXSigned(t, "", false, dpxTurnOne)
	var aliasErr contentalias.Error
	if run.calls != 0 || !errors.As(run.err, &aliasErr) || aliasErr != "post_alias_mutation" {
		t.Fatalf("block-less caller: calls=%d err=%v", run.calls, run.err)
	}
}
