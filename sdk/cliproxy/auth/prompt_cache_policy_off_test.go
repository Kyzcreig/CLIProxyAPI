package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// policyMetadataKeys are every key withPromptCacheKeyPolicy may write.
var policyMetadataKeys = []string{
	cliproxyexecutor.PromptCacheKeySourceMetadataKey,
	cliproxyexecutor.PromptCacheKeyModeMetadataKey,
	cliproxyexecutor.PromptCacheKeyIDMetadataKey,
	cliproxyexecutor.PromptCacheKeyMetadataKey,
	cliproxyexecutor.PromptPrefixFPMetadataKey,
	cliproxyexecutor.PromptFPMetadataKey,
	cliproxyexecutor.PromptParentFPMetadataKey,
}

func TestWithPromptCacheKeyPolicy_OffWritesNothing(t *testing.T) {
	payload := []byte(`{"model":"gpt-5.4","instructions":"S","input":[{"role":"user","content":"u1"}]}`)
	cases := []struct {
		name     string
		cfg      *internalconfig.Config
		wantMode string
	}{
		{"nil-config", nil, cliproxyexecutor.PromptCacheKeyModeOff},
		{"absent", &internalconfig.Config{}, cliproxyexecutor.PromptCacheKeyModeOff},
		{"off", &internalconfig.Config{Routing: internalconfig.RoutingConfig{PromptCachePolicy: "off"}}, cliproxyexecutor.PromptCacheKeyModeOff},
		{"OFF", &internalconfig.Config{Routing: internalconfig.RoutingConfig{PromptCachePolicy: " OFF "}}, cliproxyexecutor.PromptCacheKeyModeOff},
		{"shadow", &internalconfig.Config{Routing: internalconfig.RoutingConfig{PromptCachePolicy: "shadow"}}, cliproxyexecutor.PromptCacheKeyModeShadow},
		{"enforce", &internalconfig.Config{Routing: internalconfig.RoutingConfig{PromptCachePolicy: "enforce"}}, cliproxyexecutor.PromptCacheKeyModeEnforce},
		{"unknown-runs-shadow", &internalconfig.Config{Routing: internalconfig.RoutingConfig{PromptCachePolicy: "of"}}, cliproxyexecutor.PromptCacheKeyModeShadow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, &RoundRobinSelector{}, nil)
			if tc.cfg != nil {
				m.SetConfig(tc.cfg)
			} else {
				// NewManager seeds an empty config; force the nil-snapshot fallback branch.
				m.runtimeConfig.Store((*internalconfig.Config)(nil))
			}
			if got := m.promptCachePolicyMode(); got != tc.wantMode {
				t.Fatalf("promptCachePolicyMode = %q, want %q", got, tc.wantMode)
			}
			meta := map[string]any{"keep": "me"}
			opts := m.withPromptCacheKeyPolicy([]string{"codex"}, cliproxyexecutor.Request{Payload: payload}, cliproxyexecutor.Options{Metadata: meta})
			if tc.wantMode == cliproxyexecutor.PromptCacheKeyModeOff {
				if len(opts.Metadata) != 1 || opts.Metadata["keep"] != "me" {
					t.Fatalf("off must leave metadata untouched, got %#v", opts.Metadata)
				}
				for _, k := range policyMetadataKeys {
					if _, ok := meta[k]; ok {
						t.Fatalf("off wrote %q into the caller's map", k)
					}
				}
				return
			}
			if opts.Metadata[cliproxyexecutor.PromptCacheKeySourceMetadataKey] == nil || opts.Metadata[cliproxyexecutor.PromptFPMetadataKey] == nil {
				t.Fatalf("%s must label and fingerprint, got %#v", tc.wantMode, opts.Metadata)
			}
			if got := cliproxyexecutor.PromptCacheKeyModeFromMetadata(opts.Metadata); got != tc.wantMode {
				t.Fatalf("recorded mode = %q, want %q", got, tc.wantMode)
			}
		})
	}
}

func TestPromptCachePolicyMode_NilManagerIsOff(t *testing.T) {
	var m *Manager
	if got := m.promptCachePolicyMode(); got != cliproxyexecutor.PromptCacheKeyModeOff {
		t.Fatalf("nil manager mode = %q, want off", got)
	}
}
