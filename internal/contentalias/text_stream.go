package contentalias

import (
	"strings"
	"unicode"
)

// textDecoder retains at most one bounded lexical token. Code spans and
// recognized resource tokens are forwarded opaquely with constant-size state.
// 4096 bytes bounds lexical lookahead when deciding whether a token is a path.
type textDecoder struct {
	m              *RequestMap
	pending        string
	mode           byte
	ticks, closing int
}

func (d *textDecoder) flush() (string, error) {
	out, err := d.m.decodeToken(d.pending)
	d.pending = ""
	return out, err
}
func (d *textDecoder) feed(text string) (string, error) {
	var out strings.Builder
	for _, r := range text {
		if d.mode == 'o' { // opening backtick run
			if r == '`' {
				d.ticks++
				out.WriteRune(r)
				continue
			}
			d.mode = 'c'
		}
		if d.mode == 'c' {
			out.WriteRune(r)
			if r == '`' {
				d.closing++
				if d.closing == d.ticks {
					d.mode = 0
					d.closing = 0
					d.ticks = 0
				}
			} else {
				d.closing = 0
			}
			continue
		}
		if d.mode == 'r' {
			if !unicode.IsSpace(r) && r != '`' {
				out.WriteRune(r)
				continue
			}
			d.mode = 0
		}
		if unicode.IsSpace(r) || r == '`' {
			v, err := d.flush()
			if err != nil {
				return "", err
			}
			out.WriteString(v)
			out.WriteRune(r)
			if r == '`' {
				d.mode = 'o'
				d.ticks = 1
			}
			continue
		}
		d.pending += string(r)
		if strings.ContainsRune("/\\:@", r) {
			out.WriteString(d.pending)
			d.pending = ""
			d.mode = 'r'
			continue
		}
		if len(d.pending) > 4096 {
			return "", Error("prose_token_limit")
		}
	}
	return out.String(), nil
}
func (d *textDecoder) finish() (string, error) { return d.flush() }
