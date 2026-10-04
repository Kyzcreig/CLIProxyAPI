package contentalias

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

type schema struct {
	props             map[string]*schema
	forward, reverse  map[string]string
	items, additional *schema
	// words: normalized manifest words; a stray-key marker never names a
	// property that contains one (the caller replays the marker in history).
	words []string
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
	// Claude Code 2.1.284+ declares free-keyed maps with propertyNames {"type":"string"}.
	// That constraint is vacuous (every JSON member name is a string) and names no
	// member, so it is accepted in exactly that shape; any other propertyNames fails closed.
	if names := n.get("propertyNames"); names != nil && !vacuousPropertyNames(names) {
		return nil, Error("unsupported_schema")
	}
	// Claude Code 2.1.284+ (SendMessage.to) stacks string value assertions in allOf.
	// Members that only constrain the value name no member and need no mapping;
	// any structural member (properties, refs, combinators) still fails closed.
	if members := n.get("allOf"); members != nil && !valueOnlyAllOf(members) {
		return nil, Error("unsupported_schema")
	}
	for _, key := range []string{"patternProperties", "unevaluatedProperties", "if", "then", "else", "not", "prefixItems", "dependentSchemas", "contains", "minContains", "maxContains", "unevaluatedItems", "additionalItems", "$dynamicRef", "$recursiveRef", "$id"} {
		if n.has(key) {
			return nil, Error("unsupported_schema")
		}
	}
	// anyOf/oneOf members that name no property need no mapping. Structural
	// members (t_91e140ec: skill_manage's per-action op branches) compile at the
	// PARENT path, so a key shared by several branches gets one alias keyed by
	// (tool, parent path, key) and never by branch index. Their maps merge into
	// this node's map; restore then needs no branch match. A key whose branches
	// need different nested maps fails closed in merge.
	var structural []*node
	for _, key := range []string{"anyOf", "oneOf"} {
		if alternatives := n.get(key); alternatives != nil {
			if alternatives.kind != '[' {
				return nil, Error("ambiguous_schema")
			}
			for _, a := range alternatives.items {
				if !valueOnlyAlternative(a) {
					structural = append(structural, a)
				}
			}
		}
	}
	s := &schema{props: map[string]*schema{}, forward: map[string]string{}, reverse: map[string]string{}}
	for _, word := range c.st.Manifest.Words {
		s.words = append(s.words, normalize(word))
	}
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
	for _, a := range structural {
		if a.kind != '{' && a.kind != 't' && a.kind != 'f' {
			return nil, Error("ambiguous_schema")
		}
		alt, err := c.compile(a, path)
		if err != nil {
			return nil, err
		}
		if err := s.merge(alt); err != nil {
			return nil, err
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
	// Preserve literal constraints; refuse schemas whose key transform would
	// change their constrained value rather than rewriting executable literals.
	var literals func(*node, *schema) error
	literals = func(v *node, shape *schema) error {
		for _, key := range []string{"const", "enum"} {
			value := v.get(key)
			if value == nil {
				continue
			}
			values := []*node{value}
			if key == "enum" {
				values = value.items
			}
			for _, literal := range values {
				changes := []edit{}
				if err := shape.arguments(literal, false, &changes); err != nil {
					return err
				}
				if len(changes) != 0 {
					return Error("literal_schema_conflict")
				}
			}
		}
		for _, key := range []string{"anyOf", "oneOf"} {
			if alternatives := v.get(key); alternatives != nil {
				for _, alternative := range alternatives.items {
					if err := literals(alternative, shape); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	for schemaNode, shape := range c.compiled {
		if err := literals(schemaNode, shape); err != nil {
			return nil, err
		}
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
		return nil
	}
	for schemaNode := range c.paths {
		if err := visit(schemaNode); err != nil {
			return nil, err
		}
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
	strays := 0
	for _, f := range n.fields {
		key := f.key.text
		original := key
		mapped := key
		if inverse {
			if v, ok := s.reverse[key]; ok {
				original = v
				mapped = v
			} else if (strings.HasPrefix(key, "dpx_v1_p_") || strings.HasPrefix(key, "dpx_v1_t_")) && len(s.forward) > 0 {
				// t_186d86c5: the model wrote an alias that is not this object's
				// (a sibling tool's property, or a 1-2 hex near-copy). Never let the
				// raw token leave DPX: the caller would echo it, the next request
				// would literal-escape it, and the model loops on a name it cannot
				// read. A marker naming this object's real parameters makes the
				// caller's unknown-key refusal self-correcting. No silent repair.
				strays++
				mapped = s.strayMarker(strays)
			}
		} else if v, ok := s.forward[key]; ok {
			mapped = v
		} else if _, collision := s.reverse[key]; collision {
			return Error("dynamic_key_collision")
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

// strayMarker names the plain parameter names of this object, sorted. The
// caller accepts plain names (the inverse map passes them through unchanged).
func (s *schema) strayMarker(i int) string {
	names := make([]string, 0, len(s.forward))
	for key := range s.forward {
		if !containsAny(normalize(key), s.words) {
			names = append(names, key)
		}
	}
	sort.Strings(names)
	if len(names) > 40 {
		names = append(names[:40], "...")
	}
	return "unknown_key_" + strconv.Itoa(i) + " (not a parameter of this tool; its parameters are: " + strings.Join(names, ", ") + ")"
}

// merge folds a structural anyOf/oneOf member's map into its parent's. A key
// both declare must carry the same alias and an equal nested map; items and
// additionalProperties likewise. Anything else would make the map depend on
// which branch matched, so it stays ambiguous.
func (s *schema) merge(alt *schema) error {
	for key, child := range alt.props {
		alias := alt.forward[key]
		if prev, ok := s.props[key]; ok {
			if s.forward[key] != alias || !sameShape(prev, child) {
				return Error("ambiguous_schema")
			}
			continue
		}
		if owner, ok := s.reverse[alias]; ok && owner != key {
			return Error("ambiguous_schema")
		}
		s.props[key] = child
		s.forward[key] = alias
		s.reverse[alias] = key
	}
	var err error
	if s.items, err = mergeShape(s.items, alt.items); err != nil {
		return err
	}
	s.additional, err = mergeShape(s.additional, alt.additional)
	return err
}

func mergeShape(a, b *schema) (*schema, error) {
	if emptyShape(b) {
		return a, nil
	}
	if emptyShape(a) {
		return b, nil
	}
	if !sameShape(a, b) {
		return nil, Error("ambiguous_schema")
	}
	return a, nil
}

// emptyShape: the schema renames nothing anywhere below it.
func emptyShape(s *schema) bool {
	return s == nil || (len(s.props) == 0 && emptyShape(s.items) && emptyShape(s.additional))
}

// sameShape: both schemas rename exactly the same keys to the same aliases on
// every path. Descriptions and value assertions do not matter to the map.
func sameShape(a, b *schema) bool {
	if a == b || (emptyShape(a) && emptyShape(b)) {
		return true
	}
	if emptyShape(a) || emptyShape(b) || len(a.props) != len(b.props) {
		return false
	}
	for key, child := range a.props {
		other, ok := b.props[key]
		if !ok || a.forward[key] != b.forward[key] || !sameShape(child, other) {
			return false
		}
	}
	return sameShape(a.items, b.items) && sameShape(a.additional, b.additional)
}

func vacuousPropertyNames(n *node) bool {
	if n.kind != '{' || len(n.fields) != 1 || n.fields[0].key.text != "type" {
		return false
	}
	t := n.fields[0].value
	return t.kind == '"' && t.str() == "string"
}

var valueAssertionKeywords = map[string]bool{"type": true, "pattern": true, "minLength": true, "maxLength": true, "format": true, "minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true}

// valueOnlyAlternative reports whether an anyOf/oneOf member names no property
// key on any path: only type/enum/const, plus `items` whose schema is itself
// value-only (t_9cc235a9: Hermes terminal.notify = anyOf[boolean, array of
// string]). The key map then does not depend on which branch matched. A member
// with properties, refs, combinators, tuple items or anything else stays
// ambiguous. Non-object members carry no fields, as before.
func valueOnlyAlternative(a *node) bool {
	for _, f := range a.fields {
		switch f.key.text {
		case "type", "enum", "const":
		case "items":
			if f.value.kind != '{' || !valueOnlyAlternative(f.value) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func valueOnlyAllOf(n *node) bool {
	if n.kind != '[' || len(n.items) == 0 {
		return false
	}
	for _, member := range n.items {
		if member.kind != '{' {
			return false
		}
		for _, f := range member.fields {
			if !valueAssertionKeywords[f.key.text] {
				return false
			}
		}
	}
	return true
}
