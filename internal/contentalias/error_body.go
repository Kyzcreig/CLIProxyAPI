package contentalias

import (
	"encoding/json"
	"strings"
)

// RestoreErrorBody reverse-maps every symbol this request's store knows
// (words, literals, tool names) inside an upstream error body, so the caller
// sees the upstream reason in its own vocabulary. Unknown symbol-shaped tokens
// are left as-is: they are opaque digests and reveal nothing. Replacements are
// JSON-string-escaped because error bodies are JSON and symbols sit in strings.
func (m *RequestMap) RestoreErrorBody(raw []byte) []byte {
	if m == nil {
		return raw
	}
	return []byte(decodePattern.ReplaceAllStringFunc(string(raw), func(alias string) string {
		e, ok := m.st.Symbols[alias]
		if !ok {
			return alias
		}
		quoted, err := json.Marshal(e.Original)
		if err != nil {
			return alias
		}
		return strings.TrimSuffix(strings.TrimPrefix(string(quoted), `"`), `"`)
	}))
}
