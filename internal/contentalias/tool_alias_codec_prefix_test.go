package contentalias

import (
	"bytes"
	"testing"
)

// t_388a6308 (2026-10-30 12:50 PT, claude-dpx-11, wire row 68407-799, bridge
// id ca231396): the model emitted dpx_v1_stork_couch_placeholder for the
// declared v3 alias dpx_v1_t_stork_couch (mcp__bpx__terminal). The class letter
// was dropped and a word tail added, so strayClassPattern read class 's'
// followed by 't', the name was not alias-shaped, and the turn was refused
// with unknown_tool after #42 and #51 had shipped. Every undeclared name in the
// dpx_v1_ namespace is a mangled alias (a caller can never declare one:
// reserved_tool), so all of them must leave DPX as the marker.
func TestToolUndeclaredCodecPrefixBecomesMarker(t *testing.T) {
	_, m, _, key := preparedTool(t)
	for _, bad := range []string{
		"dpx_v1_stork_couch_placeholder", // the live 10-30 slip
		"dpx_v1_t_stork_couch_placeholder",
		"dpx_v1_stork_couch",
		"dpx_v1_customer",
		"dpx_v1_dpx_v1_customer",
		"dpx_v1_",
	} {
		out, err := streamAllSplits(t, m, toolEvents(bad, key))
		if err != nil {
			t.Fatalf("stream %s: turn refused: %v", bad, err)
		}
		if !bytes.Contains(out, []byte(`"name":"unknown_tool_1"`)) || bytes.Contains(out, []byte("dpx_v1_")) {
			t.Fatalf("stream %s: want marker, no alias token: %s", bad, out)
		}
		if bytes.Contains(out, []byte(`"name":"Bash"`)) {
			t.Fatalf("stream %s: repaired to a declared tool: %s", bad, out)
		}
		jout, err := m.RestoreJSON(jsonToolUse(bad, key))
		if err != nil {
			t.Fatalf("json %s: turn refused: %v", bad, err)
		}
		if !bytes.Contains(jout, []byte(`"name":"unknown_tool_1"`)) || bytes.Contains(jout, []byte("dpx_v1_")) {
			t.Fatalf("json %s: want marker, no alias token: %s", bad, jout)
		}
	}
}
