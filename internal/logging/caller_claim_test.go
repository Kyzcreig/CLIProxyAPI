package logging

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseCallerClaimAcceptsAllowlistedFields(t *testing.T) {
	h := http.Header{}
	h.Set(FleetCallerHeader, "harness=hermes;agent=daedalus-fable;platform=kanban;card=t_bc26568a;kind=aux;task=compression")
	claim := ParseCallerClaim(h)
	want := map[string]string{
		"harness": "hermes", "agent": "daedalus-fable", "platform": "kanban",
		"card": "t_bc26568a", "kind": "aux", "task": "compression",
	}
	if len(claim) != len(want) {
		t.Fatalf("claim = %v, want %v", claim, want)
	}
	for k, v := range want {
		if claim[k] != v {
			t.Fatalf("claim[%q] = %q, want %q", k, claim[k], v)
		}
	}
}

func TestParseCallerClaimDropsBadFieldsKeepsGood(t *testing.T) {
	h := http.Header{}
	// unknown key, URL value, space value, oversized value, secret-looking value, duplicate key
	h.Set(FleetCallerHeader, "harness=hermes;base_url=x;agent=http://evil;platform=dis cord;card=t_"+
		strings.Repeat("a", 70)+";session=sk-12345;harness=other;kind=main")
	claim := ParseCallerClaim(h)
	if claim["harness"] != "hermes" {
		t.Fatalf("first harness must win, got %q", claim["harness"])
	}
	if claim["kind"] != "main" {
		t.Fatalf("kind = %q", claim["kind"])
	}
	for _, k := range []string{"base_url", "agent", "platform", "card", "session"} {
		if _, ok := claim[k]; ok {
			t.Fatalf("field %q must be dropped, claim = %v", k, claim)
		}
	}
}

func TestParseCallerClaimFallsBackToHermesOrigin(t *testing.T) {
	h := http.Header{}
	h.Set(HermesOriginHeader, "agent=apollo;session=20260101_000000_abcd;platform=discord;cron=digest")
	claim := ParseCallerClaim(h)
	if claim["agent"] != "apollo" || claim["platform"] != "discord" || claim["cron"] != "digest" || claim["session"] != "20260101_000000_abcd" {
		t.Fatalf("claim = %v", claim)
	}
	h.Set(FleetCallerHeader, "harness=script")
	if claim = ParseCallerClaim(h); claim["harness"] != "script" || claim["agent"] != "" {
		t.Fatalf("X-Fleet-Caller must take precedence over X-Hermes-Origin, got %v", claim)
	}
}

func TestParseCallerClaimAbsentOrEmptyIsNil(t *testing.T) {
	if ParseCallerClaim(nil) != nil {
		t.Fatal("nil headers must yield nil")
	}
	h := http.Header{}
	if ParseCallerClaim(h) != nil {
		t.Fatal("absent header must yield nil")
	}
	h.Set(FleetCallerHeader, "garbage;;=;novalue")
	if ParseCallerClaim(h) != nil {
		t.Fatal("header with no valid field must yield nil")
	}
}
