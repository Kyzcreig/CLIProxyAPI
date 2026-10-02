package contentalias

import (
	"os"
	"path/filepath"
	"testing"
)

// A writer SIGKILLed between CreateTemp and rename leaves ".map-<digits>" behind
// (dpx-16 tripwire, 2026-10-02). The next save under the store lock must sweep it.
func TestSaveSweepsOrphanMapTemp(t *testing.T) {
	s := testSession(t)
	orphan := filepath.Join(s.dir, ".map-123")
	if err := os.WriteFile(orphan, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Prepare([]byte(requestFixture), s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan temp survived a save: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(s.dir, ".map-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left after save: %v", left)
	}
}
