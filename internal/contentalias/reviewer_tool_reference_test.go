package contentalias

import (
	"bytes"
	"fmt"
	"testing"
)

func TestReviewNativeToolReferenceContract(t *testing.T) {
	const tools = `"tools":[{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}]`
	t.Run("forward_top_level", func(t *testing.T) {
		raw := []byte(`{` + tools + `,"messages":[{"role":"user","content":[{"type":"tool_reference","tool_name":"Bash"}]}]}`)
		_, _, err := Prepare(raw, testSession(t))
		if err != nil {
			t.Fatalf("valid pinned-CPA tool_reference rejected: %v", err)
		}
	})
	t.Run("forward_nested_result", func(t *testing.T) {
		raw := []byte(`{` + tools + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":[{"type":"tool_reference","tool_name":"Bash"}]}]}]}`)
		wire, _, err := Prepare(raw, testSession(t))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(wire, []byte(`"tool_name":"Bash"`)) {
			t.Fatal("nested tool reference still advertises original name beside aliased declaration")
		}
	})
	wire, m, err := Prepare([]byte(`{`+tools+`}`), testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	alias := n.get("tools").items[0].get("name").str()
	t.Run("inverse_json", func(t *testing.T) {
		_, err := m.RestoreJSON([]byte(fmt.Sprintf(`{"content":[{"type":"tool_reference","tool_name":%q}]}`, alias)))
		if err != nil {
			t.Fatalf("valid reference response rejected: %v", err)
		}
	})
	t.Run("inverse_stream", func(t *testing.T) {
		raw := event(map[string]any{"type": "message_start", "message": map[string]any{"content": []any{}}})
		raw = append(raw, event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_reference", "tool_name": alias}})...)
		raw = append(raw, event(map[string]any{"type": "content_block_stop", "index": 0})...)
		raw = append(raw, event(map[string]any{"type": "message_stop"})...)
		out, err := m.RestoreSSE(raw)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte(alias)) {
			t.Fatal("allocated tool alias escaped native SSE inverse")
		}
	})
}
