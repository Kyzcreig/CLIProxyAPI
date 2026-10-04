package brandalias

import (
	"bytes"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const geminiRequest = `{"model":"gemini-3-flash","systemInstruction":{"parts":[{"text":"You are Hermes, built on hermes-agent. Docs: https://hermes-agent.nousresearch.com/docs; skills under ~/.hermes/skills. Peers: OpenClaw, open_claw, the claude-apx relay."}]},"contents":[{"role":"user","parts":[{"text":"hi hermes"}]}],"tools":[{"functionDeclarations":[{"name":"hermes_memory","description":"Hermes memory tool","parameters":{"type":"object","properties":{"q":{"type":"string","description":"query for hermes"}}}}]}],"generationConfig":{"temperature":0,"thinkingConfig":{"includeThoughts":true}}}`

func enabledEngine(t *testing.T, yaml string) *Engine {
	t.Helper()
	cfg, err := ParseConfig([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return NewEngine(cfg)
}

func TestConfigRefusesExceptionLane(t *testing.T) {
	for _, lane := range []string{"codex", "openai", "xai", "kimi", "grok", "chatgpt", "moonshot"} {
		if _, err := ParseConfig([]byte("enabled: true\nlanes: [" + lane + "]\n")); err == nil {
			t.Fatalf("lane %q is a policy exception and must be refused at configure time", lane)
		}
	}
	cfg, err := ParseConfig([]byte("enabled: true\nlanes: [antigravity, google, anthropic]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeShadow {
		t.Fatalf("default mode must be shadow, got %q", cfg.Mode)
	}
	if !cfg.LaneEnabled("antigravity") || !cfg.LaneEnabled("gemini") || !cfg.LaneEnabled("claude") {
		t.Fatalf("lane aliases not canonicalised: %v", cfg.Lanes)
	}
}

// Policy conformance (card test v): an excepted lane, an openai target (kimi
// or a relay loopback) and an unlisted lane are all pass-throughs.
func TestPolicyPassThroughs(t *testing.T) {
	e := enabledEngine(t, "enabled: true\nmode: enabled\nlanes: [antigravity]\n")
	cases := []struct {
		name, toFormat, reason string
		meta                   map[string]any
	}{
		{"codex", "codex", ReasonPolicyException, nil},
		{"xai via metadata", "codex", ReasonPolicyException, map[string]any{metaAffinityProvider: "xai"}},
		{"kimi via metadata", "openai", ReasonPolicyException, map[string]any{metaAffinityProvider: "kimi"}},
		{"openai-compatibility loopback", "openai", "openai_target_skipped", nil},
		{"claude not in lanes", "claude", ReasonLaneNotEnabled, nil},
		{"gemini not in lanes", "gemini", ReasonLaneNotEnabled, nil},
		{"unknown", "interactions", "unknown_lane", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, d := e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "r-" + c.name, SourceFormat: "openai", ToFormat: c.toFormat, Metadata: c.meta, Body: []byte(`{"model":"m","messages":[{"role":"user","content":"hermes"}]}`)})
			if d.Handled || len(resp.Body) != 0 || resp.Headers != nil {
				t.Fatalf("must pass through: %+v body=%d", d, len(resp.Body))
			}
			if d.Reason != c.reason {
				t.Fatalf("reason %q, want %q", d.Reason, c.reason)
			}
		})
	}
	if e.Len() != 0 {
		t.Fatalf("pass-throughs must not allocate request state, got %d", e.Len())
	}
}

func TestDisabledAndUnsupportedFormatPassThrough(t *testing.T) {
	off := enabledEngine(t, "enabled: false\nmode: enabled\nlanes: [antigravity]\n")
	if _, d := off.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "r", SourceFormat: "gemini", ToFormat: "antigravity", Body: []byte(geminiRequest)}); d.Handled || d.Reason != ReasonDisabled {
		t.Fatalf("disabled plugin must pass through: %+v", d)
	}
	e := enabledEngine(t, "enabled: true\nmode: enabled\nlanes: [antigravity]\n")
	if _, d := e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "r", SourceFormat: "openai-response", ToFormat: "antigravity", Body: []byte(`{"model":"m","input":"hermes"}`)}); d.Handled || d.Reason != ReasonUnsupportedFormat {
		t.Fatalf("responses-API body must pass through in v1: %+v", d)
	}
	if _, d := e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "r", SourceFormat: "gemini", ToFormat: "antigravity", Body: []byte(`{"contents":[`)}); d.Handled || d.Reason != ReasonInvalidJSON {
		t.Fatalf("a decode error must pass through: %+v", d)
	}
}

// Shadow: the alias and the counts are computed, the body is untouched.
func TestShadowComputesAndLeavesBodyUntouched(t *testing.T) {
	e := enabledEngine(t, "enabled: true\nmode: shadow\nlanes: [antigravity]\n")
	resp, d := e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "r", SourceFormat: "gemini", ToFormat: "antigravity", Body: []byte(geminiRequest)})
	if d.Handled || len(resp.Body) != 0 || d.Reason != ReasonShadow {
		t.Fatalf("shadow must not rewrite: %+v", d)
	}
	if d.BrandIn == 0 || d.BrandOut != 0 || d.Symbols == 0 {
		t.Fatalf("shadow must still measure: %+v", d)
	}
	// A response in shadow is untouched too (no symbols were ever sent).
	out, rd := e.InterceptResponse(pluginapi.ResponseInterceptRequest{RequestID: "r", SourceFormat: "gemini", Body: []byte(`{"candidates":[{"content":{"parts":[{"text":"dpx_v1_w_000000000000000000000000"}]}}]}`)})
	if rd.Handled || len(out.Body) != 0 {
		t.Fatalf("shadow response must pass through: %+v", rd)
	}
}

func TestEnabledGeminiRoundTrip(t *testing.T) {
	e := enabledEngine(t, "enabled: true\nmode: enabled\nlanes: [antigravity]\n")
	resp, d := e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "r", SourceFormat: "gemini", ToFormat: "antigravity", Body: []byte(geminiRequest)})
	if !d.Handled || len(resp.Body) == 0 {
		t.Fatalf("expected an aliased body: %+v", d)
	}
	if n := CountBrandTokens(resp.Body, DefaultWords); n != 0 {
		t.Fatalf("brand tokens left upstream: %d in %s", n, resp.Body)
	}
	// Fingerprint / non-text fields byte-identical.
	for _, p := range []string{"model", "generationConfig", "contents.0.role"} {
		if a, b := gjson.GetBytes(resp.Body, p).Raw, gjson.Get(geminiRequest, p).Raw; a != b {
			t.Fatalf("field %s changed: %s -> %s", p, b, a)
		}
	}
	alias := gjson.GetBytes(resp.Body, "tools.0.functionDeclarations.0.name").Str
	if !strings.HasPrefix(alias, "dpx_v1_w_") || len(alias) != len("dpx_v1_w_")+24 {
		t.Fatalf("tool name not aliased: %q", alias)
	}
	// Non-stream response: text and functionCall.name restored, unknown symbol kept verbatim.
	raw := `{"candidates":[{"content":{"parts":[{"text":"Hello from ` + alias + ` and dpx_v1_w_000000000000000000000000"},{"functionCall":{"name":"` + alias + `","args":{"q":"` + alias + `"}}}]}}]}`
	out, rd := e.InterceptResponse(pluginapi.ResponseInterceptRequest{RequestID: "r", SourceFormat: "gemini", Body: []byte(raw)})
	want := `{"candidates":[{"content":{"parts":[{"text":"Hello from hermes_memory and dpx_v1_w_000000000000000000000000"},{"functionCall":{"name":"hermes_memory","args":{"q":"hermes_memory"}}}]}}]}`
	if !rd.Handled || string(out.Body) != want {
		t.Fatalf("restore mismatch:\n got %s\nwant %s", out.Body, want)
	}
	e.Complete("r")
	if e.Len() != 0 {
		t.Fatal("Complete must drop the request state")
	}
	if _, rd := e.InterceptResponse(pluginapi.ResponseInterceptRequest{RequestID: "r", SourceFormat: "gemini", Body: []byte(raw)}); rd.Handled || rd.Reason != ReasonNoState {
		t.Fatalf("a completed request has no inverse map: %+v", rd)
	}
}

// A symbol split across two stream deltas must still restore (the streaming
// ambush: a per-delta transform silently misses exactly these).
func TestStreamSplitSymbol(t *testing.T) {
	e := enabledEngine(t, "enabled: true\nmode: enabled\nlanes: [antigravity, claude]\n")
	resp, _ := e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "g", SourceFormat: "gemini", ToFormat: "antigravity", Body: []byte(geminiRequest)})
	alias := gjson.GetBytes(resp.Body, "tools.0.functionDeclarations.0.name").Str
	cut := 12

	t.Run("gemini", func(t *testing.T) {
		c1 := `{"candidates":[{"content":{"parts":[{"text":"call ` + alias[:cut] + `"}]}}]}`
		c2 := `{"candidates":[{"content":{"parts":[{"text":"` + alias[cut:] + ` now"}]},"finishReason":"STOP"}]}`
		o1, _ := e.InterceptStreamChunk(pluginapi.StreamChunkInterceptRequest{RequestID: "g", SourceFormat: "gemini", Body: []byte(c1), ChunkIndex: 0})
		o2, _ := e.InterceptStreamChunk(pluginapi.StreamChunkInterceptRequest{RequestID: "g", SourceFormat: "gemini", Body: []byte(c2), ChunkIndex: 1})
		joined := gjson.GetBytes(o1.Body, "candidates.0.content.parts.0.text").Str + gjson.GetBytes(o2.Body, "candidates.0.content.parts.0.text").Str
		if joined != "call hermes_memory now" {
			t.Fatalf("got %q (%s | %s)", joined, o1.Body, o2.Body)
		}
	})

	t.Run("openai", func(t *testing.T) {
		req := `{"model":"gemini-3-flash","messages":[{"role":"system","content":"You are Hermes."},{"role":"user","content":[{"type":"text","text":"use hermes_memory"}]}],"tools":[{"type":"function","function":{"name":"hermes_memory","description":"Hermes memory","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`
		r, d := e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "o", SourceFormat: "openai", ToFormat: "antigravity", Body: []byte(req)})
		if !d.Handled || CountBrandTokens(r.Body, DefaultWords) != 0 {
			t.Fatalf("openai request not aliased: %+v %s", d, r.Body)
		}
		a := gjson.GetBytes(r.Body, "tools.0.function.name").Str
		c1 := `{"id":"x","choices":[{"index":0,"delta":{"content":"call ` + a[:cut] + `"},"finish_reason":null}]}`
		c2 := `{"id":"x","choices":[{"index":0,"delta":{"content":"` + a[cut:] + `"},"finish_reason":null}]}`
		c3 := `{"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"` + a + `","arguments":"{\"q\":\"` + a[:cut] + `"}}]},"finish_reason":null}]}`
		c4 := `{"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"` + a[cut:] + `\"}"}}]},"finish_reason":null}]}`
		c5 := `{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
		var content, args, name string
		for i, c := range []string{c1, c2, c3, c4, c5} {
			o, od := e.InterceptStreamChunk(pluginapi.StreamChunkInterceptRequest{RequestID: "o", SourceFormat: "openai", Body: []byte(c), ChunkIndex: i})
			if !od.Handled {
				t.Fatalf("chunk %d: %+v", i, od)
			}
			body := o.Body
			if len(body) == 0 {
				body = []byte(c)
			}
			content += gjson.GetBytes(body, "choices.0.delta.content").Str
			for _, tc := range gjson.GetBytes(body, "choices.0.delta.tool_calls").Array() {
				args += tc.Get("function.arguments").Str
				if n := tc.Get("function.name").Str; n != "" {
					name = n
				}
			}
		}
		if content != "call hermes_memory" || name != "hermes_memory" || args != `{"q":"hermes_memory"}` {
			t.Fatalf("content=%q name=%q args=%q", content, name, args)
		}
	})

	t.Run("claude", func(t *testing.T) {
		req := `{"model":"claude-sonnet-4-6","system":"You are Hermes.","messages":[{"role":"user","content":"use hermes_memory"}],"tools":[{"name":"hermes_memory","description":"Hermes memory","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`
		r, d := e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "c", SourceFormat: "claude", ToFormat: "claude", Body: []byte(req)})
		if !d.Handled || CountBrandTokens(r.Body, DefaultWords) != 0 {
			t.Fatalf("claude request not aliased: %+v %s", d, r.Body)
		}
		a := gjson.GetBytes(r.Body, "tools.0.name").Str
		ev := func(typ, data string) string { return "event: " + typ + "\ndata: " + data + "\n\n" }
		chunks := []string{
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"call `+a[:cut]+`"}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+a[cut:]+`"}}`),
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
			ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"`+a+`","input":{}}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"`+a[:cut]+`"}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"`+a[cut:]+`\"}"}}`),
			ev("content_block_stop", `{"type":"content_block_stop","index":1}`),
			ev("message_stop", `{"type":"message_stop"}`),
		}
		var text, partial, name string
		for i, c := range chunks {
			o, od := e.InterceptStreamChunk(pluginapi.StreamChunkInterceptRequest{RequestID: "c", SourceFormat: "claude", Body: []byte(c), ChunkIndex: i})
			if !od.Handled {
				t.Fatalf("chunk %d: %+v", i, od)
			}
			body := o.Body
			if len(body) == 0 {
				body = []byte(c)
			}
			for _, event := range bytes.Split(bytes.TrimSpace(body), []byte("\n\n")) {
				for _, line := range bytes.Split(event, []byte("\n")) {
					if !bytes.HasPrefix(line, []byte("data: ")) {
						continue
					}
					data := gjson.ParseBytes(line[6:])
					text += data.Get("delta.text").Str
					partial += data.Get("delta.partial_json").Str
					if n := data.Get("content_block.name").Str; n != "" {
						name = n
					}
				}
			}
		}
		if text != "call hermes_memory" || name != "hermes_memory" || partial != `{"q":"hermes_memory"}` {
			t.Fatalf("text=%q name=%q partial=%q", text, name, partial)
		}
	})
}

// Signed blocks are never touched: a thought part keeps its bytes even when it
// carries a symbol, and a Claude thinking block keeps its signature intact.
func TestSignedBlocksUntouched(t *testing.T) {
	e := enabledEngine(t, "enabled: true\nmode: enabled\nlanes: [antigravity, claude]\n")
	e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "g", SourceFormat: "gemini", ToFormat: "antigravity", Body: []byte(geminiRequest)})
	raw := `{"candidates":[{"content":{"parts":[{"thought":true,"thoughtSignature":"sig","text":"dpx_v1_w_000000000000000000000000 hermes"},{"text":"ok"}]}}]}`
	out, d := e.InterceptResponse(pluginapi.ResponseInterceptRequest{RequestID: "g", SourceFormat: "gemini", Body: []byte(raw)})
	if !d.Handled || len(out.Body) != 0 {
		t.Fatalf("thought part must stay byte-identical: %+v %s", d, out.Body)
	}
	claude := `{"model":"m","system":"hi","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hermes","signature":"abc"},{"type":"text","text":"hermes"}]},{"role":"user","content":"go"}]}`
	r, rd := e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "c", SourceFormat: "claude", ToFormat: "claude", Body: []byte(claude)})
	if !rd.Handled {
		t.Fatalf("%+v", rd)
	}
	if got := gjson.GetBytes(r.Body, "messages.0.content.0").Raw; got != gjson.Get(claude, "messages.0.content.0").Raw {
		t.Fatalf("signed thinking block changed: %s", got)
	}
	if got := gjson.GetBytes(r.Body, "messages.0.content.1.text").Str; !strings.HasPrefix(got, "dpx_v1_w_") {
		t.Fatalf("unsigned text not aliased: %q", got)
	}
}

// Determinism: the same word aliases to the same symbol across requests
// (prompt-cache prefixes stay stable), and Encode is idempotent.
func TestDeterministicAcrossRequests(t *testing.T) {
	e := enabledEngine(t, "enabled: true\nmode: enabled\nlanes: [antigravity]\n")
	a, _ := e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "1", SourceFormat: "gemini", ToFormat: "antigravity", Body: []byte(geminiRequest)})
	b, _ := e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "2", SourceFormat: "gemini", ToFormat: "antigravity", Body: []byte(geminiRequest)})
	if !bytes.Equal(a.Body, b.Body) {
		t.Fatal("alias output differs between requests")
	}
	// A credential retry re-runs the interceptor on the ORIGINAL body
	// (conductor_execution.go: execReq := req per attempt), never on the
	// aliased one; the codec escapes foreign symbols by design, so this is
	// what keeps the retry byte-identical to the first attempt.
	c, d := e.InterceptRequest(pluginapi.RequestInterceptRequest{RequestID: "1", SourceFormat: "gemini", ToFormat: "antigravity", Body: []byte(geminiRequest)})
	if !d.Handled || !bytes.Equal(c.Body, a.Body) {
		t.Fatalf("retry on the same request id diverged: %+v", d)
	}
}

func TestResolveLane(t *testing.T) {
	for _, c := range []struct{ to, lane, reason string }{
		{"antigravity", "antigravity", ""}, {"claude", "claude", ""}, {"gemini", "gemini", ""},
		{"codex", "", "policy_exception"}, {"openai", "", "openai_target_skipped"}, {"", "", "to_format_unset"}, {"interactions", "", "unknown_lane"},
	} {
		if lane, reason := ResolveLane(c.to, nil); lane != c.lane || reason != c.reason {
			t.Fatalf("%q: got (%q,%q) want (%q,%q)", c.to, lane, reason, c.lane, c.reason)
		}
	}
	if lane, _ := ResolveLane("openai", map[string]any{metaAffinityProvider: "kimi"}); lane != "kimi" {
		t.Fatalf("affinity provider must win: %q", lane)
	}
}
