package contentalias

import (
	"bytes"
	"encoding/json"
	"strings"
)

type RequestMap struct {
	st      state
	schemas map[string]*schema
	allowed map[string]bool
	reverse map[string]string
}

func canonical(raw []byte) string {
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}
func Prepare(raw []byte, s *Session) ([]byte, *RequestMap, error) {
	if s == nil {
		return bytes.Clone(raw), nil, nil
	}
	n, err := parse(raw)
	if err != nil {
		return nil, nil, err
	}
	if n.kind != '{' {
		return nil, nil, Error("request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.lock()
	if err != nil {
		return nil, nil, err
	}
	defer unlock(lock)
	st, err := s.load()
	if err != nil {
		return nil, nil, err
	}
	m := &RequestMap{st: st, schemas: map[string]*schema{}, allowed: map[string]bool{}, reverse: map[string]string{}}
	edits := []edit{}
	if tools := n.get("tools"); tools != nil {
		if tools.kind != '[' {
			return nil, nil, Error("tools")
		}
		seen := map[string]bool{}
		for _, tool := range tools.items {
			name := tool.get("name").str()
			if name == "" || seen[name] {
				return nil, nil, Error("tool_duplicate")
			}
			seen[name] = true
			if tool.has("type") && tool.get("type").str() != "custom" {
				continue
			}
			if strings.HasPrefix(name, "dpx_v1_") {
				return nil, nil, Error("reserved_tool")
			}
			schemaNode := tool.get("input_schema")
			if schemaNode == nil {
				return nil, nil, Error("schema")
			}
			schemaRaw := bytes.Clone(raw[schemaNode.start:schemaNode.end])
			if old, ok := st.Tools[name]; ok && canonical(old.Schema) != canonical(schemaRaw) {
				return nil, nil, Error("schema_changed")
			}
			alias, err := st.allocate("t", name)
			if err != nil {
				return nil, nil, err
			}
			for other := range seen {
				if alias == other {
					return nil, nil, Error("tool_collision")
				}
			}
			st.Tools[name] = storedTool{alias, schemaRaw}
			m.allowed[name] = true
			m.reverse[alias] = name
			sc, err := compileSchema(schemaNode, &st, name, &edits)
			if err != nil {
				return nil, nil, err
			}
			m.schemas[name] = sc
			edits = append(edits, replace(tool.get("name"), alias))
			if err := textEdit(tool.get("description"), st.encodeText, &edits); err != nil {
				return nil, nil, err
			}
		}
	}
	for name, tool := range st.Tools {
		if m.schemas[name] != nil {
			continue
		}
		root, err := parse(tool.Schema)
		if err != nil {
			return nil, nil, Error("store_schema")
		}
		discard := []edit{}
		sc, err := compileSchema(root, &st, name, &discard)
		if err != nil {
			return nil, nil, err
		}
		m.schemas[name] = sc
	}
	if choice := n.get("tool_choice"); choice != nil && choice.get("type").str() == "tool" {
		name := choice.get("name")
		tool, ok := st.Tools[name.str()]
		if !ok || !m.allowed[name.str()] {
			return nil, nil, Error("tool_choice")
		}
		edits = append(edits, replace(name, tool.Alias))
	}
	encodeContent := func(content *node) error { return m.forwardContent(content, &st, &edits) }
	if err := encodeContent(n.get("system")); err != nil {
		return nil, nil, err
	}
	if messages := n.get("messages"); messages != nil {
		if messages.kind != '[' {
			return nil, nil, Error("messages")
		}
		for _, message := range messages.items {
			if err := encodeContent(message.get("content")); err != nil {
				return nil, nil, err
			}
		}
	}
	wire, err := apply(raw, edits)
	if err != nil {
		return nil, nil, err
	}
	// Source duplicate keys and schema/argument collisions were checked above;
	// edits replace only typed JSON strings. Validate the emitted grammar without
	// allocating a second multi-megabyte syntax tree that is immediately discarded.
	if !json.Valid(wire) {
		return nil, nil, Error("json")
	}
	if err := s.save(st); err != nil {
		return nil, nil, err
	}
	m.st = st
	return wire, m, nil
}
func (m *RequestMap) forwardContent(n *node, st *state, edits *[]edit) error {
	if n == nil {
		return nil
	}
	if n.kind == '"' {
		return textEdit(n, st.encodeText, edits)
	}
	if n.kind != '[' {
		return Error("content")
	}
	for _, block := range n.items {
		switch block.get("type").str() {
		case "text":
			if err := textEdit(block.get("text"), st.encodeText, edits); err != nil {
				return err
			}
		case "tool_use", "tool_reference":
			name := block.get("name")
			tool, ok := st.Tools[name.str()]
			if block.has("signature") {
				return Error("signed_tool_block")
			}
			if !ok {
				return Error("history_tool")
			}
			*edits = append(*edits, replace(name, tool.Alias))
			if block.get("type").str() == "tool_use" {
				input := block.get("input")
				if input == nil || input.kind != '{' {
					return Error("tool_input")
				}
				if err := m.schemas[name.str()].arguments(input, false, edits); err != nil {
					return err
				}
			}
			// Tool result values and signed/unknown content are intentionally opaque.
		}
	}
	return nil
}
func (m *RequestMap) RestoreJSON(raw []byte) ([]byte, error) {
	if m == nil {
		return bytes.Clone(raw), nil
	}
	n, err := parse(raw)
	if err != nil {
		return nil, err
	}
	if n.kind != '{' {
		return nil, Error("response")
	}
	edits := []edit{}
	content := n.get("content")
	if content != nil {
		if content.kind != '[' {
			return nil, Error("content")
		}
		for _, block := range content.items {
			if err := m.restoreBlock(block, &edits); err != nil {
				return nil, err
			}
		}
	}
	return apply(raw, edits)
}
func (m *RequestMap) restoreBlock(block *node, edits *[]edit) error {
	switch block.get("type").str() {
	case "text":
		return textEdit(block.get("text"), m.decodeText, edits)
	case "tool_use", "tool_reference":
		name := block.get("name")
		original, ok := m.reverse[name.str()]
		if block.has("signature") {
			return Error("signed_tool_block")
		}
		if !ok || !m.allowed[original] {
			return Error("unknown_tool")
		}
		*edits = append(*edits, replace(name, original))
		if block.get("type").str() == "tool_use" {
			input := block.get("input")
			if input == nil || input.kind != '{' {
				return Error("tool_input")
			}
			return m.schemas[original].arguments(input, true, edits)
		}
	}
	return nil
}
