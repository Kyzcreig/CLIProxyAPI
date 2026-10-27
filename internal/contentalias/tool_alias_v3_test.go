package contentalias

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// t_551a4f33: map v3 tool aliases are two words from aliasWords, and any two
// distinct tool aliases differ in at least 3 characters. Property aliases keep
// the v2 word-pair scheme; v1 and v2 maps keep their 24-hex tool aliases.

var (
	v3ToolShape  = regexp.MustCompile(`^dpx_v1_t_[a-z]{5}_[a-z]{5}$`)
	hexToolShape = regexp.MustCompile(`^dpx_v1_t_[0-9a-f]{24}$`)
)

func v3Binding() Binding {
	return Binding{Principal: "sandbox", Session: "00000000-0000-4000-8000-000000000003", Version: "v3"}
}

func v3Session(t *testing.T, dir string) *Session {
	t.Helper()
	s, err := Create(dir, v3Binding(), DefaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// storedAlias is the alias st holds for (kind, original). Tests use it instead
// of symbol(), which is the v1/v2 spelling only.
func storedAlias(t *testing.T, st state, kind, original string) string {
	t.Helper()
	for alias, e := range st.Symbols {
		if e == (entry{kind, original}) {
			return alias
		}
	}
	t.Fatalf("no %s alias for %q", kind, original)
	return ""
}

func toolAliases(m *RequestMap) []string {
	var out []string
	for alias, e := range m.st.Symbols {
		if e.Kind == "t" {
			out = append(out, alias)
		}
	}
	sort.Strings(out)
	return out
}

func TestV3ToolAliasShapeAndRoundTrip(t *testing.T) {
	_, m, err := Prepare([]byte(strayFixture), v3Session(t, privateDir(t)))
	if err != nil {
		t.Fatal(err)
	}
	tools := toolAliases(m)
	if len(tools) != 2 {
		t.Fatalf("tool aliases = %v, want 2", tools)
	}
	for _, a := range tools {
		if !v3ToolShape.MatchString(a) {
			t.Fatalf("v3 tool alias shape: %q", a)
		}
		if !strayClassPattern.MatchString(a) {
			t.Fatalf("v3 tool alias %q not caught by strayClassPattern", a)
		}
	}
	for _, a := range propertyAliases(m) {
		if !v2PropertyShape.MatchString(a) {
			t.Fatalf("v3 property alias shape: %q", a)
		}
	}
	block := m.st.Tools["kanban_block"].Alias
	sc := m.schemas["kanban_block"]
	input := restoreBoth(t, m, block, `{"`+sc.forward["reason"]+`":"r"}`)
	got, _ := json.Marshal(input)
	if string(got) != `{"reason":"r"}` {
		t.Fatalf("restore: %s", got)
	}
	out, err := m.RestoreJSON([]byte(`{"content":[{"type":"tool_use","id":"x","name":"` + block + `","input":{}}]}`))
	if err != nil || !bytes.Contains(out, []byte(`"name":"kanban_block"`)) {
		t.Fatalf("v3 tool alias did not restore: %v %s", err, out)
	}
}

// Older map versions keep their 24-hex tool aliases.
func TestV1V2ToolAliasesStayHex(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		b := Binding{Principal: "sandbox", Session: "00000000-0000-4000-8000-000000000003", Version: version}
		s, err := Create(privateDir(t), b, DefaultManifest())
		if err != nil {
			t.Fatal(err)
		}
		_, m, err := Prepare([]byte(strayFixture), s)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range toolAliases(m) {
			if !hexToolShape.MatchString(a) {
				t.Fatalf("%s tool alias changed shape: %q", version, a)
			}
		}
	}
}

// A one- or two-character slip of a v3 tool alias never lands on another
// declared tool: it reaches the unknown_tool_N marker (t_7bd8c9b9) instead.
func TestV3ToolNearCopyNeverHitsSibling(t *testing.T) {
	_, m, err := Prepare([]byte(syntheticBody(t, 450<<10)), v3Session(t, privateDir(t)))
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for _, a := range toolAliases(m) {
		live[a] = true
	}
	if len(live) < 10 {
		t.Fatalf("synthetic catalog has %d tools", len(live))
	}
	letters := "abcdefghijklmnopqrstuvwxyz"
	prefix := len("dpx_v1_t_")
	for a := range live {
		for i := prefix; i < len(a); i++ {
			for _, c := range letters {
				one := a[:i] + string(c) + a[i+1:]
				if one != a && live[one] {
					t.Fatalf("1-char slip %q -> live alias %q", a, one)
				}
				for j := i + 1; j < len(a); j++ {
					for _, d := range letters {
						two := one[:j] + string(d) + one[j+1:]
						if two != a && live[two] {
							t.Fatalf("2-char slip %q -> live alias %q", a, two)
						}
					}
				}
			}
		}
	}
	// The live slip shapes on a v3 alias: one letter off, and a tail rewrite.
	block := m.st.Tools[firstTool(m)].Alias
	for _, bad := range []string{block[:len(block)-1] + string(rune('a'+(block[len(block)-1]-'a'+1)%26)), block[:len(block)-4] + "seed"} {
		out, err := m.RestoreJSON([]byte(`{"content":[{"type":"tool_use","id":"x","name":"` + bad + `","input":{"a":"b"}}]}`))
		if err != nil || !bytes.Contains(out, []byte(`"name":"unknown_tool_1"`)) {
			t.Fatalf("v3 near-copy %q: want unknown_tool_1 marker, got %v %s", bad, err, out)
		}
	}
}

func firstTool(m *RequestMap) string {
	names := make([]string, 0, len(m.st.Tools))
	for name := range m.st.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names[0]
}

// AC-M12 for v3: the same declared schema gives the same map, in a fresh store
// and after reopening the store.
func TestV3Determinism(t *testing.T) {
	maps := [][]byte{}
	for i := 0; i < 2; i++ {
		dir := privateDir(t)
		wire1, _, err := Prepare([]byte(strayFixture), v3Session(t, dir))
		if err != nil {
			t.Fatal(err)
		}
		s, err := Open(dir, v3Binding(), DefaultManifest())
		if err != nil {
			t.Fatal(err)
		}
		wire2, _, err := Prepare([]byte(strayFixture), s)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(wire1, wire2) {
			t.Fatalf("reopened store drifted:\n%s\n%s", wire1, wire2)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "map.json"))
		if err != nil {
			t.Fatal(err)
		}
		maps = append(maps, raw)
	}
	if !bytes.Equal(maps[0], maps[1]) {
		t.Fatal("same schema gave different maps in two fresh stores")
	}
}

// Probe allocation: a tool whose probe-0 alias is held by another tool takes
// the next probe, and the store accepts it on reload.
func TestV3ToolProbeOnCollisionPersists(t *testing.T) {
	dir := privateDir(t)
	s := v3Session(t, dir)
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	victim := "kanban_block"
	squat := toolSymbol(st.Binding, victim, 0)
	other := ""
	for i := 0; i < 400000 && other == ""; i++ {
		cand := "tool_" + itoa(i)
		if toolSymbol(st.Binding, cand, 0) == squat {
			other = cand
		}
	}
	if other == "" {
		t.Skip("no probe-0 collision found within bound")
	}
	if a1, err := st.allocate("t", other); err != nil || a1 != squat {
		t.Fatalf("first: %q %v", a1, err)
	}
	a2, err := st.allocate("t", victim)
	if err != nil || a2 != toolSymbol(st.Binding, victim, 1) {
		t.Fatalf("victim did not take probe 1: %q %v", a2, err)
	}
	if err := s.save(st); err != nil {
		t.Fatal(err)
	}
	re, err := s.load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := re.allocate("t", victim); got != a2 {
		t.Fatalf("victim alias moved after reload: %q -> %q", a2, got)
	}
}

func TestV3LoadRejectsForgedToolAlias(t *testing.T) {
	dir := privateDir(t)
	s := v3Session(t, dir)
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	forged := "dpx_v1_t_amber_acorn"
	if validToolAlias(st.Binding, forged, "Read") {
		t.Skip("forged alias happens to be valid")
	}
	st.Symbols[forged] = entry{"t", "Read"}
	if err := s.save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := s.load(); err == nil || err.Error() != Error("store_corrupt").Error() {
		t.Fatalf("forged v3 tool alias accepted: %v", err)
	}
	// A hex (v2-spelled) tool alias is not a v3 alias either.
	st.Symbols = map[string]entry{symbol(st.Binding, "t", "Read"): {"t", "Read"}}
	if err := s.save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := s.load(); err == nil {
		t.Fatal("hex tool alias accepted in a v3 map")
	}
}

// A v2 store does not open under a v3 binding (and vice versa).
func TestV3BindingDoesNotOpenV2Store(t *testing.T) {
	dir := privateDir(t)
	b := v3Binding()
	b.Version = "v2"
	if _, err := Create(dir, b, DefaultManifest()); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, v3Binding(), DefaultManifest()); err == nil {
		t.Fatal("v2 store opened under v3 binding")
	}
	if _, err := Create(privateDir(t), Binding{Principal: "p", Session: "s", Version: "v4"}, DefaultManifest()); err == nil {
		t.Fatal("unknown version accepted")
	}
}

// TestV3ToolPromptCost reports how many bytes v3 adds to the tool block over
// v2 (the prompt-cache re-key is a one-time miss per unit; this is the steady
// state size). A v3 alias is 20 bytes against v2's 33.
func TestV3ToolPromptCost(t *testing.T) {
	body := syntheticBody(t, 450<<10)
	size := map[string]int{}
	for _, version := range []string{"v2", "v3"} {
		b := v3Binding()
		b.Version = version
		s, err := Create(privateDir(t), b, DefaultManifest())
		if err != nil {
			t.Fatal(err)
		}
		wire, m, err := Prepare(body, s)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := parse(wire)
		tools := n.get("tools")
		size[version] = tools.end - tools.start
		t.Logf("%s: tools=%d tool_block_bytes=%d wire_bytes=%d", version, len(m.schemas), size[version], len(wire))
	}
	if size["v3"] >= size["v2"] {
		t.Fatalf("v3 tool block %d bytes, not smaller than v2 %d", size["v3"], size["v2"])
	}
}

// Confusability of TOOL aliases over the synthetic catalog, and over every
// live tool set when DPX_LIVE_MAPS names a directory of map.json copies (any
// version; each is re-prepared under a v3 binding with its own principal).
func TestV3ToolMinPairwiseDistance(t *testing.T) {
	check := func(name string, raw []byte, b Binding) {
		s, err := Create(privateDir(t), b, DefaultManifest())
		if err != nil {
			t.Fatal(err)
		}
		_, m, err := Prepare(raw, s)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		tools := toolAliases(m)
		h, l, pair := minPairwise(tools)
		maxProbe := 0
		for alias, e := range m.st.Symbols {
			if e.Kind != "t" {
				continue
			}
			if !v3ToolShape.MatchString(alias) {
				t.Fatalf("%s: tool alias shape %q", name, alias)
			}
			for k := 0; k < toolProbes; k++ {
				if toolSymbol(b, e.Original, k) == alias {
					maxProbe = max(maxProbe, k)
					break
				}
			}
		}
		props := propertyAliases(m)
		ph, pl, _ := minPairwise(props)
		t.Logf("%s: tools=%d min_hamming=%d min_levenshtein=%d closest=%v max_probe=%d | property_aliases=%d min_hamming=%d min_levenshtein=%d", name, len(tools), h, l, pair, maxProbe, len(props), ph, pl)
		if len(tools) > 1 && (h < 3 || l < 3) {
			t.Fatalf("%s: tool min distance hamming=%d levenshtein=%d (%v)", name, h, l, pair)
		}
		if len(props) > 1 && (ph < 3 || pl < 3) {
			t.Fatalf("%s: property min distance hamming=%d levenshtein=%d", name, ph, pl)
		}
	}
	check("synthetic", syntheticBody(t, 450<<10), v3Binding())
	root := os.Getenv("DPX_LIVE_MAPS")
	if root == "" {
		return
	}
	files, _ := filepath.Glob(filepath.Join(root, "*.json"))
	if len(files) == 0 {
		t.Fatalf("DPX_LIVE_MAPS=%s holds no *.json", root)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		var st state
		if json.Unmarshal(raw, &env) != nil || json.Unmarshal(env.Payload, &st) != nil {
			t.Fatalf("%s: unreadable map", f)
		}
		if len(st.Tools) == 0 {
			t.Logf("%s: no tools", filepath.Base(f))
			continue
		}
		names := make([]string, 0, len(st.Tools))
		for name := range st.Tools {
			names = append(names, name)
		}
		sort.Strings(names)
		var tools []string
		for _, name := range names {
			n, _ := json.Marshal(name)
			tools = append(tools, `{"name":`+string(n)+`,"input_schema":`+string(st.Tools[name].Schema)+`}`)
		}
		body := []byte(`{"tools":[` + strings.Join(tools, ",") + `]}`)
		b := st.Binding
		b.Version = "v3"
		check(filepath.Base(f), body, b)
	}
}
