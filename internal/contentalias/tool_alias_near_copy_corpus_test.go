package contentalias

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// t_b54ef60f: regression tests over the 10 REAL tool-alias near-copies the
// model emitted on live DPX units 2026-10-25..27 (t_7bd8c9b9). Each pair is
// (emitted, declared): the declared alias was a real tool on that unit, and
// the emitted name was refused as unknown_tool, which killed the turn.

type nearCopyPair struct {
	Unit         string `json:"unit"`
	TS           string `json:"ts"`
	Emitted      string `json:"emitted"`
	Declared     string `json:"declared"`
	Dist         int    `json:"dist"`
	DeclaredTool string `json:"declared_tool,omitempty"`
}

func loadNearCopyCorpus(t *testing.T) []nearCopyPair {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "tool_alias_near_copy_corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Pairs []nearCopyPair `json:"pairs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Pairs) != 10 {
		t.Fatalf("corpus has %d pairs, want the 10 captured", len(doc.Pairs))
	}
	for i, p := range doc.Pairs {
		if p.Emitted == p.Declared {
			t.Fatalf("pair %d: emitted == declared", i)
		}
		if !hexToolShape.MatchString(p.Declared) {
			t.Fatalf("pair %d: declared %q is not a v1/v2 hex tool alias", i, p.Declared)
		}
		if !strayClassPattern.MatchString(p.Emitted) {
			t.Fatalf("pair %d: emitted %q is not alias-shaped", i, p.Emitted)
		}
		if d := levenshtein(p.Emitted, p.Declared); d != p.Dist {
			t.Fatalf("pair %d: levenshtein(%q, %q) = %d, corpus says %d", i, p.Emitted, p.Declared, d, p.Dist)
		}
	}
	return doc.Pairs
}

// liveDeclaredMap is a v1/v2 request map in which the corpus's declared alias
// is the real alias of the declared tool "Bash" (and only that alias). The
// hex alias is a hash of the unit's binding, which the test cannot rebuild,
// so the reverse entry is re-pointed by hand; everything downstream (stream,
// RestoreJSON, schemas, allowed) is the production code path.
func liveDeclaredMap(t *testing.T, version, declared string) (*RequestMap, string) {
	t.Helper()
	s, err := Create(privateDir(t), Binding{Principal: "sandbox", Session: "00000000-0000-4000-8000-000000000001", Version: version}, DefaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	wire, m, err := Prepare([]byte(requestFixture), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	tool := n.get("tools").items[0]
	key := tool.get("input_schema").get("properties").fields[0].key.text
	bashAlias := tool.get("name").str()
	if m.reverse[bashAlias] != "Bash" || !hexToolShape.MatchString(bashAlias) {
		t.Fatalf("%s: fixture tool 0 alias %q is not a hex alias of Bash", version, bashAlias)
	}
	delete(m.reverse, bashAlias)
	m.reverse[declared] = "Bash"
	return m, key
}

// Every captured near-copy, fed through the stream (all split points) and
// RestoreJSON, leaves DPX as unknown_tool_1 with input {}: no error, no
// repair to the declared tool. The declared alias itself still restores, so
// the map the slip is tested against is live.
func TestLiveNearCopyCorpus(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		for i, p := range loadNearCopyCorpus(t) {
			m, key := liveDeclaredMap(t, version, p.Declared)

			// Positive control: the declared alias is a real, restorable tool.
			ok, err := m.RestoreJSON(jsonToolUse(p.Declared, key))
			if err != nil || !bytes.Contains(ok, []byte(`"name":"Bash"`)) {
				t.Fatalf("%s pair %d: declared alias did not restore: %v %s", version, i, err, ok)
			}

			out, err := streamAllSplits(t, m, toolEvents(p.Emitted, key))
			if err != nil {
				t.Fatalf("%s pair %d (unit %s %s) %s: stream refused: %v", version, i, p.Unit, p.TS, p.Emitted, err)
			}
			if !bytes.Contains(out, []byte(`"name":"unknown_tool_1"`)) {
				t.Fatalf("%s pair %d: stream marker missing: %s", version, i, out)
			}
			if !bytes.Contains(out, []byte(`"partial_json":"{}"`)) {
				t.Fatalf("%s pair %d: stream marker input not {}: %s", version, i, out)
			}
			if bytes.Contains(out, []byte(`"name":"Bash"`)) || bytes.Contains(out, []byte("dpx_v1_")) {
				t.Fatalf("%s pair %d: stream repaired or leaked the alias: %s", version, i, out)
			}

			jout, err := m.RestoreJSON(jsonToolUse(p.Emitted, key))
			if err != nil {
				t.Fatalf("%s pair %d %s: RestoreJSON refused: %v", version, i, p.Emitted, err)
			}
			jn, err := parse(jout)
			if err != nil {
				t.Fatal(err)
			}
			block := jn.get("content").items[1]
			if got := block.get("name").str(); got != "unknown_tool_1" {
				t.Fatalf("%s pair %d: json name = %q, want unknown_tool_1", version, i, got)
			}
			if in := block.get("input"); in == nil || in.kind != '{' || len(in.fields) != 0 {
				t.Fatalf("%s pair %d: json marker input not {}: %s", version, i, jout)
			}
			if bytes.Contains(jout, []byte(`"name":"Bash"`)) || bytes.Contains(jout, []byte("dpx_v1_")) {
				t.Fatalf("%s pair %d: json repaired or leaked the alias: %s", version, i, jout)
			}
		}
	}
}

// nearCopyOp is one Levenshtein edit on the alias tail (after "dpx_v1_t_"):
// 's' substitutes tail[at] with c, 'd' deletes tail[at], 'i' inserts c
// before tail[at].
type nearCopyOp struct {
	kind byte
	at   int
	c    byte
}

// nearCopyOps is a minimal edit script turning a into b.
func nearCopyOps(a, b string) []nearCopyOp {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := 0; j <= len(b); j++ {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
		}
	}
	var ops []nearCopyOp
	i, j := len(a), len(b)
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && a[i-1] == b[j-1] && d[i][j] == d[i-1][j-1]:
			i, j = i-1, j-1
		case i > 0 && j > 0 && d[i][j] == d[i-1][j-1]+1:
			ops = append(ops, nearCopyOp{'s', i - 1, b[j-1]})
			i, j = i-1, j-1
		case i > 0 && d[i][j] == d[i-1][j]+1:
			ops = append(ops, nearCopyOp{'d', i - 1, 0})
			i--
		default:
			ops = append(ops, nearCopyOp{'i', i, b[j-1]})
			j--
		}
	}
	return ops
}

// applyNearCopyOps applies ops (indexed on a source tail of length srcLen) to
// tail. anchorEnd re-indexes each op by its distance from the end of the
// source, so a tail slip lands on the tail of a shorter alias. ok is false
// when an op falls outside tail.
func applyNearCopyOps(tail string, ops []nearCopyOp, srcLen int, anchorEnd bool) (string, bool) {
	moved := make([]nearCopyOp, len(ops))
	for k, o := range ops {
		if anchorEnd {
			o.at = len(tail) - (srcLen - o.at)
		}
		limit := len(tail) - 1
		if o.kind == 'i' {
			limit = len(tail)
		}
		if o.at < 0 || o.at > limit {
			return "", false
		}
		moved[k] = o
	}
	// Highest index first keeps lower indices valid; at one index, the
	// substitution/deletion of tail[at] goes before an insertion before it.
	sort.SliceStable(moved, func(x, y int) bool {
		if moved[x].at != moved[y].at {
			return moved[x].at > moved[y].at
		}
		return moved[x].kind != 'i' && moved[y].kind == 'i'
	})
	b := []byte(tail)
	for _, o := range moved {
		switch o.kind {
		case 's':
			b[o.at] = o.c
		case 'd':
			b = append(b[:o.at:o.at], b[o.at+1:]...)
		case 'i':
			b = append(b[:o.at:o.at], append([]byte{o.c}, b[o.at:]...)...)
		}
	}
	return string(b), true
}

// The same edit operations the model made on live hex aliases, applied to
// every v3 alias of the 60-tool synthetic set (anchored at the tail start and
// at the tail end, with the literal new characters and with each letter a-z
// in their place), never produce another declared v3 alias: the result is
// either the alias itself (restores) or unknown (the unknown_tool_1 marker).
func TestLiveNearCopyEditOpsCannotHitV3Sibling(t *testing.T) {
	_, m, err := Prepare([]byte(syntheticBody(t, 450<<10)), v3Session(t, privateDir(t)))
	if err != nil {
		t.Fatal(err)
	}
	aliases := toolAliases(m)
	if len(aliases) != fixtureTools {
		t.Fatalf("synthetic v3 set has %d tool aliases, want %d", len(aliases), fixtureTools)
	}
	live := map[string]bool{}
	for _, a := range aliases {
		if !v3ToolShape.MatchString(a) {
			t.Fatalf("not a v3 tool alias: %q", a)
		}
		live[a] = true
	}
	const prefix = "dpx_v1_t_"
	letters := "abcdefghijklmnopqrstuvwxyz"
	applied, self, marker := 0, 0, 0
	for i, p := range loadNearCopyCorpus(t) {
		src, dst := p.Declared[len(prefix):], p.Emitted[len(prefix):]
		ops := nearCopyOps(src, dst)
		if len(ops) != p.Dist {
			t.Fatalf("pair %d: %d ops, corpus dist %d", i, len(ops), p.Dist)
		}
		if got, ok := applyNearCopyOps(src, ops, len(src), false); !ok || got != dst {
			t.Fatalf("pair %d: ops do not reproduce the slip: %q -> %q, want %q", i, src, got, dst)
		}
		variants := [][]nearCopyOp{ops}
		for _, c := range []byte(letters) {
			v := make([]nearCopyOp, len(ops))
			for k, o := range ops {
				if o.kind != 'd' {
					o.c = c
				}
				v[k] = o
			}
			variants = append(variants, v)
		}
		for _, alias := range aliases {
			tail := alias[len(prefix):]
			for _, anchorEnd := range []bool{false, true} {
				for _, v := range variants {
					got, ok := applyNearCopyOps(tail, v, len(src), anchorEnd)
					if !ok {
						continue
					}
					applied++
					bad := prefix + got
					if bad != alias && live[bad] {
						t.Fatalf("pair %d (%s -> %s) applied to %q gives declared sibling %q", i, p.Declared, p.Emitted, alias, bad)
					}
					out, err := m.RestoreJSON([]byte(`{"content":[{"type":"tool_use","id":"x","name":"` + bad + `","input":{}}]}`))
					if err != nil {
						t.Fatalf("pair %d on %q -> %q: refused: %v", i, alias, bad, err)
					}
					if bad == alias {
						self++
						if !bytes.Contains(out, []byte(`"name":"`+m.reverse[alias]+`"`)) {
							t.Fatalf("identity edit on %q did not restore: %s", alias, out)
						}
						continue
					}
					marker++
					if !bytes.Contains(out, []byte(`"name":"unknown_tool_1"`)) {
						t.Fatalf("pair %d on %q -> %q: want unknown_tool_1, got %s", i, alias, bad, out)
					}
				}
			}
		}
	}
	if marker == 0 {
		t.Fatal("no edit produced a near-copy: the test checked nothing")
	}
	t.Logf("v3 aliases=%d edits applied=%d marker=%d self=%d sibling=0", len(aliases), applied, marker, self)
}
