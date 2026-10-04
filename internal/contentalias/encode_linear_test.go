package contentalias

import (
	"strings"
	"testing"
	"time"
)

// t_129cf1ac: encodeText ran an unanchored regexp over text[pos:] at every
// position, O(n^2) in the prompt size. Prepare holds the store's exclusive lock
// for the whole encode, so a 444 KB organic prompt (4 s) serialized every request
// on the DPX unit and pushed tui turns past the bridge's 600 s deadline.
//
// The guard compares a 16x larger input against a small one: linear code grows
// ~16x, the quadratic code ~256x. The bound (64x) leaves room for timer noise and
// still fails the quadratic shape by 4x.
func TestEncodeTextIsLinearInTextSize(t *testing.T) {
	st := &state{Manifest: DefaultManifest(), Symbols: map[string]entry{}, Tools: map[string]storedTool{}}
	st.Binding = Binding{"p", "s", "v1"}
	unit := strings.Repeat("plain prose with words, numbers 12345 and paths /a/b/c.go; ", 32)
	small := strings.Repeat(unit, 4)
	large := strings.Repeat(unit, 64)
	best := func(text string) time.Duration {
		var min time.Duration
		for i := 0; i < 5; i++ {
			t0 := time.Now()
			if _, err := st.encodeText(text); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(t0); i == 0 || d < min {
				min = d
			}
		}
		return min
	}
	ds, dl := best(small), best(large)
	if ds <= 0 {
		ds = time.Microsecond
	}
	if ratio := float64(dl) / float64(ds); ratio > 64 {
		t.Fatalf("encodeText scaled %.0fx for a 16x larger input (small %v, large %v): quadratic scan is back", ratio, ds, dl)
	}
}

// The prefix guard must not change what is aliased: codec literals anywhere in
// the text (start, middle, end, adjacent to words) still become "l" symbols.
func TestEncodeTextStillAliasesCodecLiteralsEverywhere(t *testing.T) {
	st := &state{Manifest: DefaultManifest(), Symbols: map[string]entry{}, Tools: map[string]storedTool{}}
	st.Binding = Binding{"p", "s", "v1"}
	in := "dpx_v1_head mid dpx_v1_x_Y9 word-dpx_v1_tail hermes-agent end dpx_v1_"
	out, err := st.encodeText(in)
	if err != nil {
		t.Fatal(err)
	}
	literals := 0
	for _, e := range st.Symbols {
		if e.Kind == "l" {
			literals++
		}
	}
	if literals != 4 {
		t.Fatalf("want 4 codec literals aliased, got %d (out=%q)", literals, out)
	}
	if strings.Contains(out, "hermes") {
		t.Fatalf("manifest word leaked: %q", out)
	}
}
