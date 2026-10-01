package executor

import (
	"bytes"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	execpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func (e *ClaudeExecutor) dpxAliasEnabled() bool { return e.cfg != nil && e.cfg.DPXContentAlias.Enabled }

// dpxLaneGateEnabled is separate from aliasing (t_40200f8a): the d-family
// lane gate + W1 wirelog run on a re-sign-only unit (alias OFF — apx is the
// one aliasing layer per Ace 2026-09-29). Gated on the alias SECTION being
// declared (lane set or wirelog spool), so a legacy config without the
// section is untouched byte-for-byte.
func (e *ClaudeExecutor) dpxLaneGateEnabled() bool { return helps.DPXSectionDeclared(e.cfg) }
func (e *ClaudeExecutor) prepareDPXAlias(raw []byte, opts execpkg.Options, native, cloaked bool) ([]byte, *contentalias.RequestMap, error) {
	if !e.dpxLaneGateEnabled() {
		return raw, nil, nil
	}
	if opts.SourceFormat != translator.FromString("claude") || !native || cloaked {
		return nil, nil, contentalias.Error("native_route_required")
	}
	if e.cfg.RequestLog || e.cfg.Debug || e.cfg.RequestRetry != 0 || e.cfg.MaxRetryCredentials != 1 {
		return nil, nil, contentalias.Error("unsafe_daemon_config")
	}
	cfg := e.cfg.DPXContentAlias
	userAgent := ""
	if opts.Headers != nil {
		userAgent = opts.Headers.Get("User-Agent")
	}
	// The lane gate runs even when aliasing is off (a re-sign-only d-family
	// unit still admits only the entrypoints its lanes declare, t_40200f8a).
	if _, reason := helps.ResolveDPXRequestLane(cfg.EffectiveLanes(), raw, userAgent); reason != "" {
		return nil, nil, contentalias.Error(reason)
	}
	if !e.dpxAliasEnabled() {
		return raw, nil, nil
	}
	session, err := contentalias.Open(cfg.StoreDirectory, contentalias.Binding{Principal: cfg.Principal, Session: cfg.SessionID, Version: cfg.Version}, contentalias.DefaultManifest())
	if err != nil {
		return nil, nil, err
	}
	return contentalias.Prepare(raw, session)
}

// dpxWirelogClient adds the W1 wirelog row writer to a declared d-family
// unit's upstream client (the alias SECTION present — lane and/or spool —
// even with alias enabled=false, the re-sign-only shape). Off unless
// wirelog-spool is set.
func (e *ClaudeExecutor) dpxWirelogClient(client *http.Client) *http.Client {
	if !e.dpxLaneGateEnabled() || e.cfg.DPXContentAlias.WirelogSpool == "" {
		return client
	}
	cfg := e.cfg.DPXContentAlias
	return helps.DPXWirelogClient(client, helps.DPXWirelogConfig{Spool: cfg.WirelogSpool, Lane: cfg.WirelogLane, Lanes: cfg.Lanes, Sub: cfg.WirelogSub, BrandWords: contentalias.DefaultManifest().Words})
}

// validateDPXFinalBody enforces that nothing after aliasing changes the upstream
// body except CPA's native CCH signing of the Claude Code billing block: the
// " cch=00000;" placeholder insertion and the five signed digits. Every other
// byte of the aliased body must reach upstream unchanged.
func validateDPXFinalBody(before, after []byte, m *contentalias.RequestMap) error {
	if m == nil || bytes.Equal(before, after) {
		return nil
	}
	if bytes.Equal(withoutDPXCCHField(before), withoutDPXCCHField(after)) {
		return nil
	}
	return contentalias.Error("post_alias_mutation")
}

// withoutDPXCCHField returns body with the billing block's " cch=XXXXX;" field
// removed, or with only its digits zeroed when the field is not space-led.
func withoutDPXCCHField(body []byte) []byte {
	offset, ok := claudeBillingCCHDigitsOffset(body)
	if !ok {
		return body
	}
	const field = " cch="
	start, end := offset-len(field), offset+claudeCCHLength+1
	if start < 0 || end > len(body) || string(body[start:offset]) != field || body[end-1] != ';' {
		out := bytes.Clone(body)
		copy(out[offset:offset+claudeCCHLength], claudeCCHZero)
		return out
	}
	out := make([]byte, 0, len(body)-(end-start))
	out = append(out, body[:start]...)
	return append(out, body[end:]...)
}

type dpxAliasHTTPError struct{ code int }

func (e dpxAliasHTTPError) Error() string         { return "contentalias:upstream_failure" }
func (e dpxAliasHTTPError) StatusCode() int       { return e.code }
func (e dpxAliasHTTPError) IsRequestScoped() bool { return true }
