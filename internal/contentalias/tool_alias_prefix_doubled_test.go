package contentalias

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// t_9db4bc01: on unit 24 (2026-10-29 09:09 PT) the model emitted a tool name
// with the codec prefix written twice. strayClassPattern did not call it
// alias-shaped, so it was refused as unknown_tool and the turn died. Every
// codec-prefixed undeclared name is now alias-shaped (marker), and a name whose
// remainder after stripping whole leading prefix copies is a declared alias
// restores to that tool.

const doubledPfx = "dpx" + "_v1_"

type prefixDoubledPair struct {
	Unit     string  `json:"unit"`
	TS       string  `json:"ts"`
	Emitted  string  `json:"emitted"`
	Declared *string `json:"declared"`
}

func loadPrefixDoubledCorpus(t *testing.T) []prefixDoubledPair {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "tool_alias_near_copy_corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Pairs []prefixDoubledPair `json:"prefix_doubled"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Pairs) != 2 {
		t.Fatalf("prefix_doubled has %d pairs, want 2", len(doc.Pairs))
	}
	for i, p := range doc.Pairs {
		if !strings.HasPrefix(p.Emitted, doubledPfx+doubledPfx) {
			t.Fatalf("pair %d: %q does not repeat the codec prefix", i, p.Emitted)
		}
		if strayClassPattern.MatchString(p.Emitted) {
			t.Fatalf("pair %d: %q matches strayClassPattern; the corpus no longer covers the gap", i, p.Emitted)
		}
	}
	return doc.Pairs
}

func TestPrefixDoubledCorpus(t *testing.T) {
	const declared = doubledPfx + "t_acorn_brick"
	for _, version := range []string{"v1", "v2"} {
		for i, p := range loadPrefixDoubledCorpus(t) {
			target := declared
			if p.Declared != nil {
				target = *p.Declared
			}
			m, key := liveDeclaredMap(t, version, target)

			out, err := streamAllSplits(t, m, toolEvents(p.Emitted, key))
			if err != nil {
				t.Fatalf("%s pair %d %s: stream refused: %v", version, i, p.Emitted, err)
			}
			jout, err := m.RestoreJSON(jsonToolUse(p.Emitted, key))
			if err != nil {
				t.Fatalf("%s pair %d %s: RestoreJSON refused: %v", version, i, p.Emitted, err)
			}
			for path, got := range map[string][]byte{"stream": out, "json": jout} {
				if bytes.Contains(got, []byte(doubledPfx)) {
					t.Fatalf("%s pair %d %s: alias leaked: %s", version, i, path, got)
				}
				if p.Declared != nil {
					// Exact prefix strip lands on the declared tool, args restored.
					if !bytes.Contains(got, []byte(`"name":"Bash"`)) || !bytes.Contains(got, []byte("command")) || bytes.Contains(got, []byte("unknown_tool")) {
						t.Fatalf("%s pair %d %s: want restored Bash call: %s", version, i, path, got)
					}
				} else if !bytes.Contains(got, []byte(`"name":"unknown_tool_1"`)) || bytes.Contains(got, []byte(`"name":"Bash"`)) {
					t.Fatalf("%s pair %d %s: want unknown_tool_1 marker: %s", version, i, path, got)
				}
			}
		}
	}
}

// Undeclared codec-prefixed names that strayClassPattern never matched become
// the marker, never a repair: a doubled prefix whose remainder is not
// declared, a near-copy behind a doubled prefix, and a non-class word.
func TestCodecPrefixedUndeclaredBecomesMarker(t *testing.T) {
	const declared = doubledPfx + "t_acorn_brick"
	m, key := liveDeclaredMap(t, "v2", declared)
	for _, name := range []string{
		doubledPfx + doubledPfx + "t_acorn_brack",
		doubledPfx + "customer",
		doubledPfx,
		doubledPfx + doubledPfx,
		doubledPfx + "_" + declared,
	} {
		out, err := streamAllSplits(t, m, toolEvents(name, key))
		if err != nil || !bytes.Contains(out, []byte(`"name":"unknown_tool_1"`)) || !bytes.Contains(out, []byte(`"partial_json":"{}"`)) {
			t.Fatalf("stream %q: want marker, got %v %s", name, err, out)
		}
		jout, err := m.RestoreJSON(jsonToolUse(name, key))
		if err != nil || !bytes.Contains(jout, []byte(`"name":"unknown_tool_1"`)) || bytes.Contains(jout, []byte(`"name":"Bash"`)) {
			t.Fatalf("json %q: want marker, got %v %s", name, err, jout)
		}
	}
}
