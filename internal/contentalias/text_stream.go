package contentalias

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Ordinary text retains only a namespace-prefix suffix. A token containing a
// codec symbol is held until the next whitespace or backtick; that pending
// data shares the response's explicit 8 MiB budget.
type textDecoder struct {
	m       *RequestMap
	pending []byte
	symbol  bool
	// tolerant leaves an unknown symbol verbatim instead of failing the
	// stream (WordCodec consumers; the Claude response path stays strict).
	tolerant bool
}

func (d *textDecoder) flush() (string, error) {
	var out string
	var err error
	if d.tolerant {
		out, err = d.m.decodeValue(string(d.pending))
	} else {
		out, err = d.m.decodeToken(string(d.pending))
	}
	d.pending = nil
	d.symbol = false
	return out, err
}
func (d *textDecoder) feed(text string) (string, error) {
	// Encode aliases inside backtick spans and resource tokens too (t_cb095320),
	// so decode restores symbols everywhere; whitespace and backticks bound a token.
	var out strings.Builder
	for _, r := range text {
		if unicode.IsSpace(r) || r == '`' {
			v, err := d.flush()
			if err != nil {
				return "", err
			}
			out.WriteString(v)
			out.WriteRune(r)
			continue
		}
		d.pending = utf8.AppendRune(d.pending, r)
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
