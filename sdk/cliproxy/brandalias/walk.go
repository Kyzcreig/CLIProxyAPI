package brandalias

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// textFn rewrites one text field (encode on the way out, decode on the way
// back). A non-nil error aborts the walk; the caller then passes the payload
// through untouched.
type textFn func(string) (string, error)

// A walker knows where the prose, tool names and tool descriptions live in one
// source-format request/response body. Everything it does not name is never
// touched: model ids, metadata, identity fields, signed blocks, headers.
type walker interface {
	// Request rewrites the outbound request body.
	Request(raw []byte, f textFn) ([]byte, error)
	// Response rewrites a non-streaming response body.
	Response(raw []byte, f textFn) ([]byte, error)
}

// walkers by source format (sdk/translator Format strings). openai-response
// (Responses API) and interactions are not walked in v1: such requests pass
// through with reason unsupported_source_format.
var walkers = map[string]walker{
	"openai": openaiChat{},
	"claude": claudeMessages{},
	"gemini": geminiGenerate{},
}

func walkerFor(sourceFormat string) walker {
	return walkers[strings.ToLower(strings.TrimSpace(sourceFormat))]
}

// setText applies f to the string at path (no-op for a missing or non-string
// value) and writes it back only when it changed.
func setText(raw []byte, path string, f textFn) ([]byte, error) {
	v := gjson.GetBytes(raw, path)
	if v.Type != gjson.String {
		return raw, nil
	}
	out, err := f(v.Str)
	if err != nil {
		return nil, err
	}
	if out == v.Str {
		return raw, nil
	}
	return sjson.SetBytes(raw, path, out)
}

// setTextOrParts handles a content that is either a string or an array of
// parts whose text lives under textKey.
func setTextOrParts(raw []byte, path, textKey string, f textFn) ([]byte, error) {
	v := gjson.GetBytes(raw, path)
	switch {
	case v.Type == gjson.String:
		return setText(raw, path, f)
	case v.IsArray():
		var err error
		for i := range v.Array() {
			raw, err = setText(raw, fmt.Sprintf("%s.%d.%s", path, i, textKey), f)
			if err != nil {
				return nil, err
			}
		}
	}
	return raw, nil
}

// setStringValues applies f to every string VALUE under path (keys are never
// touched): tool call arguments and tool results.
func setStringValues(raw []byte, path string, f textFn) ([]byte, error) {
	v := gjson.GetBytes(raw, path)
	switch {
	case v.Type == gjson.String:
		return setText(raw, path, f)
	case v.IsObject():
		var err error
		v.ForEach(func(key, _ gjson.Result) bool {
			raw, err = setStringValues(raw, path+"."+gjson.Escape(key.Str), f)
			return err == nil
		})
		return raw, err
	case v.IsArray():
		var err error
		for i := range v.Array() {
			raw, err = setStringValues(raw, fmt.Sprintf("%s.%d", path, i), f)
			if err != nil {
				return nil, err
			}
		}
	}
	return raw, nil
}

// setSchemaDescriptions applies f to every "description" string inside a JSON
// schema. Property keys, enums, types and refs are left alone: a key that
// carries a brand word stays (counted by the wirelog, never silently lost).
func setSchemaDescriptions(raw []byte, path string, f textFn) ([]byte, error) {
	v := gjson.GetBytes(raw, path)
	switch {
	case v.IsObject():
		var err error
		v.ForEach(func(key, value gjson.Result) bool {
			child := path + "." + gjson.Escape(key.Str)
			if key.Str == "description" && value.Type == gjson.String {
				raw, err = setText(raw, child, f)
			} else {
				raw, err = setSchemaDescriptions(raw, child, f)
			}
			return err == nil
		})
		return raw, err
	case v.IsArray():
		var err error
		for i := range v.Array() {
			raw, err = setSchemaDescriptions(raw, fmt.Sprintf("%s.%d", path, i), f)
			if err != nil {
				return nil, err
			}
		}
	}
	return raw, nil
}

func count(raw []byte, path string) int { return len(gjson.GetBytes(raw, path).Array()) }

// --- openai chat completions -------------------------------------------------

type openaiChat struct{}

func (openaiChat) Request(raw []byte, f textFn) ([]byte, error) {
	var err error
	for i := 0; i < count(raw, "messages"); i++ {
		m := fmt.Sprintf("messages.%d", i)
		if raw, err = setTextOrParts(raw, m+".content", "text", f); err != nil {
			return nil, err
		}
		for j := 0; j < count(raw, m+".tool_calls"); j++ {
			tc := fmt.Sprintf("%s.tool_calls.%d.function", m, j)
			if raw, err = setText(raw, tc+".name", f); err != nil {
				return nil, err
			}
			if raw, err = setText(raw, tc+".arguments", f); err != nil {
				return nil, err
			}
		}
	}
	for i := 0; i < count(raw, "tools"); i++ {
		fn := fmt.Sprintf("tools.%d.function", i)
		for _, p := range []string{fn + ".name", fn + ".description"} {
			if raw, err = setText(raw, p, f); err != nil {
				return nil, err
			}
		}
		if raw, err = setSchemaDescriptions(raw, fn+".parameters", f); err != nil {
			return nil, err
		}
	}
	return setText(raw, "tool_choice.function.name", f)
}

func (openaiChat) Response(raw []byte, f textFn) ([]byte, error) {
	var err error
	for i := 0; i < count(raw, "choices"); i++ {
		msg := fmt.Sprintf("choices.%d.message", i)
		if raw, err = setTextOrParts(raw, msg+".content", "text", f); err != nil {
			return nil, err
		}
		if raw, err = setText(raw, msg+".reasoning_content", f); err != nil {
			return nil, err
		}
		for j := 0; j < count(raw, msg+".tool_calls"); j++ {
			tc := fmt.Sprintf("%s.tool_calls.%d.function", msg, j)
			if raw, err = setText(raw, tc+".name", f); err != nil {
				return nil, err
			}
			if raw, err = setText(raw, tc+".arguments", f); err != nil {
				return nil, err
			}
		}
	}
	return raw, nil
}

// --- claude messages ---------------------------------------------------------

type claudeMessages struct{}

func (claudeMessages) Request(raw []byte, f textFn) ([]byte, error) {
	var err error
	if raw, err = setTextOrParts(raw, "system", "text", f); err != nil {
		return nil, err
	}
	for i := 0; i < count(raw, "messages"); i++ {
		if raw, err = claudeContent(raw, fmt.Sprintf("messages.%d.content", i), f); err != nil {
			return nil, err
		}
	}
	for i := 0; i < count(raw, "tools"); i++ {
		t := fmt.Sprintf("tools.%d", i)
		// Provider-defined tools (type != custom) are opaque; their names are
		// the vendor's.
		if typ := gjson.GetBytes(raw, t+".type").Str; typ != "" && typ != "custom" {
			continue
		}
		for _, p := range []string{t + ".name", t + ".description"} {
			if raw, err = setText(raw, p, f); err != nil {
				return nil, err
			}
		}
		if raw, err = setSchemaDescriptions(raw, t+".input_schema", f); err != nil {
			return nil, err
		}
	}
	if gjson.GetBytes(raw, "tool_choice.type").Str == "tool" {
		return setText(raw, "tool_choice.name", f)
	}
	return raw, nil
}

func (claudeMessages) Response(raw []byte, f textFn) ([]byte, error) {
	return claudeContent(raw, "content", f)
}

// claudeContent walks a Claude content value (string or block list). Signed
// blocks (thinking, redacted_thinking, anything carrying a signature) and
// non-text media are never touched.
func claudeContent(raw []byte, path string, f textFn) ([]byte, error) {
	v := gjson.GetBytes(raw, path)
	if v.Type == gjson.String {
		return setText(raw, path, f)
	}
	if !v.IsArray() {
		return raw, nil
	}
	var err error
	for i, block := range v.Array() {
		if block.Get("signature").Exists() {
			continue
		}
		b := fmt.Sprintf("%s.%d", path, i)
		switch block.Get("type").Str {
		case "text":
			raw, err = setText(raw, b+".text", f)
		case "tool_use", "server_tool_use":
			if raw, err = setText(raw, b+".name", f); err != nil {
				return nil, err
			}
			raw, err = setStringValues(raw, b+".input", f)
		case "tool_result":
			raw, err = setTextOrParts(raw, b+".content", "text", f)
		}
		if err != nil {
			return nil, err
		}
	}
	return raw, nil
}

// --- gemini generateContent --------------------------------------------------

type geminiGenerate struct{}

func (geminiGenerate) Request(raw []byte, f textFn) ([]byte, error) {
	var err error
	for _, si := range []string{"systemInstruction", "system_instruction"} {
		if raw, err = geminiParts(raw, si+".parts", f); err != nil {
			return nil, err
		}
	}
	for i := 0; i < count(raw, "contents"); i++ {
		if raw, err = geminiParts(raw, fmt.Sprintf("contents.%d.parts", i), f); err != nil {
			return nil, err
		}
	}
	for i := 0; i < count(raw, "tools"); i++ {
		for _, decl := range []string{"functionDeclarations", "function_declarations"} {
			base := fmt.Sprintf("tools.%d.%s", i, decl)
			for j := 0; j < count(raw, base); j++ {
				d := fmt.Sprintf("%s.%d", base, j)
				for _, p := range []string{d + ".name", d + ".description"} {
					if raw, err = setText(raw, p, f); err != nil {
						return nil, err
					}
				}
				for _, schema := range []string{"parameters", "parametersJsonSchema", "response", "responseJsonSchema"} {
					if raw, err = setSchemaDescriptions(raw, d+"."+schema, f); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	for _, names := range []string{"toolConfig.functionCallingConfig.allowedFunctionNames", "tool_config.function_calling_config.allowed_function_names"} {
		for i := 0; i < count(raw, names); i++ {
			if raw, err = setText(raw, fmt.Sprintf("%s.%d", names, i), f); err != nil {
				return nil, err
			}
		}
	}
	return raw, nil
}

func (geminiGenerate) Response(raw []byte, f textFn) ([]byte, error) {
	var err error
	for i := 0; i < count(raw, "candidates"); i++ {
		if raw, err = geminiParts(raw, fmt.Sprintf("candidates.%d.content.parts", i), f); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

// geminiParts walks a parts array: text (unless the part is a thought or
// carries a thought signature), functionCall name + args, functionResponse
// name + response values. inlineData/fileData are opaque.
func geminiParts(raw []byte, path string, f textFn) ([]byte, error) {
	v := gjson.GetBytes(raw, path)
	if !v.IsArray() {
		return raw, nil
	}
	var err error
	for i, part := range v.Array() {
		if part.Get("thought").Bool() || part.Get("thoughtSignature").Exists() || part.Get("thought_signature").Exists() {
			continue
		}
		p := fmt.Sprintf("%s.%d", path, i)
		if raw, err = setText(raw, p+".text", f); err != nil {
			return nil, err
		}
		for _, call := range []string{"functionCall", "function_call"} {
			if raw, err = setText(raw, p+"."+call+".name", f); err != nil {
				return nil, err
			}
			if raw, err = setStringValues(raw, p+"."+call+".args", f); err != nil {
				return nil, err
			}
		}
		for _, resp := range []string{"functionResponse", "function_response"} {
			if raw, err = setText(raw, p+"."+resp+".name", f); err != nil {
				return nil, err
			}
			if raw, err = setStringValues(raw, p+"."+resp+".response", f); err != nil {
				return nil, err
			}
		}
	}
	return raw, nil
}
