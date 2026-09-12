package contentalias

import (
	"bytes"
	"fmt"
	"testing"
)

func reviewStream(t *testing.T) (*Stream, string, string) {
	t.Helper()
	wire, m, err := Prepare([]byte(requestFixture), testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	tool := n.get("tools").items[0]
	return m.NewStream(), tool.get("name").str(), tool.get("input_schema").get("properties").fields[0].key.text
}

func TestReview2BareCRRejected(t *testing.T) {
	s, _, _ := reviewStream(t)
	raw := []byte("event: content_block_start\rdata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"c\",\"name\":\"Bash\",\"input\":{}}}\n\n")
	if out, err := s.Feed(raw); err == nil || len(out) != 0 {
		t.Fatal("bare CR executable frame escaped validation")
	}
}

func TestReview2CompletedIndicesBounded(t *testing.T) {
	s, _, _ := reviewStream(t)
	if _, err := s.Feed(event(map[string]string{"type": "message_start"})); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 140000; i++ {
		raw := append(event(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]string{"type": "text", "text": ""}}), event(map[string]any{"type": "content_block_stop", "index": i})...)
		if _, err := s.Feed(raw); err != nil {
			return
		}
	}
	t.Fatal("completed index storage exceeded total response budget")
}

func TestReview2HeldCommentsBounded(t *testing.T) {
	s, name, _ := reviewStream(t)
	raw := append(event(map[string]string{"type": "message_start"}), event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "name": name, "id": "c", "input": map[string]any{}}})...)
	if _, err := s.Feed(raw); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		if _, err := s.Feed([]byte(":\n\n")); err != nil {
			return
		}
	}
	if len(s.queue) > 3 {
		t.Fatal("comments retained one queue allocation per event")
	}
}

func TestReview2LiteralSchemaConstraints(t *testing.T) {
	for _, constraint := range []string{`"const":{"command":"echo sentinel"}`, `"enum":[{"command":"echo sentinel"}]`, `"anyOf":[{"const":{"command":"echo sentinel"}}]`, `"oneOf":[{"enum":[{"command":"echo sentinel"}]}]`} {
		t.Run(constraint, func(t *testing.T) {
			raw := []byte(`{"tools":[{"name":"T","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],` + constraint + `}}]}`)
			if _, _, err := Prepare(raw, testSession(t)); err == nil {
				t.Fatal("conflicting literal constraint accepted")
			}
		})
	}
}

func TestReview2DynamicNamespaceRoundTrip(t *testing.T) {
	raw := []byte(`{"tools":[{"name":"T","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"additionalProperties":true}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"T","id":"c","input":{"command":"echo sentinel","dpx_v1_customer":"literal"}}]}]}`)
	wire, m, err := Prepare(raw, testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	b := n.get("messages").items[0].get("content").items[0]
	response := append(append([]byte(`{"content":[`), wire[b.start:b.end]...), []byte(`]}`)...)
	out, err := m.RestoreJSON(response)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"command":"echo sentinel","dpx_v1_customer":"literal"`)) {
		t.Fatal("dynamic key changed")
	}
}

func TestReview2TerminalDeltaRejectsTools(t *testing.T) {
	s, name, key := reviewStream(t)
	raw := event(map[string]string{"type": "message_start"})
	raw = append(raw, event(map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}})...)
	if _, err := s.Feed(raw); err != nil {
		t.Fatal(err)
	}
	body := toolEvents(name, key)
	first := bytes.Index(body, []byte("\n\n")) + 2
	if out, err := s.Feed(body[first:]); err == nil || bytes.Contains(out, []byte("tool_use")) {
		t.Fatal(fmt.Sprintf("post-terminal tool accepted: %v", err))
	}
}
