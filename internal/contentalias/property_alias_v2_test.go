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

// t_13128fb0: map v2 property aliases are two words from aliasWords, and any
// two distinct property aliases differ in at least 3 characters.

var v2PropertyShape = regexp.MustCompile(`^dpx_v1_p_[a-z]{5}_[a-z]{5}$`)

func v2Binding() Binding {
	return Binding{Principal: "sandbox", Session: "00000000-0000-4000-8000-000000000002", Version: "v2"}
}

func v2Session(t *testing.T, dir string) *Session {
	t.Helper()
	s, err := Create(dir, v2Binding(), DefaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func hamming(a, b string) int {
	if len(a) != len(b) {
		return max(len(a), len(b))
	}
	d := 0
	for i := range a {
		if a[i] != b[i] {
			d++
		}
	}
	return d
}

// minPairwise returns the minimum Hamming and Levenshtein distance over every
// pair of aliases, and one closest pair.
func minPairwise(aliases []string) (int, int, [2]string) {
	minH, minL := 1<<30, 1<<30
	var pair [2]string
	for i := range aliases {
		for j := i + 1; j < len(aliases); j++ {
			if h := hamming(aliases[i], aliases[j]); h < minH {
				minH = h
			}
			if l := levenshtein(aliases[i], aliases[j]); l < minL {
				minL, pair = l, [2]string{aliases[i], aliases[j]}
			}
		}
	}
	return minH, minL, pair
}

func propertyAliases(m *RequestMap) []string {
	var out []string
	for alias, e := range m.st.Symbols {
		if e.Kind == "p" {
			out = append(out, alias)
		}
	}
	sort.Strings(out)
	return out
}

func TestV2AliasWordsPairwiseDistance(t *testing.T) {
	words := DefaultManifest().Words
	seen := map[string]bool{}
	for _, w := range aliasWords {
		if len(w) != 5 || strings.ToLower(w) != w || seen[w] {
			t.Fatalf("word %q: want 5 lowercase letters, unique", w)
		}
		seen[w] = true
		if containsAny(normalize(w), words) {
			t.Fatalf("word %q contains a manifest word", w)
		}
	}
	h, l, pair := minPairwise(aliasWords[:])
	if h < 3 || l < 3 {
		t.Fatalf("aliasWords min distance hamming=%d levenshtein=%d (%v), want >= 3", h, l, pair)
	}
}

func TestV2PropertyAliasShapeAndRoundTrip(t *testing.T) {
	wire, m, err := Prepare([]byte(strayFixture), v2Session(t, privateDir(t)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte(`"reason"`)) || bytes.Contains(wire, []byte(`"path"`)) {
		t.Fatalf("property names not aliased: %s", wire)
	}
	props := propertyAliases(m)
	if len(props) != 6 {
		t.Fatalf("property aliases = %d, want 6: %v", len(props), props)
	}
	for _, a := range props {
		if !v2PropertyShape.MatchString(a) {
			t.Fatalf("v2 property alias shape: %q", a)
		}
	}
	for alias, e := range m.st.Symbols {
		if e.Kind == "t" && !regexp.MustCompile(`^dpx_v1_t_[0-9a-f]{24}$`).MatchString(alias) {
			t.Fatalf("tool alias changed shape: %q", alias)
		}
	}
	sc := m.schemas["kanban_block"]
	block := m.st.Tools["kanban_block"].Alias
	input := restoreBoth(t, m, block, `{"`+sc.forward["reason"]+`":"r","`+sc.forward["opts"]+`":{"`+sc.props["opts"].forward["note"]+`":"n"}}`)
	got, _ := json.Marshal(input)
	if string(got) != `{"opts":{"note":"n"},"reason":"r"}` {
		t.Fatalf("restore: %s", got)
	}
}

// A one- or two-character slip of a v2 alias never lands on another live
// alias: it reaches the stray-key marker (t_186d86c5) instead.
func TestV2NearCopyNeverHitsSibling(t *testing.T) {
	_, m, err := Prepare([]byte(strayFixture), v2Session(t, privateDir(t)))
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for _, a := range propertyAliases(m) {
		live[a] = true
	}
	letters := "abcdefghijklmnopqrstuvwxyz"
	for a := range live {
		prefix := len("dpx_v1_p_")
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
	sc := m.schemas["kanban_block"]
	reason := sc.forward["reason"]
	slip := reason[:len(reason)-1] + string(rune('a'+(reason[len(reason)-1]-'a'+1)%26))
	input := restoreBoth(t, m, m.st.Tools["kanban_block"].Alias, `{"`+slip+`":"blocked"}`)
	requireMarker(t, input, []string{"reason", "kind", "opts"})
}

// AC-M12 for v2: the same declared schema gives the same map, in a fresh store
// and after reopening the store.
func TestV2Determinism(t *testing.T) {
	maps := [][]byte{}
	for i := 0; i < 2; i++ {
		dir := privateDir(t)
		wire1, _, err := Prepare([]byte(strayFixture), v2Session(t, dir))
		if err != nil {
			t.Fatal(err)
		}
		s, err := Open(dir, v2Binding(), DefaultManifest())
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

// Probe allocation: an original whose probe-0 alias is held by another
// original takes the next probe, and the store accepts it on reload.
func TestV2ProbeOnCollisionPersists(t *testing.T) {
	dir := privateDir(t)
	s := v2Session(t, dir)
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	victim := "kanban_block\x00#\x00reason"
	squat := propertySymbol(st.Binding, victim, 0)
	st.Symbols[squat] = entry{"p", "squatter"}
	// Make the squat valid: squatter must own probe 0 of its own original, so
	// find an original whose probe 0 equals the victim's (search is bounded).
	delete(st.Symbols, squat)
	other := ""
	for i := 0; i < 200000 && other == ""; i++ {
		cand := "t\x00#\x00k" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + itoa(i)
		if propertySymbol(st.Binding, cand, 0) == squat {
			other = cand
		}
	}
	if other == "" {
		t.Skip("no probe-0 collision found within bound")
	}
	a1, err := st.allocate("p", other)
	if err != nil || a1 != squat {
		t.Fatalf("first: %q %v", a1, err)
	}
	a2, err := st.allocate("p", victim)
	if err != nil || a2 == squat || a2 != propertySymbol(st.Binding, victim, 1) {
		t.Fatalf("victim did not take probe 1: %q %v", a2, err)
	}
	if err := s.save(st); err != nil {
		t.Fatal(err)
	}
	re, err := s.load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := re.allocate("p", victim); got != a2 {
		t.Fatalf("victim alias moved after reload: %q -> %q", a2, got)
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestV2LoadRejectsForgedPropertyAlias(t *testing.T) {
	dir := privateDir(t)
	s := v2Session(t, dir)
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	st.Symbols["dpx_v1_p_amber_acorn"] = entry{"p", "Read\x00#\x00file_path"}
	if propertySymbol(st.Binding, "Read\x00#\x00file_path", 0) == "dpx_v1_p_amber_acorn" {
		t.Skip("forged alias happens to be valid")
	}
	if err := s.save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := s.load(); err == nil || err.Error() != Error("store_corrupt").Error() {
		t.Fatalf("forged v2 property alias accepted: %v", err)
	}
}

// A v1 store does not open under a v2 binding (and vice versa): the binding,
// version included, is part of the stored state.
func TestV2BindingDoesNotOpenV1Store(t *testing.T) {
	dir := privateDir(t)
	if _, err := Create(dir, Binding{Principal: "sandbox", Session: "00000000-0000-4000-8000-000000000002", Version: "v1"}, DefaultManifest()); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, v2Binding(), DefaultManifest()); err == nil {
		t.Fatal("v1 store opened under v2 binding")
	}
	if _, err := Create(privateDir(t), Binding{Principal: "p", Session: "s", Version: "v3"}, DefaultManifest()); err == nil {
		t.Fatal("unknown version accepted")
	}
}

// Confusability over the synthetic 60-tool catalog, and over every live tool
// set when DPX_LIVE_MAPS names a directory of v1 map.json copies (read-only
// inputs; each is re-prepared under a v2 binding with its own principal).
func TestV2MinPairwiseDistance(t *testing.T) {
	check := func(name string, raw []byte, b Binding) {
		dir := privateDir(t)
		s, err := Create(dir, b, DefaultManifest())
		if err != nil {
			t.Fatal(err)
		}
		_, m, err := Prepare(raw, s)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		props := propertyAliases(m)
		h, l, pair := minPairwise(props)
		maxProbe := 0
		for alias, e := range m.st.Symbols {
			if e.Kind != "p" {
				continue
			}
			for k := 0; k < propertyProbes; k++ {
				if propertySymbol(b, e.Original, k) == alias {
					maxProbe = max(maxProbe, k)
					break
				}
			}
		}
		t.Logf("%s: tools=%d property_aliases=%d min_hamming=%d min_levenshtein=%d closest=%v max_probe=%d", name, len(m.schemas), len(props), h, l, pair, maxProbe)
		if len(props) > 1 && (h < 3 || l < 3) {
			t.Fatalf("%s: min distance hamming=%d levenshtein=%d (%v)", name, h, l, pair)
		}
	}
	check("synthetic", syntheticBody(t, 450<<10), v2Binding())
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
		b.Version = "v2"
		check(filepath.Base(f), body, b)
	}
}
