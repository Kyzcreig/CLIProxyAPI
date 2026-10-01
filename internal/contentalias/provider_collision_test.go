package contentalias

import (
	"encoding/json"
	"testing"
)

func TestDeclaredToolCollisionWithLaterProviderTool(t *testing.T) {
	s := testSession(t)
	wire, _, err := Prepare([]byte(`{"tools":[{"name":"T","input_schema":{"type":"object"}}]}`), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	alias := n.get("tools").items[0].get("name").str()
	raw, _ := json.Marshal(map[string]any{"tools": []any{map[string]any{"name": "T", "input_schema": map[string]string{"type": "object"}}, map[string]string{"name": alias, "type": "web_search_20250305"}}})
	if _, _, err := Prepare(raw, s); err == nil {
		t.Fatal("allocated name collides with later opaque tool")
	}
}
