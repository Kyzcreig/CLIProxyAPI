package brandalias

import (
	"bytes"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Reasons a request was passed through (wirelog `reason`; "ok" = aliased,
// "shadow" = aliased in shadow, counts only).
const (
	ReasonOK                = "ok"
	ReasonShadow            = "shadow"
	ReasonDisabled          = "disabled"
	ReasonLaneNotEnabled    = "lane_not_enabled"
	ReasonPolicyException   = "policy_exception"
	ReasonUnsupportedFormat = "unsupported_source_format"
	ReasonInvalidJSON       = "invalid_json"
	ReasonWalkError         = "walk_error"
	ReasonDecodeError       = "decode_error"
	ReasonNoState           = "no_request_state"
)

const (
	maxRequestStates = 4096
	requestStateTTL  = 2 * time.Hour
)

// Decision is what the engine did with one request (tests and wirelog rows).
type Decision struct {
	Handled      bool
	Lane         string
	Mode         string
	Reason       string
	SourceFormat string
	BrandIn      int
	BrandOut     int
	Symbols      int
}

type requestState struct {
	mu      sync.Mutex
	lane    string
	format  string
	mode    string
	codec   *contentalias.WordCodec
	stream  *streamState
	created time.Time
}

// Engine holds the config and the per-request inverse maps. One Engine per
// plugin instance; every method is safe for concurrent use.
type Engine struct {
	mu     sync.RWMutex
	cfg    Config
	states map[string]*requestState
	log    *wirelog
}

// NewEngine builds an engine for cfg (already defaulted by ParseConfig).
func NewEngine(cfg Config) *Engine {
	return &Engine{cfg: cfg, states: map[string]*requestState{}, log: newWirelog(cfg.WirelogSpool)}
}

// Reconfigure swaps the config; in-flight request states keep their codec.
func (e *Engine) Reconfigure(cfg Config) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg = cfg
	e.log = newWirelog(cfg.WirelogSpool)
}

// Config returns the active config.
func (e *Engine) Config() Config {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg
}

func (e *Engine) newCodec(cfg Config) (*contentalias.WordCodec, error) {
	return contentalias.NewWordCodec(contentalias.Binding{Principal: cfg.Principal, Session: cfg.Session, Version: "v1"}, contentalias.Manifest{Words: cfg.Words})
}

// InterceptRequest is the after-auth seam. The returned response is empty
// (pass-through) unless the lane is enabled, the source format has a walker,
// the mode is enabled and the walk succeeded; the Decision says which.
func (e *Engine) InterceptRequest(req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, Decision) {
	start := time.Now()
	cfg := e.Config()
	d := Decision{Mode: cfg.Mode, SourceFormat: strings.ToLower(strings.TrimSpace(req.SourceFormat))}
	defer func() { e.log.request(req, d, time.Since(start)) }()

	if !cfg.Enabled {
		d.Reason = ReasonDisabled
		return pluginapi.RequestInterceptResponse{}, d
	}
	lane, reason := ResolveLane(req.ToFormat, req.Metadata)
	d.Lane = lane
	if lane == "" {
		d.Reason = reason
		return pluginapi.RequestInterceptResponse{}, d
	}
	if IsException(lane) {
		d.Reason = ReasonPolicyException
		return pluginapi.RequestInterceptResponse{}, d
	}
	if !cfg.LaneEnabled(lane) {
		d.Reason = ReasonLaneNotEnabled
		return pluginapi.RequestInterceptResponse{}, d
	}
	w := walkerFor(d.SourceFormat)
	if w == nil {
		d.Reason = ReasonUnsupportedFormat
		return pluginapi.RequestInterceptResponse{}, d
	}
	if !isJSONObject(req.Body) {
		d.Reason = ReasonInvalidJSON
		return pluginapi.RequestInterceptResponse{}, d
	}
	d.BrandIn = CountBrandTokens(req.Body, cfg.Words)

	st := e.state(req.RequestID, true, cfg)
	if st == nil {
		d.Reason = ReasonWalkError
		return pluginapi.RequestInterceptResponse{}, d
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.lane, st.format, st.mode = lane, d.SourceFormat, cfg.Mode
	wire, err := w.Request(bytes.Clone(req.Body), st.codec.Encode)
	if err != nil || !isJSONObject(wire) {
		d.Reason = ReasonWalkError
		return pluginapi.RequestInterceptResponse{}, d
	}
	d.BrandOut = CountBrandTokens(wire, cfg.Words)
	d.Symbols = st.codec.Symbols()
	if cfg.Mode != ModeEnabled {
		d.Reason = ReasonShadow
		return pluginapi.RequestInterceptResponse{}, d
	}
	d.Handled, d.Reason = true, ReasonOK
	if bytes.Equal(wire, req.Body) {
		// Nothing to alias: leave the host's body alone (Body non-empty would
		// still be a no-op, but an empty response is the documented pass).
		return pluginapi.RequestInterceptResponse{}, d
	}
	return pluginapi.RequestInterceptResponse{Body: wire}, d
}

// InterceptResponse restores a non-streaming response of a request this
// engine aliased. Any failure returns the body untouched (sentinels may leak
// inbound, never upstream).
func (e *Engine) InterceptResponse(req pluginapi.ResponseInterceptRequest) (pluginapi.ResponseInterceptResponse, Decision) {
	d := Decision{SourceFormat: strings.ToLower(strings.TrimSpace(req.SourceFormat))}
	st := e.state(req.RequestID, false, Config{})
	if st == nil {
		d.Reason = ReasonNoState
		return pluginapi.ResponseInterceptResponse{}, d
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	d.Lane, d.Mode = st.lane, st.mode
	if st.mode != ModeEnabled {
		d.Reason = ReasonShadow
		return pluginapi.ResponseInterceptResponse{}, d
	}
	w := walkerFor(d.SourceFormat)
	if w == nil {
		w = walkerFor(st.format)
	}
	if w == nil || !isJSONObject(req.Body) {
		d.Reason = ReasonUnsupportedFormat
		e.log.failure(req.RequestID, "response", d)
		return pluginapi.ResponseInterceptResponse{}, d
	}
	out, err := w.Response(bytes.Clone(req.Body), func(s string) (string, error) { return st.codec.Decode(s), nil })
	if err != nil || !isJSONObject(out) {
		d.Reason = ReasonDecodeError
		e.log.failure(req.RequestID, "response", d)
		return pluginapi.ResponseInterceptResponse{}, d
	}
	d.Handled, d.Reason = true, ReasonOK
	if bytes.Equal(out, req.Body) {
		return pluginapi.ResponseInterceptResponse{}, d
	}
	return pluginapi.ResponseInterceptResponse{Body: out}, d
}

// InterceptStreamChunk restores one stream chunk. The header-init call
// (ChunkIndex == StreamChunkHeaderInitIndex) is a no-op.
func (e *Engine) InterceptStreamChunk(req pluginapi.StreamChunkInterceptRequest) (pluginapi.StreamChunkInterceptResponse, Decision) {
	d := Decision{SourceFormat: strings.ToLower(strings.TrimSpace(req.SourceFormat))}
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex || len(req.Body) == 0 {
		d.Reason = ReasonOK
		return pluginapi.StreamChunkInterceptResponse{}, d
	}
	st := e.state(req.RequestID, false, Config{})
	if st == nil {
		d.Reason = ReasonNoState
		return pluginapi.StreamChunkInterceptResponse{}, d
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	d.Lane, d.Mode = st.lane, st.mode
	if st.mode != ModeEnabled {
		d.Reason = ReasonShadow
		return pluginapi.StreamChunkInterceptResponse{}, d
	}
	restore := streamRestorers[d.SourceFormat]
	if restore == nil {
		restore = streamRestorers[st.format]
	}
	if restore == nil {
		d.Reason = ReasonUnsupportedFormat
		e.log.failure(req.RequestID, "stream", d)
		return pluginapi.StreamChunkInterceptResponse{}, d
	}
	if st.stream == nil {
		st.stream = newStreamState(st.codec)
	}
	out, err := restore(bytes.Clone(req.Body), st.stream)
	if err != nil {
		d.Reason = ReasonDecodeError
		e.log.failure(req.RequestID, "stream", d)
		return pluginapi.StreamChunkInterceptResponse{}, d
	}
	d.Handled, d.Reason = true, ReasonOK
	if bytes.Equal(out, req.Body) {
		return pluginapi.StreamChunkInterceptResponse{}, d
	}
	return pluginapi.StreamChunkInterceptResponse{Body: out}, d
}

// Complete forgets a request's state (RequestLifecyclePlugin terminal event).
func (e *Engine) Complete(requestID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.states, requestID)
}

// state returns the request's state, creating it when create is set. Creation
// sweeps expired entries and refuses (nil) when the table is full, so a host
// that never delivers request.complete cannot grow it without bound.
func (e *Engine) state(requestID string, create bool, cfg Config) *requestState {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.states[requestID]; st != nil || !create {
		return st
	}
	now := time.Now()
	for id, st := range e.states {
		if now.Sub(st.created) > requestStateTTL {
			delete(e.states, id)
		}
	}
	if len(e.states) >= maxRequestStates {
		return nil
	}
	codec, err := e.newCodec(cfg)
	if err != nil {
		return nil
	}
	st := &requestState{codec: codec, created: now}
	e.states[requestID] = st
	return st
}

// Len is the number of live request states (tests).
func (e *Engine) Len() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.states)
}

func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && jsonValid(trimmed)
}
