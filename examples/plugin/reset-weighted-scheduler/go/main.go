package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"unsafe"

	rw "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/resetweighted"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// main.go: the C ABI shell and RPC method dispatch. Everything routing-related
// lives in sdk/cliproxy/resetweighted (cgo-free, unit-tested in CI).

const (
	pluginName    = rw.PluginName
	pluginVersion = "0.1.0"
)

var (
	engineMu sync.Mutex
	engine   *rw.Engine
	poller   *rw.Poller
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	_ = length
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	engineMu.Lock()
	defer engineMu.Unlock()
	if poller != nil {
		poller.Stop()
		poller = nil
	}
	if engine != nil {
		_ = engine.FlushAffinity()
	}
}

// callHost is the HostCaller bound to the C host API. It unwraps the envelope.
func callHost(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var req *C.uint8_t
	if len(payload) > 0 {
		req = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(req))
	}
	rc := C.call_host_api(cMethod, req, C.size_t(len(payload)), &response)
	var raw []byte
	if response.ptr != nil {
		raw = C.GoBytes(response.ptr, C.int(response.len))
		C.free_host_buffer(response.ptr, response.len)
	}
	if rc != 0 && len(raw) == 0 {
		return nil, fmt.Errorf("host call %s failed rc=%d", method, int(rc))
	}
	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("host call %s: bad envelope: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("host call %s: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host call %s: error", method)
	}
	return env.Result, nil
}

func hostLog(level, message string, fields map[string]any) {
	payload, errMarshal := json.Marshal(map[string]any{"level": level, "message": message, "fields": fields})
	if errMarshal != nil {
		return
	}
	_, _ = callHost(pluginabi.MethodHostLog, payload)
}

func handleMethod(method string, request []byte) (raw []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			// A panic that escapes the plugin kills the proxy. Decline instead.
			raw, err = declineFor(method), nil
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodSchedulerPick:
		return handlePick(request)
	case pluginabi.MethodRequestInterceptBefore:
		return handleInterceptBefore(request)
	case pluginabi.MethodRequestInterceptAfter:
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodManagementRegister:
		return handleManagementRegister(request)
	case pluginabi.MethodManagementHandle:
		return handleManagementHandle(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// declineFor is the neutral answer for a method whose handler panicked.
func declineFor(method string) []byte {
	switch method {
	case pluginabi.MethodSchedulerPick:
		raw, _ := okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
		return raw
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		raw, _ := okEnvelope(pluginapi.RequestInterceptResponse{})
		return raw
	case pluginabi.MethodManagementHandle:
		raw, _ := okEnvelope(pluginapi.ManagementResponse{StatusCode: 500, Body: []byte(`{"error":"plugin panic"}`)})
		return raw
	default:
		raw, _ := okEnvelope(struct{}{})
		return raw
	}
}

func configure(raw []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg, errDecode := rw.DecodeRuntimeConfig(req.ConfigYAML)
	if errDecode != nil {
		return errDecode
	}
	engineMu.Lock()
	defer engineMu.Unlock()
	if engine == nil {
		engine = rw.NewEngine(cfg, hostLog)
	} else {
		engine.Reconfigure(cfg)
	}
	if poller != nil {
		poller.Stop()
		poller = nil
	}
	if cfg.Polling {
		poller = rw.NewPoller(engine, callHost)
		poller.Start()
	} else if len(cfg.QuotaSeeds) > 0 {
		// Bench mode: static quota, no vendor traffic. Applied asynchronously because the
		// host's auth manager is not attached yet while plugin.register runs at boot.
		go seedQuotaWhenHostReady(engine)
	}
	hostLog("info", "reset-weighted-scheduler: configured", map[string]any{
		"event": "configured", "mode": cfg.Mode, "fable_reserve_mode": cfg.Score.FableReserveMode,
		"poll_interval_s": cfg.PollInterval.Seconds(), "polling": cfg.Polling, "session_header": cfg.SessionHeader,
	})
	return nil
}

func seedQuotaWhenHostReady(e *rw.Engine) {
	defer func() { _ = recover() }()
	var lastErr string
	for attempt := 0; attempt < 120; attempt++ {
		raw, errCall := callHost(pluginabi.MethodHostAuthList, []byte(`{}`))
		if errCall != nil {
			lastErr = errCall.Error()
			if attempt == 0 || attempt == 10 {
				hostLog("debug", "reset-weighted-scheduler: host.auth.list not ready", map[string]any{"event": "quota_seed_wait", "error": lastErr, "attempt": attempt})
			}
		} else {
			var resp struct {
				Files []pluginapi.HostAuthFileEntry `json:"files"`
			}
			if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
				lastErr = errUnmarshal.Error()
			} else if applied := e.ApplyQuotaSeeds(resp.Files); applied > 0 {
				hostLog("info", "reset-weighted-scheduler: quota seeds applied", map[string]any{"event": "quota_seed", "applied": applied, "auths": len(resp.Files), "attempt": attempt})
				return
			} else {
				lastErr = fmt.Sprintf("no seed matched %d host auths", len(resp.Files))
			}
		}
		time.Sleep(time.Second)
	}
	hostLog("warn", "reset-weighted-scheduler: quota seeds not applied", map[string]any{"event": "quota_seed", "applied": 0, "error": lastErr})
}

func currentEngine() *rw.Engine {
	engineMu.Lock()
	defer engineMu.Unlock()
	return engine
}

func handlePick(raw []byte) ([]byte, error) {
	e := currentEngine()
	var req pluginapi.SchedulerPickRequest
	if e == nil || json.Unmarshal(raw, &req) != nil {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	return okEnvelope(e.Pick(req))
}

func handleInterceptBefore(raw []byte) ([]byte, error) {
	e := currentEngine()
	var req pluginapi.RequestInterceptRequest
	if e == nil || json.Unmarshal(raw, &req) != nil {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	return okEnvelope(e.InterceptBefore(req))
}

func handleUsage(raw []byte) ([]byte, error) {
	e := currentEngine()
	var record pluginapi.UsageRecord
	if e != nil && json.Unmarshal(raw, &record) == nil {
		e.HandleUsage(record)
	}
	return okEnvelope(struct{}{})
}

func handleManagementRegister(raw []byte) ([]byte, error) {
	_ = raw
	return okEnvelope(map[string]any{
		"routes": []map[string]any{
			{"Method": "GET", "Path": "/reset-weighted-scheduler/status", "Description": "Per-credential headroom/urgency/reclaim and pick counters."},
		},
		"resources": []map[string]any{
			{"Path": "/status", "Menu": "Reset-Weighted Scheduler", "Description": "Per-credential headroom/urgency/reclaim and pick counters."},
		},
	})
}

func handleManagementHandle(raw []byte) ([]byte, error) {
	e := currentEngine()
	if e == nil {
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: 503, Body: []byte(`{"error":"not configured"}`)})
	}
	body, errMarshal := json.Marshal(e.Status())
	if errMarshal != nil {
		return nil, errMarshal
	}
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body})
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]any     `json:"capabilities"`
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "ANG-Ventures",
			GitHubRepository: "https://github.com/ANG-Ventures/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{rw.ModeOff, rw.ModeShadow, rw.ModeEnabled}, Description: "off: decline every pick. shadow (default): score and log, routing unchanged. enabled: the plugin picks."},
				{Name: "fable_reserve_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"off", "shadow", "enforce"}, Description: "Fable (7d overage-included) reservation: enforce withholds reserved seats from non-Fable work and ranks them first for Fable work."},
				{Name: "fable_share", Type: pluginapi.ConfigFieldTypeNumber, Description: "Fable allowance as a fraction of the total weekly window (default 0.5)."},
				{Name: "fable_reserve_margin_pct", Type: pluginapi.ConfigFieldTypeNumber, Description: "Extra total-window percent held back before a seat counts as reserved (default 5)."},
				{Name: "horizon_fraction", Type: pluginapi.ConfigFieldTypeNumber, Description: "Urgency horizon as a fraction of the window span (default 3/7: 72h on a 7-day window, ~12.9d on Kimi's 30-day window)."},
				{Name: "short_guard_pct", Type: pluginapi.ConfigFieldTypeNumber, Description: "5-hour window utilization at/over which a seat is excluded from NEW picks when an alternative exists (default 85)."},
				{Name: "balance_k", Type: pluginapi.ConfigFieldTypeNumber, Description: "Multiplicative inflight damp strength (default 0.5)."},
				{Name: "base_floor", Type: pluginapi.ConfigFieldTypeNumber, Description: "Score floor so a zero-reclaim seat stays pickable (default 0.05)."},
				{Name: "score_eps", Type: pluginapi.ConfigFieldTypeNumber, Description: "Scores within eps tie-break on limiting-window headroom, then host priority (default 0.02)."},
				{Name: "snapshot_max_age_s", Type: pluginapi.ConfigFieldTypeNumber, Description: "A quota snapshot older than this is unknown: reclaim 0, fail-open (default 900)."},
				{Name: "poll_interval_s", Type: pluginapi.ConfigFieldTypeNumber, Description: "Quota poll cadence; floored at 300 s (vendor rate limits)."},
				{Name: "usage_ace_url", Type: pluginapi.ConfigFieldTypeString, Description: "Claude quota feed (usage.ace schema 8). cpa never polls cloud Claude subs directly."},
				{Name: "claude_key_map", Type: pluginapi.ConfigFieldTypeObject, Description: "auth id or email -> usage.ace account key, when the label does not contain it."},
				{Name: "affinity_path", Type: pluginapi.ConfigFieldTypeString, Description: "Persist session -> credential bindings here (empty: memory only)."},
				{Name: "affinity_max", Type: pluginapi.ConfigFieldTypeInteger, Description: "LRU cap on bindings (default 10000)."},
				{Name: "session_header", Type: pluginapi.ConfigFieldTypeString, Description: "Header intercept_before injects with the body-derived session id for pick (default X-Rws-Session)."},
				{Name: "providers", Type: pluginapi.ConfigFieldTypeArray, Description: "Providers the plugin scores (default codex, claude, xai, kimi, antigravity). Others decline to the built-in scheduler."},
				{Name: "disable_polling", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Skip vendor polling (tests / an external feeder)."},
			},
		},
		Capabilities: rw.Capabilities(),
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := pluginabi.NewErrorEnvelope(code, message)
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
