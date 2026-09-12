package contentalias

import (
	"bytes"
	"io"
	"testing"
)

func TestUsageObserverRunsBeforeRestoreFailure(t *testing.T) {
	_, m, err := Prepare([]byte(requestFixture), testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	raw := append(event(map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]int{"input_tokens": 3}}}), event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "c", "name": "unknown", "input": map[string]any{}}})...)
	raw = append(raw, event(map[string]any{"type": "message_delta", "usage": map[string]int{"output_tokens": 7}})...)
	var observed []byte
	_, err = io.ReadAll(m.ReaderObserved(bytes.NewReader(raw), func(line []byte) { observed = append(observed, line...) }))
	if err == nil || !bytes.Contains(observed, []byte(`"input_tokens":3`)) || !bytes.Contains(observed, []byte(`"output_tokens":7`)) {
		t.Fatal("genuine usage lost on restore failure")
	}
}
