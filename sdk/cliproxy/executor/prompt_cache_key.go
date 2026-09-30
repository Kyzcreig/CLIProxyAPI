package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// Prompt-cache-key policy: the proxy is the one place every client of an OpenAI-style
// upstream passes through, so it enforces prefix-cache discipline for callers that send
// no routing key, and leaves callers that do send one untouched.
//
// Resolution (first hit wins):
//  1. passthrough - the client opted out (X-Prompt-Cache-Policy: passthrough, or an
//     explicit empty prompt_cache_key); nothing is derived, attached or pinned. Whatever
//     the client sent still goes upstream as sent.
//  2. caller      - the client sent a body prompt_cache_key, or a vendor-NATIVE routing header
//     (codex Session_id / Session-Id, xAI x-grok-conv-id): forwarded byte for byte, affinity
//     pins on the same value. The upstream sees exactly the routing state the client chose.
//  3. session     - the client sent a session id that is not a vendor cache key (execution
//     session, Claude Code session, X-Session-ID, Conversation_id, conversation ids, a bare
//     metadata.user_id). Affinity pins on it; the WIRE key is a hash of it, so a
//     user/account-bearing identifier is never forwarded to a vendor as a cache key.
//  4. derived     - "pck-" + sha256(canonical stable prefix)[:32], where the prefix is the
//     requested model, the system/developer text, the tool definitions and the first
//     4 KiB of the first user message. Requests that share those bytes share a key, so the
//     upstream routes them to one machine and the auth selector pins them to one account.
//
// Mode (routing.prompt-cache-policy): "enforce" attaches caller/session/derived keys to the
// wire and feeds the selector; "shadow" (the DEFAULT, and what any unset, empty or
// misspelled value resolves to) only labels the request (usage sinks get cache_key_source /
// cache_key_id) while the wire and the selector behave as before the policy existed. Shadow
// is the A/B control arm and the runtime kill switch: a typo in the kill switch can never
// fail into enforce. Enforce is an explicit opt-in per host.
const (
	// PromptCacheKeyMetadataKey carries the wire routing key in Options.Metadata (enforce
	// mode only): the caller's own key for source caller (so affinity pins on the same key
	// the upstream routes by), the hashed session id for session, the prefix hash for derived.
	PromptCacheKeyMetadataKey = "prompt_cache_key"
	// PromptCacheKeySourceMetadataKey carries the resolution source.
	PromptCacheKeySourceMetadataKey = "prompt_cache_key_source"
	// PromptCacheKeyIDMetadataKey carries sha256(wire key)[:16] for usage sinks. Never the key.
	PromptCacheKeyIDMetadataKey = "prompt_cache_key_id"
	// PromptCacheKeyModeMetadataKey carries the policy mode the request was resolved under.
	PromptCacheKeyModeMetadataKey = "prompt_cache_key_mode"

	// PromptCachePolicyHeader lets a caller opt out of derivation.
	PromptCachePolicyHeader = "X-Prompt-Cache-Policy"
	// PromptCachePolicyPassthrough is the opt-out value.
	PromptCachePolicyPassthrough = "passthrough"

	PromptCacheKeySourceCaller      = "caller"
	PromptCacheKeySourceSession     = "session"
	PromptCacheKeySourceDerived     = "derived"
	PromptCacheKeySourcePassthrough = "passthrough"

	PromptCacheKeyModeEnforce = "enforce"
	PromptCacheKeyModeShadow  = "shadow"

	promptCacheKeyPrefix         = "pck-"
	promptCacheFirstUserHeadSize = 4096
)

// PromptCacheKeyResolution is the outcome of ResolvePromptCacheKey.
type PromptCacheKeyResolution struct {
	// Source is caller, derived or passthrough.
	Source string
	// Key is the routing key: the caller's own for "caller", the derived one for "derived",
	// empty for "passthrough".
	Key string
	// ID is sha256(Key)[:16] (hex), empty when Key is empty.
	ID string
}

var claudeCodeSessionPattern = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// ResolvePromptCacheKey decides the routing key for one request. provider is the executor
// identity the manager resolved (codex, xai, kimi, ...); payload is the SOURCE-format body;
// headers and metadata are the inbound request's.
func ResolvePromptCacheKey(provider string, payload []byte, headers http.Header, metadata map[string]any) PromptCacheKeyResolution {
	if promptCachePassthrough(payload, headers) {
		return PromptCacheKeyResolution{Source: PromptCacheKeySourcePassthrough}
	}
	if len(payload) > 0 {
		if v := gjson.GetBytes(payload, "prompt_cache_key"); v.Exists() && strings.TrimSpace(v.String()) != "" {
			key := strings.TrimSpace(v.String())
			return PromptCacheKeyResolution{Source: PromptCacheKeySourceCaller, Key: key, ID: PromptCacheKeyID(key)}
		}
	}
	if key := nativeRoutingHeader(headers); key != "" {
		return PromptCacheKeyResolution{Source: PromptCacheKeySourceCaller, Key: key, ID: PromptCacheKeyID(key)}
	}
	if sessionID, ok := callerSessionID(payload, headers, metadata); ok {
		key := hashedPromptCacheKey("session", sessionID)
		return PromptCacheKeyResolution{Source: PromptCacheKeySourceSession, Key: key, ID: PromptCacheKeyID(key)}
	}
	key := DerivePromptCacheKey(provider, payload)
	if key == "" {
		return PromptCacheKeyResolution{Source: PromptCacheKeySourcePassthrough}
	}
	return PromptCacheKeyResolution{Source: PromptCacheKeySourceDerived, Key: key, ID: PromptCacheKeyID(key)}
}

// PromptCacheKeyID is the log-safe identity of a wire key: sha256 hex, 16 chars.
func PromptCacheKeyID(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

// ApplyPromptCacheKeyMetadata records a resolution in Options.Metadata and returns the map.
// The map is updated in place (created when nil): the request pipeline publishes the
// selected auth id into the same map and handlers may hold a reference to it. The wire
// key is recorded only in enforce mode; shadow mode records labels alone.
func ApplyPromptCacheKeyMetadata(metadata map[string]any, res PromptCacheKeyResolution, mode string) map[string]any {
	out := metadata
	if out == nil {
		out = make(map[string]any, 4)
	}
	if res.Source == "" {
		res.Source = PromptCacheKeySourcePassthrough
	}
	mode = NormalizePromptCacheKeyMode(mode)
	out[PromptCacheKeySourceMetadataKey] = res.Source
	out[PromptCacheKeyModeMetadataKey] = mode
	if res.ID != "" {
		out[PromptCacheKeyIDMetadataKey] = res.ID
	}
	if mode == PromptCacheKeyModeEnforce && res.Key != "" && res.Source != PromptCacheKeySourcePassthrough {
		out[PromptCacheKeyMetadataKey] = res.Key
	}
	return out
}

// NormalizePromptCacheKeyMode maps a config value to enforce or shadow. Only the exact word
// "enforce" (case-insensitive) enables enforcement; unset, empty and unknown values are
// shadow, so a misspelled kill switch never fails into enforce. Unknown values are reported
// by PromptCacheKeyModeUnknown for the config loader to warn about.
func NormalizePromptCacheKeyMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), PromptCacheKeyModeEnforce) {
		return PromptCacheKeyModeEnforce
	}
	return PromptCacheKeyModeShadow
}

// PromptCacheKeyModeUnknown reports a non-empty mode value that is neither enforce nor shadow.
func PromptCacheKeyModeUnknown(mode string) bool {
	m := strings.ToLower(strings.TrimSpace(mode))
	return m != "" && m != PromptCacheKeyModeEnforce && m != PromptCacheKeyModeShadow
}

// PromptCacheKeyModeFromMetadata returns the recorded mode, or "" when unresolved.
func PromptCacheKeyModeFromMetadata(metadata map[string]any) string {
	return metadataString(metadata, PromptCacheKeyModeMetadataKey)
}

// PromptCacheKeyEnforced reports whether the request was resolved under enforce mode.
func PromptCacheKeyEnforced(metadata map[string]any) bool {
	return PromptCacheKeyModeFromMetadata(metadata) == PromptCacheKeyModeEnforce
}

// WirePromptCacheKeyFromMetadata returns the wire key recorded by
// ApplyPromptCacheKeyMetadata (sources caller, session and derived; enforce mode), or ""
// when the request was passthrough or resolved in shadow mode. For source caller it is
// the caller's own body key verbatim.
func WirePromptCacheKeyFromMetadata(metadata map[string]any) string {
	return metadataString(metadata, PromptCacheKeyMetadataKey)
}

// PromptCacheKeySourceFromMetadata returns the recorded source, or "" when unresolved.
func PromptCacheKeySourceFromMetadata(metadata map[string]any) string {
	return metadataString(metadata, PromptCacheKeySourceMetadataKey)
}

// PromptCacheKeyIDFromMetadata returns the recorded key id, or "".
func PromptCacheKeyIDFromMetadata(metadata map[string]any) string {
	return metadataString(metadata, PromptCacheKeyIDMetadataKey)
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

// nativeRoutingHeaderNames are headers the vendors themselves route by. A value here is
// forwarded verbatim (source caller), never hashed: hashing would rewrite the routing state
// the client chose and force a one-time miss.
var nativeRoutingHeaderNames = []string{"Session_id", "Session-Id", "X-Grok-Conv-Id"}

func nativeRoutingHeader(headers http.Header) string {
	if headers == nil {
		return ""
	}
	for _, name := range nativeRoutingHeaderNames {
		if v := strings.TrimSpace(headers.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// callerSessionID returns a session identifier the client already sent, in the precedence
// the session-affinity selector uses for the same fields, so "session" here and the
// selector's own extraction agree. Per-request ids (X-Client-Request-Id) are not sessions.
func callerSessionID(payload []byte, headers http.Header, metadata map[string]any) (string, bool) {
	if v := metadataString(metadata, ExecutionSessionMetadataKey); v != "" {
		return v, true
	}
	if len(payload) > 0 {
		if userID := gjson.GetBytes(payload, "metadata.user_id").String(); userID != "" {
			if m := claudeCodeSessionPattern.FindStringSubmatch(userID); len(m) >= 2 {
				return m[1], true
			}
			if userID[0] == '{' {
				if sid := gjson.Get(userID, "session_id").String(); sid != "" {
					return sid, true
				}
			}
		}
	}
	if headers != nil {
		for _, name := range []string{"X-Session-ID", "Conversation_id"} {
			if v := strings.TrimSpace(headers.Get(name)); v != "" {
				return v, true
			}
		}
	}
	if len(payload) > 0 {
		if userID := strings.TrimSpace(gjson.GetBytes(payload, "metadata.user_id").String()); userID != "" {
			return userID, true
		}
		if conv := strings.TrimSpace(gjson.GetBytes(payload, "conversation_id").String()); conv != "" {
			return conv, true
		}
		// Responses API conversation object: "conversation": "conv_x" or {"id": "conv_x"}.
		if conv := gjson.GetBytes(payload, "conversation"); conv.Exists() {
			if conv.Type == gjson.String && strings.TrimSpace(conv.String()) != "" {
				return strings.TrimSpace(conv.String()), true
			}
			if id := strings.TrimSpace(conv.Get("id").String()); id != "" {
				return id, true
			}
		}
	}
	return "", false
}

// hashedPromptCacheKey turns a client identifier into a wire-safe routing key.
func hashedPromptCacheKey(kind, id string) string {
	h := sha256.Sum256([]byte(kind + ":" + id))
	return promptCacheKeyPrefix + hex.EncodeToString(h[:])[:32]
}

func promptCachePassthrough(payload []byte, headers http.Header) bool {
	if headers != nil && strings.EqualFold(strings.TrimSpace(headers.Get(PromptCachePolicyHeader)), PromptCachePolicyPassthrough) {
		return true
	}
	if len(payload) > 0 {
		if v := gjson.GetBytes(payload, "prompt_cache_key"); v.Exists() && strings.TrimSpace(v.String()) == "" {
			return true
		}
	}
	return false
}

// DerivePromptCacheKey hashes the stable prefix of a source-format request. It returns ""
// when the payload carries no prompt at all (nothing to share), so callers fall back to
// passthrough instead of pinning unrelated requests together.
func DerivePromptCacheKey(provider string, payload []byte) string {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	model := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	system, tools, firstUser := promptCachePrefixParts(payload)
	if system == "" && tools == "" && firstUser == "" {
		return ""
	}
	h := sha256.New()
	for _, part := range []string{strings.TrimSpace(provider), model, system, tools, firstUser} {
		if part == "" {
			part = "-"
		}
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return promptCacheKeyPrefix + hex.EncodeToString(h.Sum(nil))[:32]
}

// promptCachePrefixParts extracts (system text, canonical tools JSON, first-user head) from
// chat/completions, Responses, Claude and Gemini shaped bodies.
func promptCachePrefixParts(payload []byte) (system, tools, firstUser string) {
	var systemParts []string
	addSystem := func(text string) {
		if text = strings.TrimSpace(text); text != "" {
			systemParts = append(systemParts, text)
		}
	}

	// Responses API: instructions + input[] ; chat: messages[] ; Gemini: contents[].
	if v := gjson.GetBytes(payload, "instructions"); v.Type == gjson.String {
		addSystem(v.String())
	}
	for _, listKey := range []string{"messages", "input", "contents"} {
		list := gjson.GetBytes(payload, listKey)
		if listKey == "input" && list.Type == gjson.String {
			// Responses API shorthand: input is the user text.
			if firstUser == "" {
				firstUser = headOf(list.String())
			}
			continue
		}
		if !list.IsArray() {
			continue
		}
		list.ForEach(func(_, item gjson.Result) bool {
			role := item.Get("role").String()
			content := item.Get("content")
			if !content.Exists() {
				content = item.Get("parts")
			}
			text := promptCacheContentText(content)
			switch role {
			case "system", "developer":
				addSystem(text)
			case "user":
				if firstUser == "" && text != "" {
					firstUser = headOf(text)
				}
			}
			return true
		})
	}
	// Claude: top-level system (string or blocks). Gemini: systemInstruction.parts.
	if v := gjson.GetBytes(payload, "system"); v.Exists() {
		addSystem(promptCacheContentText(v))
	}
	if v := gjson.GetBytes(payload, "systemInstruction.parts"); v.IsArray() {
		addSystem(promptCacheContentText(v))
	}
	if v := gjson.GetBytes(payload, "system_instruction.parts"); v.IsArray() {
		addSystem(promptCacheContentText(v))
	}
	system = strings.Join(systemParts, "\n")

	if v := gjson.GetBytes(payload, "tools"); v.IsArray() && len(v.Array()) > 0 {
		tools = canonicalJSON(v.Raw)
	}
	return system, tools, firstUser
}

// promptCacheContentText flattens a content value (string, or an array of text parts) to text.
func promptCacheContentText(content gjson.Result) string {
	switch {
	case content.Type == gjson.String:
		return content.String()
	case content.IsArray():
		var b strings.Builder
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Type == gjson.String {
				b.WriteString(part.String())
				return true
			}
			if t := part.Get("text"); t.Type == gjson.String {
				b.WriteString(t.String())
			}
			return true
		})
		return b.String()
	case content.IsObject():
		if t := content.Get("text"); t.Type == gjson.String {
			return t.String()
		}
	}
	return ""
}

func headOf(text string) string {
	if len(text) > promptCacheFirstUserHeadSize {
		return text[:promptCacheFirstUserHeadSize]
	}
	return text
}

// canonicalJSON re-encodes raw JSON with sorted object keys and no insignificant
// whitespace, so two encodings of one tool list hash the same. Numbers are kept as their
// literal digits (no float64 round trip) and <, > and & are not escaped, so the canonical
// form of a schema with large integers or HTML in a description is its own bytes.
func canonicalJSON(raw string) string {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return strings.TrimSpace(raw)
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return strings.TrimSpace(raw)
	}
	return strings.TrimSpace(buf.String())
}
