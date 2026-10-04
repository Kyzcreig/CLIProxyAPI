package contentalias

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Synthetic request bodies for the performance gates in lockhold_test.go.
// Everything is generated in-test from neutral prose: no recorded traffic.
// The shape matches a large agent turn (the size class that held the store
// lock for seconds in t_129cf1ac): two big system blocks, fixtureTools tool
// declarations with nested JSON schemas, and fixtureMessages history messages
// alternating tool_use and tool_result. A manifest word and a codec literal
// are placed every literalEvery bytes of prose, so encode and decode both do
// real alias work.

const (
	fixtureTools    = 60
	fixtureMessages = 60
	literalEvery    = 2048
)

// Sizes the gates run at. The 450 KB body is the one budgets are asserted on.
var fixtureSizes = []int{50 << 10, 200 << 10, 450 << 10}

const proseUnit = "The build step reads the configuration, checks each input file, " +
	"and writes a short report with counts, paths like /src/app/main.go and " +
	"numbers such as 4096 or 12.5 before moving on to the next stage. "

func codecPrefix() string {
	p, _ := codecPattern.LiteralPrefix()
	return p
}

// prose returns n bytes of neutral text with a manifest word and a codec
// literal at offset 0 and every literalEvery bytes, fixed by n and seed alone.
func prose(n, seed int) string {
	word := DefaultManifest().Words[0]
	var b strings.Builder
	b.Grow(n + 64)
	next := 0
	for i := 0; b.Len() < n; i++ {
		if b.Len() >= next {
			fmt.Fprintf(&b, " %s-agent %sfx_%d_%d ", word, codecPrefix(), seed, i)
			next += literalEvery
		}
		b.WriteString(proseUnit)
	}
	return b.String()[:n]
}

func fixtureTool(i, descBytes int) map[string]any {
	return map[string]any{
		"name":        fmt.Sprintf("tool_%02d", i),
		"description": prose(descBytes, 1000+i),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "what to run"},
				"path":    map[string]any{"type": "string"},
				"options": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"timeout": map[string]any{"type": "integer"},
						"labels":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				},
			},
			"required": []string{"command"},
		},
	}
}

// syntheticBody builds a request of about target bytes (within 1%).
func syntheticBody(t testing.TB, target int) []byte {
	t.Helper()
	// Tool descriptions take ~5% of the body, as in a large agent turn.
	tools := make([]map[string]any, fixtureTools)
	for i := range tools {
		tools[i] = fixtureTool(i, target/20/fixtureTools)
	}
	build := func(systemBytes, turnBytes int) []byte {
		messages := []any{map[string]any{"role": "user", "content": prose(turnBytes, 0)}}
		for i := 1; len(messages) < fixtureMessages; i++ {
			id := fmt.Sprintf("toolu_%04d", i)
			name := fmt.Sprintf("tool_%02d", i%fixtureTools)
			messages = append(messages,
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "text", "text": prose(turnBytes/2, 2000+i)},
					map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{
						"command": "run step " + DefaultManifest().Words[0], "path": "/src/app/main.go",
					}},
				}},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": id, "content": prose(turnBytes/2, 3000+i)},
				}})
		}
		body, err := json.Marshal(map[string]any{
			"model":      "sandbox",
			"max_tokens": 1024,
			"system": []any{
				map[string]any{"type": "text", "text": prose(systemBytes/2, 1)},
				map[string]any{"type": "text", "text": prose(systemBytes-systemBytes/2, 2)},
			},
			"tools":    tools,
			"messages": messages,
		})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	// Fixed parts first, then split the rest 40/60 between system and history.
	base := len(build(0, 0))
	rest := target - base
	if rest < 0 {
		t.Fatalf("target %d below the fixed shape (%d bytes)", target, base)
	}
	turn := rest * 6 / 10 / fixtureMessages
	body := build(0, turn)
	body = build(target-len(body), turn)
	return body
}

// The fixture must keep its size and shape, or the budgets stop meaning
// what they say.
func TestSyntheticFixtureShape(t *testing.T) {
	for _, size := range fixtureSizes {
		raw := syntheticBody(t, size)
		var shape struct {
			System   []json.RawMessage
			Messages []json.RawMessage
			Tools    []json.RawMessage
		}
		if err := json.Unmarshal(raw, &shape); err != nil {
			t.Fatal(err)
		}
		if d := len(raw) - size; d < -size/100 || d > size/100 {
			t.Fatalf("size %d: got %d bytes", size, len(raw))
		}
		if len(shape.Messages) < 50 || len(shape.Tools) < 50 {
			t.Fatalf("size %d: %d messages, %d tools", size, len(shape.Messages), len(shape.Tools))
		}
		if n := bytes.Count(raw, []byte(codecPrefix())); n < size/literalEvery/2 {
			t.Fatalf("size %d: %d codec literals, want >= %d", size, n, size/literalEvery/2)
		}
		if again := syntheticBody(t, size); !bytes.Equal(raw, again) {
			t.Fatalf("size %d: fixture is not deterministic", size)
		}
	}
}
