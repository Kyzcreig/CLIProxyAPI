package contentalias

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSchemaScopedPropertyInverse(t *testing.T) {
	keys := []string{"session_id", "conversation_id", "summaryIds", "summary_id", "system_event", "agent_id", "wake_at", "wake_event", "command", "file_path"}
	props := map[string]any{}
	input := map[string]any{}
	for _, k := range keys {
		props[k] = map[string]string{"type": "string"}
		input[k] = "literal " + k
	}
	schemaDef := map[string]any{"type": "object", "properties": props, "required": keys, "dependentRequired": map[string]any{"session_id": []string{"conversation_id"}}}
	request, _ := json.Marshal(map[string]any{"tools": []any{map[string]any{"name": "custom", "input_schema": schemaDef}}, "messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "c", "name": "custom", "input": input}}}}})
	wire, m, err := Prepare(request, testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	block := n.get("messages").items[0].get("content").items[0]
	for _, f := range block.get("input").fields {
		if !strings.HasPrefix(f.key.text, "dpx_v1_p_") {
			t.Fatal("property missing")
		}
	}
	response := append([]byte(`{"content":[`), wire[block.start:block.end]...)
	response = append(response, []byte(`]}`)...)
	out, err := m.RestoreJSON(response)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := parse(out)
	for _, k := range keys {
		if got.get("content").items[0].get("input").get(k).str() != input[k] {
			t.Fatal(k)
		}
	}
}
func TestDeclaredToolSymbolTable(t *testing.T) {
	s := testSession(t)
	raw := []byte(`{"tools":[{"name":"mcp__one__TaskFoo","input_schema":{"type":"object","properties":{"arg":{"type":"string"}}}},{"name":"web_search","type":"web_search_20250305"}],"messages":[{"role":"user","content":[{"type":"tool_reference","name":"mcp__one__TaskFoo"}]}]}`)
	wire, m, err := Prepare(raw, s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	alias := n.get("tools").items[0].get("name").str()
	if n.get("messages").items[0].get("content").items[0].get("name").str() != alias {
		t.Fatal("reference")
	}
	if n.get("tools").items[1].get("name").str() != "web_search" {
		t.Fatal("provider tool changed")
	}
	for _, name := range []string{"mcp__one__TaskFoo", "dpx_v1_t_unknown"} {
		response, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": name, "input": map[string]any{}}}})
		if _, err := m.RestoreJSON(response); err == nil {
			t.Fatal("unknown/double restoration accepted")
		}
	}
}
func TestExecutableValuesUntouched(t *testing.T) {
	s := testSession(t)
	wire, m, err := Prepare([]byte(requestFixture), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	tool := n.get("tools").items[0]
	props := tool.get("input_schema").get("properties")
	key := props.fields[0].key.text
	dict := props.fields[2].key.text
	values := []string{"Hermes", "hErMeS", "OpenClaw", "Velorin", "QuorVane", tool.get("name").str(), key, "/tmp/Hermes", "owner/OpenClaw", "git@x:Owner/Hermes", "https://host/Hermes"}
	for _, v := range values {
		value, _ := json.Marshal(v)
		input := []byte(`{"` + key + `":` + string(value) + `,"` + dict + `":{"` + key + `":` + string(value) + `},"numeric":1.00e+03}`)
		response := []byte(`{"content":[{"type":"tool_use","name":"` + tool.get("name").str() + `","input":` + string(input) + `}]}`)
		out, err := m.RestoreJSON(response)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(out, []byte(`"command":`+string(value))) || !bytes.Contains(out, []byte(`"dict":{"`+key+`":`+string(value)+`}`)) || !bytes.Contains(out, []byte(`1.00e+03`)) {
			t.Fatal("value rewritten")
		}
	}
}
func TestInvalidSchemasRejectBeforeSave(t *testing.T) {
	for _, sc := range []string{`{"type":"object","properties":{"x":{},"x":{}}}`, `{"$ref":"https://example.com/schema"}`, `{"$ref":"#"}`, `{"patternProperties":{".*":{}}}`, `{"anyOf":[{"properties":{"x":{}}},{"properties":{"y":{}}}]}`, `{"type":"object","properties":{},"required":["missing"]}`} {
		s := testSession(t)
		before, err := s.load()
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = Prepare([]byte(`{"tools":[{"name":"T","input_schema":`+sc+`}]}`), s)
		if err == nil {
			t.Fatal("accepted " + sc)
		}
		after, err := s.load()
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(before)
		b, _ := json.Marshal(after)
		if !bytes.Equal(a, b) {
			t.Fatal("failed request committed map")
		}
	}
}
func TestLocalRefsNestedArraysAndDynamicKeys(t *testing.T) {
	raw := []byte(`{"tools":[{"name":"T","input_schema":{"type":"object","$defs":{"node":{"type":"object","properties":{"session_id":{"type":"string"}}}},"properties":{"array":{"type":"array","items":{"$ref":"#/$defs/node"}},"other":{"$ref":"#/properties/array"}}}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"T","input":{"array":[{"session_id":"Hermes"}],"other":[{"session_id":"OpenClaw"}]}}]}]}`)
	wire, m, err := Prepare(raw, testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	tool := n.get("tools").items[0]
	props := tool.get("input_schema").get("properties")
	ref := props.fields[1].value.get("$ref").str()
	if ref != "#/properties/"+props.fields[0].key.text {
		t.Fatal("pointer not mapped: " + ref)
	}
	block := n.get("messages").items[0].get("content").items[0]
	out, err := m.RestoreJSON([]byte(`{"content":[` + string(wire[block.start:block.end]) + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"array":[{"session_id":"Hermes"}]`)) || !bytes.Contains(out, []byte(`"other":[{"session_id":"OpenClaw"}]`)) {
		t.Fatal("nested ref inverse")
	}
}
