package contentalias

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// t_c59dfc3f: on claude-dpx-19 (CPA 8bebd0cc) the model wrote a bare class token
// dpx_v1_b (no trailing "_", value "x") next to the real code key of
// execute_code. The #32 net matched only ^dpx_v1_[A-Za-z]_, so the token was
// read as a caller dynamic key and left DPX raw. The golden is that wire row:
// its tool schema and its exact SSE response.

const bareClassGolden = "testdata/stray-bare-class-dpx19-18919-152.json"

type bareClassFixture struct {
	Request        json.RawMessage   `json:"request"`
	WireToolAlias  string            `json:"wire_tool_alias"`
	WirePropertyAl map[string]string `json:"wire_property_aliases"`
	ResponseSSE    string            `json:"response_sse"`
}

func TestStrayBareClassGoldenWireRow(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(bareClassGolden))
	if err != nil {
		t.Fatal(err)
	}
	var g bareClassFixture
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.ResponseSSE, `{\"dpx_v1_b\": \"x`) {
		t.Fatal("golden no longer carries the measured dpx_v1_b token")
	}
	_, m, err := Prepare(g.Request, testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	const tool = "mcp__bpx__execute_code"
	sc := m.schemas[tool]
	if sc == nil {
		t.Fatalf("no schema for %s", tool)
	}
	// Re-bind the row's session aliases to this test session's aliases. The
	// stray dpx_v1_b is not an alias of anything and is left as recorded.
	sse := strings.ReplaceAll(g.ResponseSSE, g.WireToolAlias, m.st.Tools[tool].Alias)
	for plain, wire := range g.WirePropertyAl {
		sse = strings.ReplaceAll(sse, wire, sc.forward[plain])
	}
	out, err := m.NewStream().Feed([]byte(sse))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var partial string
	for _, line := range strings.Split(string(out), "\n") {
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
			partial += ev.Delta.PartialJSON
		}
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(partial), &input); err != nil {
		t.Fatalf("restored input %q: %v", partial, err)
	}
	if _, ok := input["code"].(string); !ok {
		t.Fatalf("real code key not restored: %v", keysOf(input))
	}
	requireMarker(t, input, []string{"code", "reset"})
	if input[firstMarker(input)] != "x" {
		t.Fatalf("stray value not preserved: %v", keysOf(input))
	}
}

func TestStrayBareClassDynamicKeysPassThrough(t *testing.T) {
	// Class-less keys a caller may own keep passing through raw on a free map
	// and next to declared parameters with additionalProperties.
	_, m, err := Prepare([]byte(`{"tools":[{"name":"T","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"additionalProperties":true}}]}`), testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	cmd := m.schemas["T"].forward["command"]
	for _, key := range []string{"dpx_v1_customer", "dpx_v1_bb", "dpx_v1_", "dpx_v1_1"} {
		input := restoreBoth(t, m, m.st.Tools["T"].Alias, `{"`+cmd+`":"c","`+key+`":"v"}`)
		if input["command"] != "c" || input[key] != "v" || len(input) != 2 {
			t.Fatalf("%s: dynamic key changed: %v", key, input)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
