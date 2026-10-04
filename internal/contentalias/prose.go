package contentalias

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var codecPattern = regexp.MustCompile(`dpx_v1_[A-Za-z0-9_]*`)
var decodePattern = regexp.MustCompile(`dpx_v1_[wl]_[0-9a-f]{24}|dpx_v1_[A-Za-z0-9_]*`)

func (st *state) encodeText(text string) (string, error) {
	// Every word run is scanned: prose, backtick spans and resource-shaped
	// tokens (paths, URLs, "subject:" prefixes) alike (t_cb095320). A run that
	// CONTAINS a manifest word (hermes-agent, HERMES_KANBAN_TASK) is aliased as
	// one symbol; decode restores the exact bytes wherever the symbol reappears,
	// including tool_use input values, so a copied path or identifier executes
	// unchanged.
	var out strings.Builder
	out.Grow(len(text))
	words := make([]string, 0, len(st.Manifest.Words))
	for _, word := range st.Manifest.Words {
		words = append(words, normalize(word))
	}
	spellings := map[string]string{}
	for alias, e := range st.Symbols {
		if e.Kind == "w" {
			spellings[e.Original] = alias
		}
	}
	for pos := 0; pos < len(text); {
		// Escape source codec symbols once before spelling encoding, not recursively.
		// Only try the regexp where the remainder starts with its literal prefix
		// (t_129cf1ac): an unanchored FindStringIndex on text[pos:] scanned the whole
		// remainder at every position, O(n^2): 4 s per 444 KB prompt, held under the
		// store lock, so every request on the unit queued behind it.
		if strings.HasPrefix(text[pos:], "dpx_v1_") {
			if loc := codecPattern.FindStringIndex(text[pos:]); loc != nil && loc[0] == 0 {
				literal := text[pos : pos+loc[1]]
				alias, err := st.allocate("l", literal)
				if err != nil {
					return "", err
				}
				out.WriteString(alias)
				pos += loc[1]
				continue
			}
		}
		rr, sz := utf8.DecodeRuneInString(text[pos:])
		if !wordRune(rr) {
			out.WriteString(text[pos : pos+sz])
			pos += sz
			continue
		}
		end := pos + sz
		for end < len(text) {
			c, n := utf8.DecodeRuneInString(text[end:])
			if !wordRune(c) || strings.HasPrefix(text[end:], "dpx_v1_") {
				break
			}
			end += n
		}
		word := text[pos:end]
		if alias, ok := spellings[word]; ok {
			out.WriteString(alias)
		} else if containsAny(normalize(word), words) {
			alias, err := st.allocate("w", word)
			if err != nil {
				return "", err
			}
			spellings[word] = alias
			out.WriteString(alias)
		} else {
			out.WriteString(word)
		}
		pos = end
	}
	return out.String(), nil
}
func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' }
func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
func (m *RequestMap) decodeText(text string) (string, error) {
	d := textDecoder{m: m}
	first, err := d.feed(text)
	if err != nil {
		return "", err
	}
	last, err := d.finish()
	return first + last, err
}

// decodeValue restores the word and literal symbols in a tool_use input value.
// Anything else shaped like a symbol is what the model typed and passes through
// verbatim: a tool call is never refused over its argument text.
func (m *RequestMap) decodeValue(text string) (string, error) {
	return decodePattern.ReplaceAllStringFunc(text, func(alias string) string {
		if e, ok := m.st.Symbols[alias]; ok && (e.Kind == "w" || e.Kind == "l") {
			return e.Original
		}
		return alias
	}), nil
}
func (m *RequestMap) decodeToken(text string) (string, error) {
	var failure error
	out := decodePattern.ReplaceAllStringFunc(text, func(alias string) string {
		e, ok := m.st.Symbols[alias]
		if !ok || (e.Kind != "w" && e.Kind != "l") {
			failure = Error("unknown_prose_symbol")
			return alias
		}
		return e.Original
	})
	return out, failure
}
func textEdit(n *node, encode func(string) (string, error), edits *[]edit) error {
	if n == nil || n.kind != '"' {
		return nil
	}
	out, err := encode(n.text)
	if err != nil {
		return err
	}
	if out != n.text {
		*edits = append(*edits, replace(n, out))
	}
	return nil
}
