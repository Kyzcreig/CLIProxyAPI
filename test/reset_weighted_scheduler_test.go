package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	rw "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/resetweighted"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// engineScheduler adapts the plugin brain to the manager's PluginScheduler seam
// the same way the dylib does through pluginhost (Pick -> AuthID, Handled).
type engineScheduler struct {
	engine *rw.Engine
}

func (s engineScheduler) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	resp := s.engine.Pick(req)
	return resp, resp.Handled, nil
}

func (engineScheduler) HasScheduler() bool                   { return true }
func (engineScheduler) SchedulerWantsAcrossPriorities() bool { return true }

// TestResetWeightedSchedulerPickIsTheAuthTheRequestUsed proves end to end that the
// plugin's decision is what the request actually executed with: the near-reset
// credential (the one round-robin would NOT have started on) is the Authorization
// the upstream receives, and the plugin's JSON pick line names the same auth id.
// In shadow mode the same request goes to the host's own pick while the shadow
// line still records the would-be choice.
func TestResetWeightedSchedulerPickIsTheAuthTheRequestUsed(t *testing.T) {
	const model = "gpt-5.4"
	const created = `{"type":"response.created","response":{"id":"rws-ok"}}`
	const completed = `{"type":"response.completed","response":{"id":"rws-ok","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	served := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served <- strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", created, completed)
	}))
	defer server.Close()

	run := func(t *testing.T, mode string, hostFirstWins bool) {
		t.Helper()
		manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
		manager.SetRetryConfig(0, 0, 0)
		manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(&config.Config{}))
		farID, nearID := "rws-far-"+mode, "rws-near-"+mode
		for _, id := range []string{farID, nearID} {
			registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
				ID: id, Provider: "codex", Status: cliproxyauth.StatusActive,
				Attributes: map[string]string{"base_url": server.URL, "api_key": id},
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
		}

		cfg := rw.DefaultRuntimeConfig()
		cfg.Mode = mode
		cfg.Polling = false
		var lines []map[string]any
		engine := rw.NewEngine(cfg, func(_ string, _ string, fields map[string]any) { lines = append(lines, fields) })
		now := time.Now()
		week := func(used float64, resetsIn time.Duration) rw.Window {
			return rw.Window{Used: used, Known: true, Span: 7 * 24 * time.Hour, ResetsAt: now.Add(resetsIn)}
		}
		engine.SetQuota(farID, &rw.Quota{Long: week(13, 6*24*time.Hour), ObservedAt: now})
		engine.SetQuota(nearID, &rw.Quota{Long: week(13, 7*time.Hour), ObservedAt: now})
		manager.SetPluginScheduler(engineScheduler{engine: engine})

		resp, errExec := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
			Model: model, Payload: []byte(fmt.Sprintf(`{"model":%q,"input":"hello"}`, model)),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
		if errExec != nil {
			t.Fatalf("execute: %v", errExec)
		}
		if !strings.Contains(string(resp.Payload), "rws-ok") {
			t.Fatalf("payload = %s", resp.Payload)
		}
		var upstreamAuth string
		select {
		case upstreamAuth = <-served:
		default:
			t.Fatal("upstream saw no request")
		}
		if len(lines) == 0 {
			t.Fatal("plugin emitted no pick line")
		}
		line := lines[len(lines)-1]
		var decision rw.Decision
		if errUnmarshal := json.Unmarshal([]byte(line["decision"].(string)), &decision); errUnmarshal != nil {
			t.Fatalf("decision json: %v", errUnmarshal)
		}
		if decision.Chosen != nearID || decision.Reason != "reset" {
			t.Fatalf("decision = %+v, want near-reset seat for the reset reason", decision)
		}
		if hostFirstWins {
			if line["event"] != "pick_shadow" {
				t.Fatalf("event = %v, want pick_shadow", line["event"])
			}
			if upstreamAuth != farID && upstreamAuth != nearID {
				t.Fatalf("shadow routed to an unknown auth %q", upstreamAuth)
			}
			if decision.Differs != (decision.Chosen != decision.HostFirst) {
				t.Fatalf("differs flag inconsistent: %+v", decision)
			}
			return
		}
		if line["event"] != "pick" {
			t.Fatalf("event = %v, want pick", line["event"])
		}
		if upstreamAuth != nearID {
			t.Fatalf("upstream auth = %s, want the plugin's pick %s (host_first=%s)", upstreamAuth, nearID, decision.HostFirst)
		}
		if upstreamAuth != decision.Chosen {
			t.Fatalf("proxy log chosen=%s but upstream used %s", decision.Chosen, upstreamAuth)
		}
	}

	t.Run("enabled", func(t *testing.T) { run(t, rw.ModeEnabled, false) })
	t.Run("shadow", func(t *testing.T) { run(t, rw.ModeShadow, true) })
}
