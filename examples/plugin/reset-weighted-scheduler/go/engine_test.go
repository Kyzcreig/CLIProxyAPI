package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func elig(ids ...string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func TestAffinityHomeStickyFallbackReturnHomeOnce(t *testing.T) {
	m := NewAffinityMap(100, "")
	now := t0
	m.Bind("s1", "home", now)

	// Healthy home: pinned.
	if r := m.Resolve("s1", elig("home", "alt"), now); r.Target != "home" || r.Stage != "affinity" {
		t.Fatalf("resolve = %+v, want home/affinity", r)
	}
	// Home out (transient): no fallback yet -> caller scores; the served alt becomes sticky.
	if r := m.Resolve("s1", elig("alt", "alt2"), now); r.Target != "" || r.Home != "home" {
		t.Fatalf("resolve = %+v, want unresolved with home recorded", r)
	}
	m.NoteServed("s1", "alt", now)
	// Still out: the SAME alternate, not a fresh score pick.
	if r := m.Resolve("s1", elig("alt", "alt2"), now); r.Target != "alt" || r.Stage != "fallback" {
		t.Fatalf("resolve = %+v, want alt/fallback", r)
	}
	// Home recovers: return home exactly once.
	if r := m.Resolve("s1", elig("home", "alt", "alt2"), now); r.Target != "home" || r.Stage != "return_home" {
		t.Fatalf("resolve = %+v, want home/return_home", r)
	}
	// Home flaps out again: sticky alternate again (outlived the recovery).
	if r := m.Resolve("s1", elig("alt", "alt2"), now); r.Target != "alt" || r.Stage != "fallback" {
		t.Fatalf("resolve = %+v, want alt/fallback after flap", r)
	}
	// Home back a second time within the excursion: hold the fallback, do not bounce.
	if r := m.Resolve("s1", elig("home", "alt"), now); r.Target != "alt" || r.Stage != "hold_fallback" {
		t.Fatalf("resolve = %+v, want alt/hold_fallback", r)
	}
	// Fallback vanishes: home is all that is left.
	if r := m.Resolve("s1", elig("home"), now); r.Target != "home" {
		t.Fatalf("resolve = %+v, want home when fallback gone", r)
	}
	// Quota rebind clears everything.
	m.Bind("s1", "alt2", now)
	if r := m.Resolve("s1", elig("home", "alt", "alt2"), now); r.Target != "alt2" || r.Stage != "affinity" {
		t.Fatalf("resolve after rebind = %+v, want alt2/affinity", r)
	}
}

func TestAffinityPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "affinity.json")
	m := NewAffinityMap(100, path)
	m.Bind("s1", "home", t0)
	m.NoteServed("s1", "alt", t0)
	m.Bind("s2", "other", t0)
	if errFlush := m.Flush(); errFlush != nil {
		t.Fatalf("flush: %v", errFlush)
	}
	m2 := NewAffinityMap(100, path)
	if m2.Len() != 2 {
		t.Fatalf("len after reload = %d, want 2", m2.Len())
	}
	if r := m2.Resolve("s1", elig("alt"), t0); r.Target != "alt" || r.Stage != "fallback" {
		t.Fatalf("persisted fallback: %+v", r)
	}
	if home, _ := m2.Peek("s2"); home != "other" {
		t.Fatalf("persisted home = %q", home)
	}
}

func TestAffinityLRUAndDropCredential(t *testing.T) {
	m := NewAffinityMap(2, "")
	m.Bind("a", "x", t0)
	m.Bind("b", "x", t0.Add(time.Second))
	m.Bind("c", "y", t0.Add(2*time.Second))
	if _, ok := m.Peek("a"); ok {
		t.Fatal("LRU should have evicted a")
	}
	if released := m.DropCredential("x"); released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}
	if m.Len() != 1 {
		t.Fatalf("len = %d, want 1", m.Len())
	}
}

// Engine --------------------------------------------------------------------

type logCapture struct{ lines []map[string]any }

func (l *logCapture) fn(_ string, _ string, fields map[string]any) { l.lines = append(l.lines, fields) }

func newTestEngine(mode string) (*Engine, *logCapture) {
	cfg := defaultRuntimeConfig()
	cfg.Mode = mode
	cfg.Polling = false
	logs := &logCapture{}
	e := NewEngine(cfg, logs.fn)
	e.now = func() time.Time { return t0 }
	return e, logs
}

func pickReq(provider string, model string, headers http.Header, ids ...string) pluginapi.SchedulerPickRequest {
	req := pluginapi.SchedulerPickRequest{Provider: provider, Providers: []string{provider}, Model: model, Options: pluginapi.SchedulerOptions{Headers: headers}}
	for _, id := range ids {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: provider})
	}
	return req
}

func TestEnginePickEnabledPrefersNearReset(t *testing.T) {
	e, logs := newTestEngine(ModeEnabled)
	e.SetQuota("near", q(week(13, 7*time.Hour), fiveH(10)))
	e.SetQuota("far", q(week(13, 6*24*time.Hour), fiveH(10)))
	resp := e.Pick(pickReq("codex", "gpt-5.5", nil, "far", "near"))
	if !resp.Handled || resp.AuthID != "near" {
		t.Fatalf("pick = %+v, want near", resp)
	}
	if len(logs.lines) != 1 || logs.lines[0]["event"] != "pick" || logs.lines[0]["differs"] != true {
		t.Fatalf("log = %+v, want one pick line that differs from host_first", logs.lines)
	}
	var d Decision
	if errUnmarshal := json.Unmarshal([]byte(logs.lines[0]["decision"].(string)), &d); errUnmarshal != nil || d.Chosen != "near" || d.Reason != "reset" {
		t.Fatalf("decision json = %+v (%v)", d, errUnmarshal)
	}
}

func TestEngineShadowNeverHandles(t *testing.T) {
	e, logs := newTestEngine(ModeShadow)
	e.SetQuota("near", q(week(13, 7*time.Hour), fiveH(10)))
	e.SetQuota("far", q(week(13, 6*24*time.Hour), fiveH(10)))
	resp := e.Pick(pickReq("codex", "gpt-5.5", nil, "far", "near"))
	if resp.Handled || resp.AuthID != "" {
		t.Fatalf("shadow pick = %+v, want Handled=false", resp)
	}
	if logs.lines[0]["event"] != "pick_shadow" || logs.lines[0]["chosen"] != "near" {
		t.Fatalf("shadow log = %+v", logs.lines[0])
	}
	st := e.Status()
	if st["shadow_picks"].(int64) != 1 || st["shadow_differs"].(int64) != 1 || st["picks"].(int64) != 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestEngineOffAndForeignProviderDecline(t *testing.T) {
	e, _ := newTestEngine(ModeOff)
	if resp := e.Pick(pickReq("codex", "m", nil, "a")); resp.Handled {
		t.Fatal("off mode must decline")
	}
	e, _ = newTestEngine(ModeEnabled)
	if resp := e.Pick(pickReq("gemini", "m", nil, "a")); resp.Handled {
		t.Fatal("unlisted provider must decline")
	}
	if resp := e.Pick(pluginapi.SchedulerPickRequest{Provider: "codex"}); resp.Handled {
		t.Fatal("no candidates must decline")
	}
}

func TestEngineUnknownQuotaFallsBackToHostOrder(t *testing.T) {
	e, logs := newTestEngine(ModeEnabled)
	resp := e.Pick(pickReq("codex", "m", nil, "first", "second"))
	if !resp.Handled || resp.AuthID != "first" {
		t.Fatalf("pick = %+v, want host-first with no quota", resp)
	}
	if logs.lines[0]["reason"] != "unknown_quota" {
		t.Fatalf("reason = %v", logs.lines[0]["reason"])
	}
}

func TestEngineAffinityPinsThenFollowsExhaustion(t *testing.T) {
	e, logs := newTestEngine(ModeEnabled)
	e.SetQuota("a", q(week(13, 7*time.Hour), fiveH(10)))
	e.SetQuota("b", q(week(13, 6*24*time.Hour), fiveH(10)))
	headers := http.Header{defaultSessionHeader: []string{"claude:sess-1"}}
	first := e.Pick(pickReq("claude", "claude-sonnet-4-6", headers, "b", "a"))
	if first.AuthID != "a" {
		t.Fatalf("first = %+v", first)
	}
	// Make b the better score; the session must stay on a.
	e.SetQuota("b", q(week(5, 2*time.Hour), fiveH(0)))
	second := e.Pick(pickReq("claude", "claude-sonnet-4-6", headers, "b", "a"))
	if second.AuthID != "a" || logs.lines[1]["stage"] != "affinity" {
		t.Fatalf("second = %+v stage=%v, want pinned to a", second, logs.lines[1]["stage"])
	}
	// a over the 5h guard: a pin is NOT evicted by the guard.
	e.SetQuota("a", q(week(13, 7*time.Hour), fiveH(95)))
	third := e.Pick(pickReq("claude", "claude-sonnet-4-6", headers, "b", "a"))
	if third.AuthID != "a" {
		t.Fatalf("third = %+v, want a (guard never evicts a binding)", third)
	}
	// a exhausted: binding dropped, rebinds via score to b.
	capped := q(week(100, 7*time.Hour), fiveH(10))
	capped.Long.Rejected = true
	e.SetQuota("a", capped)
	fourth := e.Pick(pickReq("claude", "claude-sonnet-4-6", headers, "b", "a"))
	if fourth.AuthID != "b" {
		t.Fatalf("fourth = %+v, want b after exhaustion", fourth)
	}
	if home, _ := e.affinity.Peek("claude:sess-1"); home != "b" {
		t.Fatalf("home after rebind = %q, want b", home)
	}
}

func TestEngineFableSessionBindsUnderOwnKey(t *testing.T) {
	e, _ := newTestEngine(ModeEnabled)
	e.SetQuota("r", fableQ(80, 60, 3*time.Hour))
	e.SetQuota("o", fableQ(40, 60, 5*24*time.Hour))
	headers := http.Header{defaultSessionHeader: []string{"claude:s"}}
	nonFable := e.Pick(pickReq("claude", "claude-sonnet-4-6", headers, "r", "o"))
	fable := e.Pick(pickReq("claude", "claude-fable-5-1", headers, "r", "o"))
	if nonFable.AuthID != "o" || fable.AuthID != "r" {
		t.Fatalf("non-fable=%s fable=%s, want o / r", nonFable.AuthID, fable.AuthID)
	}
	if _, ok := e.affinity.Peek("claude:s|fable"); !ok {
		t.Fatal("fable session should bind under its own key")
	}
}

func TestEngineUsage429ReleasesBindings(t *testing.T) {
	e, _ := newTestEngine(ModeEnabled)
	e.SetQuota("a", q(week(13, 7*time.Hour), fiveH(10)))
	e.SetQuota("b", q(week(13, 6*24*time.Hour), fiveH(10)))
	headers := http.Header{defaultSessionHeader: []string{"k"}}
	if r := e.Pick(pickReq("codex", "m", headers, "a", "b")); r.AuthID != "a" {
		t.Fatalf("pick = %+v", r)
	}
	e.HandleUsage(pluginapi.UsageRecord{AuthID: "a", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429}})
	if _, ok := e.affinity.Peek("k"); ok {
		t.Fatal("429 must release the binding")
	}
	if r := e.Pick(pickReq("codex", "m", headers, "a", "b")); r.AuthID != "b" {
		t.Fatalf("after 429 pick = %+v, want b", r)
	}
}

func TestInterceptBeforeInjectsSessionHeader(t *testing.T) {
	e, _ := newTestEngine(ModeEnabled)
	body := []byte(`{"model":"claude-sonnet-4-6","metadata":{"user_id":"user_abc_account__session_0f3b-1"}}`)
	resp := e.InterceptBefore(pluginapi.RequestInterceptRequest{Body: body})
	if got := resp.Headers.Get(defaultSessionHeader); got != "claude:0f3b-1" {
		t.Fatalf("header = %q", got)
	}
	jsonUser := []byte(`{"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"s-2\"}"}}`)
	if got := e.InterceptBefore(pluginapi.RequestInterceptRequest{Body: jsonUser}).Headers.Get(defaultSessionHeader); got != "claude:s-2" {
		t.Fatalf("json user_id header = %q", got)
	}
	if got := e.InterceptBefore(pluginapi.RequestInterceptRequest{Body: []byte(`{"prompt_cache_key":"pck-9"}`)}).Headers.Get(defaultSessionHeader); got != "pck:pck-9" {
		t.Fatalf("pck header = %q", got)
	}
	if resp := e.InterceptBefore(pluginapi.RequestInterceptRequest{Body: []byte(`{"messages":[]}`)}); len(resp.Headers) != 0 {
		t.Fatalf("no session -> no header, got %+v", resp.Headers)
	}
	// Header fallbacks the scheduler reads directly.
	if got := SessionKeyFromHeaders(http.Header{"Session-Id": []string{"c1"}}, defaultSessionHeader); got != "codex:c1" {
		t.Fatalf("codex header key = %q", got)
	}
}

func TestEnginePickRecoversFromPanic(t *testing.T) {
	e, _ := newTestEngine(ModeEnabled)
	e.now = func() time.Time { panic("boom") }
	resp := e.Pick(pickReq("codex", "m", nil, "a"))
	if resp.Handled {
		t.Fatal("panic must decline, not propagate")
	}
	if e.declines.Load() != 1 {
		t.Fatalf("declines = %d", e.declines.Load())
	}
}

func TestDecodeRuntimeConfig(t *testing.T) {
	cfg, errDecode := decodeRuntimeConfig([]byte("mode: enabled\npoll_interval_s: 30\nfable_reserve_mode: shadow\nproviders: [codex, Claude]\nsession_header: X-Foo\n"))
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	if cfg.Mode != ModeEnabled || cfg.PollInterval != minPollInterval || cfg.Score.FableReserveMode != "shadow" || !cfg.Providers["claude"] || cfg.Providers["xai"] || cfg.SessionHeader != "X-Foo" {
		t.Fatalf("cfg = %+v", cfg)
	}
	cfg, _ = decodeRuntimeConfig([]byte("mode: bogus\n"))
	if cfg.Mode != ModeShadow {
		t.Fatalf("unknown mode -> shadow, got %q", cfg.Mode)
	}
	cfg, _ = decodeRuntimeConfig(nil)
	if cfg.Mode != ModeShadow || !cfg.Polling {
		t.Fatalf("defaults = %+v", cfg)
	}
	cfg, _ = decodeRuntimeConfig([]byte("mode: off\n"))
	if cfg.Polling {
		t.Fatal("off must not poll")
	}
}

func TestRegistrationCapabilities(t *testing.T) {
	reg := pluginRegistration()
	for _, cap := range []string{"scheduler", "scheduler_across_priorities", "request_interceptor", "usage_plugin", "management_api"} {
		if reg.Capabilities[cap] != true {
			t.Fatalf("capability %s missing", cap)
		}
	}
	if !strings.EqualFold(reg.Metadata.Name, pluginName) {
		t.Fatalf("name = %q", reg.Metadata.Name)
	}
}
