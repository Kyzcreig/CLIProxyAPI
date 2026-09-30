package auth

import (
	"sync"
	"time"
)

// sessionEntry stores auth binding with expiration.
type sessionEntry struct {
	authID    string
	expiresAt time.Time
}

// DefaultSessionCacheMaxEntries bounds the session-to-auth map. Derived prompt-cache keys
// give every distinct prefix its own binding, so without a cap a key flood (one-shot
// callers, fuzzers) would grow the map until the TTL sweep; at the cap the entries closest
// to expiry are evicted first.
const DefaultSessionCacheMaxEntries = 50000

// SessionCache provides TTL-based session to auth mapping with automatic cleanup.
type SessionCache struct {
	mu         sync.RWMutex
	entries    map[string]sessionEntry
	ttl        time.Duration
	maxEntries int
	stopCh     chan struct{}
}

// NewSessionCache creates a cache with the specified TTL.
// A background goroutine periodically cleans expired entries.
func NewSessionCache(ttl time.Duration) *SessionCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	c := &SessionCache{
		entries:    make(map[string]sessionEntry),
		ttl:        ttl,
		maxEntries: DefaultSessionCacheMaxEntries,
		stopCh:     make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

// SetMaxEntries changes the hard cap on bindings (<= 0 restores the default).
func (c *SessionCache) SetMaxEntries(n int) {
	if n <= 0 {
		n = DefaultSessionCacheMaxEntries
	}
	c.mu.Lock()
	c.maxEntries = n
	c.mu.Unlock()
}

// Len reports the number of bindings currently held (expired ones included until swept).
func (c *SessionCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// evictForInsertLocked makes room for one more binding when the cap is reached: expired
// entries go first, then the entries closest to expiry. Caller holds c.mu.
func (c *SessionCache) evictForInsertLocked(now time.Time) {
	if c.maxEntries <= 0 || len(c.entries) < c.maxEntries {
		return
	}
	for sid, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, sid)
		}
	}
	for len(c.entries) >= c.maxEntries {
		var victim string
		var victimExpiry time.Time
		for sid, entry := range c.entries {
			if victim == "" || entry.expiresAt.Before(victimExpiry) {
				victim, victimExpiry = sid, entry.expiresAt
			}
		}
		if victim == "" {
			return
		}
		delete(c.entries, victim)
	}
}

// Get retrieves the auth ID bound to a session, if still valid.
// Does NOT refresh the TTL on access.
func (c *SessionCache) Get(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	c.mu.RLock()
	entry, ok := c.entries[sessionID]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiresAt) {
		c.mu.Lock()
		delete(c.entries, sessionID)
		c.mu.Unlock()
		return "", false
	}
	return entry.authID, true
}

// GetAndRefresh retrieves the auth ID bound to a session and refreshes TTL on hit.
// This extends the binding lifetime for active sessions.
func (c *SessionCache) GetAndRefresh(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	entry, ok := c.entries[sessionID]
	if !ok {
		c.mu.Unlock()
		return "", false
	}
	if now.After(entry.expiresAt) {
		delete(c.entries, sessionID)
		c.mu.Unlock()
		return "", false
	}
	// Refresh TTL on successful access
	entry.expiresAt = now.Add(c.ttl)
	c.entries[sessionID] = entry
	c.mu.Unlock()
	return entry.authID, true
}

// Set binds a session to an auth ID with TTL refresh.
func (c *SessionCache) Set(sessionID, authID string) {
	if sessionID == "" || authID == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	if _, exists := c.entries[sessionID]; !exists {
		c.evictForInsertLocked(now)
	}
	c.entries[sessionID] = sessionEntry{
		authID:    authID,
		expiresAt: now.Add(c.ttl),
	}
	c.mu.Unlock()
}

// Invalidate removes a specific session binding.
func (c *SessionCache) Invalidate(sessionID string) {
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	delete(c.entries, sessionID)
	c.mu.Unlock()
}

// InvalidateAuth removes all sessions bound to a specific auth ID.
// Used when an auth becomes unavailable.
func (c *SessionCache) InvalidateAuth(authID string) {
	if authID == "" {
		return
	}
	c.mu.Lock()
	for sid, entry := range c.entries {
		if entry.authID == authID {
			delete(c.entries, sid)
		}
	}
	c.mu.Unlock()
}

// Stop terminates the background cleanup goroutine.
func (c *SessionCache) Stop() {
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
}

func (c *SessionCache) cleanupLoop() {
	ticker := time.NewTicker(c.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.cleanup()
		}
	}
}

func (c *SessionCache) cleanup() {
	now := time.Now()
	c.mu.Lock()
	for sid, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, sid)
		}
	}
	c.mu.Unlock()
}
