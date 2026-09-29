// dpx-alias-offline is a loopback-only acceptance harness, not a production server.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	execpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type settings struct {
	Upstream, ClientKey, StoreDirectory, SessionID string
	Initialize                                     bool
	// UpstreamKey defaults to a synthetic API key. A synthetic OAuth-shaped value
	// ("sk-ant-oat...") exercises CPA's native OAuth CCH signing path offline.
	UpstreamKey string
}

func main() {
	path := flag.String("config", "", "private harness config")
	flag.Parse()
	raw, err := os.ReadFile(*path)
	if err != nil {
		panic("config_read")
	}
	var c settings
	if json.Unmarshal(raw, &c) != nil || c.ClientKey == "" {
		panic("config")
	}
	upstream, err := url.Parse(c.Upstream)
	if err != nil || upstream.Scheme != "http" || upstream.User != nil {
		panic("loopback_required")
	}
	ip := net.ParseIP(upstream.Hostname())
	if ip == nil || !ip.IsLoopback() {
		panic("loopback_required")
	}
	binding := contentalias.Binding{Principal: "dedicated-offline-cli", Session: c.SessionID, Version: "v1"}
	if c.Initialize {
		if _, err := contentalias.Create(c.StoreDirectory, binding, contentalias.DefaultManifest()); err != nil {
			panic(err)
		}
	} else {
		if _, err := contentalias.Open(c.StoreDirectory, binding, contentalias.DefaultManifest()); err != nil {
			panic(err)
		}
	}
	cfg := &config.Config{MaxRetryCredentials: 1, DPXContentAlias: config.DPXContentAlias{Enabled: true, StoreDirectory: c.StoreDirectory, Principal: binding.Principal, SessionID: binding.Session, Version: binding.Version}}
	e := executor.NewClaudeExecutor(cfg)
	upstreamKey := "offline-synthetic"
	if c.UpstreamKey != "" {
		upstreamKey = c.UpstreamKey
	}
	auth := &authpkg.Auth{ID: "offline-upstream", Provider: "claude", Attributes: map[string]string{"api_key": upstreamKey, "base_url": c.Upstream, "cloak_mode": "never"}}
	if c.UpstreamKey != "" {
		// A saved OAuth credential carries its profile account; synthetic here.
		auth.Metadata = map[string]any{"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("listen")
	}
	fmt.Println(listener.Addr().String())
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != c.ClientKey && r.Header.Get("Authorization") != "Bearer "+c.ClientKey {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.Method != "POST" || (r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens") {
			http.Error(w, "unsupported_route", 400)
			return
		}
		payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
		if err != nil {
			http.Error(w, "request_limit", 400)
			return
		}
		req := execpkg.Request{Model: gjson.GetBytes(payload, "model").String(), Payload: payload}
		opts := execpkg.Options{SourceFormat: translator.FromString("claude"), Headers: r.Header.Clone(), OriginalRequest: payload}
		failure := func(err error) {
			status := 400
			var sc interface{ StatusCode() int }
			if errors.As(err, &sc) {
				status = sc.StatusCode()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": err.Error()}})
		}
		if r.URL.Path == "/v1/messages/count_tokens" {
			response, err := e.CountTokens(r.Context(), auth, req, opts)
			if err != nil {
				failure(err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(response.Payload)
			return
		}
		if !gjson.GetBytes(payload, "stream").Bool() {
			response, err := e.Execute(r.Context(), auth, req, opts)
			if err != nil {
				failure(err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(response.Payload)
			return
		}
		response, err := e.ExecuteStream(r.Context(), auth, req, opts)
		if err != nil {
			failure(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for chunk := range response.Chunks {
			if chunk.Err != nil {
				b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": "contentalias:stream_failed"}})
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
				w.(http.Flusher).Flush()
				return
			}
			w.Write(chunk.Payload)
			w.(http.Flusher).Flush()
		}
	})
	if err := http.Serve(listener, handler); err != nil {
		panic("serve")
	}
}
