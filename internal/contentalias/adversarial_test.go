package contentalias

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStreamInterleavedOpaqueAndToolBlocks(t *testing.T) {
	s, name, key := reviewStream(t)
	frames := [][]byte{
		event(map[string]string{"type": "message_start"}),
		event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "name": name, "id": "c", "input": map[string]any{}}}),
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{ \"type\":\"thinking\", \"thinking\":\"Hermes\",\"signature\":\"opaque\"}}\n\n"),
		[]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"dpx_v1_opaque\"}}\n\n"),
		event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": `{"` + key + `":"λ/Hermes"}`}}),
		event(map[string]any{"type": "content_block_stop", "index": 1}),
		event(map[string]any{"type": "content_block_stop", "index": 0}),
		event(map[string]string{"type": "message_stop"}),
	}
	var output []byte
	for i, frame := range frames {
		for _, b := range frame {
			part, err := s.Feed([]byte{b})
			if err != nil {
				t.Fatal(err)
			}
			if i < 6 && bytes.Contains(part, []byte("tool_use")) {
				t.Fatal("tool released before validation")
			}
			output = append(output, part...)
		}
	}
	if _, err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output, frames[2]) || !bytes.Contains(output, frames[3]) || !bytes.Contains(output, []byte(`\"command\"`)) || !bytes.Contains(output, []byte("λ/Hermes")) {
		t.Fatal("opaque or executable bytes changed")
	}
}

func TestStreamExplicitBounds(t *testing.T) {
	t.Run("active-blocks", func(t *testing.T) {
		s, _, _ := reviewStream(t)
		s.Feed(event(map[string]string{"type": "message_start"}))
		for i := 0; i < 65; i++ {
			_, err := s.Feed(event(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]string{"type": "thinking"}}))
			if i < 64 && err != nil {
				t.Fatal(err)
			}
			if i == 64 && err == nil {
				t.Fatal("active block limit missing")
			}
		}
	})
	t.Run("tool-bytes", func(t *testing.T) {
		s, name, _ := reviewStream(t)
		s.Feed(event(map[string]string{"type": "message_start"}))
		s.Feed(event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "name": name, "input": map[string]any{}}}))
		fragment := event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": strings.Repeat(" ", 1<<20)}})
		for i := 0; i < 5; i++ {
			out, err := s.Feed(fragment)
			if len(out) > 0 {
				t.Fatal("incomplete tool escaped")
			}
			if i < 4 && err != nil {
				t.Fatal(err)
			}
			if i == 4 && err == nil {
				t.Fatal("tool bound missing")
			}
		}
	})
	t.Run("aggregate-bytes", func(t *testing.T) {
		s, name, _ := reviewStream(t)
		s.Feed(event(map[string]string{"type": "message_start"}))
		s.Feed(event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "name": name, "input": map[string]any{}}}))
		for i := 0; i < 9; i++ {
			out, err := s.Feed([]byte(":" + strings.Repeat("x", 1<<20) + "\n\n"))
			if len(out) > 0 {
				t.Fatal("pending order bypass")
			}
			if err != nil {
				return
			}
		}
		t.Fatal("total bound missing")
	})
}

func TestReplayCorruptVersionAndConcurrentSessions(t *testing.T) {
	for _, mode := range []string{"corrupt", "version", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			s := testSession(t)
			p := filepath.Join(s.dir, "map.json")
			switch mode {
			case "corrupt":
				os.WriteFile(p, []byte(`{"Checksum":"bad"}`), 0600)
			case "version":
				st, _ := s.load()
				st.Binding.Version = "old"
				s.save(st)
			case "symlink":
				os.Rename(p, p+".saved")
				os.Symlink(p+".saved", p)
			}
			if _, _, err := Prepare([]byte(requestFixture), s); err == nil {
				t.Fatal("invalid map accepted")
			}
		})
	}
	s := testSession(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session, err := Open(s.dir, s.binding, s.manifest)
			if err != nil {
				errs <- err
				return
			}
			raw, _ := json.Marshal(map[string]string{"system": fmt.Sprintf("Hermes dpx_v1_literal_%d", i)})
			wire, m, err := Prepare(raw, session)
			if err != nil {
				errs <- err
				return
			}
			n, _ := parse(wire)
			got, err := m.decodeText(n.get("system").str())
			want, _ := parse(raw)
			if err != nil || got != want.get("system").str() {
				errs <- fmt.Errorf("concurrent round trip")
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	literals := 0
	for _, e := range st.Symbols {
		if e.Kind == "l" {
			literals++
		}
	}
	if literals != 16 {
		t.Fatal("concurrent lost update")
	}
}

func TestNoContentInDiagnosticsOrReplay(t *testing.T) {
	s := testSession(t)
	before, _ := os.ReadFile(filepath.Join(s.dir, "map.json"))
	raw := []byte(`{"tools":[{"name":"private-value","input_schema":{"$ref":"https://private-value"}}]}`)
	if _, _, err := Prepare(raw, s); err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatal("local diagnostic contains input")
	}
	after, _ := os.ReadFile(filepath.Join(s.dir, "map.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("failed input entered replay state")
	}
}
