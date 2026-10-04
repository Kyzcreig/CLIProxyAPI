package contentalias

import (
	"bytes"
	"testing"
)

// A long-lived DPX map outlives client deploys: the same tool name comes back
// with an edited input_schema (t_af766eca: a description change on one
// property refused every later tool turn with schema_changed until the unit
// restarted). The latest declaration wins; aliases depend only on names.
func TestSchemaRedeclarationRebinds(t *testing.T) {
	s := testSession(t)
	first := `{"tools":[{"name":"K","input_schema":{"type":"object","properties":{"wake":{"type":"boolean","description":"old"},"gone":{"type":"string"}}}}]}`
	wire1, _, err := Prepare([]byte(first), s)
	if err != nil {
		t.Fatal(err)
	}
	second := `{"tools":[{"name":"K","input_schema":{"type":"object","properties":{"wake":{"type":"boolean","description":"new"},"added":{"type":"string"}}}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"u","name":"K","input":{"wake":true,"added":"x","gone":"y"}}]}]}`
	wire2, m, err := Prepare([]byte(second), s)
	if err != nil {
		t.Fatalf("redeclared schema refused: %v", err)
	}
	n1, _ := parse(wire1)
	n2, _ := parse(wire2)
	a1 := n1.get("tools").items[0].get("name").str()
	a2 := n2.get("tools").items[0].get("name").str()
	if a1 != a2 || a1 == "K" {
		t.Fatalf("tool alias changed or missing: %q %q", a1, a2)
	}
	if bytes.Contains(wire2, []byte(`"added"`)) || bytes.Contains(wire2, []byte(`"wake"`)) {
		t.Errorf("declared properties left unaliased: %s", wire2)
	}
	if canonical(m.st.Tools["K"].Schema) == canonical([]byte(`{"type":"object","properties":{"wake":{"type":"boolean","description":"old"},"gone":{"type":"string"}}}`)) {
		t.Error("store kept the stale schema")
	}
	// A third request that declares no tools still restores history under the stored (new) schema.
	if _, _, err := Prepare([]byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"v","name":"K","input":{"added":"x"}}]}]}`), s); err != nil {
		t.Fatalf("history under rebound schema: %v", err)
	}
}
