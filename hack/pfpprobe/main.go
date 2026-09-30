// pfpprobe executes the prompt-fingerprint assertions as a plain program (fleet hosts deny the Go
// unit runner; CI runs prompt_fingerprint_test.go). It also carries the fingerprints through the
// conductor's usage context the way a live request does. Run: go run ./hack/pfpprobe
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

var fails int

func check(name string, ok bool, detail ...any) {
	if ok {
		fmt.Println("PASS", name)
		return
	}
	fails++
	fmt.Println("FAIL", name, fmt.Sprint(detail...))
}

const (
	leg1 = `{"model":"gpt-6-sol","tools":[{"type":"function","function":{"name":"t"}}],"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"}]}`
	leg2 = `{"model":"gpt-6-sol","tools":[{"type":"function","function":{"name":"t"}}],"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]},{"role":"tool","tool_call_id":"c1","content":"r1"}]}`
	leg3 = `{"model":"gpt-6-sol","tools":[{"type":"function","function":{"name":"t"}}],"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]},{"role":"tool","tool_call_id":"c1","content":"r1"},{"role":"assistant","content":"a2"},{"role":"user","content":"u2"}]}`
)

func fp(s string) cliproxyexecutor.PromptFingerprints {
	return cliproxyexecutor.ComputePromptFingerprints([]byte(s))
}

func main() {
	l1, l2, l3 := fp(leg1), fp(leg2), fp(leg3)
	check("16-hex fingerprints", len(l1.Prefix) == 16 && len(l2.Prompt) == 16 && len(l3.Parent) == 16, l1, l2, l3)
	check("leg 1 has no parent", l1.Parent == "", l1)
	check("stable head across legs", l1.Prefix == l2.Prefix && l2.Prefix == l3.Prefix)
	check("append-only: parent == previous prompt", l2.Parent == l1.Prompt && l3.Parent == l2.Prompt, l1, l2, l3)
	head := fp(strings.Replace(leg2, `"content":"S"`, `"content":"S at 12:01"`, 1))
	check("rewritten system prompt: prefix and parent change", head.Prefix != l1.Prefix && head.Parent != l1.Prompt, head)
	hist := fp(strings.Replace(leg2, `"content":"u1"`, `"content":"u1 (compacted)"`, 1))
	check("rewritten history: head kept, parent breaks", hist.Prefix == l1.Prefix && hist.Parent != l1.Prompt, hist)
	check("whitespace-only reformat is invisible", fp(strings.ReplaceAll(leg2, ",", ", ")) == l2)
	r1 := fp(`{"model":"gpt-6-sol","instructions":"I","input":[{"role":"user","content":"u1"}]}`)
	r2 := fp(`{"model":"gpt-6-sol","instructions":"I","input":[{"role":"user","content":"u1"},{"type":"reasoning","id":"rs"},{"type":"function_call","call_id":"c"},{"type":"function_call_output","call_id":"c","output":"o"}]}`)
	check("responses chain", r2.Parent == r1.Prompt && r1.Prefix == r2.Prefix, r1, r2)
	prev := fp(`{"model":"gpt-6-sol","previous_response_id":"resp_1","input":[{"role":"assistant","content":"a"},{"role":"user","content":"u"}]}`)
	check("previous_response_id: parent unknown", prev.Parent == "" && prev.Prompt != "", prev)
	c1 := fp(`{"model":"claude","system":[{"type":"text","text":"S"}],"messages":[{"role":"user","content":"u1"}]}`)
	c2 := fp(`{"model":"claude","system":[{"type":"text","text":"S"}],"messages":[{"role":"user","content":"u1"},{"role":"assistant","content":[{"type":"tool_use","id":"t"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t"}]}]}`)
	check("claude chain", c2.Parent == c1.Prompt, c1, c2)
	check("garbage -> zero", fp("") == (cliproxyexecutor.PromptFingerprints{}) && fp("x") == (cliproxyexecutor.PromptFingerprints{}) && fp(`{"model":"m"}`) == (cliproxyexecutor.PromptFingerprints{}))

	md := cliproxyexecutor.ApplyPromptFingerprintMetadata(nil, fp(`{"messages":[{"role":"user","content":"CANARY-SECRET"}]}`))
	leak := false
	for _, v := range md {
		leak = leak || strings.Contains(fmt.Sprint(v), "CANARY")
	}
	check("metadata carries no content", !leak && len(md) == 2, md)
	ctx := coreusage.WithPromptFingerprints(context.Background(), coreusage.PromptFingerprintInfo{Prefix: l2.Prefix, Prompt: l2.Prompt, Parent: l2.Parent})
	got := coreusage.PromptFingerprintsFromContext(ctx)
	check("usage context roundtrip", got.Prefix == l2.Prefix && got.Prompt == l2.Prompt && got.Parent == l2.Parent, got)

	if fails > 0 {
		fmt.Println("FAILURES:", fails)
		os.Exit(1)
	}
	fmt.Println("ALL PASS")
}
