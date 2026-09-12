package contentalias

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewSchemaNodesNotPropertyDictionaries(t *testing.T) {
	for _, sc := range []string{`{"type":"object","properties":{"$ref":{"type":"string"}}}`, `{"type":"object","properties":{"x":{},"default":{"$ref":"#/properties/x"}}}`} {
		func() {
			defer func() {
				if recover() != nil {
					t.Error("schema traversal panicked")
				}
			}()
			wire, _, err := Prepare([]byte(`{"tools":[{"name":"T","input_schema":`+sc+`}]}`), testSession(t))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(wire, []byte(`"$ref":"#/properties/x"`)) {
				t.Error("reference left broken")
			}
		}()
	}
}
func TestReviewMalformedTextDeltas(t *testing.T) {
	_, m, err := Prepare([]byte(`{"system":"Hermes"}`), testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []string{`{"type":"text_delta"}`, `{"type":"text_delta","text":1}`} {
		func() {
			defer func() {
				if recover() != nil {
					t.Error("delta panic")
				}
			}()
			s := m.NewStream()
			_, err := s.Feed(append(append(event(map[string]string{"type": "message_start"}), event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})...), []byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":"+delta+"}\n\n")...))
			if err == nil {
				t.Error("malformed delta accepted")
			}
		}()
	}
}
func TestReviewEmptyDeltaQueueBound(t *testing.T) {
	wire, m, err := Prepare([]byte(requestFixture), testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	name := n.get("tools").items[0].get("name").str()
	s := m.NewStream()
	_, err = s.Feed(append(event(map[string]string{"type": "message_start"}), event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "c", "name": name, "input": map[string]any{}}})...))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		_, err = s.Feed(event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": ""}}))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(s.queue) > 3 || len(s.blocks[0].deltas) > 1 {
		t.Fatal("empty deltas retain unbounded records")
	}
}
func TestReviewDynamicKeyAliasCollision(t *testing.T) {
	s := testSession(t)
	wire, _, err := Prepare([]byte(requestFixture), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	alias := n.get("tools").items[0].get("input_schema").get("properties").fields[0].key.text
	raw := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"Bash","id":"c","input":{"` + alias + `":"echo sentinel"}}]}]}`)
	if _, _, err := Prepare(raw, s); err == nil {
		t.Fatal("literal alias dynamic key reinterpreted")
	}
}
func TestReviewPropertyNamesRejected(t *testing.T) {
	raw := []byte(`{"tools":[{"name":"T","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"propertyNames":{"enum":["command"]}}}]}`)
	if _, _, err := Prepare(raw, testSession(t)); err == nil {
		t.Fatal("unsatisfiable renamed schema")
	}
}
func TestReviewAdjacentSymbols(t *testing.T) {
	original := "dpx_v1_fake-Hermes"
	raw, _ := json.Marshal(map[string]string{"system": original})
	wire, m, err := Prepare(raw, testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	response, _ := json.Marshal(map[string]any{"content": []any{map[string]string{"type": "text", "text": n.get("system").str()}}})
	out, err := m.RestoreJSON(response)
	if err != nil {
		t.Fatal(err)
	}
	n, _ = parse(out)
	if n.get("content").items[0].get("text").str() != original {
		t.Fatal("adjacent symbols")
	}
}
func TestReviewStoreToolRelation(t *testing.T) {
	s := testSession(t)
	if _, _, err := Prepare([]byte(requestFixture), s); err != nil {
		t.Fatal(err)
	}
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	tool := st.Tools["Bash"]
	tool.Alias = st.Tools["Read"].Alias
	st.Tools["Bash"] = tool
	if err := s.save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(s.dir, s.binding, s.manifest); err == nil {
		t.Fatal("inconsistent tool alias accepted")
	}
	if err := os.Chmod(filepath.Join(s.dir, "map.json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(s.dir, s.binding, s.manifest); err == nil {
		t.Fatal("public map accepted")
	}
}
