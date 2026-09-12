package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type aliasUsageSink struct{ records chan usage.Record }

func (s *aliasUsageSink) HandleUsage(_ context.Context, r usage.Record) {
	if r.AuthID == "alias-usage-observer" {
		select {
		case s.records <- r:
		default:
		}
	}
}

func TestDPXAliasActualUsageOnInverseError(t *testing.T) {
	sink := &aliasUsageSink{make(chan usage.Record, 8)}
	usage.RegisterNamedPlugin("offline-alias-observer", sink)
	usage.StartDefault(context.Background())
	for _, stream := range []bool{false, true} {
		e, req, opts := aliasFixture(t)
		opts.Stream = stream
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write(aliasSSE("dpx_v1_t_unknown", "bad", req.Model))
			} else {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"content":[{"type":"tool_use","name":"unknown","input":{}}],"usage":{"input_tokens":5,"output_tokens":7}}`)
			}
		}))
		auth := offlineAuth(server.URL)
		auth.ID = "alias-usage-observer"
		var err error
		if stream {
			response, e2 := e.ExecuteStream(context.Background(), auth, req, opts)
			err = e2
			if err == nil {
				for chunk := range response.Chunks {
					if chunk.Err != nil {
						err = chunk.Err
					}
				}
			}
		} else {
			_, err = e.Execute(context.Background(), auth, req, opts)
		}
		server.Close()
		if err == nil {
			t.Fatal("inverse should fail")
		}
		select {
		case record := <-sink.records:
			if record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 7 {
				t.Fatalf("actual usage lost stream=%t: %+v", stream, record.Detail)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no actual usage record")
		}
	}
}
