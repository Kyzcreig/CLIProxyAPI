package contentalias

import (
	"encoding/json"
	"strings"
	"testing"
)

// t_186d86c5: the model sometimes writes a SIBLING tool's property alias (or a
// 1-2 hex near-copy of one) as a tool_use key. The inverse map only knows this
// tool's aliases, so the raw dpx_v1_ token used to leave DPX and the caller
// refused "unknown parameter(s): dpx_v1_p_...". The model then saw that token
// literal-escaped (dpx_v1_l_...) and looped. A stray alias-shaped key must leave
// DPX as a readable marker that names this tool's plain parameter names.

const strayFixture = `{"tools":[` +
	`{"name":"kanban_block","input_schema":{"type":"object","properties":{"reason":{"type":"string"},"kind":{"type":"string"},"opts":{"type":"object","properties":{"note":{"type":"string"}}}},"required":["reason"]}},` +
	`{"name":"write_file","input_schema":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}}]}`

type strayAliases struct {
	block, write string
	reason, kind string
	opts, note   string
	path         string
}

func prepareStray(t *testing.T) (*RequestMap, strayAliases) {
	t.Helper()
	wire, m, err := Prepare([]byte(strayFixture), testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	n, err := parse(wire)
	if err != nil {
		t.Fatal(err)
	}
	tools := n.get("tools").items
	a := strayAliases{block: tools[0].get("name").str(), write: tools[1].get("name").str()}
	sc := m.schemas["kanban_block"]
	a.reason, a.kind, a.opts = sc.forward["reason"], sc.forward["kind"], sc.forward["opts"]
	a.note = sc.props["opts"].forward["note"]
	a.path = m.schemas["write_file"].forward["path"]
	for _, v := range []string{a.reason, a.kind, a.opts, a.note, a.path} {
		if !strings.HasPrefix(v, "dpx_v1_p_") {
			t.Fatalf("fixture alias shape: %q", v)
		}
	}
	return m, a
}

// restoreBoth restores one tool_use input through RestoreJSON and the SSE Stream
// and requires both paths to agree byte-for-byte.
func restoreBoth(t *testing.T, m *RequestMap, toolAlias, input string) map[string]any {
	t.Helper()
	response := `{"content":[{"type":"tool_use","id":"toolu_1","name":"` + toolAlias + `","input":` + input + `}]}`
	out, err := m.RestoreJSON([]byte(response))
	if err != nil {
		t.Fatalf("RestoreJSON: %v", err)
	}
	var doc struct {
		Content []struct {
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	stream := event(map[string]any{"type": "message_start", "message": map[string]any{"content": []any{}}})
	stream = append(stream, event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": toolAlias, "input": map[string]any{}}})...)
	stream = append(stream, event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": input}})...)
	stream = append(stream, event(map[string]any{"type": "content_block_stop", "index": 0})...)
	stream = append(stream, event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}})...)
	stream = append(stream, event(map[string]string{"type": "message_stop"})...)
	sout, err := m.NewStream().Feed(stream)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var streamed string
	for _, line := range strings.Split(string(sout), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(line[6:]), &ev) == nil && ev.Type == "content_block_delta" {
			streamed += ev.Delta.PartialJSON
		}
	}
	var fromJSON, fromStream map[string]any
	if err := json.Unmarshal(doc.Content[0].Input, &fromJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(streamed), &fromStream); err != nil {
		t.Fatalf("stream input %q: %v", streamed, err)
	}
	a, _ := json.Marshal(fromJSON)
	b, _ := json.Marshal(fromStream)
	if string(a) != string(b) {
		t.Fatalf("paths disagree:\njson   %s\nstream %s", a, b)
	}
	return fromJSON
}

func requireMarker(t *testing.T, input map[string]any, want []string) {
	t.Helper()
	for key := range input {
		if strings.Contains(key, "dpx_v1_") {
			t.Fatalf("raw alias key left DPX: %q", key)
		}
	}
	var marker string
	for key := range input {
		if strings.HasPrefix(key, "unknown_key") {
			marker = key
		}
	}
	if marker == "" {
		t.Fatalf("no unknown_key marker in %v", input)
	}
	for _, name := range want {
		if !strings.Contains(marker, name) {
			t.Fatalf("marker %q does not name parameter %q", marker, name)
		}
	}
}

func TestStrayAliasKeySiblingToolRefusedLoudly(t *testing.T) {
	m, a := prepareStray(t)
	// The measured sub-7 case: write_file.path's alias used as kanban_block's key.
	input := restoreBoth(t, m, a.block, `{"`+a.path+`":"blocked: need input"}`)
	requireMarker(t, input, []string{"reason", "kind", "opts"})
	if input[firstMarker(input)] != "blocked: need input" {
		t.Fatalf("value not preserved: %v", input)
	}
}

func TestStrayAliasKeyNearCopyRefusedLoudly(t *testing.T) {
	m, a := prepareStray(t)
	last := a.reason[len(a.reason)-1]
	flip := byte('0')
	if last == '0' {
		flip = '1'
	}
	near := a.reason[:len(a.reason)-1] + string(flip)
	input := restoreBoth(t, m, a.block, `{"`+near+`":"x","`+a.kind+`":"needs_input"}`)
	requireMarker(t, input, []string{"reason"})
	if input["kind"] != "needs_input" {
		t.Fatalf("own alias not restored next to a stray key: %v", input)
	}
}

func TestStrayAliasKeyTwoStraysStayDistinct(t *testing.T) {
	m, a := prepareStray(t)
	input := restoreBoth(t, m, a.block, `{"`+a.path+`":"x","dpx_v1_t_000000000000000000000000":"y"}`)
	markers := 0
	for key := range input {
		if strings.HasPrefix(key, "unknown_key") {
			markers++
		}
	}
	if markers != 2 {
		t.Fatalf("want 2 distinct markers, got %v", input)
	}
}

func TestStrayAliasKeyNestedNamesNestedParameters(t *testing.T) {
	m, a := prepareStray(t)
	input := restoreBoth(t, m, a.block, `{"`+a.reason+`":"r","`+a.opts+`":{"`+a.path+`":"n"}}`)
	opts, _ := input["opts"].(map[string]any)
	if input["reason"] != "r" || opts == nil {
		t.Fatalf("own aliases not restored: %v", input)
	}
	requireMarker(t, opts, []string{"note"})
}

func TestStrayAliasKeyOwnAndPlainKeysUnchanged(t *testing.T) {
	m, a := prepareStray(t)
	input := restoreBoth(t, m, a.block, `{"`+a.reason+`":"r","`+a.kind+`":"k"}`)
	if len(input) != 2 || input["reason"] != "r" || input["kind"] != "k" {
		t.Fatalf("own aliases: %v", input)
	}
	// A plain original name already passes the inverse map; the marker tells the
	// model to use these names, so this must stay true.
	input = restoreBoth(t, m, a.block, `{"reason":"r"}`)
	if len(input) != 1 || input["reason"] != "r" {
		t.Fatalf("plain name: %v", input)
	}
}

func TestStrayAliasKeyMarkerReplaysForward(t *testing.T) {
	// The caller replays the refused call in history; the marker key must pass
	// the forward path without a brand word or a collision error.
	s := testSession(t)
	_, m, err := Prepare([]byte(strayFixture), s)
	if err != nil {
		t.Fatal(err)
	}
	a := m.schemas["write_file"].forward["path"]
	block := m.st.Tools["kanban_block"].Alias
	input := restoreBoth(t, m, block, `{"`+a+`":"x"}`)
	key := firstMarker(input)
	history, _ := json.Marshal(map[string]any{"type": "tool_use", "id": "toolu_1", "name": "kanban_block", "input": map[string]any{key: "x"}})
	req := strings.TrimSuffix(strayFixture, "}") + `,"messages":[{"role":"assistant","content":[` + string(history) + `]}]}`
	wire, _, err := Prepare([]byte(req), s)
	if err != nil {
		t.Fatalf("history replay: %v", err)
	}
	if !strings.Contains(string(wire), key[:len("unknown_key")]) {
		t.Fatal("marker key vanished from replayed history")
	}
	for _, w := range DefaultManifest().Words {
		if strings.Contains(strings.ToLower(key), w) {
			t.Fatalf("marker carries brand word %q", w)
		}
	}
}

func TestStrayAliasKeyFreeMapAndCodecKeysUntouched(t *testing.T) {
	// Free-keyed maps declare no names to point at, and word/literal symbols
	// are codec text, not parameter aliases: both keep the old pass-through.
	m, a := prepareStray(t)
	input := restoreBoth(t, m, a.block, `{"`+a.reason+`":"r","dpx_v1_l_000000000000000000000000":"v"}`)
	if input["reason"] != "r" || input["dpx_v1_l_000000000000000000000000"] != "v" {
		t.Fatalf("codec-kind key changed: %v", input)
	}
}

func firstMarker(input map[string]any) string {
	for key := range input {
		if strings.HasPrefix(key, "unknown_key") {
			return key
		}
	}
	return ""
}
