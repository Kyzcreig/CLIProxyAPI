package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Drives the RPC shell with the JSON shapes the host sends; the aliasing
// itself is covered in sdk/cliproxy/brandalias and ./test (DPXAliasLane).

func register(t *testing.T, yaml string) ([]byte, error) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"schema_version": pluginabi.SchemaVersion, "config_yaml": []byte(yaml)})
	return handleMethod(pluginabi.MethodPluginRegister, raw)
}

func call[T any](t *testing.T, method string, req any) T {
	t.Helper()
	raw, _ := json.Marshal(req)
	out, err := handleMethod(method, raw)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("%s: bad envelope %s", method, out)
	}
	var result T
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("%s: result %s: %v", method, env.Result, err)
	}
	return result
}

func TestRegisterDeclaresInterceptorCapabilities(t *testing.T) {
	out, err := register(t, "enabled: true\nmode: enabled\nlanes: [antigravity]\n")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool         `json:"ok"`
		Result registration `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("register: %s", out)
	}
	caps := env.Result.Capabilities
	if !caps.RequestInterceptor || !caps.ResponseInterceptor || !caps.StreamChunkInterceptor || !caps.RequestLifecyclePlugin {
		t.Fatalf("capabilities: %+v", caps)
	}
	if env.Result.Metadata.Name != "dpx-alias" {
		t.Fatalf("name %q", env.Result.Metadata.Name)
	}
}

func TestRegisterRefusesPolicyExceptionLane(t *testing.T) {
	if _, err := register(t, "enabled: true\nlanes: [kimi]\n"); err == nil || !strings.Contains(err.Error(), "OFF by policy") {
		t.Fatalf("expected a policy refusal, got %v", err)
	}
	if _, err := register(t, "enabled: true\nmode: sideways\n"); err == nil {
		t.Fatal("expected a mode refusal")
	}
}

func TestInterceptRoundTripOverRPC(t *testing.T) {
	if _, err := register(t, "enabled: true\nmode: enabled\nlanes: [antigravity]\nprincipal: rpc\nsession: test\n"); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"gemini-3-flash","messages":[{"role":"user","content":"hi hermes"}],"tools":[{"type":"function","function":{"name":"hermes_memory","parameters":{"type":"object"}}}]}`)
	before := call[pluginapi.RequestInterceptResponse](t, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{RequestID: "r", Body: body})
	if len(before.Body) != 0 || before.Headers != nil {
		t.Fatalf("before-auth must be a pass-through: %+v", before)
	}
	after := call[pluginapi.RequestInterceptResponse](t, pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{RequestID: "r", SourceFormat: "openai", ToFormat: "antigravity", Body: body})
	if len(after.Body) == 0 || strings.Contains(strings.ToLower(string(after.Body)), "hermes") || after.Headers != nil || after.Terminate {
		t.Fatalf("after-auth: %+v %s", after, after.Body)
	}
	var aliased struct {
		Tools []struct {
			Function struct{ Name string } `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(after.Body, &aliased)
	alias := aliased.Tools[0].Function.Name
	resp := call[pluginapi.ResponseInterceptResponse](t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{RequestID: "r", SourceFormat: "openai", Body: []byte(`{"choices":[{"message":{"content":"` + alias + `"}}]}`)})
	if string(resp.Body) != `{"choices":[{"message":{"content":"hermes_memory"}}]}` {
		t.Fatalf("response: %s", resp.Body)
	}
	chunk := call[pluginapi.StreamChunkInterceptResponse](t, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{RequestID: "r", SourceFormat: "openai", ChunkIndex: 0, Body: []byte(`{"choices":[{"index":0,"delta":{"content":"` + alias + ` "},"finish_reason":"stop"}]}`)})
	if string(chunk.Body) != `{"choices":[{"index":0,"delta":{"content":"hermes_memory "},"finish_reason":"stop"}]}` {
		t.Fatalf("chunk: %s", chunk.Body)
	}
	call[struct{}](t, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: "r"})
	gone := call[pluginapi.ResponseInterceptResponse](t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{RequestID: "r", SourceFormat: "openai", Body: []byte(`{"choices":[{"message":{"content":"` + alias + `"}}]}`)})
	if len(gone.Body) != 0 {
		t.Fatalf("state must be dropped after request.complete: %s", gone.Body)
	}
}

// Garbage on an interceptor method is a pass-through, never an error envelope
// (an error envelope hard-fails the request at the host).
func TestInterceptorMethodsNeverError(t *testing.T) {
	if _, err := register(t, "enabled: true\nmode: enabled\nlanes: [antigravity]\n"); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{pluginabi.MethodRequestInterceptAfter, pluginabi.MethodResponseInterceptAfter, pluginabi.MethodResponseInterceptStreamChunk, pluginabi.MethodRequestComplete} {
		out, err := handleMethod(method, []byte(`not json`))
		if err != nil {
			t.Fatalf("%s returned an error: %v", method, err)
		}
		var env envelope
		if json.Unmarshal(out, &env) != nil || !env.OK {
			t.Fatalf("%s: %s", method, out)
		}
	}
}
