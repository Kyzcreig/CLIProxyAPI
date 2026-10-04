// Package brandalias is the cgo-free core of the dpx-alias plugin
// (examples/plugin/dpx-alias): brand-word aliasing for every cpa vendor lane at
// the RequestInterceptor.InterceptRequestAfterAuth seam, with the inverse map
// applied on the response and stream interceptors (card t_a37235c0).
//
// It reuses internal/contentalias's word codec (the DPX aliaser that already
// runs on the Claude native route) and adds per-source-format field walkers:
// text, tool names and tool descriptions are aliased; model ids, vendor
// fingerprint fields, headers, identity metadata and signed blocks are never
// touched. Every doubt is a pass-through: an unknown lane, an unsupported
// source format or any walk/decode error leaves the payload byte-identical and
// writes one wirelog row naming the reason. A vendor call is never refused.
package brandalias

import "strings"

// PluginName is the plugin id (config key under plugins.configs and the
// dpx-alias-v<ver>.<ext> file stem). It is one of the names the hermes-home
// lint cpa_brand_scrub_policy recognises as the fleet's own aliaser.
const PluginName = "dpx-alias"

// Policy as ruled by Ace 2026-10-03 20:40 PT (card t_8909e01d,
// hermes-home config/cpa-brand-scrub-policy.json). Lanes are the raw executor
// lanes a request resolves to (see ResolveLane); canonicalLane maps the policy
// aliases onto them for the exception / required checks.
var (
	// requiredOn lanes MUST be aliased; the lint reports them when the plugin
	// does not list them. The plugin itself only acts on configured Lanes.
	requiredOn = []string{"claude", "antigravity", "gemini"}
	// exceptions are OFF by ruling: the plugin refuses to alias them even when
	// a config lists them (policy_exception), so flipping the policy is a
	// change to this table, not to the walkers.
	exceptions = map[string]bool{"codex": true, "openai": true, "xai": true, "kimi": true}
	// laneAliases: policy spelling -> raw lane.
	laneAliases = map[string]string{
		"anthropic": "claude",
		"google":    "gemini",
		"chatgpt":   "codex",
		"grok":      "xai",
		"moonshot":  "kimi",
		"vertex":    "gemini",
		"aistudio":  "gemini",
	}
)

// Modes.
const (
	// ModeShadow computes the alias and the counts and leaves every payload
	// untouched (rollout step 1: >=24h of wirelog rows on the Studio).
	ModeShadow = "shadow"
	// ModeEnabled rewrites requests and restores responses.
	ModeEnabled = "enabled"
)

// DefaultWords is the manifest: the brand/harness/component names the apx
// bl-tokens corpus bans, restricted to the entries that are WORDS (the
// contentalias codec aliases a word run containing any of them, after
// case-folding and stripping '-'/'_'). Bare transport nouns ("proxy", "relay")
// and agent names are deliberately absent: a false positive on this forward-only
// seam corrupts the outbound question (standing ruling 2026-08-07), and the
// vendor name ("anthropic") is legitimate prose on every lane.
var DefaultWords = []string{
	"hermes",
	"openclaw",
	"nousresearch",
	"claude-apx",
	"claude-bpx",
	"claude-cpx",
	"claude-cpr",
	"claude-apr",
	"claude-bpr",
	"claude-bpp",
	"claude-pool",
	"claude-bridge",
	"claude-api-proxy",
}

// RequiredLanes returns a copy of the lanes the policy requires ON.
func RequiredLanes() []string { return append([]string(nil), requiredOn...) }

// IsException reports whether the policy forbids aliasing lane.
func IsException(lane string) bool { return exceptions[canonicalLane(lane)] }

func canonicalLane(lane string) string {
	lane = strings.ToLower(strings.TrimSpace(lane))
	if mapped, ok := laneAliases[lane]; ok {
		return mapped
	}
	return lane
}
