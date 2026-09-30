package executor

import (
	"regexp"
	"strings"
	"testing"
)

var fp16 = regexp.MustCompile(`^[0-9a-f]{16}$`)

const (
	chatLeg1 = `{"model":"gpt-6-sol","tools":[{"type":"function","function":{"name":"t"}}],"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"}]}`
	chatLeg2 = `{"model":"gpt-6-sol","tools":[{"type":"function","function":{"name":"t"}}],"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]},{"role":"tool","tool_call_id":"c1","content":"r1"}]}`
	chatLeg3 = `{"model":"gpt-6-sol","tools":[{"type":"function","function":{"name":"t"}}],"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]},{"role":"tool","tool_call_id":"c1","content":"r1"},{"role":"assistant","content":"a2"},{"role":"user","content":"u2"}]}`
)

func TestPromptFingerprintsChainAcrossAppendOnlyLegs(t *testing.T) {
	l1, l2, l3 := ComputePromptFingerprints([]byte(chatLeg1)), ComputePromptFingerprints([]byte(chatLeg2)), ComputePromptFingerprints([]byte(chatLeg3))
	for _, fp := range []PromptFingerprints{l1, l2, l3} {
		if !fp16.MatchString(fp.Prefix) || !fp16.MatchString(fp.Prompt) {
			t.Fatalf("fingerprints not 16 hex: %+v", fp)
		}
	}
	if l1.Parent != "" {
		t.Fatalf("leg 1 has no assistant turn, parent must be empty: %+v", l1)
	}
	if l1.Prefix != l2.Prefix || l2.Prefix != l3.Prefix {
		t.Fatalf("stable head changed across append-only legs: %s %s %s", l1.Prefix, l2.Prefix, l3.Prefix)
	}
	if l2.Parent != l1.Prompt || l3.Parent != l2.Prompt {
		t.Fatalf("append-only leg parent must equal the previous prompt: l1=%+v l2=%+v l3=%+v", l1, l2, l3)
	}
}

func TestPromptFingerprintsDetectRewrittenHeadAndHistory(t *testing.T) {
	l1 := ComputePromptFingerprints([]byte(chatLeg1))
	head := ComputePromptFingerprints([]byte(strings.Replace(chatLeg2, `"content":"S"`, `"content":"S at 12:01"`, 1)))
	if head.Prefix == l1.Prefix || head.Parent == l1.Prompt {
		t.Fatalf("a rewritten system prompt must change prefix and parent: %+v vs %+v", head, l1)
	}
	hist := ComputePromptFingerprints([]byte(strings.Replace(chatLeg2, `"content":"u1"`, `"content":"u1 (compacted)"`, 1)))
	if hist.Prefix != l1.Prefix || hist.Parent == l1.Prompt {
		t.Fatalf("a rewritten history must keep the head and break the parent: %+v vs %+v", hist, l1)
	}
}

func TestPromptFingerprintsIgnoreJSONWhitespace(t *testing.T) {
	a := ComputePromptFingerprints([]byte(chatLeg2))
	b := ComputePromptFingerprints([]byte(strings.ReplaceAll(strings.ReplaceAll(chatLeg2, ",", ", "), ":", ": ")))
	if a != b {
		t.Fatalf("whitespace-only reformat changed fingerprints: %+v vs %+v", a, b)
	}
}

func TestPromptFingerprintsResponsesAndClaudeShapes(t *testing.T) {
	r1 := ComputePromptFingerprints([]byte(`{"model":"gpt-6-sol","instructions":"I","input":[{"role":"user","content":"u1"}]}`))
	r2 := ComputePromptFingerprints([]byte(`{"model":"gpt-6-sol","instructions":"I","input":[{"role":"user","content":"u1"},{"type":"reasoning","id":"rs"},{"type":"function_call","call_id":"c"},{"type":"function_call_output","call_id":"c","output":"o"}]}`))
	if r2.Parent != r1.Prompt || r1.Prefix != r2.Prefix {
		t.Fatalf("responses append-only chain broken: %+v %+v", r1, r2)
	}
	prev := ComputePromptFingerprints([]byte(`{"model":"gpt-6-sol","previous_response_id":"resp_1","input":[{"role":"assistant","content":"a"},{"role":"user","content":"u"}]}`))
	if prev.Parent != "" || prev.Prompt == "" {
		t.Fatalf("history upstream (previous_response_id): parent unknown, prompt known: %+v", prev)
	}
	c1 := ComputePromptFingerprints([]byte(`{"model":"claude","system":[{"type":"text","text":"S"}],"messages":[{"role":"user","content":"u1"}]}`))
	c2 := ComputePromptFingerprints([]byte(`{"model":"claude","system":[{"type":"text","text":"S"}],"messages":[{"role":"user","content":"u1"},{"role":"assistant","content":[{"type":"tool_use","id":"t"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t"}]}]}`))
	if c2.Parent != c1.Prompt {
		t.Fatalf("claude append-only chain broken: %+v %+v", c1, c2)
	}
}

func TestPromptFingerprintsNeverCarryContentAndEmptyOnGarbage(t *testing.T) {
	for _, body := range []string{"", "not json", `{"model":"m"}`} {
		if fp := ComputePromptFingerprints([]byte(body)); fp != (PromptFingerprints{}) {
			t.Fatalf("%q: want zero fingerprints, got %+v", body, fp)
		}
	}
	fp := ComputePromptFingerprints([]byte(`{"messages":[{"role":"user","content":"CANARY-SECRET"}]}`))
	md := ApplyPromptFingerprintMetadata(nil, fp)
	for k, v := range md {
		if strings.Contains(v.(string), "CANARY") {
			t.Fatalf("metadata %s carries content", k)
		}
	}
	if got := PromptFingerprintsFromMetadata(md); got != fp {
		t.Fatalf("metadata roundtrip: %+v vs %+v", got, fp)
	}
}
