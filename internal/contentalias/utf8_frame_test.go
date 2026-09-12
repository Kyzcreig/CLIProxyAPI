package contentalias

import "testing"

func TestStreamInvalidUTF8CommentRejected(t *testing.T) {
	s, _, _ := reviewStream(t)
	if out, err := s.Feed([]byte{':', 0xff, '\n', '\n'}); err == nil || len(out) != 0 {
		t.Fatal("invalid UTF-8 frame passed through")
	}
}
