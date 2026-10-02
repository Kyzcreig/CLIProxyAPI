package contentalias

import (
	"bytes"
	"testing"
)

// A zero-argument tool call streams as content_block_start(input {}) followed by either a single
// input_json_delta with partial_json "" or no delta at all. Both must restore to a tool_use with
// input {} (t_982a8e9e: Hermes mem0_profile / lcm_status / lcm_doctor failed with
// contentalias:stream_json on the dtlx lane while every tool that takes arguments passed).
func TestStreamZeroArgumentToolUse(t *testing.T) {
	s := testSession(t)
	wire, m, err := Prepare([]byte(requestFixture), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	name := n.get("tools").items[1].get("name").str()
	for _, tc := range []struct {
		label  string
		deltas []string
	}{{"empty_partial_json", []string{""}}, {"no_delta", nil}, {"whitespace", []string{" "}}} {
		raw := event(map[string]any{"type": "message_start", "message": map[string]any{"content": []any{}}})
		raw = append(raw, event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "call1", "name": name, "input": map[string]any{}}})...)
		for _, d := range tc.deltas {
			raw = append(raw, event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": d}})...)
		}
		raw = append(raw, event(map[string]any{"type": "content_block_stop", "index": 0})...)
		raw = append(raw, event(map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}})...)
		raw = append(raw, event(map[string]string{"type": "message_stop"})...)
		st := m.NewStream()
		out, err := st.Feed(raw)
		if err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
		end, err := st.Finish()
		if err != nil {
			t.Fatalf("%s: finish: %v", tc.label, err)
		}
		out = append(out, end...)
		if !bytes.Contains(out, []byte(`"name":"Read"`)) || !bytes.Contains(out, []byte(`"partial_json":"{}"`)) {
			t.Fatalf("%s: not restored: %s", tc.label, out)
		}
		if bytes.Index(out, []byte(`"partial_json":"{}"`)) > bytes.Index(out, []byte(`content_block_stop`)) {
			t.Fatalf("%s: delta after stop: %s", tc.label, out)
		}
	}
}
