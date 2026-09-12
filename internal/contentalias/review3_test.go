package contentalias

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestReview3ContainsRejected(t *testing.T) {
	raw := []byte(`{"tools":[{"name":"T","input_schema":{"type":"object","properties":{"envelope":{"type":"array","contains":{"type":"object","properties":{"command":{"type":"string","description":"Hermes command"}},"required":["command"]}}}}}]}`)
	if _, _, err := Prepare(raw, testSession(t)); err == nil {
		t.Fatal("unsupported contains schema dispatched")
	}
}
func TestReview3ReleasedExpansionBudget(t *testing.T) {
	property := strings.Repeat("x", 1<<20)
	raw, _ := json.Marshal(map[string]any{"tools": []any{map[string]any{"name": "T", "input_schema": map[string]any{"type": "object", "properties": map[string]any{property: map[string]string{"type": "string"}}}}}})
	wire, m, err := Prepare(raw, testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	tool := n.get("tools").items[0]
	alias := tool.get("input_schema").get("properties").fields[0].key.text
	stream := event(map[string]string{"type": "message_start"})
	for i := 0; i < 10; i++ {
		stream = append(stream, event(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "name": tool.get("name").str(), "input": map[string]any{}}})...)
		stream = append(stream, event(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]string{"type": "input_json_delta", "partial_json": `{"` + alias + `":""}`}})...)
		stream = append(stream, event(map[string]any{"type": "content_block_stop", "index": i})...)
	}
	stream = append(stream, event(map[string]string{"type": "message_stop"})...)
	out, err := m.NewStream().Feed(stream)
	if err == nil || len(out) > maxPending {
		t.Fatalf("restored batch escaped budget: output=%d err=%v", len(out), err)
	}
}
func TestReview3SignedTextOpaque(t *testing.T) {
	raw := []byte(`{"system":[{ "type":"text", "text":"Hermes", "signature":"opaque" }]}`)
	wire, m, err := Prepare(raw, testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wire, raw) {
		t.Error("signed text changed forward")
	}
	encoded, err := m.st.encodeText("Hermes")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := json.Marshal(map[string]string{"type": "text", "text": encoded, "signature": "opaque"})
	response := []byte(`{"content":[` + string(block) + `]}`)
	out, err := m.RestoreJSON(response)
	if err != nil || !bytes.Equal(out, response) {
		t.Error("signed text changed inverse")
	}
	stream := append(event(map[string]string{"type": "message_start"}), []byte(`data: {"type":"content_block_start","index":0,"content_block":`+string(block)+"}\n\n")...)
	stream = append(stream, event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": encoded}})...)
	stream = append(stream, event(map[string]any{"type": "content_block_stop", "index": 0})...)
	stream = append(stream, event(map[string]string{"type": "message_stop"})...)
	out, err = m.RestoreSSE(stream)
	if err != nil || !bytes.Equal(out, stream) {
		t.Fatal("signed text changed in stream")
	}
}
