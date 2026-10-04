package helps

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// t_997bc8a2: the row carries prepareMs/lockWaitMs when the request context
// holds a Prepare timing, and neither key when it does not (re-sign-only units).
func TestDPXWirelogRowCarriesPrepareTiming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message"}`)
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name   string
		timing *DPXPrepareTiming
	}{
		{"with timing", &DPXPrepareTiming{Total: 1500 * time.Millisecond, LockWait: 1200 * time.Millisecond}},
		{"without timing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spool := filepath.Join(t.TempDir(), "spool.jsonl")
			ctx := context.Background()
			if tc.timing != nil {
				ctx = WithDPXPrepareTiming(ctx, *tc.timing)
			}
			resp, err := dpxPost(ctx, dpxTestClient(t, spool), srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			row, _ := dpxOneRow(t, spool)
			if tc.timing == nil {
				for _, k := range []string{"prepareMs", "lockWaitMs"} {
					if _, ok := row[k]; ok {
						t.Fatalf("row has %s without a Prepare timing: %v", k, row)
					}
				}
				return
			}
			if got := row["prepareMs"]; got != float64(1500) {
				t.Fatalf("prepareMs = %v, want 1500", got)
			}
			if got := row["lockWaitMs"]; got != float64(1200) {
				t.Fatalf("lockWaitMs = %v, want 1200", got)
			}
		})
	}
}
