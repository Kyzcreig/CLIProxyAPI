package contentalias

import (
	"encoding/binary"
	"encoding/json"
	"strings"
)

// Map v2 property aliases (t_13128fb0). v1 named every schema property
// dpx_v1_p_<24 hex>. The model mis-copied those: 1-2 hex near-copies and a
// sibling tool's alias (write_file#path typed for kanban_block#reason, both
// dpx_v1_p_4e3...). v2 spells a property alias as two words from aliasWords:
// dpx_v1_p_<word>_<word>. Every word is 5 lowercase letters and any two words
// differ in at least 3 positions (Hamming and Levenshtein), so two DISTINCT v2
// property aliases always differ in at least 3 characters. A 1-2 character slip
// never lands on another live alias; it reaches the stray-key marker instead.
//
// The "dpx_v1_" prefix stays: it is the codec namespace (prose literal escape,
// reserved_property, the stray-key marker), not the map version. The binding
// Version ("v2") selects the scheme. Tool, word and literal symbols are
// unchanged.
//
// Allocation is a pure function of (binding, original, probe). Probe 0 is used
// unless that alias is already held by another original in this session's map,
// in which case the next probe is tried. The store persists the result, so an
// alias never moves for the life of the session, whatever tool sets later
// requests declare.

const (
	bindingV1      = "v1"
	bindingV2      = "v2"
	propertyProbes = 64
)

var aliasWords = [...]string{
	"amber", "acorn", "agate", "anvil", "apple", "apron", "aspen", "atlas", "attic", "badge", "banjo", "basin",
	"beach", "berry", "bloom", "board", "boots", "brass", "bread", "brick", "cable", "candy", "cargo", "cedar",
	"chalk", "chess", "chimp", "cliff", "clock", "coast", "cobra", "couch", "crane", "creek", "crown", "cubic",
	"daisy", "delta", "denim", "depot", "diver", "dozen", "drift", "ebony", "elbow", "elder", "fence", "field",
	"fjord", "flask", "fleet", "flute", "focus", "forge", "gecko", "ghost", "giant", "globe", "grain", "guava",
	"guide", "gusto", "haiku", "hatch", "haven", "hinge", "hippo", "honey", "husky", "igloo", "inlet", "ivory",
	"jelly", "jewel", "joker", "kayak", "kebab", "kiosk", "koala", "lemon", "lilac", "linen", "llama", "lunar",
	"lyric", "marsh", "merit", "mocha", "molar", "moose", "motel", "nacho", "nerve", "north", "nutty", "ocean",
	"olive", "onion", "opera", "orbit", "otter", "paper", "pasta", "patio", "pearl", "pecan", "penny", "pilot",
	"pinto", "pixel", "polka", "poppy", "prism", "pulse", "quail", "relic", "rhino", "robin", "rodeo", "salad",
	"sauna", "scarf", "scone", "sheep", "shrub", "sigma", "sloth", "spice", "squid", "stamp", "stork", "topaz",
	"trout", "tulip", "uncle", "valve", "vault", "vigor", "viola", "vinyl", "wagon", "waltz", "wheat", "widow",
	"woman",
}

func validVersion(v string) bool { return v == bindingV1 || v == bindingV2 }

// propertySymbol is probe k's v2 alias for a property original.
func propertySymbol(b Binding, original string, k int) string {
	key, _ := json.Marshal([]any{b.Principal, b.Session, b.Version, "p", original, k})
	h := digestBytes(key)
	n := uint32(len(aliasWords))
	first := binary.BigEndian.Uint32(h[0:4]) % n
	second := binary.BigEndian.Uint32(h[4:8]) % n
	return "dpx_v1_p_" + aliasWords[first] + "_" + aliasWords[second]
}

// validPropertyAlias reports whether alias is one of original's probes.
func validPropertyAlias(b Binding, alias, original string) bool {
	if !strings.HasPrefix(alias, "dpx_v1_p_") || len(alias) != len("dpx_v1_p_")+11 {
		return false
	}
	for k := 0; k < propertyProbes; k++ {
		if propertySymbol(b, original, k) == alias {
			return true
		}
	}
	return false
}

// allocateProperty returns original's v2 alias, allocating the first free
// probe. properties indexes original -> alias for this state; it is built once
// per state from Symbols (linear), never scanned per call.
func (st *state) allocateProperty(original string) (string, error) {
	if st.properties == nil {
		st.properties = map[string]string{}
		for alias, e := range st.Symbols {
			if e.Kind == "p" {
				st.properties[e.Original] = alias
			}
		}
	}
	if alias, ok := st.properties[original]; ok {
		return alias, nil
	}
	words := make([]string, 0, len(st.Manifest.Words))
	for _, word := range st.Manifest.Words {
		words = append(words, normalize(word))
	}
	for k := 0; k < propertyProbes; k++ {
		alias := propertySymbol(st.Binding, original, k)
		if _, taken := st.Symbols[alias]; taken || containsAny(normalize(alias), words) {
			continue
		}
		st.Symbols[alias] = entry{"p", original}
		st.properties[original] = alias
		return alias, nil
	}
	return "", Error("symbol_collision")
}
