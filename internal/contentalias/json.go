// Package contentalias implements opt-in, typed, lossless content aliases.
package contentalias

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"unicode/utf8"
)

type Error string

func (e Error) Error() string { return "contentalias:" + string(e) }

type field struct {
	key   *node
	value *node
}
type node struct {
	start, end int
	kind       byte
	text       string
	fields     []field
	items      []*node
}

func (n *node) get(key string) *node {
	if n != nil {
		for _, f := range n.fields {
			if f.key.text == key {
				return f.value
			}
		}
	}
	return nil
}
func (n *node) str() string {
	if n != nil && n.kind == '"' {
		return n.text
	}
	return ""
}
func (n *node) has(key string) bool { return n.get(key) != nil }

type parser struct {
	raw []byte
	pos int
}

func (p *parser) space() {
	for p.pos < len(p.raw) && bytes.ContainsRune([]byte(" \t\r\n"), rune(p.raw[p.pos])) {
		p.pos++
	}
}
func parse(raw []byte) (*node, error) {
	if len(raw) > 16<<20 {
		return nil, Error("request_limit")
	}
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, Error("json")
	}
	p := parser{raw: raw}
	n, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.space()
	if p.pos != len(raw) {
		return nil, Error("json")
	}
	return n, nil
}
func (p *parser) value(depth int) (*node, error) {
	if depth > 128 {
		return nil, Error("depth")
	}
	p.space()
	n := &node{start: p.pos, kind: p.raw[p.pos]}
	switch n.kind {
	case '{':
		p.pos++
		p.space()
		seen := map[string]bool{}
		for p.raw[p.pos] != '}' {
			k, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			if seen[k.text] {
				return nil, Error("duplicate_key")
			}
			seen[k.text] = true
			p.space()
			p.pos++
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			n.fields = append(n.fields, field{k, v})
			p.space()
			if p.raw[p.pos] != ',' {
				break
			}
			p.pos++
			p.space()
		}
		p.pos++
	case '[':
		p.pos++
		p.space()
		for p.raw[p.pos] != ']' {
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			n.items = append(n.items, v)
			p.space()
			if p.raw[p.pos] != ',' {
				break
			}
			p.pos++
			p.space()
		}
		p.pos++
	case '"':
		p.pos++
		escaped := false
		for p.raw[p.pos] != '"' {
			if p.raw[p.pos] == '\\' {
				escaped = true
				p.pos++
				if p.raw[p.pos] == 'u' {
					v, _ := strconv.ParseUint(string(p.raw[p.pos+1:p.pos+5]), 16, 16)
					if v >= 0xd800 && v <= 0xdbff {
						if p.pos+10 >= len(p.raw) || string(p.raw[p.pos+5:p.pos+7]) != "\\u" {
							return nil, Error("unicode")
						}
						low, _ := strconv.ParseUint(string(p.raw[p.pos+7:p.pos+11]), 16, 16)
						if low < 0xdc00 || low > 0xdfff {
							return nil, Error("unicode")
						}
						p.pos += 10
					} else if v >= 0xdc00 && v <= 0xdfff {
						return nil, Error("unicode")
					} else {
						p.pos += 4
					}
				}
			}
			p.pos++
		}
		p.pos++
		if !escaped {
			// parse already validated JSON and UTF-8; no unescape is needed.
			n.text = string(p.raw[n.start+1 : p.pos-1])
		} else if err := json.Unmarshal(p.raw[n.start:p.pos], &n.text); err != nil {
			return nil, Error("json")
		}
	default:
		for p.pos < len(p.raw) && !bytes.ContainsRune([]byte(" ,}\t\r\n]"), rune(p.raw[p.pos])) {
			p.pos++
		}
	}
	n.end = p.pos
	return n, nil
}

type edit struct {
	start, end int
	raw        []byte
}

func replace(n *node, s string) edit { raw, _ := json.Marshal(s); return edit{n.start, n.end, raw} }
func apply(raw []byte, edits []edit) ([]byte, error) {
	if len(edits) == 0 {
		return bytes.Clone(raw), nil
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out bytes.Buffer
	size := len(raw)
	for _, e := range edits {
		size += len(e.raw) - (e.end - e.start)
	}
	if size < 0 || size > 16<<20 {
		return nil, Error("request_limit")
	}
	out.Grow(size)
	last := 0
	for _, e := range edits {
		if e.start < last || e.start < 0 || e.end < e.start || e.end > len(raw) {
			return nil, errors.New("contentalias:edit_overlap")
		}
		out.Write(raw[last:e.start])
		out.Write(e.raw)
		last = e.end
	}
	out.Write(raw[last:])
	return out.Bytes(), nil
}
