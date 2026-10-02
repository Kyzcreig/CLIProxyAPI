package logging

import (
	"net/http"
	"regexp"
	"strings"
)

// FleetCallerHeader carries the caller's self-declared identity: a `key=value;key=value`
// list naming the harness that made the request (hermes, pr-agent, claude-code, script, ...),
// the agent/profile, the platform, the kanban card and whether the call is a main turn or an
// auxiliary task. Several callers may share one API key (the fleet's shared hermes key), so the
// key alone cannot say WHO spent the window; this header can. It is a CLAIM, recorded for
// attribution only: nothing routes on it and it is never forwarded upstream.
const FleetCallerHeader = "X-Fleet-Caller"

// HermesOriginHeader is the older Hermes attribution header (`agent=;session=;platform=;cron=`),
// accepted as a fallback when X-Fleet-Caller is absent. Same grammar, same allowlist.
const HermesOriginHeader = "X-Hermes-Origin"

// callerClaimKeys is the closed allowlist of claim keys. Unknown keys are dropped, never stored.
var callerClaimKeys = map[string]struct{}{
	"harness": {}, "agent": {}, "platform": {}, "card": {}, "kind": {},
	"task": {}, "session": {}, "cron": {}, "host": {},
}

// callerClaimValue is the value grammar: no spaces, slashes, commas or quotes, length capped,
// so a value can never hold a URL, a routing rule, a token or a prompt fragment.
var callerClaimValue = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// callerClaimDeny drops values that look like secrets or routing config even when they pass
// the charset (a hit means config leaked into an identity field).
var callerClaimDeny = []string{"http", "://", "api_key", "apikey", "sk-", "bearer", "token"}

// ParseCallerClaim reads X-Fleet-Caller (or X-Hermes-Origin) from the downstream request headers
// and returns the validated key/value claims, or nil when the request carries none. Every field is
// validated independently: a bad field is dropped and the rest survive. The first header value wins.
func ParseCallerClaim(headers http.Header) map[string]string {
	if headers == nil {
		return nil
	}
	raw := strings.TrimSpace(headers.Get(FleetCallerHeader))
	if raw == "" {
		raw = strings.TrimSpace(headers.Get(HermesOriginHeader))
	}
	if raw == "" {
		return nil
	}
	return parseCallerClaimValue(raw)
}

func parseCallerClaimValue(raw string) map[string]string {
	if len(raw) > 512 {
		raw = raw[:512]
	}
	var claim map[string]string
	for _, field := range strings.Split(raw, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if _, allowed := callerClaimKeys[key]; !allowed {
			continue
		}
		if !callerClaimValue.MatchString(value) {
			continue
		}
		lower := strings.ToLower(value)
		denied := false
		for _, bad := range callerClaimDeny {
			if strings.Contains(lower, bad) {
				denied = true
				break
			}
		}
		if denied {
			continue
		}
		if claim == nil {
			claim = make(map[string]string, 4)
		}
		if _, dup := claim[key]; !dup {
			claim[key] = value
		}
	}
	return claim
}
