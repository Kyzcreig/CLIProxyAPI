package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
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
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/brandalias"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// dpx-alias: the cpa brand-alias interceptor plugin (card t_a37235c0). This
// file is the C ABI shell and method dispatch only; every decision lives in
// sdk/cliproxy/brandalias (cgo-free, unit-tested by the main module's CI).
//
// Contract with the host: an interceptor method NEVER returns an error
// envelope and never panics out of the shell. Any doubt is an empty (pass
// through) result; the brandalias wirelog names the reason.

const pluginVersion = "0.1.0"

var (
	engineMu sync.Mutex
	engine   *brandalias.Engine
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	RequestInterceptor     bool `json:"request_interceptor"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
	ResponseInterceptor    bool `json:"response_interceptor"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
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
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	engineMu.Lock()
	defer engineMu.Unlock()
	engine = nil
}

// handleMethod dispatches one RPC. Interceptor methods are wrapped so a panic
// becomes a pass-through, never a crash of the proxy that loaded us.
func handleMethod(method string, request []byte) (raw []byte, err error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			raw, err = okEnvelope(struct{}{})
		}
	}()
	switch method {
	case pluginabi.MethodRequestInterceptBefore:
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodRequestInterceptAfter:
		return interceptAfterAuth(request)
	case pluginabi.MethodResponseInterceptAfter:
		return interceptResponse(request)
	case pluginabi.MethodResponseInterceptStreamChunk:
		return interceptStreamChunk(request)
	case pluginabi.MethodRequestComplete:
		return completeRequest(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	if req.SchemaVersion < 2 {
		return fmt.Errorf("%s requires host schema version 2 or newer (request ids on interceptor calls)", brandalias.PluginName)
	}
	cfg, errParse := brandalias.ParseConfig(req.ConfigYAML)
	if errParse != nil {
		return errParse
	}
	engineMu.Lock()
	defer engineMu.Unlock()
	if engine == nil {
		engine = brandalias.NewEngine(cfg)
	} else {
		engine.Reconfigure(cfg)
	}
	return nil
}

func currentEngine() *brandalias.Engine {
	engineMu.Lock()
	defer engineMu.Unlock()
	return engine
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             brandalias.PluginName,
			Version:          pluginVersion,
			Author:           "Kyzcreig",
			GitHubRepository: "https://github.com/ANG-Ventures/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "mode", Type: pluginapi.ConfigFieldTypeString, Description: "shadow (measure only, default) or enabled (rewrite requests, restore responses)."},
				{Name: "lanes", Type: pluginapi.ConfigFieldTypeString, Description: "YAML list of lanes to alias: antigravity, gemini, claude. codex/openai/xai/kimi are OFF by policy and refused."},
				{Name: "principal", Type: pluginapi.ConfigFieldTypeString, Description: "Symbol-derivation binding (operator-owned). Default cpa."},
				{Name: "session", Type: pluginapi.ConfigFieldTypeString, Description: "Symbol-derivation binding (operator-owned). Default default."},
				{Name: "wirelog-spool", Type: pluginapi.ConfigFieldTypeString, Description: "Append-only JSONL of digest-only rows (lane, mode, reason, brandTokens). Empty = off."},
			},
		},
		Capabilities: registrationCapability{
			RequestInterceptor:     true,
			RequestLifecyclePlugin: true,
			ResponseInterceptor:    true,
			StreamChunkInterceptor: true,
		},
	}
}

func interceptAfterAuth(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	e := currentEngine()
	if e == nil {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	resp, _ := e.InterceptRequest(req)
	return okEnvelope(resp)
}

func interceptResponse(raw []byte) ([]byte, error) {
	var req pluginapi.ResponseInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	e := currentEngine()
	if e == nil {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	resp, _ := e.InterceptResponse(req)
	return okEnvelope(resp)
}

func interceptStreamChunk(raw []byte) ([]byte, error) {
	var req pluginapi.StreamChunkInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	e := currentEngine()
	if e == nil {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	resp, _ := e.InterceptStreamChunk(req)
	return okEnvelope(resp)
}

func completeRequest(raw []byte) ([]byte, error) {
	var completion pluginapi.RequestCompletion
	if errUnmarshal := json.Unmarshal(raw, &completion); errUnmarshal == nil {
		if e := currentEngine(); e != nil {
			e.Complete(completion.RequestID)
		}
	}
	return okEnvelope(struct{}{})
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, errMarshal := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
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
