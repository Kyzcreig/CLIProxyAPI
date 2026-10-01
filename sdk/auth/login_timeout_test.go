package auth

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCallbackWaitDefaultsToFiveMinutes(t *testing.T) {
	if got := callbackWait(nil); got != DefaultCallbackTimeout {
		t.Fatalf("callbackWait(nil) = %v, want %v", got, DefaultCallbackTimeout)
	}
	if got := callbackWait(&LoginOptions{}); got != 5*time.Minute {
		t.Fatalf("callbackWait(zero) = %v, want 5m", got)
	}
	if got := callbackWait(&LoginOptions{CallbackTimeout: 30 * time.Minute}); got != 30*time.Minute {
		t.Fatalf("callbackWait(30m) = %v, want 30m", got)
	}
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// The callback wait must come from LoginOptions.CallbackTimeout, not a hard-coded 5 minutes.
func TestAntigravityLoginHonorsCallbackTimeout(t *testing.T) {
	opts := &LoginOptions{
		NoBrowser:       true,
		CallbackPort:    freeLocalPort(t),
		CallbackTimeout: 150 * time.Millisecond,
	}
	start := time.Now()
	_, err := AntigravityAuthenticator{}.Login(context.Background(), &config.Config{}, opts)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "authentication timed out") {
		t.Fatalf("Login error = %v, want authentication timed out", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Login waited %v, want ~150ms (CallbackTimeout ignored)", elapsed)
	}
}
