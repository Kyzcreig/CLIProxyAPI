package helps

// DPX wirelog: a failed upstream hop names its class (card t_759af151).
//
// Before this, a transport error wrote `res.error` = the first 120 bytes of the
// Go error text (prose, not a class), and a body that stalled after the headers
// was recorded as a plain status-200 row with nothing wrong in it: the capture
// only fired at EOF or Close, and Close carried no "did not reach EOF" mark.
//
//   W1  refused port            -> status 0, phase headers, errorClass connect_refused
//   W2  accepts, never answers  -> status 0, phase headers, errorClass deadline_exceeded
//   W3  headers + partial body, caller closes (the executor's context cut)
//                               -> status 200, phase body, bodyComplete false,
//                                  errorClass closed_before_eof
//   W4  deadline fires mid-body -> status 200, phase body, bodyComplete false,
//                                  errorClass deadline_exceeded
//   W5  a normal 200            -> no phase / errorClass / bodyComplete key (inert)
//   W6  the class table on synthetic errors

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func dpxTestClient(t *testing.T, spool string) *http.Client {
	t.Helper()
	base := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	return DPXWirelogClient(base, DPXWirelogConfig{Spool: spool, Lane: "dtlx", Sub: "99"})
}

func dpxReadRows(t *testing.T, spool string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("row not JSON: %v: %s", err, line)
		}
		rows = append(rows, row)
	}
	return rows
}

func dpxOneRow(t *testing.T, spool string) (map[string]any, map[string]any) {
	t.Helper()
	rows := dpxReadRows(t, spool)
	if len(rows) != 1 {
		t.Fatalf("want exactly one row, got %d", len(rows))
	}
	res, ok := rows[0]["res"].(map[string]any)
	if !ok {
		t.Fatalf("row has no res object: %v", rows[0])
	}
	if _, ok := rows[0]["durationMs"]; !ok {
		t.Fatalf("row has no durationMs: %v", rows[0])
	}
	return rows[0], res
}

func dpxPost(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"model":"claude-x","messages":[]}`))
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func TestDPXWirelogRefusedPortNamesConnectRefused(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "wire.jsonl")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens here now
	_, err = dpxPost(context.Background(), dpxTestClient(t, spool), "http://"+addr+"/v1/messages")
	if err == nil {
		t.Fatal("expected a connection error")
	}
	_, res := dpxOneRow(t, spool)
	if res["status"] != float64(0) || res["phase"] != "headers" || res["errorClass"] != "connect_refused" {
		t.Fatalf("W1 res = %v", res)
	}
	if s, _ := res["error"].(string); s == "" {
		t.Fatalf("W1 prose kept beside the class: %v", res)
	}
}

func TestDPXWirelogNeverAnswersNamesDeadline(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "wire.jsonl")
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer func() { close(release); srv.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := dpxPost(ctx, dpxTestClient(t, spool), srv.URL+"/v1/messages")
	if err == nil {
		t.Fatal("expected a deadline error")
	}
	_, res := dpxOneRow(t, spool)
	if res["status"] != float64(0) || res["phase"] != "headers" || res["errorClass"] != "deadline_exceeded" {
		t.Fatalf("W2 res = %v", res)
	}
}

func TestDPXWirelogStalledBodyClosedByCaller(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "wire.jsonl")
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7}}}\n\n")
		w.(http.Flusher).Flush()
		<-release // stall: never sends message_stop, never closes
	}))
	defer func() { close(release); srv.Close() }()
	resp, err := dpxPost(context.Background(), dpxTestClient(t, spool), srv.URL+"/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}
	// The executor gives up on the stall and closes the body (its context ran out).
	_ = resp.Body.Close()
	_, res := dpxOneRow(t, spool)
	if res["status"] != float64(200) || res["phase"] != "body" || res["bodyComplete"] != false || res["errorClass"] != "closed_before_eof" {
		t.Fatalf("W3 res = %v", res)
	}
	body, _ := res["body"].(map[string]any)
	if body == nil || body["bytes"].(float64) <= 0 {
		t.Fatalf("W3 the bytes that did arrive are still digested: %v", res)
	}
}

func TestDPXWirelogDeadlineMidBody(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "wire.jsonl")
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"id":"msg_1","usage":{"input_tokens":1`)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer func() { close(release); srv.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	resp, err := dpxPost(ctx, dpxTestClient(t, spool), srv.URL+"/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body) // blocks until the deadline cuts the read
	if err == nil {
		t.Fatal("expected the deadline to cut the body read")
	}
	_ = resp.Body.Close()
	_, res := dpxOneRow(t, spool)
	if res["status"] != float64(200) || res["phase"] != "body" || res["bodyComplete"] != false || res["errorClass"] != "deadline_exceeded" {
		t.Fatalf("W4 res = %v", res)
	}
}

func TestDPXWirelogNormalResponseIsInert(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "wire.jsonl")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":2}}`)
	}))
	defer srv.Close()
	resp, err := dpxPost(context.Background(), dpxTestClient(t, spool), srv.URL+"/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_, res := dpxOneRow(t, spool)
	for _, key := range []string{"phase", "errorClass", "bodyComplete", "error"} {
		if _, present := res[key]; present {
			t.Fatalf("W5 a complete 200 must not carry %q: %v", key, res)
		}
	}
	if res["status"] != float64(200) {
		t.Fatalf("W5 res = %v", res)
	}
}

type dpxFakeTimeout struct{}

func (dpxFakeTimeout) Error() string   { return "i/o timeout" }
func (dpxFakeTimeout) Timeout() bool   { return true }
func (dpxFakeTimeout) Temporary() bool { return false }

func TestDPXTransportErrorClassTable(t *testing.T) {
	cases := map[string]error{
		"":                  nil,
		"deadline_exceeded": context.DeadlineExceeded,
		"canceled":          context.Canceled,
		"dns":               &net.DNSError{Err: "no such host", Name: "api.example"},
		"connect_refused":   &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}},
		"connection_reset":  &net.OpError{Op: "read", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}},
		"timeout":           dpxFakeTimeout{},
		"tls":               errors.New("remote error: tls: handshake failure"),
		"eof":               io.ErrUnexpectedEOF,
		"other":             errors.New("something else"),
	}
	for want, err := range cases {
		if got := DPXTransportErrorClass(err); got != want {
			t.Errorf("class(%v) = %q, want %q", err, got, want)
		}
	}
	// A deadline that wraps a net timeout is still the deadline (the cause).
	wrapped := &net.OpError{Op: "read", Err: context.DeadlineExceeded}
	if got := DPXTransportErrorClass(wrapped); got != "deadline_exceeded" {
		t.Errorf("wrapped deadline = %q", got)
	}
}
