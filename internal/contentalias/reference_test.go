package contentalias

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// Shapes are the pinned CPA protocol, not arbitrary recursion into result data.
func referenceShapes(name string) []string {
	ref := fmt.Sprintf(`{"type":"tool_reference","tool_name":%q,"name":"ordinary","id":"unchanged"}`, name)
	return []string{ref, `{"type":"tool_result","tool_use_id":"call","content":[` + ref + `]}`, `{"type":"tool_search_tool_result","tool_use_id":"call","content":{"type":"tool_search_tool_search_result","tool_references":[` + ref + `]}}`}
}
func referenceStream(block string) []byte {
	return []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":" + block + "}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}
func TestToolReferenceTypedRoundTrip(t *testing.T) {
	const tools = `"tools":[{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}]`
	for i, original := range referenceShapes("Bash") {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			raw := []byte(`{` + tools + `,"messages":[{"role":"user","content":[` + original + `]}]}`)
			wire, m, err := Prepare(raw, testSession(t))
			if err != nil {
				t.Fatal(err)
			}
			n, _ := parse(wire)
			alias := n.get("tools").items[0].get("name").str()
			encoded := referenceShapes(alias)[i]
			if !bytes.Contains(wire, []byte(encoded)) {
				t.Fatal("reference not encoded exactly")
			}
			out, err := m.RestoreJSON([]byte(`{"content":[` + encoded + `]}`))
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != `{"content":[`+original+`]}` {
				t.Fatal("reference inverse changed other fields")
			}
			stream := referenceStream(encoded)
			for split := 0; split <= len(stream); split++ {
				s := m.NewStream()
				a, e := s.Feed(stream[:split])
				if e != nil {
					t.Fatal(e)
				}
				b, e := s.Feed(stream[split:])
				if e != nil {
					t.Fatal(e)
				}
				if _, e = s.Finish(); e != nil {
					t.Fatal(e)
				}
				out := append(a, b...)
				if !bytes.Contains(out, []byte(original)) || bytes.Contains(out, []byte(alias)) {
					t.Fatalf("split %d", split)
				}
			}
		})
	}
}
func TestToolReferenceFailClosed(t *testing.T) {
	wire, m, err := Prepare([]byte(requestFixture), testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	alias := n.get("tools").items[0].get("name").str()
	for _, name := range []string{"unknown", "dpx_v1_t_unknown", "Bash", ""} {
		for _, block := range referenceShapes(name) {
			if _, e := m.RestoreJSON([]byte(`{"content":[` + block + `]}`)); e == nil {
				t.Fatal("unknown reference accepted")
			}
			if out, e := m.RestoreSSE(referenceStream(block)); e == nil || len(out) > 0 {
				t.Fatal("unknown reference escaped")
			}
		}
	}
	for _, block := range referenceShapes(alias) {
		signed := strings.Replace(block, `{`, `{"signature":"opaque",`, 1)
		if _, e := m.RestoreJSON([]byte(`{"content":[` + signed + `]}`)); e == nil {
			t.Fatal("signed container rewritten")
		}
		if _, e := m.RestoreSSE(referenceStream(signed)); e == nil {
			t.Fatal("signed stream rewritten")
		}
	}
	// A name-only pseudo-reference must not regain the old, incorrect contract.
	if _, e := m.RestoreJSON([]byte(fmt.Sprintf(`{"content":[{"type":"tool_reference","name":%q}]}`, alias))); e == nil {
		t.Fatal("wrong protocol field accepted")
	}
	for _, block := range referenceShapes(alias) {
		stream := referenceStream(block)
		delta := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n")
		stream = bytes.Replace(stream, []byte("event: content_block_stop"), append(delta, []byte("event: content_block_stop")...), 1)
		if _, e := m.RestoreSSE(stream); e == nil {
			t.Fatal("unsupported reference delta accepted")
		}
	}
}
func TestToolReferenceOpaqueValues(t *testing.T) {
	_, m, err := Prepare([]byte(requestFixture), testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	// Only immediate typed references in protocol containers qualify. Values,
	// unknown blocks, text, signed blocks, and deeper result objects do not.
	for _, block := range []string{
		`{"type":"tool_result","content":"Hermes Bash dpx_v1_t_unknown"}`,
		`{"type":"tool_result","content":[{"type":"text","text":"Hermes","tool_name":"Bash"},{"type":"unknown","content":[{"type":"tool_reference","tool_name":"Bash"}]}]}`,
		`{"type":"tool_result","content":{"type":"tool_reference","tool_name":"Bash"}}`,
		`{"type":"unknown","content":{"tool_references":[{"type":"tool_reference","tool_name":"Bash"}]}}`,
		`{"type":"thinking","thinking":"Hermes","signature":"signed","tool_name":"Bash"}`,
	} {
		raw := []byte(`{"messages":[{"role":"user","content":[` + block + `]}]}`)
		wire, _, e := Prepare(raw, testSession(t))
		if e != nil || !bytes.Equal(raw, wire) {
			t.Fatalf("forward opaque changed: %v", e)
		}
		response := []byte(`{"content":[` + block + `]}`)
		out, e := m.RestoreJSON(response)
		if e != nil || !bytes.Equal(response, out) {
			t.Fatalf("inverse opaque changed: %v", e)
		}
		stream := referenceStream(block)
		out, e = m.RestoreSSE(stream)
		if e != nil || !bytes.Equal(stream, out) {
			t.Fatalf("stream opaque changed: %v", e)
		}
	}
}
