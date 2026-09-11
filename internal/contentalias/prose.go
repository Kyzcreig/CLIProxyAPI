package contentalias

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var codecPattern = regexp.MustCompile(`dpx_v1_[A-Za-z0-9_]*`)

func (st *state) encodeText(text string) (string, error) {
	// All resource-shaped whitespace tokens and backtick spans are exempt.
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] == '`' {
			j := i
			for j < len(text) && text[j] == '`' {
				j++
			}
			marker := text[i:j]
			end := strings.Index(text[j:], marker)
			if end < 0 {
				out.WriteString(text[i:])
				break
			}
			end += j + len(marker)
			out.WriteString(text[i:end])
			i = end
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if unicode.IsSpace(r) {
			out.WriteString(text[i : i+size])
			i += size
			continue
		}
		j := i
		for j < len(text) {
			r, n := utf8.DecodeRuneInString(text[j:])
			if unicode.IsSpace(r) || text[j] == '`' {
				break
			}
			j += n
		}
		token := text[i:j]
		if strings.ContainsAny(token, "/\\:@") {
			out.WriteString(token)
			i = j
			continue
		}
		// Escape source codec symbols once before spelling encoding, not recursively.
		for pos := 0; pos < len(token); {
			if loc := codecPattern.FindStringIndex(token[pos:]); loc != nil && loc[0] == 0 {
				literal := token[pos : pos+loc[1]]
				alias, err := st.allocate("l", literal)
				if err != nil {
					return "", err
				}
				out.WriteString(alias)
				pos += loc[1]
				continue
			}
			rr, sz := utf8.DecodeRuneInString(token[pos:])
			if unicode.IsLetter(rr) || unicode.IsDigit(rr) || rr == '_' || rr == '-' {
				end := pos + sz
				for end < len(token) {
					c, n := utf8.DecodeRuneInString(token[end:])
					if (!unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_' && c != '-') || strings.HasPrefix(token[end:], "dpx_v1_") {
						break
					}
					end += n
				}
				word := token[pos:end]
				match := false
				for _, candidate := range st.Manifest.Words {
					if normalize(word) == normalize(candidate) {
						match = true
						break
					}
				}
				if match {
					alias, err := st.allocate("w", word)
					if err != nil {
						return "", err
					}
					out.WriteString(alias)
				} else {
					out.WriteString(word)
				}
				pos = end
			} else {
				out.WriteString(token[pos : pos+sz])
				pos += sz
			}
		}
		i = j
	}
	return out.String(), nil
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
func (m *RequestMap) decodeToken(text string) (string, error) {
	var failure error
	out := codecPattern.ReplaceAllStringFunc(text, func(alias string) string {
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
