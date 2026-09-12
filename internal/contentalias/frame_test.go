package contentalias

import (
	"testing"
)

func TestStreamRejectsAmbiguousFrames(t *testing.T) {
	_, m, err := Prepare([]byte(requestFixture), testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := [][]byte{
		[]byte("event: content_block_start\ndata: {\"type\":\"ping\",\"content_block\":{\"type\":\"tool_use\",\"name\":\"Bash\"}}\n\n"),
		event(map[string]any{"type": "message_start", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{}}}}}),
		append(event(map[string]string{"type": "message_start"}), event(map[string]any{"type": "content_block_start", "content_block": map[string]string{"type": "text", "text": ""}})...),
	}
	for i, raw := range cases {
		if _, err := m.NewStream().Feed(raw); err == nil {
			t.Errorf("ambiguous frame %d accepted", i)
		}
	}
}
func TestSignedToolBlockRejectsBeforeAlias(t *testing.T) {
	s := testSession(t)
	if _, _, err := Prepare([]byte(requestFixture), s); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"Bash","id":"c","signature":"opaque","input":{"command":"echo hi"}}]}]}`)
	if _, _, err := Prepare(raw, s); err == nil {
		t.Fatal("signed tool block changed")
	}
}
