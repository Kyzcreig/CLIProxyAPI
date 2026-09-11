package contentalias

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

type Binding struct{ Principal, Session, Version string }
type Manifest struct{ Words []string }

func DefaultManifest() Manifest { return Manifest{[]string{"hermes", "openclaw"}} }
func digest(s string) string    { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func normalize(s string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(s))
}
func (m Manifest) validate() error {
	if len(m.Words) == 0 {
		return Error("manifest")
	}
	for i, a := range m.Words {
		if len(a) < 2 || len(a) > 128 {
			return Error("manifest")
		}
		for j, b := range m.Words {
			if i != j && (strings.Contains(normalize(a), normalize(b)) || strings.Contains(normalize(b), normalize(a))) {
				return Error("vocabulary_overlap")
			}
		}
	}
	return nil
}

type entry struct{ Kind, Original string }
type storedTool struct {
	Alias  string
	Schema json.RawMessage
}
type state struct {
	Binding  Binding
	Manifest Manifest
	Symbols  map[string]entry
	Tools    map[string]storedTool
}
type envelope struct {
	Payload  json.RawMessage
	Checksum string
}
type Session struct {
	dir      string
	binding  Binding
	manifest Manifest
	mu       sync.Mutex
}

func session(dir string, b Binding, m Manifest) (*Session, error) {
	if b.Principal == "" || b.Session == "" || b.Version != "v1" {
		return nil, Error("binding")
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, Error("store_permissions")
	}
	return &Session{dir: dir, binding: b, manifest: m}, nil
}
func Create(dir string, b Binding, m Manifest) (*Session, error) {
	s, err := session(dir, b, m)
	if err != nil {
		return nil, err
	}
	lock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock(lock)
	if _, err := os.Lstat(filepath.Join(dir, "map.json")); !os.IsNotExist(err) {
		return nil, Error("store_exists")
	}
	st := state{b, m, map[string]entry{}, map[string]storedTool{}}
	if err := s.save(st); err != nil {
		return nil, err
	}
	return s, nil
}
func Open(dir string, b Binding, m Manifest) (*Session, error) {
	s, err := session(dir, b, m)
	if err != nil {
		return nil, err
	}
	lock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock(lock)
	_, err = s.load()
	return s, err
}
func (s *Session) lock() (*os.File, error) {
	fd, err := syscall.Open(filepath.Join(s.dir, "lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, Error("store_lock")
	}
	f := os.NewFile(uintptr(fd), "lock")
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, Error("store_lock")
	}
	return f, nil
}
func unlock(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
func (s *Session) load() (state, error) {
	var st state
	if _, err := session(s.dir, s.binding, s.manifest); err != nil {
		return st, err
	}
	fd, err := syscall.Open(filepath.Join(s.dir, "map.json"), syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return st, Error("store_missing")
	}
	f := os.NewFile(uintptr(fd), "map")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16<<20 {
		return st, Error("store_permissions")
	}
	raw := make([]byte, info.Size())
	n, err := f.ReadAt(raw, 0)
	if err != nil || n != len(raw) {
		return st, Error("store_read")
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || digest(string(env.Payload)) != env.Checksum || json.Unmarshal(env.Payload, &st) != nil {
		return st, Error("store_corrupt")
	}
	expected, _ := json.Marshal(s.manifest)
	actual, _ := json.Marshal(st.Manifest)
	if st.Binding != s.binding || string(expected) != string(actual) || st.Symbols == nil || st.Tools == nil {
		return st, Error("store_version")
	}
	for alias, e := range st.Symbols {
		if alias != symbol(st.Binding, e.Kind, e.Original) {
			return st, Error("store_corrupt")
		}
	}
	return st, nil
}
func (s *Session) save(st state) error {
	payload, err := json.Marshal(st)
	if err != nil {
		return Error("store_write")
	}
	raw, err := json.Marshal(envelope{payload, digest(string(payload))})
	if err != nil || len(raw) > 16<<20 {
		return Error("store_limit")
	}
	f, err := os.CreateTemp(s.dir, ".map-")
	if err != nil {
		return Error("store_write")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return Error("store_write")
	}
	if err = os.Rename(name, filepath.Join(s.dir, "map.json")); err != nil {
		return Error("store_write")
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return Error("store_write")
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return Error("store_write")
	}
	return nil
}
func symbol(b Binding, kind, original string) string {
	key, _ := json.Marshal([]string{b.Principal, b.Session, b.Version, kind, original})
	return "dpx_v1_" + kind + "_" + digest(string(key))[:24]
}
func (st *state) allocate(kind, original string) (string, error) {
	alias := symbol(st.Binding, kind, original)
	if old, ok := st.Symbols[alias]; ok && old != (entry{kind, original}) {
		return "", Error("symbol_collision")
	}
	st.Symbols[alias] = entry{kind, original}
	return alias, nil
}
