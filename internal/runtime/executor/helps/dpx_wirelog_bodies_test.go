package helps

// DPX wirelog body modes (card t_f21d58d8, Ace ruling 2026-10-08: full bodies
// on every lane, on the box, 45 days).
//
//   B1  default (Bodies "") + body spool -> one row on the DISK spool with
//       req.body.raw / res.body.raw (wirelog.js schema) beside the digest;
//       nothing on the RAM spool
//   B2  shape / none -> digest-only row on the RAM spool, bodies=<mode>
//   B3  full but no body spool -> digest row on RAM, bodiesSuspended=no_body_spool
//   B4  full under the free-space floor -> digest row on RAM, bodiesSuspended=low_disk
//   B5  body spool unwritable -> digest row on RAM, bodiesSuspended=write_failed
//   B6  undecodable Content-Encoding -> res.body.raw base64 + rawEncoding
//   B7  a stream response keeps the whole SSE text, past the 1 MiB digest cap
//   B8  a transport failure still lands its request body in full mode

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const dpxBodiesReq = `{"model":"claude-x","messages":[{"role":"user","content":"hello raw"}]}`

func dpxBodiesClient(cfg DPXWirelogConfig) *http.Client {
	base := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	if cfg.Lane == "" {
		cfg.Lane = "dtlx"
	}
	cfg.Sub = "99"
	return DPXWirelogClient(base, cfg)
}

func dpxBodiesDo(t *testing.T, client *http.Client, url string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(dpxBodiesReq))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
}

func dpxJSONServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dpxNoFile(t *testing.T, path string) {
	t.Helper()
	if raw, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		t.Fatalf("%s should be empty, has %q", filepath.Base(path), raw)
	}
}

func dpxRowBodies(t *testing.T, row map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	req, _ := row["req"].(map[string]any)
	res, _ := row["res"].(map[string]any)
	reqBody, _ := req["body"].(map[string]any)
	resBody, _ := res["body"].(map[string]any)
	if reqBody == nil || resBody == nil {
		t.Fatalf("row lacks req.body/res.body: %v", row)
	}
	return reqBody, resBody
}

func TestDPXWirelogFullBodiesDefaultToDiskSpool(t *testing.T) {
	dir := t.TempDir()
	ram, disk := filepath.Join(dir, "ram.jsonl"), filepath.Join(dir, "disk.jsonl")
	resp := `{"id":"m","content":[{"type":"text","text":"raw answer"}],"usage":{"input_tokens":3,"output_tokens":2}}`
	srv := dpxJSONServer(t, resp)
	dpxBodiesDo(t, dpxBodiesClient(DPXWirelogConfig{Spool: ram, BodySpool: disk}), srv.URL+"/v1/messages")
	dpxNoFile(t, ram)
	row, _ := dpxOneRow(t, disk)
	if row["bodies"] != "full" || row["bodiesSuspended"] != nil {
		t.Fatalf("B1 bodies = %v / %v", row["bodies"], row["bodiesSuspended"])
	}
	reqBody, resBody := dpxRowBodies(t, row)
	if reqBody["raw"] != dpxBodiesReq || reqBody["model"] != "claude-x" {
		t.Fatalf("B1 req.body = %v", reqBody)
	}
	if resBody["raw"] != resp || resBody["usage"] == nil {
		t.Fatalf("B1 res.body = %v", resBody)
	}
}

func TestDPXWirelogShapeAndNoneStayDigestOnRAM(t *testing.T) {
	for _, mode := range []string{"shape", "none", "SHAPE"} {
		dir := t.TempDir()
		ram, disk := filepath.Join(dir, "ram.jsonl"), filepath.Join(dir, "disk.jsonl")
		srv := dpxJSONServer(t, `{"usage":{"input_tokens":1}}`)
		dpxBodiesDo(t, dpxBodiesClient(DPXWirelogConfig{Spool: ram, BodySpool: disk, Bodies: mode}), srv.URL+"/v1/messages")
		dpxNoFile(t, disk)
		row, _ := dpxOneRow(t, ram)
		if row["bodies"] != strings.ToLower(mode) {
			t.Fatalf("B2 %s: bodies = %v", mode, row["bodies"])
		}
		reqBody, resBody := dpxRowBodies(t, row)
		if _, ok := reqBody["raw"]; ok {
			t.Fatalf("B2 %s: digest row carries req raw", mode)
		}
		if _, ok := resBody["raw"]; ok {
			t.Fatalf("B2 %s: digest row carries res raw", mode)
		}
	}
}

func TestDPXWirelogFullWithoutBodySpoolIsSuspended(t *testing.T) {
	ram := filepath.Join(t.TempDir(), "ram.jsonl")
	srv := dpxJSONServer(t, `{}`)
	dpxBodiesDo(t, dpxBodiesClient(DPXWirelogConfig{Spool: ram, Bodies: "full"}), srv.URL+"/v1/messages")
	row, _ := dpxOneRow(t, ram)
	if row["bodies"] != "shape" || row["bodiesSuspended"] != "no_body_spool" {
		t.Fatalf("B3 = %v / %v", row["bodies"], row["bodiesSuspended"])
	}
	if reqBody, _ := dpxRowBodies(t, row); reqBody["raw"] != nil {
		t.Fatal("B3 raw leaked onto the RAM spool")
	}
}

func TestDPXWirelogFloorSuspendsFullBodies(t *testing.T) {
	prev := dpxFreeBytes
	dpxFreeBytes = func(string) (uint64, bool) { return 4 << 30, true } // 4 GB free < 5 GB floor
	dpxFreeMu.Lock()
	dpxFreeCache = map[string]dpxFreeSample{}
	dpxFreeMu.Unlock()
	t.Cleanup(func() {
		dpxFreeBytes = prev
		dpxFreeMu.Lock()
		dpxFreeCache = map[string]dpxFreeSample{}
		dpxFreeMu.Unlock()
	})
	dir := t.TempDir()
	ram, disk := filepath.Join(dir, "ram.jsonl"), filepath.Join(dir, "disk.jsonl")
	srv := dpxJSONServer(t, `{}`)
	dpxBodiesDo(t, dpxBodiesClient(DPXWirelogConfig{Spool: ram, BodySpool: disk}), srv.URL+"/v1/messages")
	dpxNoFile(t, disk)
	row, _ := dpxOneRow(t, ram)
	if row["bodies"] != "shape" || row["bodiesSuspended"] != "low_disk" {
		t.Fatalf("B4 = %v / %v", row["bodies"], row["bodiesSuspended"])
	}
	// A lower configured floor lets the same 4 GB through.
	dpxFreeMu.Lock()
	dpxFreeCache = map[string]dpxFreeSample{}
	dpxFreeMu.Unlock()
	dir2 := t.TempDir()
	ram2, disk2 := filepath.Join(dir2, "ram.jsonl"), filepath.Join(dir2, "disk.jsonl")
	dpxBodiesDo(t, dpxBodiesClient(DPXWirelogConfig{Spool: ram2, BodySpool: disk2, MinFreeGB: 1}), srv.URL+"/v1/messages")
	dpxNoFile(t, ram2)
	if row, _ := dpxOneRow(t, disk2); row["bodies"] != "full" {
		t.Fatalf("B4 floor=1: bodies = %v", row["bodies"])
	}
}

func TestDPXWirelogBodySpoolWriteFailureFallsBackToDigest(t *testing.T) {
	dir := t.TempDir()
	ram := filepath.Join(dir, "ram.jsonl")
	disk := filepath.Join(dir, "missing-dir", "disk.jsonl") // parent never created: open fails
	srv := dpxJSONServer(t, `{}`)
	dpxBodiesDo(t, dpxBodiesClient(DPXWirelogConfig{Spool: ram, BodySpool: disk}), srv.URL+"/v1/messages")
	row, _ := dpxOneRow(t, ram)
	if row["bodies"] != "shape" || row["bodiesSuspended"] != "write_failed" {
		t.Fatalf("B5 = %v / %v", row["bodies"], row["bodiesSuspended"])
	}
	reqBody, resBody := dpxRowBodies(t, row)
	if reqBody["raw"] != nil || resBody["raw"] != nil {
		t.Fatalf("B5 raw left on the fallback digest row: %v", row)
	}
}

func TestDPXWirelogUndecodableResponseIsBase64(t *testing.T) {
	dir := t.TempDir()
	ram, disk := filepath.Join(dir, "ram.jsonl"), filepath.Join(dir, "disk.jsonl")
	payload := []byte{0x00, 0xff, 0x10, 'x'}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "x-unknown")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	dpxBodiesDo(t, dpxBodiesClient(DPXWirelogConfig{Spool: ram, BodySpool: disk}), srv.URL+"/v1/messages")
	row, _ := dpxOneRow(t, disk)
	_, resBody := dpxRowBodies(t, row)
	if resBody["rawEncoding"] != "base64+x-unknown" || resBody["raw"] != base64.StdEncoding.EncodeToString(payload) {
		t.Fatalf("B6 res.body = %v", resBody)
	}
}

func TestDPXWirelogFullStreamKeepsWholeBody(t *testing.T) {
	dir := t.TempDir()
	ram, disk := filepath.Join(dir, "ram.jsonl"), filepath.Join(dir, "disk.jsonl")
	var sse strings.Builder
	sse.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n")
	chunk := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" + strings.Repeat("a", 1000) + "\"}}\n\n"
	for sse.Len() < 1<<20+4096 { // past the 1 MiB digest cap
		sse.WriteString(chunk)
	}
	sse.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n")
	want := sse.String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, want)
	}))
	defer srv.Close()
	dpxBodiesDo(t, dpxBodiesClient(DPXWirelogConfig{Spool: ram, BodySpool: disk}), srv.URL+"/v1/messages")
	row, _ := dpxOneRow(t, disk)
	_, resBody := dpxRowBodies(t, row)
	if got, _ := resBody["raw"].(string); got != want {
		t.Fatalf("B7 raw len %d, want %d", len(got), len(want))
	}
	if resBody["rawTruncated"] != nil {
		t.Fatal("B7 marked truncated under the full cap")
	}
	usage, _ := resBody["usage"].(map[string]any)
	if usage["output_tokens"] != float64(5) {
		t.Fatalf("B7 usage past 1 MiB lost: %v", resBody["usage"])
	}
}

func TestDPXWirelogTransportFailureKeepsRequestBody(t *testing.T) {
	dir := t.TempDir()
	ram, disk := filepath.Join(dir, "ram.jsonl"), filepath.Join(dir, "disk.jsonl")
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // refused
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/messages", strings.NewReader(dpxBodiesReq))
	if _, err := dpxBodiesClient(DPXWirelogConfig{Spool: ram, BodySpool: disk}).Do(req); err == nil {
		t.Fatal("expected a transport error")
	}
	rows := dpxReadRows(t, disk)
	if len(rows) != 1 || rows[0]["bodies"] != "full" {
		t.Fatalf("B8 rows = %v", rows)
	}
	req0, _ := rows[0]["req"].(map[string]any)
	if body, _ := req0["body"].(map[string]any); body["raw"] != dpxBodiesReq {
		t.Fatalf("B8 req.body = %v", req0["body"])
	}
}
