package helps

// DPX wirelog (site W1, SPEC-modes §7): one digest-only v2 row per upstream
// request, describing the body that actually left (aliased, CPA-signed). Rows go
// to an append-only spool file inside the unit's RAM state directory; the
// Studio launcher relays them to ~/.claude-wirelog/dpx/<sub>/. No content, no
// credential value is written: bodies are reduced to counts, hashes and the
// billing block's fields, and credential-bearing headers are replaced by a
// fixed marker.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

// DPXWirelogConfig names the spool and the row labels.
type DPXWirelogConfig struct {
	Spool string
	Lane  string
	// Lanes, when set, labels each row with the lane its request resolves to
	// (ResolveDPXRequestLane) instead of the static Lane.
	Lanes      []string
	Sub        string
	BrandWords []string
}

var (
	dpxWirelogMu  sync.Mutex
	dpxWirelogSeq atomic.Int64
	dpxBillingRE  = regexp.MustCompile(`x-anthropic-billing-header:([^\n]*)`)
)

var dpxRedactedHeaders = map[string]bool{"authorization": true, "x-api-key": true, "cookie": true, "proxy-authorization": true}

// DPXPrepareTiming is the content-alias Prepare cost of one request: Total
// is the wall time of Open+Prepare, LockWait the part spent waiting for the
// unit's store lock (t_997bc8a2). It rides the request context to the row.
type DPXPrepareTiming struct{ Total, LockWait time.Duration }

type dpxPrepareTimingKey struct{}

// WithDPXPrepareTiming attaches t to ctx for the wirelog row of the request
// built from it.
func WithDPXPrepareTiming(ctx context.Context, t DPXPrepareTiming) context.Context {
	return context.WithValue(ctx, dpxPrepareTimingKey{}, t)
}

// DPXWirelogClient wraps client so every request it sends produces one row.
func DPXWirelogClient(client *http.Client, cfg DPXWirelogConfig) *http.Client {
	if client == nil || cfg.Spool == "" {
		return client
	}
	wrapped := *client
	base := wrapped.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	wrapped.Transport = dpxWirelogTransport{base: base, cfg: cfg}
	return &wrapped
}

type dpxWirelogTransport struct {
	base http.RoundTripper
	cfg  DPXWirelogConfig
}

func (t dpxWirelogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		raw, errRead := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if errRead != nil {
			return nil, errRead
		}
		body = raw
		req.Body = io.NopCloser(bytes.NewReader(raw))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	}
	start := time.Now()
	lane := t.cfg.Lane
	if len(t.cfg.Lanes) > 0 {
		// The gate already admitted this request on the inbound body; the body
		// that left carries the same billing block (DPX never writes it) and
		// CPA forwards the caller's UA. "unmatched" would mean they diverged.
		lane, _ = ResolveDPXRequestLane(t.cfg.Lanes, body, req.Header.Get("User-Agent"))
		if lane == "" {
			lane = "unmatched"
		}
	}
	row := map[string]any{
		"v": 2, "id": dpxRowID(), "lane": lane, "sub": t.cfg.Sub, "host": dpxHost(),
		"ts": start.UTC().Format("2006-01-02T15:04:05.000Z"), "capture": "dpx",
		"req": map[string]any{"method": req.Method, "path": req.URL.RequestURI(), "headers": dpxHeaders(req.Header), "body": DPXBodyDigest(body, t.cfg.BrandWords)},
	}
	if timing, ok := req.Context().Value(dpxPrepareTimingKey{}).(DPXPrepareTiming); ok {
		row["prepareMs"] = timing.Total.Milliseconds()
		row["lockWaitMs"] = timing.LockWait.Milliseconds()
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		// No response headers: the transport failed. `errorClass` is the NAMED
		// class (connect_refused, timeout, deadline_exceeded, tls, dns, ...);
		// `error` keeps the bounded prose for a reader that needs the detail.
		row["durationMs"] = time.Since(start).Milliseconds()
		row["res"] = map[string]any{"status": 0, "phase": "headers", "errorClass": DPXTransportErrorClass(err), "error": dpxErrorClass(err)}
		t.write(row)
		return resp, err
	}
	res := map[string]any{"status": resp.StatusCode, "headers": dpxHeaders(resp.Header)}
	stream := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	resp.Body = &dpxWirelogBody{ReadCloser: resp.Body, stream: stream, done: func(raw []byte, n int, end dpxBodyEnd) {
		decoded, errDecode := dpxDecode(raw, encoding)
		res["body"] = dpxResponseDigest(decoded, n, stream)
		if errDecode != nil {
			res["body"].(map[string]any)["usageErr"] = "decode:" + encoding
		}
		// Headers arrived but the body did not run to EOF: a mid-body stall the
		// executor's context cut (closed_before_eof) or a transport error while
		// streaming. Named here so a 200 row never passes for a complete response.
		if !end.complete {
			res["phase"] = "body"
			res["bodyComplete"] = false
			if end.err != nil {
				res["errorClass"] = DPXTransportErrorClass(end.err)
				res["error"] = dpxErrorClass(end.err)
			} else {
				res["errorClass"] = "closed_before_eof"
			}
		}
		row["res"] = res
		row["durationMs"] = time.Since(start).Milliseconds()
		t.write(row)
	}}
	return resp, nil
}

// dpxBodyEnd says how a response body ended: complete at EOF, cut by a read
// error, or closed by the caller before EOF (a stall or an abort).
type dpxBodyEnd struct {
	complete bool
	err      error
}

// DPXTransportErrorClass names the class of a transport-level failure (no bytes
// or an incomplete body from upstream). The vocabulary is fixed so a reader can
// group on it; the prose stays in the `error` field. Order matters: a context
// deadline wraps a net timeout on the Go client, and the deadline is the cause.
func DPXTransportErrorClass(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNREFUSED:
			return "connect_refused"
		case syscall.ECONNRESET:
			return "connection_reset"
		case syscall.EPIPE:
			return "broken_pipe"
		case syscall.EHOSTUNREACH, syscall.ENETUNREACH:
			return "unreachable"
		case syscall.ETIMEDOUT:
			return "timeout"
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var certErr *tls.CertificateVerificationError
	var alertErr tls.AlertError
	var recordErr tls.RecordHeaderError
	if errors.As(err, &certErr) || errors.As(err, &alertErr) || errors.As(err, &recordErr) || strings.Contains(err.Error(), "tls:") {
		return "tls"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "eof"
	}
	return "other"
}

func (t dpxWirelogTransport) write(row map[string]any) {
	line, err := json.Marshal(row)
	if err != nil {
		return
	}
	dpxWirelogMu.Lock()
	defer dpxWirelogMu.Unlock()
	f, err := os.OpenFile(t.cfg.Spool, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// dpxWirelogBody keeps at most 1 MiB of the response for the usage digest and
// fires done exactly once: complete at EOF, or incomplete on a read error or a
// Close before EOF (the executor's context cut a stalled body).
type dpxWirelogBody struct {
	io.ReadCloser
	stream bool
	buf    bytes.Buffer
	n      int
	once   sync.Once
	done   func([]byte, int, dpxBodyEnd)
}

func (b *dpxWirelogBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.n += n
		if b.buf.Len() < 1<<20 {
			b.buf.Write(p[:n])
		}
	}
	if err == io.EOF {
		b.finish(dpxBodyEnd{complete: true})
	} else if err != nil {
		b.finish(dpxBodyEnd{err: err})
	}
	return n, err
}

func (b *dpxWirelogBody) Close() error {
	b.finish(dpxBodyEnd{})
	return b.ReadCloser.Close()
}

func (b *dpxWirelogBody) finish(end dpxBodyEnd) {
	b.once.Do(func() { b.done(b.buf.Bytes(), b.n, end) })
}

func dpxHeaders(h http.Header) [][2]string {
	out := make([][2]string, 0, len(h))
	for name, values := range h {
		lower := strings.ToLower(name)
		for _, v := range values {
			if dpxRedactedHeaders[lower] {
				v = "<redacted>"
			}
			out = append(out, [2]string{lower, v})
		}
	}
	return out
}

func dpxHash(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

// DPXBodyDigest reduces a Messages body to the wirecap v2 digest fields plus
// brandTokens: case-insensitive occurrences of the alias manifest's words in
// system, messages and tools of the body that left (the corpus gate reads it).
func DPXBodyDigest(body []byte, brandWords []string) map[string]any {
	d := map[string]any{"bytes": len(body)}
	if len(body) == 0 {
		d["parseOk"], d["parseErr"] = false, "empty"
		return d
	}
	if !gjson.ValidBytes(body) {
		d["parseOk"], d["parseErr"] = false, "invalid_json"
		return d
	}
	root := gjson.ParseBytes(body)
	d["parseOk"] = true
	keys := []string{}
	root.ForEach(func(k, _ gjson.Result) bool { keys = append(keys, k.String()); return true })
	d["topLevelKeys"] = keys
	d["model"] = root.Get("model").String()
	d["stream"] = root.Get("stream").Bool()
	d["max_tokens"] = root.Get("max_tokens").Int()
	d["messages"] = len(root.Get("messages").Array())
	system := root.Get("system")
	d["systemBlocks"] = len(system.Array())
	if system.Type == gjson.String {
		d["systemBlocks"] = 1
	}
	d["billing"] = nil
	if m := dpxBillingRE.FindStringSubmatch(root.Get("system.0.text").String()); m != nil {
		billing := map[string]any{"raw_len": len(m[0])}
		for _, part := range strings.Split(m[1], ";") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) == 2 {
				billing[kv[0]] = kv[1]
			}
		}
		d["billing"] = billing
	}
	tools := root.Get("tools").Array()
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Get("name").String())
	}
	d["tools"] = len(tools)
	d["toolNames"] = names
	d["toolsHash"] = dpxHash([]byte(root.Get("tools").Raw))
	if uid := root.Get("metadata.user_id"); uid.Exists() {
		d["metadataUserIdHash"] = dpxHash([]byte(uid.String()))[:23]
	}
	scope := strings.ToLower(system.Raw + root.Get("messages").Raw + root.Get("tools").Raw)
	hits := 0
	for _, w := range brandWords {
		if w != "" {
			hits += strings.Count(scope, strings.ToLower(w))
		}
	}
	d["brandTokens"] = hits
	return d
}

func dpxResponseDigest(raw []byte, n int, stream bool) map[string]any {
	d := map[string]any{"bytes": n}
	if !stream {
		if gjson.ValidBytes(raw) {
			d["parseOk"] = true
			if u := gjson.GetBytes(raw, "usage"); u.Exists() {
				d["usage"] = json.RawMessage(u.Raw)
			}
		} else {
			d["parseOk"] = false
		}
		return d
	}
	d["parseOk"], d["parseSkip"] = false, "stream"
	usage := map[string]any{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		ev := gjson.ParseBytes(bytes.TrimSpace(line[5:]))
		var u gjson.Result
		switch ev.Get("type").String() {
		case "message_start":
			u = ev.Get("message.usage")
		case "message_delta":
			u = ev.Get("usage")
		}
		u.ForEach(func(k, v gjson.Result) bool { usage[k.String()] = json.RawMessage(v.Raw); return true })
	}
	if len(usage) > 0 {
		d["usage"] = usage
	}
	return d
}

// dpxDecode undoes the upstream Content-Encoding on the captured copy only; the
// caller's stream is untouched. A truncated capture (>1 MiB) decodes partially.
func dpxDecode(raw []byte, encoding string) ([]byte, error) {
	var r io.Reader
	switch encoding {
	case "", "identity":
		return raw, nil
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return raw, err
		}
		r = zr
	case "br":
		r = brotli.NewReader(bytes.NewReader(raw))
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return raw, err
		}
		defer zr.Close()
		r = zr
	default:
		return raw, io.ErrUnexpectedEOF
	}
	out, err := io.ReadAll(r)
	if len(out) > 0 {
		return out, nil
	}
	return raw, err
}

func dpxErrorClass(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func dpxHost() string { h, _ := os.Hostname(); return h }

func dpxRowID() string {
	return strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(dpxWirelogSeq.Add(1), 10)
}
