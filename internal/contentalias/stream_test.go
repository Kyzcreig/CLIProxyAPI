package contentalias

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func event(v any) []byte { b, _ := json.Marshal(v); return []byte("data: " + string(b) + "\n\n") }
func toolEvents(name, key string) []byte {
	var out []byte
	for _, v := range []any{
		map[string]any{"type": "message_start", "message": map[string]any{"content": []any{}, "usage": map[string]int{"input_tokens": 7}}},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "call1", "name": name, "input": map[string]any{}}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": "{\"" + key + "\":"}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": "\"Hermes Velorin\"}"}},
		map[string]any{"type": "content_block_stop", "index": 0},
		map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}},
		map[string]any{"type": "message_stop"},
	} {
		out = append(out, event(v)...)
	}
	return out
}
func TestStreamAllSplitPointsAndInterleaving(t *testing.T) {
	s := testSession(t)
	wire, m, err := Prepare([]byte(requestFixture), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	tool := n.get("tools").items[0]
	key := tool.get("input_schema").get("properties").fields[0].key.text
	raw := toolEvents(tool.get("name").str(), key)
	var baseline []byte
	for split := 0; split <= len(raw); split++ {
		stream := m.NewStream()
		a, err := stream.Feed(raw[:split])
		if err != nil {
			t.Fatal(err)
		}
		b, err := stream.Feed(raw[split:])
		if err != nil {
			t.Fatal(err)
		}
		c, err := stream.Finish()
		if err != nil {
			t.Fatal(err)
		}
		got := bytes.Join([][]byte{a, b, c}, nil)
		if split == 0 {
			baseline = got
		} else if !bytes.Equal(baseline, got) {
			t.Fatal("split-dependent output")
		}
	}
	if !bytes.Contains(baseline, []byte(`"name":"Bash"`)) || !bytes.Contains(baseline, []byte(`\"command\"`)) || !bytes.Contains(baseline, []byte("Hermes Velorin")) {
		t.Fatalf("not restored %s", baseline)
	}
}
func TestStreamIncompleteNeverReleased(t *testing.T) {
	s := testSession(t)
	wire, m, err := Prepare([]byte(requestFixture), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	tool := n.get("tools").items[0]
	raw := toolEvents(tool.get("name").str(), "bad")
	stop := bytes.Index(raw, []byte(`data: {"index":0,"type":"content_block_stop"}`))
	if stop < 0 {
		t.Fatal("fixture stop")
	}
	st := m.NewStream()
	out, err := st.Feed(raw[:stop])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("tool_use")) {
		t.Fatal("incomplete tool escaped")
	}
	if _, err := st.Finish(); err == nil {
		t.Fatal("EOF accepted")
	}
}
func TestStreamProseAcrossDeltaEvents(t *testing.T) {
	s := testSession(t)
	wire, m, err := Prepare([]byte(`{"system":"Hermes"}`), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	symbol := n.get("system").str()
	for split := 0; split <= len(symbol); split++ {
		st := m.NewStream()
		raw := event(map[string]string{"type": "message_start"})
		raw = append(raw, event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})...)
		for _, text := range []string{symbol[:split], symbol[split:]} {
			raw = append(raw, event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": text}})...)
		}
		raw = append(raw, event(map[string]any{"type": "content_block_stop", "index": 0})...)
		raw = append(raw, event(map[string]string{"type": "message_stop"})...)
		out, err := st.Feed(raw)
		if err != nil {
			t.Fatal(err)
		}
		end, err := st.Finish()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, end...)
		if !bytes.Contains(out, []byte("Hermes")) || bytes.Contains(out, []byte(symbol)) {
			t.Fatal(fmt.Sprintf("text split %d: %s", split, out))
		}
	}
}
