package executor

import (
	"net/http"
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The Kimi identity headers must never name this proxy or the machine it runs
// on (t_a37235c0: the former literals were "CLIProxyAPI" and os.Hostname()).
// They are config with generic defaults.
func TestKimiIdentityHeadersDefaultsAreGeneric(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://api.kimi.com/coding/v1/chat/completions", nil)
	applyKimiHeaders(req, "tok", false, kimiIdentityFromConfig(nil))
	if got := req.Header.Get("X-Msh-Platform"); got != "kimi-cli" {
		t.Fatalf("X-Msh-Platform = %q, want kimi-cli", got)
	}
	if got := req.Header.Get("X-Msh-Device-Name"); got != "host" {
		t.Fatalf("X-Msh-Device-Name = %q, want host", got)
	}
	if hostname, err := os.Hostname(); err == nil && hostname != "" && req.Header.Get("X-Msh-Device-Name") == hostname {
		t.Fatalf("X-Msh-Device-Name leaks the hostname %q", hostname)
	}
}

func TestKimiIdentityHeadersFromConfig(t *testing.T) {
	cfg := &config.Config{Kimi: config.KimiConfig{Platform: "my-ide", DeviceName: "laptop"}}
	req, _ := http.NewRequest(http.MethodPost, "https://api.kimi.com/coding/v1/chat/completions", nil)
	applyKimiHeaders(req, "tok", true, kimiIdentityFromConfig(cfg))
	if req.Header.Get("X-Msh-Platform") != "my-ide" || req.Header.Get("X-Msh-Device-Name") != "laptop" {
		t.Fatalf("config not applied: %v", req.Header)
	}
	if req.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("stream Accept header lost: %q", req.Header.Get("Accept"))
	}
	// Blank values fall back to the generic defaults, never to the hostname.
	req2, _ := http.NewRequest(http.MethodPost, "https://api.kimi.com/coding/v1/chat/completions", nil)
	applyKimiHeaders(req2, "tok", false, kimiIdentityFromConfig(&config.Config{Kimi: config.KimiConfig{Platform: "  "}}))
	if req2.Header.Get("X-Msh-Platform") != "kimi-cli" || req2.Header.Get("X-Msh-Device-Name") != "host" {
		t.Fatalf("blank config must default: %v", req2.Header)
	}
}
