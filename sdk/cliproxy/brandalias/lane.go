package brandalias

import "strings"

// Metadata keys the host publishes on the intercept request (sdk/cliproxy/executor
// metadata keys; strings survive the plugin RPC sanitizer).
const (
	metaAffinityProvider = "session_affinity_provider"
)

// ResolveLane names the raw executor lane an after-auth request is bound for.
// ToFormat is the selected upstream protocol (set after credential selection);
// it is unambiguous for antigravity, claude and gemini. codex and openai
// ToFormats are shared by several providers, all of which are policy
// exceptions or relay loopbacks, so they resolve to "" with a reason unless the
// host's affinity-provider metadata names one. "" means pass-through.
func ResolveLane(toFormat string, metadata map[string]any) (lane, reason string) {
	if provider, ok := metadata[metaAffinityProvider].(string); ok {
		if p := canonicalLane(provider); p != "" {
			switch p {
			case "antigravity", "claude", "gemini", "codex", "xai", "kimi":
				return p, ""
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(toFormat)) {
	case "antigravity":
		return "antigravity", ""
	case "claude":
		return "claude", ""
	case "gemini":
		return "gemini", ""
	case "codex":
		// codex or xai: both OFF by policy.
		return "", "policy_exception"
	case "openai":
		// kimi (OFF) or an openai-compatibility target: the latter is relay
		// loopback already scrubbed by apx/bpx (one aliaser per lane).
		return "", "openai_target_skipped"
	case "":
		return "", "to_format_unset"
	}
	return "", "unknown_lane"
}
