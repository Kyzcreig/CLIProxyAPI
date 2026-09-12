package contentalias

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestDPXAliasResourceMeasurement(t *testing.T) {
	if os.Getenv("DPX_RESOURCE_GATE") != "1" {
		t.Skip("run separately with DPX_RESOURCE_GATE=1; timing under race instrumentation is not comparable")
	}
	tools := make([]any, 100)
	for i := range tools {
		tools[i] = map[string]any{"name": fmt.Sprintf("Tool%d", i), "description": "Hermes sandbox tool", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]string{"type": "string"}}, "required": []string{"command"}}}
	}
	fixture := map[string]any{"tools": tools, "system": "", "messages": []any{map[string]string{"role": "user", "content": "Hermes"}}}
	base, _ := json.Marshal(fixture)
	size := (1 << 20) - len(base)
	unit := "ordinary prose Hermes OpenClaw. "
	fixture["system"] = (strings.Repeat(unit, size/len(unit)+1))[:size]
	raw, _ := json.Marshal(fixture)
	if len(raw) != 1<<20 {
		t.Fatalf("fixture bytes=%d", len(raw))
	}
	s := testSession(t)
	if _, _, err := Prepare(raw, s); err != nil {
		t.Fatal(err)
	}
	samples := make([]float64, 31)
	var maximum uint64
	for i := range samples {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		if _, _, err := Prepare(raw, s); err != nil {
			t.Fatal(err)
		}
		samples[i] = float64(time.Since(start).Nanoseconds()) / 1e6
		runtime.ReadMemStats(&after)
		if after.TotalAlloc-before.TotalAlloc > maximum {
			maximum = after.TotalAlloc - before.TotalAlloc
		}
	}
	sort.Float64s(samples)
	p95 := samples[29]
	t.Logf("request_bytes=%d schemas=100 iterations=%d p95_ms=%.3f max_allocated_bytes=%d host=%s/%s GOMAXPROCS=%d", len(raw), len(samples), p95, maximum, runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0))
	if p95 >= 50 {
		t.Fatalf("added codec p95 %.3fms exceeds target 50ms", p95)
	}
}
