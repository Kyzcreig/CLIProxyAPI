package contentalias

import (
	"bytes"
	"io"
)

// ReaderObserved observes complete original wire lines before restoring their
// content. This keeps accounting independent of an inverse-map failure.
func (m *RequestMap) ReaderObserved(source io.Reader, observe func([]byte)) io.Reader {
	if m == nil || observe == nil {
		return m.Reader(source)
	}
	return m.Reader(io.TeeReader(source, &lineObserver{observe: observe}))
}

type lineObserver struct {
	pending []byte
	observe func([]byte)
}

func (w *lineObserver) Write(raw []byte) (int, error) {
	size := len(raw)
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, '\n')
		if end < 0 {
			if len(w.pending)+len(raw) > maxPending {
				return 0, Error("stream_limit")
			}
			w.pending = append(w.pending, raw...)
			break
		}
		if len(w.pending)+end > maxPending {
			return 0, Error("stream_limit")
		}
		w.pending = append(w.pending, raw[:end]...)
		w.observe(w.pending)
		w.pending = w.pending[:0]
		raw = raw[end+1:]
	}
	return size, nil
}
