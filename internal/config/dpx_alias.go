package config

// DPXContentAlias is default-off and belongs to a dedicated single-session daemon.
// The binding is operator-owned configuration, never caller metadata.
type DPXContentAlias struct {
	Enabled        bool   `yaml:"enabled" json:"enabled"`
	StoreDirectory string `yaml:"store-directory" json:"-"`
	Principal      string `yaml:"principal" json:"-"`
	SessionID      string `yaml:"session-id" json:"-"`
	Version        string `yaml:"version" json:"version"`
}
