package contentalias

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLiteralSymbolsInsideExemptSpans(t *testing.T) {
	s := testSession(t)
	wire, _, err := Prepare([]byte(`{"system":"Hermes"}`), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	alias := n.get("system").str()
	for _, original := range []string{"`" + alias + "`", "/tmp/" + alias, "owner/" + alias, "prefix" + alias, "Hermes2 2Hermes"} {
		raw, _ := json.Marshal(map[string]string{"system": original})
		wire, m, err := Prepare(raw, s)
		if err != nil {
			t.Fatal(err)
		}
		n, _ = parse(wire)
		response, _ := json.Marshal(map[string]any{"content": []any{map[string]string{"type": "text", "text": n.get("system").str()}}})
		out, err := m.RestoreJSON(response)
		if err != nil {
			t.Fatal(err)
		}
		n, _ = parse(out)
		if n.get("content").items[0].get("text").str() != original {
			t.Fatalf("exempt corruption: %s", original)
		}
	}
}
func TestNoUnboundedAliasSuffix(t *testing.T) {
	s := testSession(t)
	wire, m, err := Prepare([]byte(`{"system":"Hermes"}`), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	symbol := n.get("system").str()
	// The suffix cut falls inside the marker prefix, not only the hash.
	st := m.NewStream()
	raw := event(map[string]string{"type": "message_start"})
	raw = append(raw, event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})...)
	raw = append(raw, event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": symbol + strings.Repeat(" ", 34)}})...)
	raw = append(raw, event(map[string]any{"type": "content_block_stop", "index": 0})...)
	raw = append(raw, event(map[string]string{"type": "message_stop"})...)
	out, err := st.Feed(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Finish(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "dpx_v1_") || !strings.Contains(string(out), "Hermes") {
		t.Fatalf("split marker escaped: %s", out)
	}
}
