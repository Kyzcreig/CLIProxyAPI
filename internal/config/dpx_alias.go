package config

// DPXContentAlias is default-off and belongs to a dedicated single-session daemon.
// The binding is operator-owned configuration, never caller metadata.
type DPXContentAlias struct {
	Enabled        bool   `yaml:"enabled" json:"enabled"`
	StoreDirectory string `yaml:"store-directory" json:"-"`
	Principal      string `yaml:"principal" json:"-"`
	SessionID      string `yaml:"session-id" json:"-"`
	Version        string `yaml:"version" json:"version"`
	// Wirelog (site W1): when WirelogSpool is set, every upstream request of an
	// alias-enabled daemon appends one digest-only v2 row to this file. The
	// Studio unit keeps it in its RAM state dir and relays rows out.
	WirelogSpool string `yaml:"wirelog-spool" json:"-"`
	WirelogLane  string `yaml:"wirelog-lane" json:"-"`
	WirelogSub   string `yaml:"wirelog-sub" json:"-"`
}
