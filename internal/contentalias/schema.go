package contentalias

import (
	"encoding/json"
	"strings"
)

type schema struct {
	props             map[string]*schema
	forward, reverse  map[string]string
	items, additional *schema
}
type compiler struct {
	root     *node
	st       *state
	tool     string
	edits    *[]edit
	visiting map[*node]bool
	compiled map[*node]*schema
	paths    map[*node]string
}

func pointerPart(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
func (c *compiler) resolve(ref string) (*node, error) {
	if ref == "#" {
		return c.root, nil
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, Error("external_ref")
	}
	n := c.root
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		n = n.get(part)
		if n == nil {
			return nil, Error("unresolved_ref")
		}
	}
	return n, nil
}
func (c *compiler) compile(n *node, path string) (*schema, error) {
	if n == nil {
		return nil, Error("schema")
	}
	if c.visiting[n] {
		return nil, Error("schema_cycle")
	}
	if s := c.compiled[n]; s != nil {
		return s, nil
	}
	if n.kind != '{' {
		if n.kind == 't' || n.kind == 'f' {
			return &schema{}, nil
		}
		return nil, Error("schema")
	}
	c.visiting[n] = true
	defer delete(c.visiting, n)
	c.paths[n] = path
	if ref := n.get("$ref"); ref != nil {
		if len(n.fields) != 1 {
			return nil, Error("ref_siblings")
		}
		target, err := c.resolve(ref.str())
		if err != nil {
			return nil, err
		}
		return c.compile(target, ref.str())
	}
	for _, key := range []string{"patternProperties", "unevaluatedProperties", "if", "then", "else", "not", "allOf", "prefixItems", "dependentSchemas"} {
		if n.has(key) {
			return nil, Error("unsupported_schema")
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if alternatives := n.get(key); alternatives != nil {
			for _, a := range alternatives.items {
				for _, f := range a.fields {
					if f.key.text != "type" && f.key.text != "enum" && f.key.text != "const" {
						return nil, Error("ambiguous_schema")
					}
				}
			}
		}
	}
	s := &schema{props: map[string]*schema{}, forward: map[string]string{}, reverse: map[string]string{}}
	if p := n.get("properties"); p != nil {
		if p.kind != '{' {
			return nil, Error("schema_properties")
		}
		for _, f := range p.fields {
			key := f.key.text
			if strings.HasPrefix(key, "dpx_v1_") {
				return nil, Error("reserved_property")
			}
			alias, err := c.st.allocate("p", c.tool+"\x00"+path+"\x00"+key)
			if err != nil {
				return nil, err
			}
			if p.has(alias) {
				return nil, Error("property_collision")
			}
			s.forward[key] = alias
			s.reverse[alias] = key
			*c.edits = append(*c.edits, replace(f.key, alias))
			child, err := c.compile(f.value, path+"/properties/"+pointerPart(key))
			if err != nil {
				return nil, err
			}
			s.props[key] = child
		}
	}
	for _, key := range []string{"required", "dependentRequired", "dependencies"} {
		if names := n.get(key); names != nil {
			if key == "required" {
				if names.kind != '[' {
					return nil, Error("schema_required")
				}
				for _, name := range names.items {
					alias, ok := s.forward[name.str()]
					if !ok {
						return nil, Error("schema_required")
					}
					*c.edits = append(*c.edits, replace(name, alias))
				}
			}
			if key != "required" {
				if names.kind != '{' {
					return nil, Error("schema_dependencies")
				}
				for _, dep := range names.fields {
					alias, ok := s.forward[dep.key.text]
					if !ok || dep.value.kind != '[' {
						return nil, Error("schema_dependencies")
					}
					*c.edits = append(*c.edits, replace(dep.key, alias))
					for _, name := range dep.value.items {
						alias, ok := s.forward[name.str()]
						if !ok {
							return nil, Error("schema_dependencies")
						}
						*c.edits = append(*c.edits, replace(name, alias))
					}
				}
			}
		}
	}
	if items := n.get("items"); items != nil {
		var err error
		s.items, err = c.compile(items, path+"/items")
		if err != nil {
			return nil, err
		}
	}
	if a := n.get("additionalProperties"); a != nil && a.kind == '{' {
		var err error
		s.additional, err = c.compile(a, path+"/additionalProperties")
		if err != nil {
			return nil, err
		}
	}
	for _, key := range []string{"$defs", "definitions"} {
		if defs := n.get(key); defs != nil {
			for _, f := range defs.fields {
				if _, err := c.compile(f.value, path+"/"+key+"/"+pointerPart(f.key.text)); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, key := range []string{"description", "title"} {
		if err := textEdit(n.get(key), c.st.encodeText, c.edits); err != nil {
			return nil, err
		}
	}
	c.compiled[n] = s
	return s, nil
}
func compileSchema(n *node, st *state, tool string, edits *[]edit) (*schema, error) {
	c := compiler{n, st, tool, edits, map[*node]bool{}, map[*node]*schema{}, map[*node]string{}}
	s, err := c.compile(n, "#")
	if err != nil {
		return nil, err
	}
	// Rewrite local pointers only where a traversed segment is a declared property key.
	var visit func(*node) error
	visit = func(v *node) error {
		if v == nil {
			return nil
		}
		if ref := v.get("$ref"); ref != nil {
			text := ref.str()
			if text != "#" {
				cur := n
				parts := strings.Split(text[2:], "/")
				out := make([]string, len(parts))
				for i, part := range parts {
					decoded := strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
					out[i] = part
					if i > 0 && parts[i-1] == "properties" {
						for _, e := range *edits {
							for _, f := range cur.fields {
								if f.key.text == decoded && e.start == f.key.start {
									var alias string
									if json.Unmarshal(e.raw, &alias) != nil {
										return Error("schema_pointer")
									}
									out[i] = pointerPart(alias)
								}
							}
						}
					}
					cur = cur.get(decoded)
					if cur == nil {
						return Error("unresolved_ref")
					}
				}
				mapped := "#/" + strings.Join(out, "/")
				if mapped != text {
					*edits = append(*edits, replace(ref, mapped))
				}
			}
		}
		for _, f := range v.fields {
			if f.key.text == "enum" || f.key.text == "const" || f.key.text == "default" || f.key.text == "examples" {
				continue
			}
			if err := visit(f.value); err != nil {
				return err
			}
		}
		for _, a := range v.items {
			if err := visit(a); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(n); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *schema) arguments(n *node, inverse bool, edits *[]edit) error {
	if s == nil || n == nil {
		return nil
	}
	if n.kind == '[' {
		for _, a := range n.items {
			if err := s.items.arguments(a, inverse, edits); err != nil {
				return err
			}
		}
		return nil
	}
	if n.kind != '{' {
		return nil
	}
	seen := map[string]bool{}
	for _, f := range n.fields {
		key := f.key.text
		original := key
		mapped := key
		if inverse {
			if v, ok := s.reverse[key]; ok {
				original = v
				mapped = v
			} else if strings.HasPrefix(key, "dpx_v1_") && len(s.props) > 0 {
				return Error("unknown_property")
			}
		} else if v, ok := s.forward[key]; ok {
			mapped = v
		}
		if seen[mapped] {
			return Error("inverse_collision")
		}
		seen[mapped] = true
		if mapped != key {
			*edits = append(*edits, replace(f.key, mapped))
		}
		child, ok := s.props[original]
		if !ok {
			child = s.additional
		}
		if err := child.arguments(f.value, inverse, edits); err != nil {
			return err
		}
	}
	return nil
}
