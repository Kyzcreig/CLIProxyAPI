package contentalias

import (
	"strings"
	"testing"
)

func TestStreamLongTokensWithinResponseBudget(t *testing.T) {
	_, m, err := Prepare([]byte(`{"system":"Hermes"}`), testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	var alias string
	for a, e := range m.st.Symbols {
		if e.Kind == "w" {
			alias = a
		}
	}
	for _, input := range []string{strings.Repeat("λ", 20000), alias + strings.Repeat("!", 10000), alias + strings.Repeat("!", 10000) + "/file"} {
		d := textDecoder{m: m}
		var out strings.Builder
		for _, r := range input {
			text, err := d.feed(string(r))
			if err != nil {
				t.Fatal(err)
			}
			out.WriteString(text)
		}
		end, err := d.finish()
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString(end)
		// Resource-shaped tokens decode too (t_cb095320).
		expected := strings.ReplaceAll(input, alias, "Hermes")
		if out.String() != expected {
			t.Fatal("long-token decoded bytes differ")
		}
	}
}
