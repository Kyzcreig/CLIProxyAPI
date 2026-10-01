package contentalias

import (
	"bytes"
	"testing"
)

func TestJSONSerializationIndependent(t *testing.T) {
	for _, raw := range []string{` { "a" : "\\\"\n", "n":1.00e+03, "u":"\ud83d\ude00" } `, `{"x":[true,null,{"y":"é"}]}`} {
		n, err := parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		out, err := apply([]byte(raw), nil)
		if err != nil || !bytes.Equal(out, []byte(raw)) || n == nil {
			t.Fatal("identity")
		}
	}
	for _, raw := range []string{`{"a":1,"\u0061":2}`, `{"x":{"a":1,"a":2}}`, `{"x":"\ud800"}`, `{"x":1} {}`} {
		if _, err := parse([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid JSON: %s", raw)
		}
	}
}

func TestRawSpanEdits(t *testing.T) {
	raw := []byte(` { "name": "Bash", "input":{ "command" : "echo Hermes", "n":1.00 } } `)
	n, err := parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := apply(raw, []edit{replace(n.get("name"), "tool_0")})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(` { "name": "tool_0", "input":{ "command" : "echo Hermes", "n":1.00 } } `)
	if !bytes.Equal(out, want) {
		t.Fatalf("span edit: %s", out)
	}
	if _, err := apply(raw, []edit{{1, 5, []byte("x")}, {2, 6, []byte("y")}}); err == nil {
		t.Fatal("overlap accepted")
	}
}
