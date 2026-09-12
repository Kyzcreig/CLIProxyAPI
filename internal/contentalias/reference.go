package contentalias

// referenceEdits visits only the native protocol's reference slots. In
// particular it does not recurse into executable inputs or ordinary result data.
func (m *RequestMap) referenceEdits(block *node, st *state, inverse bool, edits *[]edit) error {
	var refs []*node
	switch block.get("type").str() {
	case "tool_reference":
		refs = []*node{block}
	case "tool_result":
		if content := block.get("content"); content != nil && content.kind == '[' {
			refs = content.items
		}
	case "tool_search_tool_result":
		if content := block.get("content").get("tool_references"); content != nil && content.kind == '[' {
			refs = content.items
		}
	}
	for _, ref := range refs {
		if ref.get("type").str() != "tool_reference" {
			continue
		}
		if block.has("signature") || ref.has("signature") || block.get("content").has("signature") {
			return Error("signed_tool_block")
		}
		name := ref.get("tool_name")
		var mapped string
		if inverse {
			original, ok := m.reverse[name.str()]
			if !ok || !m.allowed[original] {
				return Error("unknown_tool")
			}
			mapped = original
		} else {
			tool, ok := st.Tools[name.str()]
			if !ok {
				return Error("history_tool")
			}
			mapped = tool.Alias
		}
		*edits = append(*edits, replace(name, mapped))
	}
	return nil
}
