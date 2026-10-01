package contentalias

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Ordinary text retains only a namespace-prefix suffix. A token containing a
// codec symbol needs lookahead until its resource classification is known;
// that pending data shares the response's explicit 8 MiB budget.
type textDecoder struct {
	m              *RequestMap
	pending        []byte
	symbol         bool
	mode           byte
	ticks, closing int
}

func (d *textDecoder) flush() (string, error) {
	out, err := d.m.decodeToken(string(d.pending))
	d.pending = nil
	d.symbol = false
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
		d.pending = utf8.AppendRune(d.pending, r)
		if strings.ContainsRune("/\\:@", r) {
			out.Write(d.pending)
			d.pending = nil
			d.symbol = false
			d.mode = 'r'
			continue
		}
		if !d.symbol {
			d.symbol = bytes.Contains(d.pending, []byte("dpx_v1_"))
			if !d.symbol && len(d.pending) > 7 {
				n := len(d.pending) - 7
				for n > 0 && !utf8.RuneStart(d.pending[n]) {
					n--
				}
				out.Write(d.pending[:n])
				d.pending = append(d.pending[:0], d.pending[n:]...)
			}
		}
		if len(d.pending) > maxPending {
			return "", Error("stream_limit")
		}
	}
	return out.String(), nil
}
func (d *textDecoder) finish() (string, error) { return d.flush() }
