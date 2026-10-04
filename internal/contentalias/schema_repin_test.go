package contentalias

import (
	"strings"
	"testing"
)

// t_dbf31d31: a unit's alias map outlives any one client build. When the
// caller ships a new tool schema for a tool name the map already holds (here:
// a property description edit, as Hermes a1c31d3a79 did to kanban_create.wake),
// Prepare must re-pin the stored schema, not refuse every tool-bearing request
// with contentalias:schema_changed until the map is wiped.
func schemaRepinRequest(wakeDescription string, extraProperty string) string {
	props := `"title":{"type":"string"},"wake":{"type":"boolean","description":"` + wakeDescription + `"}`
	if extraProperty != "" {
		props += `,"` + extraProperty + `":{"type":"string"}`
	}
	return `{"model":"sandbox","tools":[{"name":"kanban_create","description":"create a card","input_schema":{"type":"object","properties":{` + props + `},"required":["title"]}}],"messages":[{"role":"user","content":"go"}]}`
}

func toolPropertyAliases(t *testing.T, wire []byte) (string, map[string]string) {
	t.Helper()
	n, err := parse(wire)
	if err != nil {
		t.Fatal(err)
	}
	tool := n.get("tools").items[0]
	props := map[string]string{}
	for i, f := range tool.get("input_schema").get("properties").fields {
		props[[]string{"title", "wake", "board"}[i]] = f.key.text
	}
	return tool.get("name").str(), props
}

func TestPrepareRepinsChangedToolSchema(t *testing.T) {
	s := testSession(t)
	first, _, err := Prepare([]byte(schemaRepinRequest("Defaults to false", "")), s)
	if err != nil {
		t.Fatal(err)
	}
	toolAlias, before := toolPropertyAliases(t, first)

	second, m, err := Prepare([]byte(schemaRepinRequest("Default: wake", "board")), s)
	if err != nil {
		t.Fatalf("changed schema for a known tool must re-pin, got %v", err)
	}
	if !strings.Contains(string(second), "Default: wake") {
		t.Fatal("the new schema must be what goes upstream")
	}
	gotAlias, after := toolPropertyAliases(t, second)
	if gotAlias != toolAlias || after["title"] != before["title"] || after["wake"] != before["wake"] {
		t.Fatalf("unchanged names must keep their aliases across a re-pin: %v -> %v", before, after)
	}
	if after["board"] == "board" || after["board"] == "" {
		t.Fatal("an added property must be aliased")
	}

	// The re-pinned map restores a tool call that uses old and new properties.
	resp := `{"content":[{"type":"tool_use","id":"c1","name":"` + toolAlias + `","input":{"` + after["title"] + `":"x","` + after["board"] + `":"y"}}]}`
	out, err := m.RestoreJSON([]byte(resp))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"content":[{"type":"tool_use","id":"c1","name":"kanban_create","input":{"title":"x","board":"y"}}]}`; string(out) != want {
		t.Fatalf("restore after re-pin:\n got %s\nwant %s", out, want)
	}

	// A later toolless request (history only) compiles from the re-pinned schema.
	history := `{"model":"sandbox","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"kanban_create","input":{"title":"x","board":"y"}}]}]}`
	wire, _, err := Prepare([]byte(history), s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"`+after["board"]+`":"y"`) {
		t.Fatalf("history must alias with the re-pinned schema: %s", wire)
	}
}
