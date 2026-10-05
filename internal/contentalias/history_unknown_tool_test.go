package contentalias

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// t_ca4ca1e2 (10-05 13:20-13:27): an Apollo session's history carried a
// tool_use for "ssh", a tool that never existed (the caller answered "Tool
// 'ssh' does not exist"). On the DPX lane every request of that session got
// 400 contentalias:history_tool, deterministic, so 17 seats x2 retries all
// failed. A history-only name must be aliased deterministically and stay
// non-callable.
const unknownHistoryRequest = `{"tools":[{"name":"terminal","description":"Run a command.","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],` +
	`"messages":[` +
	`{"role":"user","content":"check the deploy leg"},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"ssh","input":{"command":"cd ~/.hermes && sed -n 440,500p scripts/x.sh"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","is_error":true,"content":"Tool 'ssh' does not exist. Available tools: terminal"}]},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_02","name":"terminal","input":{"command":"ls"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_02","content":"ok"}]}]}`

func TestHistoryToolUseAbsentFromToolsIsAliased(t *testing.T) {
	s := testSession(t)
	wire, m, err := Prepare([]byte(unknownHistoryRequest), s)
	if err != nil {
		t.Fatalf("history tool_use absent from tools[] refused: %v", err)
	}
	n, err := parse(wire)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.load()
	if err != nil {
		t.Fatalf("store does not reload after history-only alias: %v", err)
	}
	want := symbol(st.Binding, "t", "ssh")
	got := n.get("messages").items[1].get("content").items[0].get("name").str()
	if got != want {
		t.Fatalf("history-only name = %q, want deterministic %q", got, want)
	}
	if bytes.Contains(wire, []byte(`"name":"ssh"`)) {
		t.Fatal("original history tool name left DPX")
	}
	if bytes.Contains(wire, []byte("hermes")) {
		t.Fatal("tool_use input value not encoded")
	}
	if _, registered := st.Tools["ssh"]; registered {
		t.Fatal("history-only name registered as a declared tool")
	}
	// The model must not be able to call it: it was never offered.
	call := []byte(`{"content":[{"type":"tool_use","id":"toolu_03","name":"` + want + `","input":{}}]}`)
	if _, err := m.RestoreJSON(call); err == nil {
		t.Fatal("history-only tool became callable")
	}
	// Same body again: identical wire (stable alias, prefix cache intact).
	again, _, err := Prepare([]byte(unknownHistoryRequest), s)
	if err != nil || !bytes.Equal(again, wire) {
		t.Fatalf("second Prepare not byte-identical: %v", err)
	}
	// If the tool is declared later, it keeps the same alias.
	declared := strings.Replace(unknownHistoryRequest, `"tools":[`, `"tools":[{"name":"ssh","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}},`, 1)
	out, _, err := Prepare([]byte(declared), s)
	if err != nil {
		t.Fatalf("declaring a previously history-only tool: %v", err)
	}
	dn, _ := parse(out)
	if dn.get("tools").items[0].get("name").str() != want {
		t.Fatal("declared alias differs from history-only alias")
	}
}

func TestHistoryReferenceAbsentFromToolsIsAliased(t *testing.T) {
	s := testSession(t)
	for _, block := range referenceShapes("unknown") {
		raw := []byte(`{"messages":[{"role":"user","content":[` + block + `]}]}`)
		wire, m, err := Prepare(raw, s)
		if err != nil {
			t.Fatalf("history tool_reference absent from tools[] refused: %v", err)
		}
		st, _ := s.load()
		alias := symbol(st.Binding, "t", "unknown")
		if !bytes.Contains(wire, []byte(`"tool_name":"`+alias+`"`)) {
			t.Fatalf("reference not aliased: %s", wire)
		}
		response := []byte(`{"content":[` + strings.Replace(block, `"unknown"`, `"`+alias+`"`, 1) + `]}`)
		if _, err := m.RestoreJSON(response); err == nil {
			t.Fatal("history-only reference enabled in a response")
		}
	}
}

func TestHistoryToolNameShapedLikeAliasStillRefused(t *testing.T) {
	s := testSession(t)
	before, _ := s.load()
	beforeJSON, _ := json.Marshal(before)
	raw := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"dpx_v1_t_000000000000000000000000","input":{}}]}]}`)
	if out, _, err := Prepare(raw, s); err == nil || len(out) > 0 {
		t.Fatal("alias-shaped history name not refused")
	}
	raw = []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"","input":{}}]}]}`)
	if out, _, err := Prepare(raw, s); err == nil || len(out) > 0 {
		t.Fatal("empty history name not refused")
	}
	after, _ := s.load()
	afterJSON, _ := json.Marshal(after)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatal("refused preparation changed persisted map")
	}
}
