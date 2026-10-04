package brandalias

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// streamState is one response stream's inverse-map state: a boundary-safe
// text decoder per text channel (a symbol may straddle two deltas) and, for
// Claude, the block kinds seen so far.
type streamState struct {
	codec    *contentalias.WordCodec
	decoders map[string]*contentalias.TextDecoder
	kinds    map[string]string
}

func newStreamState(codec *contentalias.WordCodec) *streamState {
	return &streamState{codec: codec, decoders: map[string]*contentalias.TextDecoder{}, kinds: map[string]string{}}
}

func (s *streamState) decoder(key string) *contentalias.TextDecoder {
	d := s.decoders[key]
	if d == nil {
		d = s.codec.NewTextDecoder()
		s.decoders[key] = d
	}
	return d
}

// flush releases the decoder's held text and forgets it.
func (s *streamState) flush(key string) string {
	d := s.decoders[key]
	if d == nil {
		return ""
	}
	delete(s.decoders, key)
	return d.Flush()
}

// streamRestorer restores one stream chunk of a source format.
type streamRestorer func(raw []byte, st *streamState) ([]byte, error)

var streamRestorers = map[string]streamRestorer{
	"openai": openaiChunk,
	"claude": claudeChunk,
	"gemini": geminiChunk,
}

// feedText runs the decoder for key over the string at path.
func feedText(raw []byte, path, key string, st *streamState) ([]byte, error) {
	v := gjson.GetBytes(raw, path)
	if v.Type != gjson.String {
		return raw, nil
	}
	out := st.decoder(key).Feed(v.Str)
	if out == v.Str {
		return raw, nil
	}
	return sjson.SetBytes(raw, path, out)
}

// appendText appends held text to the string at path, creating it when the
// field is absent or null.
func appendText(raw []byte, path, held string) ([]byte, error) {
	if held == "" {
		return raw, nil
	}
	v := gjson.GetBytes(raw, path)
	if v.Type == gjson.String {
		return sjson.SetBytes(raw, path, v.Str+held)
	}
	return sjson.SetBytes(raw, path, held)
}

// --- openai chat completion chunks (raw JSON object per chunk) --------------

func openaiChunk(raw []byte, st *streamState) ([]byte, error) {
	if !gjson.ValidBytes(raw) || !gjson.ParseBytes(raw).IsObject() {
		return raw, nil
	}
	var err error
	for i, choice := range gjson.GetBytes(raw, "choices").Array() {
		idx := int(choice.Get("index").Int())
		if !choice.Get("index").Exists() {
			idx = i
		}
		c := fmt.Sprintf("choices.%d", i)
		keyContent, keyReasoning := fmt.Sprintf("c%d", idx), fmt.Sprintf("r%d", idx)
		if raw, err = feedText(raw, c+".delta.content", keyContent, st); err != nil {
			return nil, err
		}
		if raw, err = feedText(raw, c+".delta.reasoning_content", keyReasoning, st); err != nil {
			return nil, err
		}
		for j, call := range gjson.GetBytes(raw, c+".delta.tool_calls").Array() {
			tcIdx := int(call.Get("index").Int())
			if !call.Get("index").Exists() {
				tcIdx = j
			}
			tc := fmt.Sprintf("%s.delta.tool_calls.%d", c, j)
			for _, p := range []string{tc + ".id", tc + ".function.name"} {
				if v := gjson.GetBytes(raw, p); v.Type == gjson.String {
					if decoded := st.codec.Decode(v.Str); decoded != v.Str {
						if raw, err = sjson.SetBytes(raw, p, decoded); err != nil {
							return nil, err
						}
					}
				}
			}
			if raw, err = feedText(raw, tc+".function.arguments", fmt.Sprintf("c%d.t%d", idx, tcIdx), st); err != nil {
				return nil, err
			}
		}
		finish := gjson.GetBytes(raw, c+".finish_reason")
		if !finish.Exists() || finish.Type == gjson.Null {
			continue
		}
		// Terminal chunk for this choice: release everything held.
		if raw, err = appendText(raw, c+".delta.content", st.flush(keyContent)); err != nil {
			return nil, err
		}
		if raw, err = appendText(raw, c+".delta.reasoning_content", st.flush(keyReasoning)); err != nil {
			return nil, err
		}
		prefix := fmt.Sprintf("c%d.t", idx)
		for key := range st.decoders {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			tcIdx, errIdx := strconv.Atoi(strings.TrimPrefix(key, prefix))
			if errIdx != nil {
				continue
			}
			held := st.flush(key)
			if held == "" {
				continue
			}
			slot := -1
			for j, call := range gjson.GetBytes(raw, c+".delta.tool_calls").Array() {
				if int(call.Get("index").Int()) == tcIdx {
					slot = j
					break
				}
			}
			if slot < 0 {
				slot = count(raw, c+".delta.tool_calls")
				if raw, err = sjson.SetBytes(raw, fmt.Sprintf("%s.delta.tool_calls.%d", c, slot), map[string]any{"index": tcIdx, "function": map[string]any{"arguments": ""}}); err != nil {
					return nil, err
				}
			}
			if raw, err = appendText(raw, fmt.Sprintf("%s.delta.tool_calls.%d.function.arguments", c, slot), held); err != nil {
				return nil, err
			}
		}
	}
	return raw, nil
}

// --- claude SSE events (one or more complete events per chunk) ---------------

func claudeChunk(raw []byte, st *streamState) ([]byte, error) {
	var out []byte
	rest := raw
	changed := false
	for len(rest) > 0 {
		end := bytes.Index(rest, []byte("\n\n"))
		var event []byte
		if end < 0 {
			event, rest = rest, nil
		} else {
			event, rest = rest[:end+2], rest[end+2:]
		}
		replaced, ok, err := claudeEvent(event, st)
		if err != nil {
			return nil, err
		}
		if ok {
			changed = true
			out = append(out, replaced...)
		} else {
			out = append(out, event...)
		}
	}
	if !changed {
		return raw, nil
	}
	return out, nil
}

// claudeEvent returns the rewritten event bytes and true when it changed.
func claudeEvent(event []byte, st *streamState) ([]byte, bool, error) {
	var eventName string
	var data []byte
	dataLines := 0
	for _, line := range bytes.Split(bytes.TrimRight(event, "\n"), []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			eventName = string(bytes.TrimSpace(line[6:]))
		case bytes.HasPrefix(line, []byte("data:")):
			data = bytes.TrimPrefix(line[5:], []byte(" "))
			dataLines++
		}
	}
	if dataLines != 1 || !gjson.ValidBytes(data) {
		return nil, false, nil
	}
	typ := gjson.GetBytes(data, "type").Str
	if eventName == "" {
		eventName = typ
	}
	index := gjson.GetBytes(data, "index")
	idx := strconv.FormatInt(index.Int(), 10)
	var prelude []byte
	var err error
	rewritten := data
	switch typ {
	case "content_block_start":
		kind := gjson.GetBytes(data, "content_block.type").Str
		st.kinds[idx] = kind
		if kind == "tool_use" || kind == "server_tool_use" {
			for _, p := range []string{"content_block.id", "content_block.name"} {
				if v := gjson.GetBytes(data, p); v.Type == gjson.String {
					if decoded := st.codec.Decode(v.Str); decoded != v.Str {
						if rewritten, err = sjson.SetBytes(rewritten, p, decoded); err != nil {
							return nil, false, err
						}
					}
				}
			}
		}
	case "content_block_delta":
		switch gjson.GetBytes(data, "delta.type").Str {
		case "text_delta":
			rewritten, err = feedText(rewritten, "delta.text", "t"+idx, st)
		case "input_json_delta":
			rewritten, err = feedText(rewritten, "delta.partial_json", "j"+idx, st)
		}
		if err != nil {
			return nil, false, err
		}
	case "content_block_stop":
		if held := st.flush("t" + idx); held != "" {
			prelude = append(prelude, claudeDeltaEvent(index.Int(), "text_delta", "text", held)...)
		}
		if held := st.flush("j" + idx); held != "" {
			prelude = append(prelude, claudeDeltaEvent(index.Int(), "input_json_delta", "partial_json", held)...)
		}
		delete(st.kinds, idx)
	}
	if len(prelude) == 0 && bytes.Equal(rewritten, data) {
		return nil, false, nil
	}
	var out []byte
	out = append(out, prelude...)
	if bytes.Equal(rewritten, data) {
		out = append(out, event...)
	} else {
		out = append(out, []byte("event: "+eventName+"\ndata: ")...)
		out = append(out, rewritten...)
		out = append(out, []byte("\n\n")...)
	}
	return out, true, nil
}

func claudeDeltaEvent(index int64, deltaType, field, text string) []byte {
	payload, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": deltaType, field: text}})
	return append(append([]byte("event: content_block_delta\ndata: "), payload...), []byte("\n\n")...)
}

// --- gemini streamGenerateContent chunks (raw JSON object per chunk) ---------

func geminiChunk(raw []byte, st *streamState) ([]byte, error) {
	if !gjson.ValidBytes(raw) || !gjson.ParseBytes(raw).IsObject() {
		return raw, nil
	}
	var err error
	for i, cand := range gjson.GetBytes(raw, "candidates").Array() {
		idx := int(cand.Get("index").Int())
		if !cand.Get("index").Exists() {
			idx = i
		}
		c := fmt.Sprintf("candidates.%d", i)
		key := fmt.Sprintf("g%d", idx)
		lastText := -1
		for j, part := range gjson.GetBytes(raw, c+".content.parts").Array() {
			p := fmt.Sprintf("%s.content.parts.%d", c, j)
			if part.Get("thought").Bool() || part.Get("thoughtSignature").Exists() || part.Get("thought_signature").Exists() {
				continue
			}
			if part.Get("text").Type == gjson.String {
				lastText = j
				if raw, err = feedText(raw, p+".text", key, st); err != nil {
					return nil, err
				}
			}
			for _, call := range []string{"functionCall", "function_call"} {
				if name := gjson.GetBytes(raw, p+"."+call+".name"); name.Type == gjson.String {
					if decoded := st.codec.Decode(name.Str); decoded != name.Str {
						if raw, err = sjson.SetBytes(raw, p+"."+call+".name", decoded); err != nil {
							return nil, err
						}
					}
				}
				if raw, err = setStringValues(raw, p+"."+call+".args", func(s string) (string, error) { return st.codec.Decode(s), nil }); err != nil {
					return nil, err
				}
			}
		}
		if !gjson.GetBytes(raw, c+".finishReason").Exists() && !gjson.GetBytes(raw, c+".finish_reason").Exists() {
			continue
		}
		held := st.flush(key)
		if held == "" {
			continue
		}
		if lastText >= 0 {
			raw, err = appendText(raw, fmt.Sprintf("%s.content.parts.%d.text", c, lastText), held)
		} else {
			raw, err = sjson.SetBytes(raw, fmt.Sprintf("%s.content.parts.%d", c, count(raw, c+".content.parts")), map[string]any{"text": held})
		}
		if err != nil {
			return nil, err
		}
	}
	return raw, nil
}
