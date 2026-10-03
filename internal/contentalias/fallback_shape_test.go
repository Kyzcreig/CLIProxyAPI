package contentalias

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// t_d27c2365: a server-side-fallback request (opus-5-5 -> opus-4-8) carries a
// top-level fallback_credit_token and no thinking block. The aliaser must pass
// both the token and the model through byte-for-byte.
func TestFallbackCreditTokenPassesThrough(t *testing.T) {
	const token = "fct_opaque.Hermes-looking-but-opaque/AAAA+bbbb=="
	raw := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":[{"type":"text","text":"Hermes list src"}]}],"system":[{"type":"text","text":"Hermes"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}],"metadata":{"user_id":"u"},"max_tokens":64000,"output_config":{"effort":"high"},"diagnostics":{"previous_message_id":null},"fallback_credit_token":"` + token + `","stream":true}`)
	wire, m, err := Prepare(raw, testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["fallback_credit_token"]) != `"`+token+`"` {
		t.Fatalf("fallback_credit_token rewritten: %s", got["fallback_credit_token"])
	}
	if string(got["model"]) != `"claude-opus-4-8"` || string(got["output_config"]) != `{"effort":"high"}` {
		t.Fatalf("fallback shape rewritten: model=%s output_config=%s", got["model"], got["output_config"])
	}
	if bytes.Contains(wire, []byte(`"text":"Hermes list src"`)) {
		t.Fatal("content was not aliased")
	}
	if m == nil {
		t.Fatal("no request map")
	}
}

func TestRestoreErrorBody(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":"Hermes /tmp/Hermes-Hermes"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]}`)
	wire, m, err := Prepare(raw, testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Messages []struct{ Content string } `json:"messages"`
		Tools    []struct{ Name string }    `json:"tools"`
	}
	if err := json.Unmarshal(wire, &req); err != nil {
		t.Fatal(err)
	}
	aliasedText, aliasedTool := req.Messages[0].Content, req.Tools[0].Name
	if !strings.Contains(aliasedText, "dpx_v1_w_") || !strings.HasPrefix(aliasedTool, "dpx_v1_t_") {
		t.Fatalf("fixture not aliased: %q %q", aliasedText, aliasedTool)
	}
	unknown := "dpx_v1_w_000000000000000000000000"
	upstream := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages.0: ` + aliasedText + ` / tool ` + aliasedTool + ` / ` + unknown + `"}}`)
	out := m.RestoreErrorBody(upstream)
	if !json.Valid(out) {
		t.Fatalf("restored body is not JSON: %s", out)
	}
	msg := gjsonMessage(t, out)
	if msg != "messages.0: Hermes /tmp/Hermes-Hermes / tool Read / "+unknown {
		t.Fatalf("restore: %q", msg)
	}
	var nilMap *RequestMap
	if !bytes.Equal(nilMap.RestoreErrorBody(upstream), upstream) {
		t.Fatal("nil map must be identity")
	}
}

func gjsonMessage(t *testing.T, body []byte) string {
	t.Helper()
	var v struct {
		Error struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v.Error.Message
}
