package contentalias

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The CPA consumer of the fleet leak corpus (t_4c8005fc). CPA is not one of the
// corpus's three components; it runs the cases that opt in with
// external_consumers ["cpa"], vendored verbatim at testdata/leak-corpus-cpa.json
// by the private generator (claude-bpx bridge/scripts/cpa-corpus-subset.js, whose
// --check is the canonical-side drift gate). Same contract as the Python and Node
// loaders: a case with no binding, an unknown kind, an empty or tampered fixture,
// or a changed case set is a FAILURE, never a skip.

const cpaCorpusPath = "testdata/leak-corpus-cpa.json"

// Anti-deletion ratchet (SPEC-leak-prevention.md L11): removing a case from the
// fixture must also edit this list, in this repo's diff.
var cpaPinnedCaseIDs = []string{"LC-14", "LC-15"}

type cpaCase struct {
	ID       string `json:"id"`
	Class    int    `json:"class"`
	Rule     string `json:"rule"`
	Kind     string `json:"kind"`
	Surface  string `json:"surface"`
	Input    string `json:"input"`
	Needle   string `json:"needle"`
	Incident string `json:"incident"`
}

func loadCPACorpus(t *testing.T) []cpaCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(cpaCorpusPath))
	if err != nil {
		t.Fatalf("[leak-corpus] cannot read %s: %v", cpaCorpusPath, err)
	}
	var top struct {
		Consumer    string          `json:"consumer"`
		CasesSHA256 string          `json:"cases_sha256"`
		Cases       json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("[leak-corpus] %s: %v", cpaCorpusPath, err)
	}
	if top.Consumer != "cpa" {
		t.Fatalf("[leak-corpus] fixture consumer %q, want cpa", top.Consumer)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, top.Cases); err != nil {
		t.Fatalf("[leak-corpus] cases: %v", err)
	}
	sum := sha256.Sum256(compact.Bytes())
	if got := hex.EncodeToString(sum[:]); got != top.CasesSHA256 {
		t.Fatalf("[leak-corpus] cases_sha256 mismatch (got %s, fixture says %s): the vendored cases were edited by hand; regenerate with cpa-corpus-subset.js --write", got, top.CasesSHA256)
	}
	var cases []cpaCase
	if err := json.Unmarshal(top.Cases, &cases); err != nil {
		t.Fatalf("[leak-corpus] cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("[leak-corpus] fixture has no cases; refusing to report green")
	}
	return cases
}

func cpaRequestBody(t *testing.T) func(string) string {
	return func(input string) string {
		wire, _, err := Prepare([]byte(input), testSession(t))
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		return string(wire)
	}
}

func TestLeakCorpusCPA(t *testing.T) {
	cases := loadCPACorpus(t)
	bindings := map[string]func(string) string{
		"request_body": cpaRequestBody(t),
	}

	got := make([]string, 0, len(cases))
	for _, c := range cases {
		got = append(got, c.ID)
	}
	sort.Strings(got)
	want := append([]string(nil), cpaPinnedCaseIDs...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("[leak-corpus] case set %v != pinned %v: a case must never be silently dropped; pin new ids here in the same commit", got, want)
	}

	// Positive control: the bound transform must be live, or every survival case
	// passes for the wrong reason.
	probe := `{"model":"m","messages":[{"role":"user","content":"Hermes list src"}]}`
	if out := bindings["request_body"](probe); strings.Contains(strings.ToLower(out), "hermes list") {
		t.Fatalf("[leak-corpus] request_body binding did not alias content; the rig is vacuous: %s", out)
	}

	for _, c := range cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			bind, ok := bindings[c.Surface]
			if !ok {
				t.Fatalf("[leak-corpus] %s: no binding for surface %q; an unbound case is an uncovered leak class, not a pass", c.ID, c.Surface)
			}
			out := bind(c.Input)
			has := strings.Contains(strings.ToLower(out), strings.ToLower(c.Needle))
			var pass bool
			switch c.Kind {
			case "literal_serialized":
				pass = has
			case "scrub_serialized":
				pass = !has
			default:
				t.Fatalf("[leak-corpus] %s: kind %q has no CPA evaluator", c.ID, c.Kind)
			}
			if !pass {
				t.Fatalf("LEAK-CORPUS %s FAILED (class %d, rule %s)\n  surface : %s\n  needle  : %q\n  result  : %s\n  incident: %s",
					c.ID, c.Class, c.Rule, c.Surface, c.Needle, out, c.Incident)
			}
		})
	}
}
