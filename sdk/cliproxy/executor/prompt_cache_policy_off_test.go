package executor

import "testing"

// routing.prompt-cache-policy: absent/empty and "off" are the zero value (skip the policy);
// enforce and shadow are explicit; anything else runs as shadow and is reported unknown.
func TestPromptCachePolicyModeFromConfig(t *testing.T) {
	cases := []struct {
		raw     string
		mode    string
		unknown bool
	}{
		{"", PromptCacheKeyModeOff, false},
		{"   ", PromptCacheKeyModeOff, false},
		{"off", PromptCacheKeyModeOff, false},
		{" OFF ", PromptCacheKeyModeOff, false},
		{"shadow", PromptCacheKeyModeShadow, false},
		{" Shadow ", PromptCacheKeyModeShadow, false},
		{"enforce", PromptCacheKeyModeEnforce, false},
		{"ENFORCE", PromptCacheKeyModeEnforce, false},
		{"of", PromptCacheKeyModeShadow, true},
		{"enforced", PromptCacheKeyModeShadow, true},
		{"bogus", PromptCacheKeyModeShadow, true},
		{"false", PromptCacheKeyModeShadow, true},
	}
	for _, tc := range cases {
		mode, unknown := PromptCachePolicyModeFromConfig(tc.raw)
		if mode != tc.mode || unknown != tc.unknown {
			t.Fatalf("PromptCachePolicyModeFromConfig(%q) = (%q, %v), want (%q, %v)", tc.raw, mode, unknown, tc.mode, tc.unknown)
		}
		if PromptCacheKeyModeUnknown(tc.raw) != tc.unknown {
			t.Fatalf("PromptCacheKeyModeUnknown(%q) = %v, want %v", tc.raw, !tc.unknown, tc.unknown)
		}
	}
}

// A content-alias daemon accepts only the literal "off" (spec D7 DPX rule): absent, typos,
// case/whitespace variants and explicit shadow/enforce all refuse.
func TestPromptCachePolicyAliasSafe(t *testing.T) {
	if !PromptCachePolicyAliasSafe("off") {
		t.Fatal(`literal "off" must be alias-safe`)
	}
	for _, raw := range []string{"", " ", "of", "Off", "OFF", "off ", " off", "shadow", "shadow ", "Shadow", "enforce", "bogus"} {
		if PromptCachePolicyAliasSafe(raw) {
			t.Fatalf("%q must not be alias-safe", raw)
		}
	}
}
