package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestServerStop_ViolentShutdownImmediatelyClosesWithoutContextDeadlineError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Create a listener on a random available port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port

	cfg := &config.Config{
		Host: "127.0.0.1",
		Port: port,
	}

	server := NewServer(cfg, nil, nil, "")
	engine := gin.New()
	reqStarted := make(chan struct{})
	engine.GET("/slow", func(c *gin.Context) {
		close(reqStarted)
		time.Sleep(2 * time.Second)
		c.String(http.StatusOK, "ok")
	})
	server.server.Handler = engine

	// Start the server with the listener.
	go func() {
		_ = server.server.Serve(ln)
	}()

	// Start an in-flight request.
	reqDone := make(chan error, 1)
	go func() {
		resp, errGet := http.Get("http://" + ln.Addr().String() + "/slow")
		if errGet != nil {
			reqDone <- errGet
			return
		}
		defer func() {
			if errCloseBody := resp.Body.Close(); errCloseBody != nil {
				t.Logf("response body close error: %v", errCloseBody)
			}
		}()
		reqDone <- nil
	}()

	<-reqStarted

	// Call server.Stop with an expired context (simulating the expired shutdown deadline).
	// Under violent shutdown (s.server.Close), the server should immediately close in-flight
	// connections without waiting and without returning context deadline exceeded.
	expiredCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	stopStart := time.Now()
	errStop := server.Stop(expiredCtx)
	stopDuration := time.Since(stopStart)

	if stopDuration >= time.Second {
		t.Fatalf("server.Stop took %v, want immediate violent shutdown (<1s)", stopDuration)
	}
	if errStop != nil {
		t.Fatalf("server.Stop error = %v, want nil for violent shutdown", errStop)
	}

	// Verify the in-flight request was terminated / interrupted.
	select {
	case errReq := <-reqDone:
		if errReq == nil {
			t.Fatal("expected in-flight request to be interrupted/error, but it completed successfully")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for in-flight request to be interrupted by server.Stop")
	}
}

// TestServerStop_DrainsInFlightRequest pins t_240c35f4: a restart must let an
// in-flight request finish with 200 instead of cutting it (499 upstream), and
// must stop accepting new connections while it drains.
func TestServerStop_DrainsInFlightRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{Host: "127.0.0.1", Port: 0}
	server := NewServer(cfg, nil, nil, "")
	engine := gin.New()
	reqStarted := make(chan struct{})
	release := make(chan struct{})
	engine.GET("/slow", func(c *gin.Context) {
		close(reqStarted)
		<-release
		c.String(http.StatusOK, "drained")
	})
	server.server.Handler = engine
	server.server.Addr = "127.0.0.1:0"

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr := ln.Addr().String()
	httpListener := newMuxListener(ln.Addr(), 16)
	server.listenerMu.Lock()
	server.muxBaseListener = ln
	server.muxHTTPListener = httpListener
	server.listenerMu.Unlock()
	go func() { _ = server.server.Serve(httpListener) }()
	go func() { _ = server.acceptMuxConnections(ln, httpListener) }()

	type result struct {
		code int
		body string
		err  error
	}
	reqDone := make(chan result, 1)
	go func() {
		resp, errGet := http.Get("http://" + addr + "/slow")
		if errGet != nil {
			reqDone <- result{err: errGet}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, errRead := io.ReadAll(resp.Body)
		reqDone <- result{code: resp.StatusCode, body: string(body), err: errRead}
	}()
	<-reqStarted

	stopDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopDone <- server.Stop(ctx)
	}()

	// While draining: Stop has not returned and new connections are refused.
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, errDial := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if errDial != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener still accepting connections after Stop began")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case errStop := <-stopDone:
		t.Fatalf("Stop returned (%v) before the in-flight request finished", errStop)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case r := <-reqDone:
		if r.err != nil || r.code != http.StatusOK || r.body != "drained" {
			t.Fatalf("in-flight request = (%d, %q, %v), want (200, \"drained\", nil)", r.code, r.body, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not complete after release")
	}
	select {
	case errStop := <-stopDone:
		if errStop != nil {
			t.Fatalf("Stop error = %v, want nil after a clean drain", errStop)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the drain finished")
	}
}

// TestHealthzReportsInFlightPOST pins the idle signal cliproxyapi-restart.sh waits on.
func TestHealthzReportsInFlightPOST(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := NewServer(&config.Config{Host: "127.0.0.1"}, nil, nil, "")
	inHandler := make(chan struct{})
	release := make(chan struct{})
	server.engine.POST("/t240/slow", func(c *gin.Context) {
		close(inHandler)
		<-release
		c.Status(http.StatusOK)
	})
	ts := httptest.NewServer(server.engine)
	defer ts.Close()

	inFlight := func() int64 {
		resp, errGet := http.Get(ts.URL + "/healthz")
		if errGet != nil {
			t.Fatalf("healthz: %v", errGet)
		}
		defer func() { _ = resp.Body.Close() }()
		var body struct {
			InFlight *int64 `json:"in_flight"`
		}
		if errDecode := json.NewDecoder(resp.Body).Decode(&body); errDecode != nil || body.InFlight == nil {
			t.Fatalf("healthz body missing in_flight (err=%v)", errDecode)
		}
		return *body.InFlight
	}

	if got := inFlight(); got != 0 {
		t.Fatalf("idle in_flight = %d, want 0", got)
	}
	done := make(chan struct{})
	go func() {
		resp, errPost := http.Post(ts.URL+"/t240/slow", "application/json", nil)
		if errPost == nil {
			_ = resp.Body.Close()
		}
		close(done)
	}()
	<-inHandler
	if got := inFlight(); got != 1 {
		t.Fatalf("in_flight during a POST = %d, want 1", got)
	}
	close(release)
	<-done
	if got := inFlight(); got != 0 {
		t.Fatalf("in_flight after the POST = %d, want 0", got)
	}
}
