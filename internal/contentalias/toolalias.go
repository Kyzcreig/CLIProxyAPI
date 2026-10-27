package contentalias

import (
	"encoding/binary"
	"encoding/json"
	"strings"
)

// Map v3 tool aliases (t_551a4f33). v1 and v2 name every tool
// dpx_v1_t_<24 hex>. The model near-copies those: 10 of 6,347 tool_use names
// over 2026-10-25..27 were 1-5 characters off a declared alias, either a 2-hex
// slip or a hex tail rewritten as a word (...648c7ce0 -> ...648c7seed). The
// unknown_tool_N marker (t_7bd8c9b9) keeps such a slip from failing the turn;
// v3 lowers how often it happens. A v3 tool alias is spelled like a v2 property
// alias, two words from aliasWords: dpx_v1_t_<word>_<word>. Any two words differ
// in at least 3 positions, so two distinct v3 tool aliases differ in at least 3
// characters, and a 1-2 character slip never lands on another declared tool.
// The tail is a closed 5-letter word, never an open hex run the model can
// finish as English.
//
// v3 keeps v2's property aliases unchanged and keeps the "dpx_v1_" codec
// namespace (prose.go, request.go, strayClassPattern and the marker net key on
// it). Word and literal symbols are unchanged. v1 and v2 maps keep loading
// with their 24-hex tool aliases.
//
// Allocation is the property scheme: a pure function of (binding, name,
// probe), probe 0 unless that alias is already held in this session's map. The
// store persists the result, so an alias never moves for the life of the
// session. The ">= 3" bound is over tool aliases (the names a tool_use block
// can carry); a property alias differs from every tool alias in its class
// letter and is never looked up as a tool name.

const (
	bindingV3  = "v3"
	toolProbes = 64
)

// wordPairAliases reports whether b spells property aliases as word pairs.
func wordPairAliases(b Binding) bool { return b.Version == bindingV2 || b.Version == bindingV3 }

// wordPairTools reports whether b spells tool aliases as word pairs.
func wordPairTools(b Binding) bool { return b.Version == bindingV3 }

// toolSymbol is probe k's v3 alias for a tool name.
func toolSymbol(b Binding, name string, k int) string {
	key, _ := json.Marshal([]any{b.Principal, b.Session, b.Version, "t", name, k})
	h := digestBytes(key)
	n := uint32(len(aliasWords))
	first := binary.BigEndian.Uint32(h[0:4]) % n
	second := binary.BigEndian.Uint32(h[4:8]) % n
	return "dpx_v1_t_" + aliasWords[first] + "_" + aliasWords[second]
}

// validToolAlias reports whether alias is one of name's v3 probes.
func validToolAlias(b Binding, alias, name string) bool {
	if !strings.HasPrefix(alias, "dpx_v1_t_") || len(alias) != len("dpx_v1_t_")+11 {
		return false
	}
	for k := 0; k < toolProbes; k++ {
		if toolSymbol(b, name, k) == alias {
			return true
		}
	}
	return false
}

// allocateTool returns name's v3 alias, allocating the first free probe. The
// tools index is built once per state from Symbols, like properties.
func (st *state) allocateTool(name string) (string, error) {
	if st.tools == nil {
		st.tools = map[string]string{}
		for alias, e := range st.Symbols {
			if e.Kind == "t" {
				st.tools[e.Original] = alias
			}
		}
	}
	if alias, ok := st.tools[name]; ok {
		return alias, nil
	}
	words := make([]string, 0, len(st.Manifest.Words))
	for _, word := range st.Manifest.Words {
		words = append(words, normalize(word))
	}
	for k := 0; k < toolProbes; k++ {
		alias := toolSymbol(st.Binding, name, k)
		if _, taken := st.Symbols[alias]; taken || containsAny(normalize(alias), words) {
			continue
		}
		st.Symbols[alias] = entry{"t", name}
		st.tools[name] = alias
		return alias, nil
	}
	return "", Error("symbol_collision")
}
