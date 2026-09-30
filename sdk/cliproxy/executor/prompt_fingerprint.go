package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

// Prompt fingerprints: log-safe hashes of the prompt a request sends, so a usage sink can
// tell a CALLER that changed its cacheable prefix from a vendor-side cache miss (eviction,
// replica placement) without ever seeing prompt content.
//
//   - prefix: the stable head - tools, instructions / system, and the leading system or
//     developer messages. A change here invalidates every cached token after it.
//   - prompt: the head plus every conversation item, in order.
//   - parent: the head plus the items BEFORE the last assistant turn (the trailing run of
//     assistant output items and everything after it removed). When a caller only appends
//     (assistant output + new tool results / user message), a leg's parent equals the
//     previous leg's prompt: the prefix the vendor could have cached reached it unchanged.
//     Empty when the prompt has no assistant turn yet, or when the history lives upstream
//     (Responses previous_response_id), because then the proxy cannot see it.
//
// Every value is sha256 hex, 16 chars, over the compacted JSON of each part (key order as the
// client sent it). Content is never stored or logged, only these digests.
const (
	PromptPrefixFPMetadataKey = "prompt_prefix_fp"
	PromptFPMetadataKey       = "prompt_fp"
	PromptParentFPMetadataKey = "prompt_parent_fp"
)

// PromptFingerprints is the outcome of ComputePromptFingerprints; empty fields are unknown.
type PromptFingerprints struct {
	Prefix string
	Prompt string
	Parent string
}

// ComputePromptFingerprints fingerprints a SOURCE-format request body (OpenAI chat
// completions, OpenAI Responses or Anthropic messages). An empty or non-JSON payload, or one
// with no prompt at all, yields zero fingerprints.
func ComputePromptFingerprints(payload []byte) PromptFingerprints {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return PromptFingerprints{}
	}
	root := gjson.ParseBytes(payload)
	var items []gjson.Result
	switch in := root.Get("input"); {
	case root.Get("messages").IsArray():
		items = root.Get("messages").Array()
	case in.IsArray():
		items = in.Array()
	case in.Type == gjson.String:
		items = []gjson.Result{in}
	}
	head := sha256.New()
	headParts := 0
	for _, field := range []string{"tools", "instructions", "system"} {
		if v := root.Get(field); v.Exists() && v.Type != gjson.Null {
			writePart(head, field, v.Raw)
			headParts++
		}
	}
	lead := 0
	for lead < len(items) && isHeadItem(items[lead]) {
		writePart(head, "lead", items[lead].Raw)
		headParts++
		lead++
	}
	items = items[lead:]
	if headParts == 0 && len(items) == 0 {
		return PromptFingerprints{}
	}
	prefixSum := head.Sum(nil)
	out := PromptFingerprints{Prefix: hex.EncodeToString(prefixSum)[:16]}

	digests := make([][]byte, len(items))
	for i, it := range items {
		h := sha256.New()
		writePart(h, "item", it.Raw)
		digests[i] = h.Sum(nil)
	}
	out.Prompt = chainFP(prefixSum, digests)
	if root.Get("previous_response_id").String() != "" {
		return out
	}
	last := -1
	for i := len(items) - 1; i >= 0; i-- {
		if isAssistantItem(items[i]) {
			last = i
			break
		}
	}
	if last < 0 {
		return out
	}
	for last > 0 && isAssistantItem(items[last-1]) {
		last--
	}
	out.Parent = chainFP(prefixSum, digests[:last])
	return out
}

// ApplyPromptFingerprintMetadata records non-empty fingerprints in metadata (created when nil).
func ApplyPromptFingerprintMetadata(metadata map[string]any, fp PromptFingerprints) map[string]any {
	if fp == (PromptFingerprints{}) {
		return metadata
	}
	if metadata == nil {
		metadata = make(map[string]any, 3)
	}
	for key, value := range map[string]string{
		PromptPrefixFPMetadataKey: fp.Prefix,
		PromptFPMetadataKey:       fp.Prompt,
		PromptParentFPMetadataKey: fp.Parent,
	} {
		if value != "" {
			metadata[key] = value
		}
	}
	return metadata
}

// PromptFingerprintsFromMetadata returns the fingerprints recorded in metadata.
func PromptFingerprintsFromMetadata(metadata map[string]any) PromptFingerprints {
	return PromptFingerprints{
		Prefix: metadataString(metadata, PromptPrefixFPMetadataKey),
		Prompt: metadataString(metadata, PromptFPMetadataKey),
		Parent: metadataString(metadata, PromptParentFPMetadataKey),
	}
}

func chainFP(prefix []byte, digests [][]byte) string {
	h := sha256.New()
	h.Write(prefix)
	for _, d := range digests {
		h.Write(d)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// writePart writes a tagged, length-prefixed, compacted JSON part so part boundaries are unambiguous.
func writePart(h interface{ Write([]byte) (int, error) }, tag, raw string) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		buf.Reset()
		buf.WriteString(raw)
	}
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(tag)))
	h.Write(n[:])
	h.Write([]byte(tag))
	binary.BigEndian.PutUint64(n[:], uint64(buf.Len()))
	h.Write(n[:])
	h.Write(buf.Bytes())
}

func itemRole(it gjson.Result) string {
	return strings.ToLower(strings.TrimSpace(it.Get("role").String()))
}

// isHeadItem: a leading system / developer message belongs to the stable head.
func isHeadItem(it gjson.Result) bool {
	if !it.IsObject() {
		return false
	}
	role := itemRole(it)
	return role == "system" || role == "developer"
}

// isAssistantItem: model output echoed back by the client (chat assistant message, Responses
// assistant message / function_call / reasoning item).
func isAssistantItem(it gjson.Result) bool {
	if !it.IsObject() {
		return false
	}
	if itemRole(it) == "assistant" {
		return true
	}
	switch it.Get("type").String() {
	case "function_call", "custom_tool_call", "reasoning", "local_shell_call", "web_search_call":
		return true
	}
	return false
}
