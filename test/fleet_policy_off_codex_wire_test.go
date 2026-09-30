package test

// TestFleetPolicyOffCodexWire is the Phase 0 wire gate of the one-CLIProxyAPI-lineage spec
// (§3 I2, §7 Phase 0, AC3). A keyless codex / xAI /v1/responses request through the fleet
// tree must leave with:
//   - the fork's keyless-client FIX present: in shadow (and with the key absent, today's
//     zero value) the legacy per-API-key UUID (c97fb61e codex, ba5ea095 xAI); in enforce
//     the resolver's derived key (1e7437e1);
//   - everything else byte-identical to pristine <BASE> (testdata/fleet_wire/pristine-*.json,
//     recorded on v8.0.4 d33f63f8 with TestFleetWireRecord).
// The diff against <BASE> must be exactly the routing key: body prompt_cache_key plus the
// header that carries the same value (codex Session-Id, xAI X-Grok-Conv-Id). Content-Length
// is derived from the body and excluded. The "off" arm is added with the Phase 1 `off`
// commit; it does not exist on this tree.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func fleetWireExpectedKey(t *testing.T, arm fleetWireArm) string {
	t.Helper()
	switch arm.Policy {
	case "enforce":
		key := cliproxyexecutor.DerivePromptCacheKey(arm.Provider, []byte(arm.Payload))
		if !strings.HasPrefix(key, "pck-") {
			t.Fatalf("%s: resolver produced no derived key (%q)", arm.Name, key)
		}
		return key
	case "shadow", "":
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:"+arm.Provider+":prompt-cache:"+fleetWireClientAPIKey)).String()
	default:
		t.Fatalf("%s: no expectation for policy %q", arm.Name, arm.Policy)
	}
	return ""
}

func fleetWireLoadPristine(t *testing.T, name string) fleetWireRecord {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", "fleet_wire", "pristine-"+name+".json"))
	if errRead != nil {
		t.Fatalf("pristine golden %s: %v", name, errRead)
	}
	var rec fleetWireRecord
	if errJSON := json.Unmarshal(raw, &rec); errJSON != nil {
		t.Fatalf("pristine golden %s: %v", name, errJSON)
	}
	return rec
}

// fleetWireSubstituteKey returns rec with every occurrence of oldKey (body prompt_cache_key
// and any header value equal to it) replaced by newKey, and Content-Length dropped. It
// reports which header carried the key.
func fleetWireSubstituteKey(rec fleetWireRecord, oldKey, newKey string) (fleetWireRecord, []string) {
	out := fleetWireRecord{Method: rec.Method, Path: rec.Path, Headers: map[string][]string{}, Body: map[string]any{}}
	var carriers []string
	for k, v := range rec.Headers {
		if k == "Content-Length" {
			continue
		}
		vv := make([]string, len(v))
		for i, s := range v {
			if s == oldKey {
				s = newKey
				carriers = append(carriers, k)
			}
			vv[i] = s
		}
		out.Headers[k] = vv
	}
	for k, v := range rec.Body {
		out.Body[k] = v
	}
	if got, _ := out.Body["prompt_cache_key"].(string); got == oldKey {
		out.Body["prompt_cache_key"] = newKey
	}
	return out, carriers
}

func TestFleetPolicyOffCodexWire(t *testing.T) {
	for _, arm := range fleetWireArms() {
		arm := arm
		t.Run(arm.Name, func(t *testing.T) {
			pristine := fleetWireLoadPristine(t, arm.Name)
			got := fleetWireNormalize(fleetRecordWire(t, arm))
			want := fleetWireExpectedKey(t, arm)

			gotKey, _ := got.Body["prompt_cache_key"].(string)
			if gotKey != want {
				t.Fatalf("fix missing: prompt_cache_key = %q, want %q (policy %q)", gotKey, want, arm.Policy)
			}
			if gotKey == "spoofed" {
				t.Fatalf("client-supplied metadata look-alike reached the wire")
			}
			pristineKey, _ := pristine.Body["prompt_cache_key"].(string)
			if pristineKey == "" {
				t.Fatalf("pristine golden carries no prompt_cache_key; re-record on <BASE>")
			}
			expected, carriers := fleetWireSubstituteKey(pristine, pristineKey, want)
			if len(carriers) != 1 {
				t.Fatalf("pristine key must ride exactly one header, got %v", carriers)
			}
			actual, _ := fleetWireSubstituteKey(got, want, want)
			if !reflect.DeepEqual(expected, actual) {
				e, _ := json.MarshalIndent(expected, "", "  ")
				a, _ := json.MarshalIndent(actual, "", "  ")
				t.Fatalf("wire differs from pristine <BASE> beyond the routing key (carrier %v)\nwant %s\ngot  %s", carriers, e, a)
			}
		})
	}
}
