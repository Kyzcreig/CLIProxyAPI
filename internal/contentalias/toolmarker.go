package contentalias

import "strconv"

// Near-copied tool aliases (t_7bd8c9b9). Tool aliases are dpx_v1_t_<24 hex>.
// The model mis-copies them at ~0.16% of tool calls (2026-10-25..27: 10 of
// 6,347): a 2-hex slip (...223191dce for ...223158dce) or a tail rewritten as a
// word (...648c7seed for ...648c7ce0). Refusing the response with unknown_tool
// failed the whole turn and reaped the caller's session.
//
// A response tool_use whose name is alias-SHAPED (strayClassPattern, the same
// net the property marker uses) but not declared by this request leaves DPX as
// unknownToolMarker with input {}. The caller has no tool of that name and
// answers with its own unknown-tool error, which names its tools (Hermes:
// "Tool 'unknown_tool_1' does not exist. Available tools: ..."). The model
// re-calls on the next step. There is no nearest-neighbour repair: two declared
// aliases can sit a few characters apart, and a guess could run the wrong tool.
//
// The marker is a plain tool-name token (^[a-zA-Z0-9_-]{1,64}$), unlike the
// property marker unknown_key_N (...): the caller stores it as the tool_use
// name in history, and a later hop that does not run through DPX (fallback to
// another lane) sends that history to Anthropic raw, which validates names.
//
// A name that is not alias-shaped (for example a raw "Bash") stays a hard
// unknown_tool refusal. The model only ever sees aliases, so a raw name is not
// a copy slip: it means original names reached the model, or a different
// fault. None occurred in the census, and a loud refusal keeps it visible.
//
// The marker contains no dpx_v1_ token, so the next request's history maps it
// with historyToolAlias like any other history-only name: a deterministic,
// non-callable alias, no history_tool refusal.
func unknownToolMarker(i int) string {
	return "unknown_tool_" + strconv.Itoa(i)
}
