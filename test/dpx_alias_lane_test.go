package test

// cpa brand-alias plugin, Antigravity lane (card t_a37235c0). Goldens under
// testdata/dpx_alias_lane/ are the redacted shapes of the 2026-10-03 B.2
// captures (t_ec59f890): an OpenAI-chat caller (the Hermes cpa provider and
// the fleet probes) and a Gemini-native caller, both routed to antigravity.
// Each golden is run twice through the REAL AntigravityExecutor against a fake
// upstream: a control (no alias) and an aliased run (brandalias.Engine applied
// the way the plugin host applies an InterceptRequestAfterAuth result). The
// gate is:
//
//	(i)   every Antigravity fingerprint field the executor writes is
//	      byte-identical between the two runs;
//	(ii)  brandTokens == 0 on the body that left (the whole body, not only
//	      the prose);
//	(iii) the client-facing response of the aliased run, after the response
//	      interceptor, is byte-identical to the control's;
//	(iv)  the mutation arm: a brand word injected past the walkers MUST turn
//	      the gate red (proved as a positive detection here; the manual
//	      golden mutation is recorded on the card).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/brandalias"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// antigravityFingerprint is the set of Antigravity envelope fields the plugin
// must never influence (policy never_touch.antigravity + the model id).
var antigravityFingerprint = []string{"userAgent", "requestType", "project", "model", "request.model", "request.generationConfig", "request.safetySettings"}

type laneGolden struct {
	name         string
	sourceFormat string
	requestFile  string
	responseFile string // upstream Antigravity response the fake server returns, with {{TOOL}} = the tool's wire name
	toolNamePath string // where the first tool's name lives in the source body
}

var antigravityGoldens = []laneGolden{
	{"openai-chat", "openai", "openai-chat-request.json", "antigravity-upstream-response.json", "tools.0.function.name"},
	{"gemini", "gemini", "gemini-request.json", "antigravity-upstream-response.json", "tools.0.functionDeclarations.0.name"},
}

func readLaneGolden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "dpx_alias_lane", name))
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	return bytes.TrimSpace(raw)
}

// antigravityRun drives the real executor once and returns the body that
// reached the fake upstream and the executor's (source-format) response.
func antigravityRun(t *testing.T, sourceFormat string, payload, upstreamResponse []byte) (sent []byte, response []byte) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "generateContent") {
			sent = body
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(upstreamResponse)
	}))
	defer server.Close()
	exec := executor.NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	auth := &cliproxyauth.Auth{ID: "antigravity-lane-test", Provider: "antigravity",
		Metadata:   map[string]any{"access_token": "test-token", "expired": time.Now().Add(24 * time.Hour).Format(time.RFC3339), "project_id": "test-proj"},
		Attributes: map[string]string{"base_url": server.URL}}
	model := gjson.GetBytes(payload, "model").String()
	from := sdktranslator.FromString(sourceFormat)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: model, Payload: payload},
		cliproxyexecutor.Options{SourceFormat: from, ResponseFormat: from, OriginalRequest: payload})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(sent) == 0 {
		t.Fatal("fake upstream saw no generateContent body")
	}
	return sent, resp.Payload
}

func newLaneEngine(t *testing.T, mode string) *brandalias.Engine {
	t.Helper()
	cfg, err := brandalias.ParseConfig([]byte("enabled: true\nmode: " + mode + "\nlanes: [antigravity]\nprincipal: lane-test\nsession: golden\n"))
	if err != nil {
		t.Fatal(err)
	}
	return brandalias.NewEngine(cfg)
}

func TestDPXAliasLaneAntigravity(t *testing.T) {
	for _, g := range antigravityGoldens {
		t.Run(g.name, func(t *testing.T) {
			request := readLaneGolden(t, g.requestFile)
			upstreamTemplate := string(readLaneGolden(t, g.responseFile))
			words := brandalias.DefaultWords
			if n := brandalias.CountBrandTokens(request, words); n == 0 {
				t.Fatal("golden carries no brand tokens; the gate would be vacuous")
			}

			// Aliased run: what the plugin host does with the interceptor result.
			engine := newLaneEngine(t, brandalias.ModeEnabled)
			reqID := "lane-" + g.name
			ir, decision := engine.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: reqID, SourceFormat: g.sourceFormat, ToFormat: "antigravity", Model: gjson.GetBytes(request, "model").String(), Body: request})
			if !decision.Handled || len(ir.Body) == 0 {
				t.Fatalf("plugin did not alias: %+v", decision)
			}
			aliasedTool := gjson.GetBytes(ir.Body, g.toolNamePath).String()
			originalTool := gjson.GetBytes(request, g.toolNamePath).String()
			if !strings.HasPrefix(aliasedTool, "dpx_v1_w_") {
				t.Fatalf("tool name not aliased at %s: %q", g.toolNamePath, aliasedTool)
			}

			controlSent, controlResp := antigravityRun(t, g.sourceFormat, request, []byte(strings.ReplaceAll(upstreamTemplate, "{{TOOL}}", originalTool)))
			aliasedSent, aliasedRespRaw := antigravityRun(t, g.sourceFormat, ir.Body, []byte(strings.ReplaceAll(upstreamTemplate, "{{TOOL}}", aliasedTool)))

			// (i) fingerprint fields byte-identical.
			for _, p := range antigravityFingerprint {
				c, a := gjson.GetBytes(controlSent, p), gjson.GetBytes(aliasedSent, p)
				if c.Raw != a.Raw {
					t.Errorf("fingerprint %s differs: control=%s aliased=%s", p, c.Raw, a.Raw)
				}
			}
			for _, p := range []string{"userAgent", "requestType", "project", "requestId"} {
				if !gjson.GetBytes(aliasedSent, p).Exists() {
					t.Errorf("executor envelope field %s missing on the aliased run", p)
				}
			}
			if !gjson.GetBytes(controlSent, "requestId").Exists() {
				t.Fatalf("control run has no requestId: %s", controlSent)
			}
			// (ii) nothing brand-shaped left upstream, in any field.
			if n := brandalias.CountBrandTokens(aliasedSent, words); n != 0 {
				t.Errorf("brandTokens=%d on the upstream body: %s", n, aliasedSent)
			}
			if n := brandalias.CountBrandTokens(controlSent, words); n == 0 {
				t.Fatal("control body carries no brand tokens; the executor dropped the prose?")
			}
			// (iii) client-facing response round-trips to the control's bytes.
			rr, rd := engine.InterceptResponse(pluginapi.ResponseInterceptRequest{RequestID: reqID, SourceFormat: g.sourceFormat, Body: aliasedRespRaw})
			if !rd.Handled {
				t.Fatalf("response not restored: %+v", rd)
			}
			restored := rr.Body
			if len(restored) == 0 {
				restored = aliasedRespRaw
			}
			// Translator-minted tool-call ids embed a nanosecond clock, so they
			// are compared by shape (original tool name prefix, no symbol) and
			// blanked for the byte comparison.
			normalizedControl, normalizedRestored := controlResp, restored
			for _, path := range []string{"choices.0.message.tool_calls.0.id"} {
				if id := gjson.GetBytes(restored, path); id.Exists() {
					if !strings.HasPrefix(id.Str, originalTool+"-") || strings.Contains(id.Str, "dpx_v1_") {
						t.Errorf("tool call id not restored: %q", id.Str)
					}
					normalizedControl, _ = sjson.SetBytes(normalizedControl, path, "")
					normalizedRestored, _ = sjson.SetBytes(normalizedRestored, path, "")
				}
			}
			if !bytes.Equal(normalizedRestored, normalizedControl) {
				t.Errorf("restored response differs from control:\n control %s\n aliased %s", normalizedControl, normalizedRestored)
			}
			if strings.Contains(string(restored), "dpx_v1_") {
				t.Errorf("symbol leaked to the client: %s", restored)
			}
		})
	}
}

// Shadow mode through the same executor: the body that leaves is the control's.
func TestDPXAliasLaneAntigravityShadowIsByteIdentical(t *testing.T) {
	g := antigravityGoldens[0]
	request := readLaneGolden(t, g.requestFile)
	engine := newLaneEngine(t, brandalias.ModeShadow)
	ir, d := engine.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "shadow", SourceFormat: g.sourceFormat, ToFormat: "antigravity", Body: request})
	if d.Handled || len(ir.Body) != 0 || d.BrandIn == 0 || d.BrandOut != 0 {
		t.Fatalf("shadow must measure without rewriting: %+v", d)
	}
}

// Mutation arm: a brand word injected where no walker looks must be caught by
// the gate. If this test ever passes with n == 0 the gate is blind.
func TestDPXAliasLaneMutationArmHasTeeth(t *testing.T) {
	g := antigravityGoldens[1]
	request := readLaneGolden(t, g.requestFile)
	mutated, err := sjson.SetBytes(request, "generationConfig.injected", brandalias.DefaultWords[0]+" was here")
	if err != nil {
		t.Fatal(err)
	}
	engine := newLaneEngine(t, brandalias.ModeEnabled)
	ir, d := engine.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "mut", SourceFormat: g.sourceFormat, ToFormat: "antigravity", Body: mutated})
	if !d.Handled {
		t.Fatalf("%+v", d)
	}
	if d.BrandOut == 0 || brandalias.CountBrandTokens(ir.Body, brandalias.DefaultWords) == 0 {
		t.Fatalf("gate has no teeth: injected brand word not counted (decision %+v)", d)
	}
	// And the plugin never touched the field it does not walk.
	if got := gjson.GetBytes(ir.Body, "generationConfig.injected").String(); !strings.HasPrefix(got, brandalias.DefaultWords[0]) {
		t.Fatalf("walker reached a fingerprint-class field: %q", got)
	}
}

// Determinism golden: the aliased source body for a fixed binding is pinned,
// so a codec change that silently re-maps symbols (and breaks every cached
// prompt prefix) is a visible diff.
func TestDPXAliasLaneGoldenPinned(t *testing.T) {
	g := antigravityGoldens[1]
	request := readLaneGolden(t, g.requestFile)
	engine := newLaneEngine(t, brandalias.ModeEnabled)
	ir, d := engine.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "pin", SourceFormat: g.sourceFormat, ToFormat: "antigravity", Body: request})
	if !d.Handled {
		t.Fatalf("%+v", d)
	}
	want := readLaneGolden(t, "gemini-request.aliased.json")
	var gotC, wantC bytes.Buffer
	if err := json.Compact(&gotC, ir.Body); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&wantC, want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotC.Bytes(), wantC.Bytes()) {
		t.Fatalf("aliased golden drifted:\n got  %s\n want %s", gotC.Bytes(), wantC.Bytes())
	}
}
