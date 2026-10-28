package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// transientDropError mirrors the codex executor's incomplete-stream error: a
// request-scoped 408 that the upstream produced by closing the stream before
// response.completed. It carries the transient-upstream marker.
type transientDropError struct{ code int }

func (e transientDropError) Error() string {
	return "stream error: stream disconnected before completion: stream closed before response.completed"
}
func (e transientDropError) StatusCode() int         { return e.code }
func (transientDropError) IsRequestScoped() bool     { return true }
func (transientDropError) IsTransientUpstream() bool { return true }

// requestFaultError is a request-scoped client fault (no transient marker).
type requestFaultError struct{}

func (requestFaultError) Error() string         { return "invalid_request_error: bad field" }
func (requestFaultError) StatusCode() int       { return http.StatusBadRequest }
func (requestFaultError) IsRequestScoped() bool { return true }

type transientStreamMode int

const (
	transientStreamReturnErr  transientStreamMode = iota // ExecuteStream returns the error (bootstrap-buffered path)
	transientStreamErrChunk                              // first chunk is the error, zero payload bytes
	transientStreamAfterBytes                            // a payload chunk, then the error
	transientStreamEmpty                                 // the stream closes with no chunk at all
)

// transientRetryExecutor fails the first `failures` calls with `failErr`, then succeeds.
type transientRetryExecutor struct {
	identifier string
	failures   int
	failErr    error
	streamMode transientStreamMode

	mu    sync.Mutex
	calls []string
}

func (e *transientRetryExecutor) Identifier() string { return e.identifier }

func (e *transientRetryExecutor) record(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, id)
	return len(e.calls)
}

func (e *transientRetryExecutor) callIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func (e *transientRetryExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if n := e.record(auth.ID); n <= e.failures {
		return cliproxyexecutor.Response{}, e.failErr
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *transientRetryExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	n := e.record(auth.ID)
	if n > e.failures {
		ch := make(chan cliproxyexecutor.StreamChunk, 1)
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: ok\n\n")}
		close(ch)
		return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
	}
	switch e.streamMode {
	case transientStreamErrChunk:
		ch := make(chan cliproxyexecutor.StreamChunk, 1)
		ch <- cliproxyexecutor.StreamChunk{Err: e.failErr}
		close(ch)
		return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
	case transientStreamAfterBytes:
		ch := make(chan cliproxyexecutor.StreamChunk, 2)
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: partial\n\n")}
		ch <- cliproxyexecutor.StreamChunk{Err: e.failErr}
		close(ch)
		return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
	case transientStreamEmpty:
		ch := make(chan cliproxyexecutor.StreamChunk)
		close(ch)
		return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
	default:
		return nil, e.failErr
	}
}

func (*transientRetryExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *transientRetryExecutor) CountTokens(_ context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(context.Background(), auth, req, opts)
}

func (*transientRetryExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func newTransientRetryManager(t *testing.T, requestRetry int, exec *transientRetryExecutor, model string, authIDs ...string) *Manager {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(requestRetry, 20e9, 0)
	manager.RegisterExecutor(exec)
	reg := registry.GetGlobalRegistry()
	for _, id := range authIDs {
		reg.RegisterClient(id, exec.identifier, []*registry.ModelInfo{{ID: model}})
		id := id
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: id, Provider: exec.identifier}); errRegister != nil {
			t.Fatalf("register %s: %v", id, errRegister)
		}
	}
	return manager
}

func drainStream(t *testing.T, result *cliproxyexecutor.StreamResult) ([]string, error) {
	t.Helper()
	if result == nil || result.Chunks == nil {
		t.Fatal("stream result is nil")
	}
	var payloads []string
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		payloads = append(payloads, string(chunk.Payload))
	}
	return payloads, streamErr
}

func assertTwoDistinctCalls(t *testing.T, calls []string) {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %v, want exactly 2 (one failed, one retried)", calls)
	}
	if calls[0] == calls[1] {
		t.Fatalf("retry reused credential %q, want a different credential: %v", calls[0], calls)
	}
}

// (a) non-streaming: codex 408 drop on one credential, 200 on the other.
func TestTransientRetry_NonStream408RetriesOnOtherCredential(t *testing.T) {
	exec := &transientRetryExecutor{identifier: "transient-retry-a", failures: 1, failErr: transientDropError{code: http.StatusRequestTimeout}}
	manager := newTransientRetryManager(t, 3, exec, "transient-model-a", "tr-a-1", "tr-a-2")

	resp, err := manager.Execute(context.Background(), []string{exec.identifier}, cliproxyexecutor.Request{Model: "transient-model-a"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute error = %v, want 200 after retry; calls=%v", err, exec.callIDs())
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("payload = %q", resp.Payload)
	}
	assertTwoDistinctCalls(t, exec.callIDs())
}

// 5xx variants of the transient drop are retried too.
func TestTransientRetry_NonStream5xxRetries(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		exec := &transientRetryExecutor{identifier: "transient-retry-5xx", failures: 1, failErr: transientDropError{code: code}}
		manager := newTransientRetryManager(t, 3, exec, "transient-model-5xx", "tr-5-1", "tr-5-2")
		if _, err := manager.Execute(context.Background(), []string{exec.identifier}, cliproxyexecutor.Request{Model: "transient-model-5xx"}, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("status %d: Execute error = %v, want success after retry; calls=%v", code, err, exec.callIDs())
		}
		assertTwoDistinctCalls(t, exec.callIDs())
	}
}

// (b) streaming, error before any byte: retried on the other credential.
func TestTransientRetry_StreamZeroBytesRetries(t *testing.T) {
	for name, mode := range map[string]transientStreamMode{"returned-error": transientStreamReturnErr, "error-chunk": transientStreamErrChunk} {
		t.Run(name, func(t *testing.T) {
			exec := &transientRetryExecutor{identifier: "transient-retry-b-" + name, failures: 1, failErr: transientDropError{code: http.StatusRequestTimeout}, streamMode: mode}
			manager := newTransientRetryManager(t, 3, exec, "transient-model-b", "tr-b-1-"+name, "tr-b-2-"+name)

			result, err := manager.ExecuteStream(context.Background(), []string{exec.identifier}, cliproxyexecutor.Request{Model: "transient-model-b"}, cliproxyexecutor.Options{Stream: true})
			if err != nil {
				t.Fatalf("ExecuteStream error = %v, want retried stream; calls=%v", err, exec.callIDs())
			}
			payloads, streamErr := drainStream(t, result)
			if streamErr != nil || len(payloads) != 1 || payloads[0] != "data: ok\n\n" {
				t.Fatalf("stream payloads=%q err=%v, want one ok chunk; calls=%v", payloads, streamErr, exec.callIDs())
			}
			assertTwoDistinctCalls(t, exec.callIDs())
		})
	}
}

// An upstream stream that closes before its first payload (empty_stream) is retried too.
func TestTransientRetry_StreamEmptyRetries(t *testing.T) {
	exec := &transientRetryExecutor{identifier: "transient-retry-empty", failures: 1, streamMode: transientStreamEmpty}
	manager := newTransientRetryManager(t, 3, exec, "transient-model-empty", "tr-empty-1", "tr-empty-2")

	result, err := manager.ExecuteStream(context.Background(), []string{exec.identifier}, cliproxyexecutor.Request{Model: "transient-model-empty"}, cliproxyexecutor.Options{Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error = %v, want retried stream; calls=%v", err, exec.callIDs())
	}
	payloads, streamErr := drainStream(t, result)
	if streamErr != nil || len(payloads) != 1 {
		t.Fatalf("stream payloads=%q err=%v, want one ok chunk; calls=%v", payloads, streamErr, exec.callIDs())
	}
	assertTwoDistinctCalls(t, exec.callIDs())
}

// (c) streaming, error after a chunk reached the caller: never retried.
func TestTransientRetry_StreamAfterBytesNotRetried(t *testing.T) {
	exec := &transientRetryExecutor{identifier: "transient-retry-c", failures: 1, failErr: transientDropError{code: http.StatusRequestTimeout}, streamMode: transientStreamAfterBytes}
	manager := newTransientRetryManager(t, 3, exec, "transient-model-c", "tr-c-1", "tr-c-2")

	result, err := manager.ExecuteStream(context.Background(), []string{exec.identifier}, cliproxyexecutor.Request{Model: "transient-model-c"}, cliproxyexecutor.Options{Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error = %v, want stream that carries the error", err)
	}
	payloads, streamErr := drainStream(t, result)
	if len(payloads) != 1 || payloads[0] != "data: partial\n\n" || streamErr == nil {
		t.Fatalf("payloads=%q err=%v, want partial chunk then the error", payloads, streamErr)
	}
	if calls := exec.callIDs(); len(calls) != 1 {
		t.Fatalf("upstream calls = %v, want 1 (no retry after first byte)", calls)
	}
}

// (d) a 400 request fault is never retried.
func TestTransientRetry_400NotRetried(t *testing.T) {
	exec := &transientRetryExecutor{identifier: "transient-retry-d", failures: 1, failErr: requestFaultError{}}
	manager := newTransientRetryManager(t, 3, exec, "transient-model-d", "tr-d-1", "tr-d-2")

	if _, err := manager.Execute(context.Background(), []string{exec.identifier}, cliproxyexecutor.Request{Model: "transient-model-d"}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("Execute error = nil, want the 400")
	}
	if calls := exec.callIDs(); len(calls) != 1 {
		t.Fatalf("upstream calls = %v, want 1", calls)
	}
}

// (e) request-retry 0 disables the transient retry.
func TestTransientRetry_RequestRetryZeroDisables(t *testing.T) {
	exec := &transientRetryExecutor{identifier: "transient-retry-e", failures: 1, failErr: transientDropError{code: http.StatusRequestTimeout}}
	manager := newTransientRetryManager(t, 0, exec, "transient-model-e", "tr-e-1", "tr-e-2")

	_, err := manager.Execute(context.Background(), []string{exec.identifier}, cliproxyexecutor.Request{Model: "transient-model-e"}, cliproxyexecutor.Options{})
	if err == nil || statusCodeFromError(err) != http.StatusRequestTimeout {
		t.Fatalf("Execute error = %v, want the 408 surfaced", err)
	}
	if calls := exec.callIDs(); len(calls) != 1 {
		t.Fatalf("upstream calls = %v, want 1", calls)
	}
}

// The retry budget is request-retry: with every credential dropping, attempts stop at 1+N.
func TestTransientRetry_BudgetBoundedByRequestRetry(t *testing.T) {
	exec := &transientRetryExecutor{identifier: "transient-retry-f", failures: 100, failErr: transientDropError{code: http.StatusRequestTimeout}}
	manager := newTransientRetryManager(t, 2, exec, "transient-model-f", "tr-f-1", "tr-f-2", "tr-f-3", "tr-f-4", "tr-f-5")

	_, err := manager.Execute(context.Background(), []string{exec.identifier}, cliproxyexecutor.Request{Model: "transient-model-f"}, cliproxyexecutor.Options{})
	if err == nil || statusCodeFromError(err) != http.StatusRequestTimeout {
		t.Fatalf("Execute error = %v, want the 408 after the budget", err)
	}
	if calls := exec.callIDs(); len(calls) != 3 {
		t.Fatalf("upstream calls = %v, want 3 (1 + request-retry 2)", calls)
	}
}
