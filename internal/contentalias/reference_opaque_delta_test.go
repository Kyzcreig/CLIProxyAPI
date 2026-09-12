package contentalias

import (
	"bytes"
	"testing"
)

func TestToolReferenceUnselectedResultDeltasOpaque(t *testing.T) {
	_, m, e := Prepare([]byte(requestFixture), testSession(t))
	if e != nil {
		t.Fatal(e)
	}
	for _, block := range []string{`{"type":"tool_result","signature":"signed","content":"Hermes"}`, `{"type":"tool_search_tool_result","content":{"type":"opaque","data":"Hermes"}}`} {
		stream := referenceStream(block)
		delta := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"dpx_v1_t_unknown\"}}\n\n")
		stream = bytes.Replace(stream, []byte("event: content_block_stop"), append(delta, []byte("event: content_block_stop")...), 1)
		out, err := m.RestoreSSE(stream)
		if err != nil || !bytes.Equal(out, stream) {
			t.Fatalf("unselected opaque block changed: %v", err)
		}
	}
}
