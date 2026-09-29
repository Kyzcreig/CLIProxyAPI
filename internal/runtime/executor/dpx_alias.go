package executor

import (
	"bytes"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
	execpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func (e *ClaudeExecutor) dpxAliasEnabled() bool { return e.cfg != nil && e.cfg.DPXContentAlias.Enabled }
func (e *ClaudeExecutor) prepareDPXAlias(raw []byte, opts execpkg.Options, native, cloaked bool) ([]byte, *contentalias.RequestMap, error) {
	if !e.dpxAliasEnabled() {
		return raw, nil, nil
	}
	if opts.SourceFormat != translator.FromString("claude") || !native || cloaked {
		return nil, nil, contentalias.Error("native_route_required")
	}
	if e.cfg.RequestLog || e.cfg.Debug || e.cfg.RequestRetry != 0 || e.cfg.MaxRetryCredentials != 1 {
		return nil, nil, contentalias.Error("unsafe_daemon_config")
	}
	cfg := e.cfg.DPXContentAlias
	session, err := contentalias.Open(cfg.StoreDirectory, contentalias.Binding{Principal: cfg.Principal, Session: cfg.SessionID, Version: cfg.Version}, contentalias.DefaultManifest())
	if err != nil {
		return nil, nil, err
	}
	return contentalias.Prepare(raw, session)
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
