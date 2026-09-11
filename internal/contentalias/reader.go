package contentalias

import (
	"bytes"
	"io"
)

// IsRequestScoped prevents credential rotation for a deterministic local failure.
func (e Error) IsRequestScoped() bool { return true }
func (e Error) StatusCode() int       { return 400 }

// Reader restores complete events before any downstream translator or replay cache.
// The caller retains ownership of the upstream response body and cancellation.
func (m *RequestMap) Reader(source io.Reader) io.Reader {
	if m == nil {
		return source
	}
	return &streamReader{source: source, stream: m.NewStream()}
}

type streamReader struct {
	source  io.Reader
	stream  *Stream
	pending []byte
	err     error
}

func (r *streamReader) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 && r.err == nil {
		buf := make([]byte, 32<<10)
		n, err := r.source.Read(buf)
		if n > 0 {
			r.pending, r.err = r.stream.Feed(buf[:n])
			if r.err != nil {
				break
			}
		}
		if err != nil {
			if err == io.EOF {
				end, finishErr := r.stream.Finish()
				r.pending = append(r.pending, end...)
				if finishErr != nil {
					r.err = finishErr
				} else {
					r.err = io.EOF
				}
			} else {
				r.stream.fail(Error("stream_cancel"))
				r.err = Error("stream_transport")
			}
		}
	}
	if len(r.pending) > 0 {
		n := copy(dst, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	return 0, r.err
}
func (m *RequestMap) RestoreSSE(raw []byte) ([]byte, error) {
	if m == nil {
		return bytes.Clone(raw), nil
	}
	return io.ReadAll(m.Reader(bytes.NewReader(raw)))
}
