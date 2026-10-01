package config

import (
	"os"
	"path/filepath"
	"strings"
)

// DPXContentAlias is default-off and belongs to a dedicated single-session daemon.
// The binding is operator-owned configuration, never caller metadata.
type DPXContentAlias struct {
	Enabled        bool   `yaml:"enabled" json:"enabled"`
	StoreDirectory string `yaml:"store-directory" json:"-"`
	Principal      string `yaml:"principal" json:"-"`
	SessionID      string `yaml:"session-id" json:"-"`
	Version        string `yaml:"version" json:"version"`
	// Lane is the grammar-v2 d lane this daemon serves (e.g. "dtlx"). When set,
	// every request's billing-block entrypoint must be the one that lane's
	// genuine client emits (helps.CheckDPXLaneEntrypoint); DPX never writes it.
	Lane string `yaml:"lane" json:"lane"`
	// Lanes is the set of d lanes one unit serves (e.g. [dtlx, dslx, dlx]).
	// When non-empty it replaces Lane: a request is admitted when its
	// entrypoint fits any lane in the set, and its wirelog row is labelled with
	// the first lane (in this order) that admits it (t_bf75897d).
	Lanes []string `yaml:"lanes" json:"lanes"`
	// Wirelog (site W1): when WirelogSpool is set, every upstream request of an
	// alias-enabled daemon appends one digest-only v2 row to this file. The
	// Studio unit keeps it in its RAM state dir and relays rows out.
	WirelogSpool string `yaml:"wirelog-spool" json:"-"`
	WirelogLane  string `yaml:"wirelog-lane" json:"-"`
	WirelogSub   string `yaml:"wirelog-sub" json:"-"`
	// AuthFileCount is the number of `*.json` credential files under auth-dir
	// when this config snapshot was (re)loaded (spec one-cliproxyapi-lineage
	// D4 / Phase 2, AC9). One principal = one credential: an alias daemon that
	// sees more than one is the shared-proxy shape and refuses every Claude
	// request with unsafe_daemon_config. It is a LOAD-TIME snapshot taken by
	// the loader (CPA hot-reloads config through the watcher), never a
	// filesystem check on the request path; an auth-dir that grows after start
	// trips the refusal on the next reload, and between reloads the unit's
	// auth-dir tripwire is the detector. Not settable from YAML/JSON: it is
	// exported only because CloneForRuntime cannot copy unexported fields.
	AuthFileCount int `yaml:"-" json:"-"`
}

// DPXAuthDirUnreadable is the AuthFileCount recorded when auth-dir could not be
// listed at load time for a reason other than "does not exist yet". An unknown
// credential shape is treated as the unsafe one (fail closed).
const DPXAuthDirUnreadable = -1

// EffectiveLanes is the lane set the gate enforces: Lanes when set, else the
// one-element set {Lane}, else nil (ungated legacy lab config).
func (c DPXContentAlias) EffectiveLanes() []string {
	if len(c.Lanes) > 0 {
		return c.Lanes
	}
	if c.Lane != "" {
		return []string{c.Lane}
	}
	return nil
}

// SnapshotAuthFiles records the number of `*.json` files directly under
// authDir (a leading `~` expands to the user's home; empty = DefaultAuthDir)
// into AuthFileCount. Called by the loader only for an alias-enabled config, so
// a non-alias deployment never touches its auth-dir at load (off = upstream).
func (c *DPXContentAlias) SnapshotAuthFiles(authDir string) {
	c.AuthFileCount = CountDPXAuthFiles(authDir)
}

// CountDPXAuthFiles returns the number of `*.json` entries directly under
// authDir, 0 when the directory does not exist yet, DPXAuthDirUnreadable on any
// other error.
func CountDPXAuthFiles(authDir string) int {
	dir := strings.TrimSpace(authDir)
	if dir == "" {
		dir = DefaultAuthDir
	}
	if strings.HasPrefix(dir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return DPXAuthDirUnreadable
		}
		rest := strings.TrimLeft(strings.TrimPrefix(dir, "~"), `/\`)
		if rest == "" {
			dir = home
		} else {
			dir = filepath.Join(home, rest)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		return DPXAuthDirUnreadable
	}
	n := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			n++
		}
	}
	return n
}
