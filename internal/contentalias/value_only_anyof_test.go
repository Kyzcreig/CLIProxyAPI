package contentalias

import (
	"bytes"
	"strings"
	"testing"
)

// t_9cc235a9: Hermes's terminal tool declares `notify` as
// anyOf:[{type:boolean},{type:array,items:{type:string}}]. That shape names no
// property in any alternative, so nothing in it needs a key alias, but the
// combinator check allowed only type/enum/const per alternative and refused
// the whole request (`ambiguous_schema`). With host tools on every d-l row, every
// dtlx/dslx turn that carries the terminal tool hit that refusal.
const valueOnlyAnyOfTool = `{"name":"mcp__bpx__terminal","description":"Run a command","input_schema":{"type":"object","properties":{"command":{"type":"string"},"notify":{"description":"pattern list or true","anyOf":[{"type":"boolean"},{"type":"array","items":{"type":"string"}}]}},"required":["command"]}}`

func TestValueOnlyAnyOfItemsAccepted(t *testing.T) {
	request := []byte(`{"tools":[` + valueOnlyAnyOfTool + `],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"mcp__bpx__terminal","input":{"command":"ls","notify":["ready"]}}]}]}`)
	wire, m, err := Prepare(request, testSession(t))
	if err != nil {
		t.Fatalf("value-only anyOf with items refused: %v", err)
	}
	n, err := parse(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(n.get("tools").items[0].get("name").str(), "dpx_v1_") {
		t.Fatal("tool name not aliased")
	}
	// The alternatives carry no property keys, so they leave unchanged.
	if !bytes.Contains(wire, []byte(`"anyOf":[{"type":"boolean"},{"type":"array","items":{"type":"string"}}]`)) {
		t.Fatalf("value-only alternatives were rewritten: %s", wire)
	}
	block := n.get("messages").items[0].get("content").items[0]
	response := append([]byte(`{"content":[`), wire[block.start:block.end]...)
	response = append(response, []byte(`]}`)...)
	out, err := m.RestoreJSON(response)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"notify":["ready"]`)) || !bytes.Contains(out, []byte(`"command":"ls"`)) {
		t.Fatalf("arguments did not round-trip: %s", out)
	}
}

// Momus lens note (t_9cc235a9): `items:{type:object}` (free-form object items, no
// declared keys) is admitted like a bare {type:object} alternative already was.
// Its keys are not schema properties, so they pass through unaliased and
// round-trip unchanged.
func TestValueOnlyAnyOfFreeFormObjectItems(t *testing.T) {
	tool := `{"name":"T","input_schema":{"type":"object","properties":{"v":{"anyOf":[{"type":"boolean"},{"type":"array","items":{"type":"object"}}]}}}}`
	request := []byte(`{"tools":[` + tool + `],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"T","input":{"v":[{"free_key":"x"}]}}]}]}`)
	wire, m, err := Prepare(request, testSession(t))
	if err != nil {
		t.Fatalf("free-form object items refused: %v", err)
	}
	if !bytes.Contains(wire, []byte(`{"free_key":"x"}`)) {
		t.Fatalf("free-form keys were rewritten: %s", wire)
	}
	n, _ := parse(wire)
	block := n.get("messages").items[0].get("content").items[0]
	response := append([]byte(`{"content":[`), wire[block.start:block.end]...)
	response = append(response, []byte(`]}`)...)
	out, err := m.RestoreJSON(response)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"v":[{"free_key":"x"}]`)) {
		t.Fatalf("arguments did not round-trip: %s", out)
	}
}

// t_91e140ec: structural alternatives are admitted when their key map is
// branch-free (structural_anyof_test.go); these former refusals are now in
// TestStructuralAnyOfAdmitted. A tuple `items` array is still no schema the
// compiler accepts, so it fails closed.
func TestStructuralAnyOfStillRefused(t *testing.T) {
	sc := `{"type":"object","properties":{"command":{"type":"string"},"v":{"anyOf":[{"type":"boolean"},{"type":"array","items":[{"type":"string"}]}]}}}`
	if _, _, err := Prepare([]byte(`{"tools":[{"name":"T","input_schema":`+sc+`}]}`), testSession(t)); err == nil {
		t.Error("tuple items alternative: want refusal, got nil")
	}
}
