package contentalias

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}
func testSession(t *testing.T) *Session {
	t.Helper()
	dir := privateDir(t)
	s, err := Create(dir, Binding{Principal: "sandbox", Session: "00000000-0000-4000-8000-000000000001", Version: "v1"}, DefaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const requestFixture = `{"model":"sandbox","system":"Hermes and hErMeS open_claw OpenClaw Velorin QuorVane","tools":[{"name":"Bash","description":"Hermes tool","input_schema":{"type":"object","properties":{"command":{"type":"string"},"meta":{"type":"object","properties":{"session_id":{"type":"string"}}},"dict":{"type":"object","additionalProperties":true}},"required":["command"]}},{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}],"tool_choice":{"type":"tool","name":"Bash"},"messages":[{"role":"user","content":"Hermes \u0060Hermes\u0060 https://x/Hermes owner/Hermes /tmp/Hermes git@x:owner/Hermes"}]}`

func TestDPXAliasDisabledByteIdentity(t *testing.T) {
	raw := []byte(" not even JSON ")
	wire, m, err := Prepare(raw, nil)
	if err != nil || !bytes.Equal(wire, raw) {
		t.Fatal("off")
	}
	out, err := m.RestoreJSON(raw)
	if err != nil || !bytes.Equal(out, raw) {
		t.Fatal("off inverse")
	}
}
func TestRequestResponseCorpus(t *testing.T) {
	s := testSession(t)
	wire, m, err := Prepare([]byte(requestFixture), s)
	if err != nil {
		t.Fatal(err)
	}
	n, err := parse(wire)
	if err != nil {
		t.Fatal(err)
	}
	tool := n.get("tools").items[0]
	alias := tool.get("name").str()
	if alias == "Bash" {
		t.Fatal("forward absent")
	}
	prop := tool.get("input_schema").get("properties").fields[0].key.text
	if prop == "command" {
		t.Fatal("property forward absent")
	}
	if n.get("tool_choice").get("name").str() != alias {
		t.Fatal("choice")
	}
	input := `{ "` + prop + `" : "printf 'Hermes Velorin' > /tmp/OpenClaw", "n":1.00e+03 }`
	response := `{"content":[{"type":"tool_use","id":"call1","name":"` + alias + `","input":` + input + `}],"usage":{"input_tokens":11}}`
	out, err := m.RestoreJSON([]byte(response))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"content":[{"type":"tool_use","id":"call1","name":"Bash","input":{ "command" : "printf 'Hermes Velorin' > /tmp/OpenClaw", "n":1.00e+03 }}],"usage":{"input_tokens":11}}`
	if string(out) != want {
		t.Fatalf("not token-exact:\n%s", out)
	}
}
func TestExactSpellingVocabulary(t *testing.T) {
	s := testSession(t)
	wire, m, err := Prepare([]byte(requestFixture), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	encoded := n.get("system").str()
	if strings.Contains(encoded, "Hermes") || strings.Contains(encoded, "OpenClaw") {
		t.Fatal("forward absent")
	}
	raw, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": encoded}}})
	out, err := m.RestoreJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := parse(out)
	original, _ := parse([]byte(requestFixture))
	if v.get("content").items[0].get("text").str() != original.get("system").str() {
		t.Fatal("exact spelling")
	}
	exempt := n.get("messages").items[0].get("content").str()
	for _, v := range []string{"`Hermes`", "https://x/Hermes", "owner/Hermes", "/tmp/Hermes", "git@x:owner/Hermes"} {
		if !strings.Contains(exempt, v) {
			t.Fatal("identifier mutated")
		}
	}
	bad := DefaultManifest()
	bad.Words = append(bad.Words, "herm")
	if _, err := Create(privateDir(t), Binding{"p", "s", "v1"}, bad); err == nil {
		t.Fatal("overlap accepted")
	}
}
func TestLiteralAliasesAndNamespaceEscapes(t *testing.T) {
	s := testSession(t)
	wire, _, err := Prepare([]byte(`{"system":"Hermes"}`), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	symbol := n.get("system").str()
	original := "literal " + symbol + " Hermes Velorin QuorVane"
	raw, _ := json.Marshal(map[string]string{"system": original})
	wire, m, err := Prepare(raw, s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ = parse(wire)
	response, _ := json.Marshal(map[string]any{"content": []any{map[string]string{"type": "text", "text": n.get("system").str()}}})
	out, err := m.RestoreJSON(response)
	if err != nil {
		t.Fatal(err)
	}
	n, _ = parse(out)
	if n.get("content").items[0].get("text").str() != original {
		t.Fatal("literal collision")
	}
}
func TestOpaqueBlocksByteIdentity(t *testing.T) {
	s := testSession(t)
	for _, block := range []string{
		`{ "type":"thinking", "thinking":"Hermes dpx_v1_symbol", "signature":"opaque" }`,
		`{ "type":"redacted_thinking", "data":"Hermes dpx_v1_symbol", "signature":"opaque" }`,
		`{ "type":"future_unknown", "name":"Hermes", "input":{"command":"dpx_v1_symbol"} }`,
	} {
		raw := []byte(`{"system":"Hermes","messages":[{"role":"assistant","content":[` + block + `]}]}`)
		wire, m, err := Prepare(raw, s)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(wire, []byte(block)) {
			t.Fatal("opaque forward")
		}
		out, err := m.RestoreJSON([]byte(`{"content":[` + block + `]}`))
		if err != nil || !bytes.Contains(out, []byte(block)) {
			t.Fatal("opaque inverse")
		}
	}
}
func TestSessionRestartCachePrefix(t *testing.T) {
	s := testSession(t)
	wire, _, err := Prepare([]byte(requestFixture), s)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(s.dir, s.binding, s.manifest)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := Prepare([]byte(requestFixture), resumed)
	if err != nil || !bytes.Equal(wire, again) {
		t.Fatal("unstable restart")
	}
	if err := os.Remove(filepath.Join(s.dir, "map.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(s.dir, s.binding, s.manifest); err == nil {
		t.Fatal("missing map accepted")
	}
	if _, _, err := Prepare([]byte(requestFixture), s); err == nil {
		t.Fatal("deleted map rebuilt")
	}
}
func TestAuthenticatedSessionIsolation(t *testing.T) {
	s := testSession(t)
	raw := []byte(`{"system":"Hermes"}`)
	wire, m, err := Prepare(raw, s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	symbol := n.get("system").str()
	other, err := Create(privateDir(t), Binding{"other", "s", "v1"}, DefaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	_, om, err := Prepare(raw, other)
	if err != nil {
		t.Fatal(err)
	}
	response, _ := json.Marshal(map[string]any{"content": []any{map[string]string{"type": "text", "text": symbol}}})
	out, err := m.RestoreJSON(response)
	if err != nil || !bytes.Contains(out, []byte("Hermes")) {
		t.Fatal("own decode")
	}
	if _, err := om.RestoreJSON(response); err == nil {
		t.Fatal("foreign codec symbol accepted")
	}
	if _, err := Create(privateDir(t), Binding{}, DefaultManifest()); err == nil {
		t.Fatal("missing binding")
	}
}
