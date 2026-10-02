package contentalias

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// t_cb095320: the shapes a genuine full-harness Claude Code request carried
// past the aliaser (18 captured bodies, t_14119440: 6-25 manifest words left per
// request). Three exemptions let them through: resource-shaped tokens (paths,
// "hermes-home:" commit subjects in the system git-status block), backtick
// spans, and tool_result text. Compound identifiers (hermes-agent,
// HERMES_KANBAN_TASK) also escaped the whole-word match. Sanitized copies of
// the captured lines; the leak gate is brand count 0 over system+messages+tools,
// counted the way the DPX wirelog's brandTokens counts it.
const residueGitStatus = "gitStatus: This is the git status at the start of the conversation.\nCurrent branch: main\n\nStatus:\n(clean)\n\nRecent commits:\n50cea9db9 hermes-home: isolated remote sync [home-autocommit]\nb59bf735d hermes-home: autocommit (skills-shared:4) [home-autocommit]"

const residueCardText = "You are working Hermes Kanban card t_00000000 in the git worktree /tmp/t_00000000/fixture (your cwd).\n" +
	"   hermes kanban show t_00000000 --json\n" +
	"   Never push. Never touch ~/.hermes. Never create extra git worktrees or branches.\n" +
	"3. Run the tests the card names: cd skills-shared/coding/kanban-foreign-lane && /Users/u/.hermes/scripts/test-gate --local /Users/u/.hermes/hermes-agent/.venv/bin/python -m pytest -q tests/test_x.py\n" +
	"Test command: `cd skills-shared/coding/kanban-foreign-lane && /Users/u/.hermes/scripts/test-gate --local /Users/u/.hermes/hermes-agent/.venv/bin/python -m pytest -q tests/test_x.py`\n" +
	"env: HERMES_KANBAN_TASK=t_00000000 OpenClaw open_claw https://github.com/o/hermes-agent git@github.com:o/Hermes.git"

const residueCommand = "/Users/u/.hermes/hermes-agent/.venv/bin/python -m pytest -q tests/test_x.py && cat ~/.hermes/config.yaml"

func residueRequest(t *testing.T) []byte {
	t.Helper()
	numbered := ""
	for i, line := range strings.Split(residueCardText, "\n") {
		numbered += strings.Repeat(" ", 5) + string(rune('1'+i)) + "\t" + line + "\n"
	}
	body := map[string]any{
		"model": "sandbox",
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.283; cc_entrypoint=cli;"},
			map[string]any{"type": "text", "text": "You are Claude Code.\n" + residueGitStatus},
		},
		"tools": []any{map[string]any{"name": "Bash", "description": "Runs `~/.hermes/scripts/x` and hermes-agent tools.", "input_schema": map[string]any{
			"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string", "description": "e.g. `hermes kanban show`"}}, "required": []string{"command"}}}},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "<system-reminder>\n" + residueGitStatus + "\n</system-reminder>"},
				map[string]any{"type": "text", "text": residueCardText},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": residueCommand}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": numbered},
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{
					map[string]any{"type": "text", "text": residueGitStatus},
					map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "aGVybWVz"}},
				}},
			}},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// brandCount mirrors helps.DPXBodyDigest's brandTokens over the wire body.
func brandCount(t *testing.T, wire []byte) int {
	t.Helper()
	var v map[string]json.RawMessage
	if err := json.Unmarshal(wire, &v); err != nil {
		t.Fatal(err)
	}
	scope := strings.ToLower(string(v["system"]) + string(v["messages"]) + string(v["tools"]))
	n := 0
	for _, w := range DefaultManifest().Words {
		n += strings.Count(scope, w)
	}
	return n
}

func TestFullHarnessBrandResidueZero(t *testing.T) {
	raw := residueRequest(t)
	if before := brandCount(t, raw); before < 20 {
		t.Fatalf("fixture lost its residue (%d manifest words)", before)
	}
	wire, _, err := Prepare(raw, testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	if n := brandCount(t, wire); n != 0 {
		t.Fatalf("%d manifest words left on the wire after alias:\n%s", n, wire)
	}
	// Non-text tool_result items stay opaque (the image bytes are untouched).
	if !bytes.Contains(wire, []byte(`"data":"aGVybWVz"`)) {
		t.Fatal("opaque tool_result item changed")
	}
	// The billing block is untouched.
	if !bytes.Contains(wire, []byte("cc_version=2.1.283; cc_entrypoint=cli;")) {
		t.Fatal("billing block changed")
	}
}

// What the model read aliased must execute as the caller's original bytes: a
// tool_use whose value copies aliased paths/identifiers off the wire restores
// exactly, on the JSON and the SSE path; text replies restore too.
func TestFullHarnessAliasedValuesRoundTrip(t *testing.T) {
	s := testSession(t)
	wire, m, err := Prepare(residueRequest(t), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	history := n.get("messages").items[1].get("content").items[0].get("input")
	alias := history.fields[0].key.text
	aliasedCommand := history.fields[0].value.str()
	if aliasedCommand == residueCommand || strings.Contains(strings.ToLower(aliasedCommand), "hermes") {
		t.Fatalf("history tool_use value not aliased: %s", aliasedCommand)
	}
	aliasedCard := n.get("messages").items[0].get("content").items[1].get("text").str()
	toolAlias := n.get("tools").items[0].get("name").str()

	input := map[string]string{alias: aliasedCommand}
	response, _ := json.Marshal(map[string]any{"content": []any{
		map[string]any{"type": "text", "text": aliasedCard},
		map[string]any{"type": "tool_use", "id": "toolu_2", "name": toolAlias, "input": input},
	}})
	out, err := m.RestoreJSON(response)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := parse(out)
	if got.get("content").items[0].get("text").str() != residueCardText {
		t.Fatal("text reply not restored byte-exact")
	}
	if cmd := got.get("content").items[1].get("input").get("command").str(); cmd != residueCommand {
		t.Fatalf("tool_use value not restored: %q", cmd)
	}

	partial, _ := json.Marshal(input)
	stream := event(map[string]string{"type": "message_start"})
	stream = append(stream, event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_2", "name": toolAlias, "input": map[string]any{}}})...)
	mid := len(partial) / 2
	stream = append(stream, event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": string(partial[:mid])}})...)
	stream = append(stream, event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": string(partial[mid:])}})...)
	stream = append(stream, event(map[string]any{"type": "content_block_stop", "index": 0})...)
	stream = append(stream, event(map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}})...)
	stream = append(stream, event(map[string]string{"type": "message_stop"})...)
	sse, err := m.RestoreSSE(stream)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(map[string]string{"partial_json": `{"command":` + string(mustJSON(residueCommand)) + `}`})
	if !bytes.Contains(sse, want[1:len(want)-1]) {
		t.Fatalf("SSE tool_use value not restored:\n%s", sse)
	}

	// The next turn re-sends the restored history: it must alias to the same
	// bytes the model produced (cache-prefix stable).
	again, _, err := Prepare(residueRequest(t), s)
	if err != nil || !bytes.Equal(again, wire) {
		t.Fatal("re-prepared history differs (cache prefix unstable)")
	}
}

func mustJSON(s string) []byte { b, _ := json.Marshal(s); return b }
