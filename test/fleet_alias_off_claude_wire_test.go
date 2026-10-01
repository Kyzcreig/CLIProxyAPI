package test

// TestFleetAliasOffClaudeWire is the Phase 2 wire gate of the one-CLIProxyAPI-lineage
// spec (§3 I2, §7 Phase 2, AC2): with dpx-content-alias ABSENT, 50 replayed Claude
// requests through the fleet tree reach upstream byte-identical to pristine <BASE>
// (testdata/fleet_wire/pristine-claude-*.json, recorded on v8.0.4 d33f63f8 with
// TestFleetClaudeWireRecord). This is what lets :18812 carry the alias code: off =
// upstream, on the wire, for every client shape.
//
// TestFleetAliasOnDPXShapeNoPolicyMetadata is Phase 2 Negative (b): a DPX-shaped daemon
// (alias on, routing.prompt-cache-policy: off) aliases the body AND writes no
// cache_key_source / prompt-fingerprint metadata on the turn (D1); the same daemon with
// the policy absent or set to anything but the literal "off" is refused with
// unsafe_daemon_config before dispatch (D7 DPX rule).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
)

func fleetClaudeWireLoadPristine(t *testing.T, name string) fleetClaudeWireRecord {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", "fleet_wire", "pristine-"+name+".json"))
	if errRead != nil {
		t.Fatalf("pristine golden %s: %v (record on <BASE> with TestFleetClaudeWireRecord)", name, errRead)
	}
	var rec fleetClaudeWireRecord
	if errJSON := json.Unmarshal(raw, &rec); errJSON != nil {
		t.Fatalf("pristine golden %s: %v", name, errJSON)
	}
	return rec
}

func TestFleetAliasOffClaudeWire(t *testing.T) {
	arms := fleetClaudeWireArms()
	if len(arms) != 50 {
		t.Fatalf("gate replays %d arms, spec says 50", len(arms))
	}
	for _, arm := range arms {
		arm := arm
		t.Run(arm.Name, func(t *testing.T) {
			pristine := fleetClaudeWireLoadPristine(t, arm.Name)
			// Alias absent: the zero value of dpx-content-alias (the :18812 shape).
			rec, meta, err := fleetRecordClaudeWire(t, &config.Config{}, arm)
			if err != nil || rec == nil {
				t.Fatalf("fleet tree: err=%v, upstream reached=%v", err, rec != nil)
			}
			got := fleetClaudeWireNormalize(*rec)
			if got.Body != pristine.Body {
				t.Fatalf("upstream body differs from pristine <BASE>\nwant %s\ngot  %s", pristine.Body, got.Body)
			}
			if !reflect.DeepEqual(got, pristine) {
				e, _ := json.MarshalIndent(pristine, "", "  ")
				a, _ := json.MarshalIndent(got, "", "  ")
				t.Fatalf("upstream request differs from pristine <BASE>\nwant %s\ngot  %s", e, a)
			}
			for _, k := range fleetWirePolicyMetadataKeys {
				if v, ok := meta[k]; ok {
					t.Fatalf("alias absent + policy absent wrote %s=%v", k, v)
				}
			}
		})
	}
}

// fleetDPXShapeConfig is a single-principal alias daemon config with a fresh store.
func fleetDPXShapeConfig(t *testing.T, policy string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	binding := contentalias.Binding{Principal: "fleet-dpx-shape", Session: "11111111-2222-4333-8444-555555555555", Version: "v1"}
	if _, err := contentalias.Create(dir, binding, contentalias.DefaultManifest()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{MaxRetryCredentials: 1, DPXContentAlias: config.DPXContentAlias{Enabled: true, StoreDirectory: dir, Principal: binding.Principal, SessionID: binding.Session, Version: "v1", AuthFileCount: 1}}
	cfg.Routing.PromptCachePolicy = policy
	return cfg
}

func fleetDPXShapeArm(t *testing.T) fleetClaudeWireArm {
	t.Helper()
	for _, arm := range fleetClaudeWireArms() {
		if arm.Name == "claude-cli-tools" {
			return arm
		}
	}
	t.Fatal("claude-cli-tools arm missing")
	return fleetClaudeWireArm{}
}

func TestFleetAliasOnDPXShapeNoPolicyMetadata(t *testing.T) {
	arm := fleetDPXShapeArm(t)
	rec, meta, err := fleetRecordClaudeWire(t, fleetDPXShapeConfig(t, "off"), arm)
	if err != nil || rec == nil {
		t.Fatalf("DPX shape: err=%v, upstream reached=%v", err, rec != nil)
	}
	if meta == nil {
		t.Fatal("executor wrapper saw no call")
	}
	for _, k := range fleetWirePolicyMetadataKeys {
		if v, ok := meta[k]; ok {
			t.Fatalf("DPX shape wrote policy metadata %s=%v (D1: off must skip the resolver and the fingerprints)", k, v)
		}
	}
	// The turn was aliased: the client's tool name and system text never reach upstream.
	if strings.Contains(rec.Body, `"name":"Read"`) || strings.Contains(rec.Body, `"Hermes"`) || !strings.Contains(rec.Body, `dpx_v1_t_`) {
		t.Fatalf("DPX shape did not alias the upstream body: %s", rec.Body)
	}
}

func TestFleetAliasOnDPXShapePolicyNotOffRefuses(t *testing.T) {
	arm := fleetDPXShapeArm(t)
	for _, policy := range []string{"", "of", "shadow ", "Shadow", "shadow", "enforce"} {
		rec, _, err := fleetRecordClaudeWire(t, fleetDPXShapeConfig(t, policy), arm)
		var aliasErr contentalias.Error
		if !errors.As(err, &aliasErr) || aliasErr != "unsafe_daemon_config" {
			t.Fatalf("policy %q: err=%v, want unsafe_daemon_config", policy, err)
		}
		if rec != nil {
			t.Fatalf("policy %q: refused request reached upstream", policy)
		}
	}
}
