package brandalias

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

var countSeparators = strings.NewReplacer("_", "", "-", "")

// CountBrandTokens counts manifest-word occurrences in body the way the codec
// matches them: case-folded, with '-' and '_' removed on both sides, so
// "Quor-Vane" and "openclaw" both count. It scans the WHOLE body (the DPX
// wirelog's brandTokens scans only Claude system/messages/tools), so a brand
// word left in a schema key or an unwalked field is visible, not hidden.
func CountBrandTokens(body []byte, words []string) int {
	if len(body) == 0 {
		return 0
	}
	scope := strings.ToLower(countSeparators.Replace(string(body)))
	hits := 0
	for _, w := range words {
		w = strings.ToLower(countSeparators.Replace(strings.TrimSpace(w)))
		if w != "" {
			hits += strings.Count(scope, w)
		}
	}
	return hits
}

func jsonValid(raw []byte) bool { return gjson.ValidBytes(raw) }

// wirelog appends digest-only rows (no content, no headers, no credential
// material) to a JSONL spool: one per intercepted request, one per
// response-side failure. A missing spool disables it.
type wirelog struct {
	mu   sync.Mutex
	path string
	seq  atomic.Int64
	host string
}

func newWirelog(path string) *wirelog {
	host, _ := os.Hostname()
	return &wirelog{path: strings.TrimSpace(path), host: host}
}

func (w *wirelog) enabled() bool { return w != nil && w.path != "" }

func (w *wirelog) request(req pluginapi.RequestInterceptRequest, d Decision, dur time.Duration) {
	if !w.enabled() {
		return
	}
	w.write(map[string]any{
		"v": 2, "capture": "cpa-alias", "id": w.rowID(), "host": w.host,
		"ts":            time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"lane":          laneLabel(d.Lane),
		"mode":          d.Mode,
		"reqId":         req.RequestID,
		"sourceFormat":  d.SourceFormat,
		"toFormat":      req.ToFormat,
		"stream":        req.Stream,
		"handled":       d.Handled,
		"reason":        d.Reason,
		"bytes":         len(req.Body),
		"brandTokensIn": d.BrandIn,
		"brandTokens":   d.BrandOut,
		"symbols":       d.Symbols,
		"durationMs":    dur.Milliseconds(),
	})
}

func (w *wirelog) failure(requestID, phase string, d Decision) {
	if !w.enabled() {
		return
	}
	w.write(map[string]any{
		"v": 2, "capture": "cpa-alias", "id": w.rowID(), "host": w.host,
		"ts":     time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"lane":   laneLabel(d.Lane),
		"mode":   d.Mode,
		"reqId":  requestID,
		"phase":  phase,
		"reason": d.Reason,
	})
}

func (w *wirelog) write(row map[string]any) {
	line, err := json.Marshal(row)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

func (w *wirelog) rowID() string {
	return strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(w.seq.Add(1), 10)
}

func laneLabel(lane string) string {
	if lane == "" {
		return "cpa-unresolved"
	}
	return "cpa-" + lane
}
