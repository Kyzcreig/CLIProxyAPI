package brandalias

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
)

// The apx brand corpus (claude-apx corpus/brand-corpus.json, 134 cases,
// byte-identical across apx/bpx/pool) is the gold set for the plugin's word
// codec (card t_a37235c0). Contract per case:
//   - must_scrub: no brand-A/B token survives Encode;
//   - !must_scrub && !bounded: Encode is the identity (no over-scrub);
//   - bounded: component-specific (apx's word boundary vs DPX's contains-match
//     differ by design); only the round trip is asserted;
//   - every case: Decode(Encode(x)) == x.
//
// Templated cases substitute the sentinel placeholders with DPX symbols, which
// Encode escapes as literals and Decode restores.
const apxCorpusPath = "testdata/apx-brand-corpus.json"

type apxCase struct {
	ID        string `json:"id"`
	Input     string `json:"input"`
	MustScrub bool   `json:"must_scrub"`
	Bounded   bool   `json:"bounded"`
	Templated bool   `json:"templated"`
}

func TestApxBrandCorpus(t *testing.T) {
	raw, err := os.ReadFile(apxCorpusPath)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var top struct {
		Counts struct{ Total int } `json:"counts"`
		Cases  []apxCase           `json:"cases"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("corpus: %v", err)
	}
	if len(top.Cases) == 0 || len(top.Cases) != top.Counts.Total {
		t.Fatalf("corpus has %d cases, header says %d; refusing to report green", len(top.Cases), top.Counts.Total)
	}
	brand := []string{"hermes", "openclaw"}
	sentinels := strings.NewReplacer(
		"{SENT_A}", "dpx_v1_w_000000000000000000000000", "{SENT_A_LOWER}", "dpx_v1_w_000000000000000000000000",
		"{SENT_B}", "dpx_v1_w_111111111111111111111111", "{SENT_B_LOWER}", "dpx_v1_w_111111111111111111111111")
	for _, c := range top.Cases {
		t.Run(c.ID, func(t *testing.T) {
			codec, err := contentalias.NewWordCodec(contentalias.Binding{Principal: "p", Session: "s", Version: "v1"}, contentalias.Manifest{Words: DefaultWords})
			if err != nil {
				t.Fatal(err)
			}
			in := c.Input
			if c.Templated {
				in = sentinels.Replace(in)
			}
			out, err := codec.Encode(in)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if back := codec.Decode(out); back != in {
				t.Fatalf("round trip: %q -> %q -> %q", in, out, back)
			}
			if c.MustScrub {
				if n := CountBrandTokens([]byte(out), brand); n != 0 {
					t.Fatalf("leak: %q -> %q (%d brand tokens)", in, out, n)
				}
			} else if !c.Bounded && !c.Templated && out != in {
				t.Fatalf("over-scrub: %q -> %q", in, out)
			}
		})
	}
}

// Standing ruling (Ace 2026-08-07): agent names and user identity are never
// scrubbed, and neither are the vendor's own name or bare transport nouns.
// A false positive on this forward-only seam corrupts the outbound question.
func TestNeverScrubbedVocabulary(t *testing.T) {
	codec, err := contentalias.NewWordCodec(contentalias.Binding{Principal: "p", Session: "s", Version: "v1"}, contentalias.Manifest{Words: DefaultWords})
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{
		"You are Apollo, coordinating with Aegis, Athena, Clanker, Daedalus, Argus and Momus.",
		"Ace asked about the Apollo missions.",
		"Anthropic publishes the Claude models; the proxy and the relay are ordinary words.",
		"Set MAX_THINKING_TOKENS and NODE_ENV in the config.",
		"Home Assistant owns the entity registry.",
	} {
		out, err := codec.Encode(src)
		if err != nil {
			t.Fatal(err)
		}
		if out != src {
			t.Fatalf("scrubbed a protected word: %q -> %q", src, out)
		}
	}
}

// Mutation arm for the corpus gate: a brand word the walkers leave in place
// (here: one never passed to Encode at all) MUST be counted, or the
// brandTokens==0 gate has no teeth.
func TestCountBrandTokensHasTeeth(t *testing.T) {
	// Variants are built from the manifest bytes so the fixture cannot drift
	// from the words it is supposed to catch.
	a, b := DefaultWords[0], DefaultWords[1]
	note := strings.ToUpper(a) + " and " + b[:4] + "-" + strings.ToUpper(b[4:5]) + b[5:] + " and " + b[:4] + "_" + b[4:]
	if n := CountBrandTokens([]byte(`{"model":"x","note":"`+note+`"}`), []string{a, b}); n != 3 {
		t.Fatalf("want 3 brand tokens in %q, got %d", note, n)
	}
	if n := CountBrandTokens([]byte(`{"text":"the cat will open claw marks"}`), []string{"openclaw"}); n != 0 {
		t.Fatalf("space-separated words are not a brand token, got %d", n)
	}
}
