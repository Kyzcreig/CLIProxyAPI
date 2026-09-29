package executor

// DPX coupling probe (t_e8f5d4b3). Measures, against the CPA tree this file is
// overlaid onto, how CPA is coupled to the Claude Code CLI version and to model ids:
//
//   - signer: for each GENUINE CLI body (captured by scripts/cli_sign_capture.py with
//     the CLI signing its own cch), does CPA's signAnthropicMessagesBody reproduce
//     the CLI's digits byte-for-byte?
//   - detector: does CPA's native-client detector confirm the genuine request, and
//     does the header path keep the CLI's User-Agent / X-Stainless-Package-Version?
//   - version window: which synthetic CLI versions pass the UA plausibility gate with
//     the stock baseline and with a claude-header-defaults.user-agent override?
//   - models: are the given model ids in the build's embedded Claude catalog?
//
// Driven by env; skips when DPX_COUPLING_REPORT is unset, so ordinary CI is unaffected.
//   DPX_COUPLING_CAPTURES  JSON list from cli_sign_capture.py (optional)
//   DPX_COUPLING_MODELS    comma-separated model ids (optional)
//   DPX_COUPLING_UA_OVERRIDE  e.g. "claude-cli/2.2.0 (external, cli)" (optional)
//   DPX_COUPLING_REPORT    output JSON path (required)

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

var dpxProbeCCH = regexp.MustCompile(`cch=([0-9a-f]{5});`)

type dpxProbeCapture struct {
	CLIVersion string              `json:"cli_version"`
	Path       string              `json:"path"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
}

func dpxProbeHeaders(h map[string][]string) http.Header {
	out := http.Header{}
	for k, vs := range h {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	return out
}

func dpxProbeHeaderPath(in http.Header, cfg *config.Config) (string, string) {
	r, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	helps.ApplyClaudeLegacyDeviceHeaders(r, in, cfg, true)
	return r.Header.Get("User-Agent"), r.Header.Get("X-Stainless-Package-Version")
}

func TestDPXCouplingProbe(t *testing.T) {
	reportPath := os.Getenv("DPX_COUPLING_REPORT")
	if reportPath == "" {
		t.Skip("DPX_COUPLING_REPORT unset")
	}
	report := map[string]any{}
	stock := &config.Config{}
	override := strings.TrimSpace(os.Getenv("DPX_COUPLING_UA_OVERRIDE"))
	overrideCfg := &config.Config{}
	overrideCfg.ClaudeHeaderDefaults.UserAgent = override
	report["baseline_user_agent"] = dpxProbeBaselineUA(stock)
	report["baseline_cc_version"] = helps.DefaultClaudeVersion(stock)

	if path := os.Getenv("DPX_COUPLING_CAPTURES"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read captures: %v", err)
		}
		var caps []dpxProbeCapture
		if err := json.Unmarshal(raw, &caps); err != nil {
			t.Fatalf("parse captures: %v", err)
		}
		var rows []map[string]any
		for _, c := range caps {
			body := []byte(c.Body)
			row := map[string]any{"cli_version": c.CLIVersion, "path": c.Path}
			m := dpxProbeCCH.FindSubmatch(body)
			if m == nil {
				row["cli_cch"] = nil
			} else {
				row["cli_cch"] = string(m[1])
				signed, err := signAnthropicMessagesBody(body)
				if err != nil {
					row["signer_error"] = err.Error()
				} else if mm := dpxProbeCCH.FindSubmatch(signed); mm != nil {
					row["cpa_cch"] = string(mm[1])
					row["signer_equal"] = bytes.Equal(signed, body)
				}
			}
			hdr := dpxProbeHeaders(c.Headers)
			det := helps.DetectClaudeCodeRequest(hdr, body, false, stock)
			row["detector_confirmed"] = det.Confirmed
			row["detector_ua_plausible"] = det.UserAgent
			row["entrypoint"] = det.Entrypoint
			ua, pkg := dpxProbeHeaderPath(hdr, stock)
			row["client_user_agent"] = hdr.Get("User-Agent")
			row["client_package_version"] = hdr.Get("X-Stainless-Package-Version")
			row["upstream_user_agent"] = ua
			row["upstream_package_version"] = pkg
			row["ua_preserved"] = ua == hdr.Get("User-Agent")
			row["package_version_preserved"] = pkg == hdr.Get("X-Stainless-Package-Version")
			rows = append(rows, row)
		}
		report["captures"] = rows
	}

	window := []string{"2.1.258", "2.1.279", "2.1.280", "2.1.284", "2.1.999", "2.2.0", "3.0.0"}
	var ver []map[string]any
	for _, v := range window {
		ua := "claude-cli/" + v + " (external, cli)"
		row := map[string]any{"version": v,
			"plausible_stock": dpxProbeUAPlausible(ua, stock)}
		if override != "" {
			row["plausible_with_override"] = dpxProbeUAPlausible(ua, overrideCfg)
		}
		ver = append(ver, row)
	}
	report["version_window"] = ver
	if override != "" {
		report["ua_override"] = override
	}

	if models := os.Getenv("DPX_COUPLING_MODELS"); models != "" {
		known := map[string]bool{}
		for _, m := range registry.GetClaudeModels() {
			known[m.ID] = true
		}
		cov := map[string]bool{}
		for _, id := range strings.Split(models, ",") {
			if id = strings.TrimSpace(id); id != "" {
				cov[id] = known[id]
			}
		}
		report["model_catalog"] = cov
		report["model_catalog_size"] = len(known)
	}

	out, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(reportPath, append(out, '\n'), 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}
}

// dpxProbeUAPlausible runs the real detector on a minimal header set whose only
// variable is the User-Agent; UserAgent in the detection is plausibleClaudeCodeUserAgent.
func dpxProbeUAPlausible(ua string, cfg *config.Config) bool {
	h := http.Header{}
	h.Set("User-Agent", ua)
	return helps.DetectClaudeCodeRequest(h, []byte(`{}`), false, cfg).UserAgent
}

func dpxProbeBaselineUA(cfg *config.Config) string {
	r, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	helps.ApplyClaudeDefaultDeviceProfileHeaders(r, cfg)
	return r.Header.Get("User-Agent")
}
