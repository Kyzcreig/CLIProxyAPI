package executor

// D4 guard tests (spec one-cliproxyapi-lineage §7 Phase 2 Negative (a)/(b), AC9):
// a `fleet` binary with dpx-content-alias.enabled under anything but the
// single-principal daemon shape refuses the first confirmed-native, uncloaked
// Claude request with unsafe_daemon_config — never masked by
// native_route_required — and dispatches nothing upstream. One test per
// predicate arm, plus the shared-proxy (:18812) shape as a whole and the
// "auth-dir grows, config reloads, refusal trips" path through the real loader.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
)

// dpxShapeRun executes the fixture's confirmed-native request against a counting
// loopback upstream and returns the upstream call count and the error.
func dpxShapeRun(t *testing.T, mutate func(cfg *config.Config)) (int, error) {
	t.Helper()
	e, req, opts := aliasFixture(t)
	mutate(e.cfg)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, req.Model)
	}))
	defer upstream.Close()
	_, err := e.Execute(context.Background(), offlineAuth(upstream.URL), req, opts)
	return calls, err
}

func dpxAssertRefused(t *testing.T, name string, calls int, err error) {
	t.Helper()
	var aliasErr contentalias.Error
	if !errors.As(err, &aliasErr) {
		t.Fatalf("%s: err=%v (calls=%d), want contentalias unsafe_daemon_config", name, err, calls)
	}
	if aliasErr == "native_route_required" {
		t.Fatalf("%s: refusal masked by native_route_required (fixture must be confirmed-native, uncloaked)", name)
	}
	if aliasErr != "unsafe_daemon_config" {
		t.Fatalf("%s: err=%q, want unsafe_daemon_config", name, aliasErr)
	}
	if calls != 0 {
		t.Fatalf("%s: %d upstream call(s) despite refusal", name, calls)
	}
}

// TestDPXDaemonShapeBaselineServes pins the control: the fixture (principal,
// session, store, one credential, prompt-cache-policy "off") is served.
func TestDPXDaemonShapeBaselineServes(t *testing.T) {
	calls, err := dpxShapeRun(t, func(*config.Config) {})
	if err != nil || calls != 1 {
		t.Fatalf("baseline daemon shape: calls=%d err=%v, want served", calls, err)
	}
}

// TestDPXDaemonShapeSharedProxyRefuses is the :18812 shape under alias on:
// many credentials, bearer api-keys, 0.0.0.0, prompt-cache-policy shadow, no
// binding. The first confirmed-native request is refused before dispatch.
func TestDPXDaemonShapeSharedProxyRefuses(t *testing.T) {
	calls, err := dpxShapeRun(t, func(cfg *config.Config) {
		cfg.Host = "0.0.0.0"
		cfg.APIKeys = []string{"shared-proxy-bearer-test-key"}
		cfg.Routing.PromptCachePolicy = "shadow"
		cfg.DPXContentAlias.Principal = ""
		cfg.DPXContentAlias.SessionID = ""
		cfg.DPXContentAlias.AuthFileCount = 7
	})
	dpxAssertRefused(t, "shared-proxy shape", calls, err)
}

func TestDPXDaemonShapePrincipalEmptyRefuses(t *testing.T) {
	calls, err := dpxShapeRun(t, func(cfg *config.Config) { cfg.DPXContentAlias.Principal = "" })
	dpxAssertRefused(t, "principal empty", calls, err)
}

func TestDPXDaemonShapeSessionEmptyRefuses(t *testing.T) {
	calls, err := dpxShapeRun(t, func(cfg *config.Config) { cfg.DPXContentAlias.SessionID = "" })
	dpxAssertRefused(t, "session-id empty", calls, err)
}

func TestDPXDaemonShapeStoreEmptyRefuses(t *testing.T) {
	calls, err := dpxShapeRun(t, func(cfg *config.Config) { cfg.DPXContentAlias.StoreDirectory = "" })
	dpxAssertRefused(t, "store-directory empty", calls, err)
}

// TestDPXDaemonShapeMultiCredentialRefuses: one principal = one credential.
func TestDPXDaemonShapeMultiCredentialRefuses(t *testing.T) {
	for _, n := range []int{2, 3, config.DPXAuthDirUnreadable} {
		calls, err := dpxShapeRun(t, func(cfg *config.Config) { cfg.DPXContentAlias.AuthFileCount = n })
		dpxAssertRefused(t, fmt.Sprintf("auth-file count %d", n), calls, err)
	}
	for _, n := range []int{0, 1} {
		if calls, err := dpxShapeRun(t, func(cfg *config.Config) { cfg.DPXContentAlias.AuthFileCount = n }); err != nil || calls != 1 {
			t.Fatalf("auth-file count %d: calls=%d err=%v, want served", n, calls, err)
		}
	}
}

// TestDPXDaemonShapePromptCachePolicyRefuses: on an alias daemon the key is
// REQUIRED and only the literal "off" is accepted (D7 DPX rule). The absent
// arm asserts the REFUSAL error, not "no metadata".
func TestDPXDaemonShapePromptCachePolicyRefuses(t *testing.T) {
	for _, raw := range []string{"", "of", "shadow ", "Shadow", "Off", "shadow", "enforce"} {
		calls, err := dpxShapeRun(t, func(cfg *config.Config) { cfg.Routing.PromptCachePolicy = raw })
		dpxAssertRefused(t, fmt.Sprintf("prompt-cache-policy %q", raw), calls, err)
	}
}

// TestDPXDaemonShapeRetryLogArms keeps the original arms covered one by one.
func TestDPXDaemonShapeRetryLogArms(t *testing.T) {
	arms := map[string]func(cfg *config.Config){
		"request-log":           func(cfg *config.Config) { cfg.RequestLog = true },
		"debug":                 func(cfg *config.Config) { cfg.Debug = true },
		"request-retry":         func(cfg *config.Config) { cfg.RequestRetry = 1 },
		"max-retry-credentials": func(cfg *config.Config) { cfg.MaxRetryCredentials = 2 },
	}
	for name, mutate := range arms {
		calls, err := dpxShapeRun(t, mutate)
		dpxAssertRefused(t, name, calls, err)
	}
}

// TestDPXDaemonShapeReSignOnlyUnitUnaffected: a declared d-family unit with
// alias OFF (lane gate + wirelog only, t_40200f8a) carries no binding and must
// not be refused by the alias-only arms.
func TestDPXDaemonShapeReSignOnlyUnitUnaffected(t *testing.T) {
	cfg := &config.Config{MaxRetryCredentials: 1, DPXContentAlias: config.DPXContentAlias{Enabled: false, Lane: "dslx", AuthFileCount: 3}}
	if dpxUnsafeDaemonConfig(cfg) {
		t.Fatal("alias-off unit refused by alias-only arms")
	}
	cfg.DPXContentAlias.Enabled = true
	if !dpxUnsafeDaemonConfig(cfg) {
		t.Fatal("alias-on without binding not refused")
	}
}

// TestDPXDaemonShapeAuthDirGrowsOnReload drives the real loader: a daemon
// config whose auth-dir holds one credential loads and serves; a second
// credential file appears; the next (re)load snapshots count=2 and the
// executor built from that snapshot refuses. The request path never reads the
// filesystem: the executor holding the OLD snapshot still serves until reload.
func TestDPXDaemonShapeAuthDirGrowsOnReload(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	if err := os.Chmod(store, 0o700); err != nil {
		t.Fatal(err)
	}
	binding := contentalias.Binding{Principal: "one-local-principal", Session: "11111111-2222-4333-8444-555555555555", Version: "v1"}
	if _, err := contentalias.Create(store, binding, contentalias.DefaultManifest()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "claude-one.json"), []byte(`{"type":"claude"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "cpa.yaml")
	yamlDoc := "port: 18799\nhost: \"127.0.0.1\"\nauth-dir: \"" + authDir + "\"\nrequest-retry: 0\nmax-retry-credentials: 1\ndebug: false\nrequest-log: false\nrouting:\n  prompt-cache-policy: off\ndpx-content-alias:\n  enabled: true\n  store-directory: \"" + store + "\"\n  principal: \"" + binding.Principal + "\"\n  session-id: \"" + binding.Session + "\"\n  version: v1\n"
	if err := os.WriteFile(cfgPath, []byte(yamlDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	load := func() *config.Config {
		t.Helper()
		cfg, err := config.LoadConfig(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	run := func(cfg *config.Config) (int, error) {
		t.Helper()
		e, req, opts := aliasFixture(t)
		e.cfg = cfg
		calls := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, req.Model)
		}))
		defer upstream.Close()
		_, err := e.Execute(context.Background(), offlineAuth(upstream.URL), req, opts)
		return calls, err
	}

	first := load()
	if first.DPXContentAlias.AuthFileCount != 1 {
		t.Fatalf("loader snapshot = %d, want 1", first.DPXContentAlias.AuthFileCount)
	}
	if first.Routing.PromptCachePolicy != "off" {
		t.Fatalf("prompt-cache-policy loaded as %q", first.Routing.PromptCachePolicy)
	}
	if calls, err := run(first); err != nil || calls != 1 {
		t.Fatalf("one credential: calls=%d err=%v, want served", calls, err)
	}
	// Auth dir grows.
	if err := os.WriteFile(filepath.Join(authDir, "claude-two.json"), []byte(`{"type":"claude"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not a live filesystem check: the executor on the old snapshot still serves.
	if calls, err := run(first); err != nil || calls != 1 {
		t.Fatalf("old snapshot after growth: calls=%d err=%v, want served until reload", calls, err)
	}
	second := load()
	if second.DPXContentAlias.AuthFileCount != 2 {
		t.Fatalf("reload snapshot = %d, want 2", second.DPXContentAlias.AuthFileCount)
	}
	calls, err := run(second)
	dpxAssertRefused(t, "auth-dir grew, config reloaded", calls, err)
	// The snapshot survives the runtime clone the service hands executors.
	if cloned := second.CloneForRuntime(); cloned.DPXContentAlias.AuthFileCount != 2 {
		t.Fatalf("CloneForRuntime dropped AuthFileCount: %d", cloned.DPXContentAlias.AuthFileCount)
	}
}

// TestDPXDaemonShapeLoaderSkipsAliasOff: an alias-off config never snapshots
// its auth-dir (the loader path stays upstream's for :18812).
func TestDPXDaemonShapeLoaderSkipsAliasOff(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.json", "b.json", "c.json"} {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "cpa.yaml")
	if err := os.WriteFile(cfgPath, []byte("port: 18812\nauth-dir: \""+authDir+"\"\nrouting:\n  prompt-cache-policy: shadow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DPXContentAlias.AuthFileCount != 0 {
		t.Fatalf("alias-off config snapshotted auth-dir: %d", cfg.DPXContentAlias.AuthFileCount)
	}
	if config.CountDPXAuthFiles(authDir) != 3 {
		t.Fatalf("CountDPXAuthFiles(%s) != 3", authDir)
	}
	if config.CountDPXAuthFiles(filepath.Join(dir, "missing")) != 0 {
		t.Fatal("missing auth-dir must count 0")
	}
}
