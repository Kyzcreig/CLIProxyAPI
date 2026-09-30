package helps

// DPX lane entrypoint gate (card t_2c3f91f8, Apollo ruling 2026-09-29 16:45).
//
// The apx 3-way entrypoint (cli | sdk-cli | sdk-ts), ported to the d lanes as an
// HONEST RECORD, not an author: DPX never writes cc_entrypoint, cc_turn_origin
// or the User-Agent (SPEC-modes INV-M1). Each d lane's mode letter names the
// genuine client that produces its traffic, and therefore the only entrypoint
// that lane may carry upstream:
//
//	mode  lane        genuine client                       entrypoint  turn_origin
//	t     dtlx/dtlr   interactive TUI over a real PTY      cli         human
//	h     dhlx/dhlr   headless `claude -p`                 sdk-cli     sdk
//	s     dslx/dslr   Agent SDK (TypeScript)               sdk-ts      sdk
//	a     dalx/dalr   slim emulator, no PTY                sdk-cli | sdk-ts (never cli)
//	bare  dlx/dpx...  full genuine CLI, caller decides     open (passed through untouched)
//
// A request whose billing block (or UA) disagrees with its lane is refused
// before dispatch. In particular no d lane other than t may send cli: cli is
// only ever carried by a genuine interactive session, never stamped.

import (
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// Entrypoint values Claude Code writes into the billing block.
const (
	DPXEntrypointCLI    = "cli"
	DPXEntrypointSDKCLI = "sdk-cli"
	DPXEntrypointSDKTS  = "sdk-ts"
)

// Lane grammar v2 (Ace 2026-09-29 06:14), restricted to the d family:
// claude-d[t|h|s|a]<p|l><x|r>[f|s][-N]. The "claude-" prefix is optional
// because Studio rows name the lane bare ("dtlx").
var dpxLaneRE = regexp.MustCompile(`^(?:claude-)?d([thsa]?)[pl][xr][fs]?(?:-[0-9]+)?$`)

// DPXLanePolicy is what a lane admits. Open lanes pass the caller's identity
// through; pre-decided lanes admit exactly the entrypoints in Allowed.
type DPXLanePolicy struct {
	Mode    string // "", "t", "h", "s" or "a"
	Open    bool
	Allowed []string
}

// ResolveDPXLane maps a lane name to its policy. ok is false for a name that is
// not a grammar-v2 d lane.
func ResolveDPXLane(lane string) (DPXLanePolicy, bool) {
	m := dpxLaneRE.FindStringSubmatch(strings.TrimSpace(lane))
	if m == nil {
		return DPXLanePolicy{}, false
	}
	p := DPXLanePolicy{Mode: m[1]}
	switch m[1] {
	case "":
		p.Open = true
	case "t":
		p.Allowed = []string{DPXEntrypointCLI}
	case "h":
		p.Allowed = []string{DPXEntrypointSDKCLI}
	case "s":
		p.Allowed = []string{DPXEntrypointSDKTS}
	case "a":
		p.Allowed = []string{DPXEntrypointSDKCLI, DPXEntrypointSDKTS}
	}
	return p, true
}

// DPXTurnOriginFor is the cc_turn_origin that genuinely accompanies an
// entrypoint (cli => human, sdk-* => sdk; Metering doc section 2c).
func DPXTurnOriginFor(entrypoint string) string {
	if entrypoint == DPXEntrypointCLI {
		return "human"
	}
	return "sdk"
}

// DPXBillingFields parses system[0]'s x-anthropic-billing-header block into
// its key=value fields. ok is false when the body carries no block.
func DPXBillingFields(body []byte) (map[string]string, bool) {
	text := gjson.GetBytes(body, "system.0.text")
	const prefix = "x-anthropic-billing-header:"
	if text.Type != gjson.String || !strings.HasPrefix(text.String(), prefix) {
		return nil, false
	}
	fields := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(text.String(), prefix), ";") {
		k, v, found := strings.Cut(strings.TrimSpace(part), "=")
		if found {
			fields[k] = v
		}
	}
	return fields, true
}

var dpxUAModeRE = regexp.MustCompile(`\(external, ([a-z-]+)`)

// CheckDPXLaneEntrypoint returns "" when body/userAgent are consistent with the
// lane, else a refusal reason. An empty lane is not checked (legacy lab
// configs); an unparseable one is refused.
func CheckDPXLaneEntrypoint(lane string, body []byte, userAgent string) string {
	if strings.TrimSpace(lane) == "" {
		return ""
	}
	policy, ok := ResolveDPXLane(lane)
	if !ok {
		return "unknown_lane"
	}
	if policy.Open {
		return ""
	}
	fields, ok := DPXBillingFields(body)
	if !ok {
		return "lane_entrypoint_mismatch"
	}
	entrypoint := fields["cc_entrypoint"]
	allowed := false
	for _, want := range policy.Allowed {
		if entrypoint == want {
			allowed = true
		}
	}
	if !allowed {
		return "lane_entrypoint_mismatch"
	}
	if origin, present := fields["cc_turn_origin"]; present && origin != DPXTurnOriginFor(entrypoint) {
		return "lane_entrypoint_mismatch"
	}
	if m := dpxUAModeRE.FindStringSubmatch(userAgent); m != nil && m[1] != entrypoint {
		return "lane_entrypoint_mismatch"
	}
	return ""
}
