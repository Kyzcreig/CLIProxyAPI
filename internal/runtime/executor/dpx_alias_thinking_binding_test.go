package executor

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// A preserved-thinking model (here claude-haiku-5-5) with adaptive thinking and a
// signed thinking block in history gains thinking.block_binding from CPA. On the
// alias route that member must be added BEFORE aliasing; added after, the byte
// guard refused every such turn with 400 contentalias:post_alias_mutation
// (Studio dlr relay 2026-10-19 15:01, sub-vps-15; card t_ea3a4a0b).
const dpxSignedThinkingTurn = `[{"role":"user","content":"Read sandbox"},` +
	`{"role":"assistant","content":[{"type":"thinking","thinking":"earlier reasoning","signature":"` + preservedThinkingTestSignature + `"},{"type":"text","text":"ok"}]},` +
	`{"role":"user","content":"again"}]`

func TestDPXAliasPreservedThinkingBindingNotRefused(t *testing.T) {
	for _, stream := range []bool{false, true} {
		run := runDPXSignedThinking(t, stream)
		assertDPXSignedCapture(t, run)
		if got := gjson.GetBytes(run.body, "thinking.block_binding.prefix_mismatch_behavior").String(); got != "drop_block" {
			t.Fatalf("stream=%v: block_binding = %q, want drop_block; body=%s", stream, got, run.body)
		}
	}
}

func runDPXSignedThinking(t *testing.T, stream bool) dpxSignedRun {
	t.Helper()
	return runDPXSignedWith(t, dpxStockBlockNoCC, stream, dpxSignedThinkingTurn, func(p string) string {
		p = strings.Replace(p, `"model":"claude-sonnet-4-6"`, `"model":"claude-haiku-5-5","thinking":{"type":"adaptive"}`, 1)
		return p
	}, "claude-haiku-5-5")
}
