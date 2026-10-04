package contentalias

// WordCodec is the prose aliaser of Prepare detached from the Claude request
// walker and the file store: the same word-run tokenizer, the same
// dpx_v1_w_/dpx_v1_l_ symbol grammar (a pure function of the binding and the
// original bytes), the same literal escaping, held in memory for one request.
// The cpa brand-alias plugin (sdk/cliproxy/brandalias, t_a37235c0) walks the
// text fields of non-Claude vendor shapes with it. Decode is tolerant: a symbol
// the codec never allocated is what the model typed and passes through verbatim
// (brand sentinels may leak INBOUND, never upstream).
type WordCodec struct{ st state }

// NewWordCodec validates the manifest the way a session does and returns an
// empty codec bound to b. Not safe for concurrent use.
func NewWordCodec(b Binding, m Manifest) (*WordCodec, error) {
	if b.Principal == "" || b.Session == "" || b.Version != "v1" {
		return nil, Error("binding")
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &WordCodec{st: state{Binding: b, Manifest: m, Symbols: map[string]entry{}, Tools: map[string]storedTool{}}}, nil
}

// Encode aliases every word run containing a manifest word and escapes source
// codec literals; it is idempotent for a given codec.
func (c *WordCodec) Encode(text string) (string, error) { return c.st.encodeText(text) }

// Decode restores the word and literal symbols this codec allocated; anything
// else shaped like a symbol passes through verbatim.
func (c *WordCodec) Decode(text string) string {
	out, _ := (&RequestMap{st: c.st}).decodeValue(text)
	return out
}

// Symbols is the number of distinct symbols allocated so far.
func (c *WordCodec) Symbols() int { return len(c.st.Symbols) }

// TextDecoder is the boundary-safe streaming form of Decode: text arrives in
// arbitrary slices (SSE deltas) and a symbol may straddle two of them. Feed
// returns what is safe to release now, holding back at most a partial symbol
// (or the 7-byte window in which one could begin); Flush releases the rest.
type TextDecoder struct{ d textDecoder }

// NewTextDecoder returns a decoder over this codec's symbols.
func (c *WordCodec) NewTextDecoder() *TextDecoder {
	return &TextDecoder{d: textDecoder{m: &RequestMap{st: c.st}, tolerant: true}}
}

// Feed consumes the next slice and returns the decoded prefix that cannot be
// changed by later input. An over-long symbol-bearing token (>8 MiB pending)
// is released undecoded rather than failing the stream.
func (t *TextDecoder) Feed(text string) string {
	out, err := t.d.feed(text)
	if err != nil {
		held := string(t.d.pending)
		t.d.pending = nil
		t.d.symbol = false
		return out + held
	}
	return out
}

// Flush releases whatever Feed held back, decoded.
func (t *TextDecoder) Flush() string {
	out, _ := t.d.finish()
	return out
}
