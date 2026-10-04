package brandalias

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the plugin's own YAML (plugins.configs.dpx-alias). `enabled` and
// `priority` are owned by the host; everything else is read here.
type Config struct {
	Enabled bool `yaml:"enabled"`
	// Mode is shadow (default) or enabled.
	Mode string `yaml:"mode"`
	// Lanes is the allowlist of raw executor lanes the plugin acts on
	// (antigravity, gemini, claude). A lane the policy excepts is refused.
	Lanes []string `yaml:"lanes"`
	// Words overrides DefaultWords when non-empty.
	Words []string `yaml:"words"`
	// Principal and Session bind the symbol derivation: the same word aliases
	// to the same dpx_v1_w_ symbol across turns, so prompt-cache prefixes stay
	// stable. Operator-owned, never caller metadata.
	Principal string `yaml:"principal"`
	Session   string `yaml:"session"`
	// WirelogSpool is an append-only JSONL file of digest-only rows (one per
	// intercepted request, plus one per response-side failure). Empty = off.
	WirelogSpool string `yaml:"wirelog-spool"`
}

// ParseConfig decodes raw YAML and fills defaults. A lane the policy excepts
// is an error at configure time: the ruling is config, not code.
func ParseConfig(raw []byte) (Config, error) {
	cfg := Config{}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("dpx-alias: config: %w", err)
		}
	}
	return cfg.withDefaults()
}

func (c Config) withDefaults() (Config, error) {
	switch strings.ToLower(strings.TrimSpace(c.Mode)) {
	case "", ModeShadow:
		c.Mode = ModeShadow
	case ModeEnabled:
		c.Mode = ModeEnabled
	default:
		return Config{}, fmt.Errorf("dpx-alias: mode %q is not shadow|enabled", c.Mode)
	}
	lanes := make([]string, 0, len(c.Lanes))
	for _, lane := range c.Lanes {
		lane = canonicalLane(lane)
		if lane == "" {
			continue
		}
		if IsException(lane) {
			return Config{}, fmt.Errorf("dpx-alias: lane %q is OFF by policy (exceptions: codex, openai, xai, kimi)", lane)
		}
		lanes = append(lanes, lane)
	}
	c.Lanes = lanes
	if len(c.Words) == 0 {
		c.Words = append([]string(nil), DefaultWords...)
	}
	if strings.TrimSpace(c.Principal) == "" {
		c.Principal = "cpa"
	}
	if strings.TrimSpace(c.Session) == "" {
		c.Session = "default"
	}
	return c, nil
}

// LaneEnabled reports whether lane is in the allowlist.
func (c Config) LaneEnabled(lane string) bool {
	lane = canonicalLane(lane)
	for _, l := range c.Lanes {
		if l == lane {
			return true
		}
	}
	return false
}
