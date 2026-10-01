package contentalias

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestToolReferenceHistoryAndAtomicRefusal(t *testing.T) {
	s := testSession(t)
	wire, _, err := Prepare([]byte(requestFixture), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	alias := n.get("tools").items[0].get("name").str()
	// Valid original history retains its established alias without re-advertising
	// a removed tool as an available response capability.
	for _, block := range referenceShapes("Bash") {
		raw := []byte(`{"messages":[{"role":"user","content":[` + block + `]}]}`)
		out, m, e := Prepare(raw, s)
		if e != nil || !bytes.Contains(out, []byte(alias)) {
			t.Fatalf("historical mapping: %v", e)
		}
		response := []byte(`{"content":[` + strings.Replace(block, `"Bash"`, `"`+alias+`"`, 1) + `]}`)
		if _, e := m.RestoreJSON(response); e == nil {
			t.Fatal("historical-only capability enabled")
		}
	}
	before, _ := s.load()
	beforeJSON, _ := json.Marshal(before)
	bad := referenceShapes("unknown")
	for _, block := range referenceShapes("Bash") {
		bad = append(bad, strings.Replace(block, `{`, `{"signature":"signed",`, 1))
		bad = append(bad, strings.Replace(block, `"tool_name"`, `"signature":"signed","tool_name"`, 1))
	}
	for _, block := range bad {
		raw := []byte(`{"system":"Hermes novel spelling hERmes","messages":[{"role":"user","content":[` + block + `]}]}`)
		if out, _, e := Prepare(raw, s); e == nil || len(out) > 0 {
			t.Fatal("invalid reference not refused")
		}
		after, _ := s.load()
		afterJSON, _ := json.Marshal(after)
		if !bytes.Equal(beforeJSON, afterJSON) {
			t.Fatal("failed preparation changed persisted map")
		}
	}
}
