package contentalias

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// t_91e140ec: Hermes's skill_manage declares operations.items as an anyOf of
// per-action object branches (create / patch / patch-rewrite / write_file /
// remove_file / delete). Each branch has properties, so the value-only rule
// refused it (`ambiguous_schema`) and every dtlr/dtlx turn carrying the tool
// failed. Property aliases are keyed by (tool, parent path, key), never by
// branch index, so the same key in two branches gets the same alias and both
// directions of the map are branch-free.
func skillManageTool(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/skill-manage-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func roundTripToolUse(t *testing.T, tool []byte, name, input string) ([]byte, []byte) {
	t.Helper()
	request := []byte(`{"tools":[` + string(tool) + `],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"` + name + `","input":` + input + `}]}]}`)
	wire, m, err := Prepare(request, testSession(t))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	n, err := parse(wire)
	if err != nil {
		t.Fatal(err)
	}
	block := n.get("messages").items[0].get("content").items[0]
	response := append([]byte(`{"content":[`), wire[block.start:block.end]...)
	response = append(response, []byte(`]}`)...)
	out, err := m.RestoreJSON(response)
	if err != nil {
		t.Fatal(err)
	}
	return wire, out
}

func TestStructuralAnyOfSkillManageGolden(t *testing.T) {
	tool := skillManageTool(t)
	input := `{"operations":[` +
		`{"name":"a","action":"create","content":"body","category":"devops","local":false},` +
		`{"name":"a","action":"patch","old_string":"x","new_string":"y","replace_all":true,"file_path":"references/r.md"},` +
		`{"name":"a","action":"patch","content":"rewrite"},` +
		`{"name":"a","action":"write_file","file_path":"references/r.md","file_content":"t"},` +
		`{"name":"a","action":"remove_file","file_path":"references/r.md"},` +
		`{"name":"a","action":"delete","absorbed_into":"b"}]}`
	wire, out := roundTripToolUse(t, tool, "skill_manage", input)

	// No original property key survives on the wire, in the schema or in the arguments.
	for _, key := range []string{"operations", "old_string", "new_string", "replace_all", "file_path", "file_content", "absorbed_into", "category"} {
		if bytes.Contains(wire, []byte(`"`+key+`"`)) {
			t.Errorf("property %q leaked to the wire", key)
		}
	}
	// The restored arguments are byte-identical to what the client sent.
	var got, want any
	n, _ := parse(out)
	block := n.get("content").items[0]
	if err := json.Unmarshal(out[block.get("input").start:block.get("input").end], &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(input), &want); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("arguments did not round-trip:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	// Branch-free: a key shared by several branches maps to one alias, and every
	// branch's `required` names that same alias.
	wn, _ := parse(wire)
	items := wn.get("tools").items[0].get("input_schema")
	var opsAlias string
	for _, f := range items.get("properties").fields {
		opsAlias = f.key.text
	}
	branches := items.get("properties").get(opsAlias).get("items").get("anyOf").items
	if len(branches) != 6 {
		t.Fatalf("want 6 branches, got %d", len(branches))
	}
	aliases := map[string]string{}
	for _, b := range branches {
		for _, f := range b.get("properties").fields {
			if prev, ok := aliases[f.key.text]; ok && prev != f.key.text {
				t.Fatalf("alias mismatch")
			}
			aliases[f.key.text] = f.key.text
		}
		for _, r := range b.get("required").items {
			if _, ok := aliases[r.str()]; !ok {
				t.Errorf("required %q is not a declared alias of its branch", r.str())
			}
		}
	}
	// name, action, content, file_path appear in several branches: one alias each.
	if len(aliases) != 11 {
		t.Errorf("want 11 distinct property aliases across branches, got %d", len(aliases))
	}
}

// A model reply (no history) restores through the same branch-free map.
func TestStructuralAnyOfReplyRestore(t *testing.T) {
	tool := skillManageTool(t)
	_, out := roundTripToolUse(t, tool, "skill_manage", `{"operations":[{"name":"a","action":"write_file","file_path":"scripts/x.sh","file_content":"echo"}]}`)
	if !bytes.Contains(out, []byte(`"file_path":"scripts/x.sh"`)) || !bytes.Contains(out, []byte(`"file_content":"echo"`)) || !bytes.Contains(out, []byte(`"operations":[`)) {
		t.Fatalf("restore lost keys: %s", out)
	}
}

// Admitted: object alternatives whose shared keys need the same nested map.
func TestStructuralAnyOfAdmitted(t *testing.T) {
	for _, alt := range []string{
		`{"type":"array","items":{"type":"object","properties":{"x":{"type":"string"}}}}`,
		`{"type":"array","items":{"anyOf":[{"properties":{"x":{}}}]}}`,
		`{"type":"array","items":{"$ref":"#/properties/command"}}`,
		`{"type":"object","additionalProperties":{"type":"string"}}`,
		`{"type":"object","properties":{"command":{"type":"string","description":"other text"}},"required":["command"]}`,
	} {
		sc := `{"type":"object","properties":{"command":{"type":"string"},"v":{"anyOf":[{"type":"boolean"},` + alt + `]}}}`
		if _, _, err := Prepare([]byte(`{"tools":[{"name":"T","input_schema":`+sc+`}]}`), testSession(t)); err != nil {
			t.Errorf("alternative %s: want admitted, got %v", alt, err)
		}
	}
}

// Fail-closed controls: the same key at the same parent needs different nested
// maps in two branches, or a branch is not a schema object.
func TestStructuralAnyOfConflictRefused(t *testing.T) {
	for _, alts := range []string{
		// x is an object with key a in one branch and a free string in the other.
		`{"properties":{"x":{"type":"object","properties":{"a":{}}}}},{"properties":{"x":{"type":"string"}}}`,
		// x's nested keys differ between branches.
		`{"properties":{"x":{"properties":{"a":{}}}}},{"properties":{"x":{"properties":{"b":{}}}}}`,
		// items shapes differ between branches.
		`{"items":{"properties":{"a":{}}}},{"items":{"properties":{"b":{}}}}`,
		// additionalProperties shapes differ between branches.
		`{"additionalProperties":{"properties":{"a":{}}}},{"additionalProperties":{"properties":{"b":{}}}}`,
		// the same key reached through a $ref at another path gets another alias.
		`{"properties":{"x":{}}},{"$ref":"#/$defs/D"}`,
	} {
		sc := `{"type":"object","$defs":{"D":{"properties":{"x":{}}}},"properties":{"v":{"anyOf":[` + alts + `]}}}`
		_, _, err := Prepare([]byte(`{"tools":[{"name":"T","input_schema":`+sc+`}]}`), testSession(t))
		if err == nil {
			t.Errorf("alternatives %s: want refusal, got nil", alts)
		} else if !strings.Contains(err.Error(), "ambiguous_schema") {
			t.Errorf("alternatives %s: want ambiguous_schema, got %v", alts, err)
		}
	}
}
