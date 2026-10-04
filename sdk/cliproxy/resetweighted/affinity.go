package resetweighted

import (
	"container/list"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// affinityEntry is one session -> credential binding.
//
// Home is the credential the session is bound to; Fallback is the alternate that
// served the session while Home was ineligible (transient outage, 5h guard,
// Fable withhold, tried-set exclusion). The fallback is sticky: it outlives
// Home's recovery so the NEXT excursion lands on the same warm alternate
// (relay lesson t_4411fc15: 857/947 repeat excursions on a fresh alternate were
// each a full prompt-cache write). Only a quota rebind, drop or operator
// release clears it.
//
// ReturnedHome counts recoveries: the session goes home at most ONCE per
// excursion. A second flap within the same excursion (home eligible again after
// having already returned) stays on the fallback, so a flapping home cannot
// bounce a session between two caches every turn.
type affinityEntry struct {
	Home         string    `json:"home"`
	Fallback     string    `json:"fallback,omitempty"`
	BoundAt      time.Time `json:"bound_at"`
	LastTouch    time.Time `json:"last_touch"`
	OnFallback   bool      `json:"on_fallback,omitempty"`
	ReturnedHome bool      `json:"returned_home,omitempty"`
}

// AffinityMap is a bounded, persisted session -> credential map. Time never
// evicts a healthy binding; only exhaustion (caller-driven Drop/Rebind) or the
// LRU cap ends one.
type AffinityMap struct {
	mu      sync.Mutex
	max     int
	path    string
	entries map[string]*list.Element
	order   *list.List // front = LRU, back = MRU
	dirty   bool
	saveMu  sync.Mutex
	lastErr error
}

type affinityNode struct {
	key   string
	entry *affinityEntry
}

// NewAffinityMap loads any persisted bindings from path (empty = memory only).
func NewAffinityMap(maxSize int, path string) *AffinityMap {
	if maxSize < 1 {
		maxSize = 1
	}
	m := &AffinityMap{max: maxSize, path: path, entries: make(map[string]*list.Element), order: list.New()}
	m.load()
	return m
}

func (m *AffinityMap) load() {
	if m.path == "" {
		return
	}
	raw, errRead := os.ReadFile(m.path)
	if errRead != nil {
		return
	}
	var saved map[string]affinityEntry
	if errUnmarshal := json.Unmarshal(raw, &saved); errUnmarshal != nil {
		_ = os.Rename(m.path, m.path+".corrupt-"+time.Now().UTC().Format("20060102T150405Z"))
		return
	}
	// Oldest touch first so the LRU order survives the round trip.
	keys := make([]string, 0, len(saved))
	for key := range saved {
		keys = append(keys, key)
	}
	sortByTouch(keys, saved)
	for _, key := range keys {
		entry := saved[key]
		if entry.Home == "" {
			continue
		}
		e := entry
		m.entries[key] = m.order.PushBack(&affinityNode{key: key, entry: &e})
	}
	m.evictLocked()
}

func sortByTouch(keys []string, saved map[string]affinityEntry) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && saved[keys[j]].LastTouch.Before(saved[keys[j-1]].LastTouch); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}

func (m *AffinityMap) evictLocked() {
	for m.order.Len() > m.max {
		front := m.order.Front()
		node := front.Value.(*affinityNode)
		delete(m.entries, node.key)
		m.order.Remove(front)
		m.dirty = true
	}
}

// Flush writes the map atomically when dirty. Called outside the pick path.
func (m *AffinityMap) Flush() error {
	if m.path == "" {
		return nil
	}
	m.mu.Lock()
	if !m.dirty {
		m.mu.Unlock()
		return nil
	}
	snapshot := make(map[string]affinityEntry, len(m.entries))
	for key, el := range m.entries {
		snapshot[key] = *el.Value.(*affinityNode).entry
	}
	m.dirty = false
	m.mu.Unlock()

	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	raw, errMarshal := json.Marshal(snapshot)
	if errMarshal != nil {
		return errMarshal
	}
	if errMkdir := os.MkdirAll(filepath.Dir(m.path), 0o755); errMkdir != nil {
		return errMkdir
	}
	tmp := m.path + ".tmp"
	if errWrite := os.WriteFile(tmp, raw, 0o600); errWrite != nil {
		return errWrite
	}
	return os.Rename(tmp, m.path)
}

// Len returns the number of bindings.
func (m *AffinityMap) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.order.Len()
}

func (m *AffinityMap) touch(el *list.Element, now time.Time) *affinityEntry {
	node := el.Value.(*affinityNode)
	node.entry.LastTouch = now
	m.order.MoveToBack(el)
	m.dirty = true
	return node.entry
}

// Bind sets (or replaces) the home for key and clears any fallback state.
func (m *AffinityMap) Bind(key, home string, now time.Time) {
	if key == "" || home == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.entries[key]; ok {
		node := el.Value.(*affinityNode)
		node.entry.Home = home
		node.entry.Fallback = ""
		node.entry.OnFallback = false
		node.entry.ReturnedHome = false
		node.entry.BoundAt = now
		m.touch(el, now)
		return
	}
	entry := &affinityEntry{Home: home, BoundAt: now, LastTouch: now}
	m.entries[key] = m.order.PushBack(&affinityNode{key: key, entry: entry})
	m.dirty = true
	m.evictLocked()
}

// Drop removes a binding (quota exhaustion of the home, operator release).
func (m *AffinityMap) Drop(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.entries[key]; ok {
		delete(m.entries, key)
		m.order.Remove(el)
		m.dirty = true
	}
}

// DropCredential removes every binding whose home is cred and clears every
// fallback pointing at it. Returns the number of homes released.
func (m *AffinityMap) DropCredential(cred string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	released := 0
	for key, el := range m.entries {
		node := el.Value.(*affinityNode)
		if node.entry.Home == cred {
			delete(m.entries, key)
			m.order.Remove(el)
			released++
			m.dirty = true
			continue
		}
		if node.entry.Fallback == cred {
			node.entry.Fallback = ""
			node.entry.OnFallback = false
			m.dirty = true
		}
	}
	return released
}

// Peek returns the home without touching LRU order.
func (m *AffinityMap) Peek(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.entries[key]
	if !ok {
		return "", false
	}
	return el.Value.(*affinityNode).entry.Home, true
}

// Resolution is the outcome of Resolve.
type Resolution struct {
	// Target is the credential to use; empty = no pinned decision, score a new pick.
	Target string
	// Stage is one of "affinity" (home), "fallback" (sticky alternate), "return_home",
	// "hold_fallback" (home eligible again but already returned once), "" (unbound/released).
	Stage string
	// Home is the bound home when a binding exists (for logs).
	Home string
}

// Resolve applies the affinity gate for key against the eligible set.
//
//   - home eligible, never left        -> home ("affinity")
//   - home eligible, on fallback, not yet returned -> home ("return_home"), marks returned
//   - home eligible, on fallback, already returned  -> fallback if eligible ("hold_fallback")
//   - home ineligible, fallback eligible            -> fallback ("fallback")
//   - home ineligible, no eligible fallback         -> "" (caller scores; NoteServed records the alternate)
//   - unbound                                        -> ""
func (m *AffinityMap) Resolve(key string, eligible map[string]bool, now time.Time) Resolution {
	if key == "" {
		return Resolution{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.entries[key]
	if !ok {
		return Resolution{}
	}
	entry := m.touch(el, now)
	res := Resolution{Home: entry.Home}
	if eligible[entry.Home] {
		if !entry.OnFallback {
			res.Target, res.Stage = entry.Home, "affinity"
			return res
		}
		if !entry.ReturnedHome {
			entry.ReturnedHome = true
			entry.OnFallback = false
			res.Target, res.Stage = entry.Home, "return_home"
			return res
		}
		if entry.Fallback != "" && eligible[entry.Fallback] {
			res.Target, res.Stage = entry.Fallback, "hold_fallback"
			return res
		}
		// Fallback gone: home is all we have; take it and reset the latch.
		entry.OnFallback = false
		res.Target, res.Stage = entry.Home, "return_home"
		return res
	}
	if entry.Fallback != "" && eligible[entry.Fallback] {
		entry.OnFallback = true
		res.Target, res.Stage = entry.Fallback, "fallback"
		return res
	}
	return res
}

// NoteServed records the credential a NEW (scored) pick landed on for a bound
// session whose home was ineligible: it becomes the sticky fallback. For an
// unbound key it binds the home. Returns true when the call created a binding.
func (m *AffinityMap) NoteServed(key, served string, now time.Time) bool {
	if key == "" || served == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.entries[key]
	if !ok {
		entry := &affinityEntry{Home: served, BoundAt: now, LastTouch: now}
		m.entries[key] = m.order.PushBack(&affinityNode{key: key, entry: entry})
		m.dirty = true
		m.evictLocked()
		return true
	}
	entry := m.touch(el, now)
	if entry.Home == served {
		entry.OnFallback = false
		return false
	}
	if entry.Fallback != served {
		entry.Fallback = served
		entry.ReturnedHome = false // a new excursion target resets the return latch
	}
	entry.OnFallback = true
	return false
}

// BoundCounts returns live home bindings per credential (for the status endpoint).
func (m *AffinityMap) BoundCounts() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int)
	for _, el := range m.entries {
		out[el.Value.(*affinityNode).entry.Home]++
	}
	return out
}
