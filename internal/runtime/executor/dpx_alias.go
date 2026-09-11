package executor

import (
	"bytes"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/contentalias"
	execpkg "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func (e *ClaudeExecutor) dpxAliasEnabled() bool { return e.cfg != nil && e.cfg.DPXContentAlias.Enabled }
func (e *ClaudeExecutor) prepareDPXAlias(raw []byte, opts execpkg.Options, native, cloaked, signing bool) ([]byte, *contentalias.RequestMap, error) {
	if !e.dpxAliasEnabled() {
		return raw, nil, nil
	}
	if opts.SourceFormat != translator.FromString("claude") || !native || cloaked {
		return nil, nil, contentalias.Error("native_route_required")
	}
	if signing {
		return nil, nil, contentalias.Error("body_signing_unsupported")
	}
	if _, signed := claudeBillingCCHDigitsOffset(raw); signed {
		return nil, nil, contentalias.Error("signed_body")
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
func validateDPXFinalBody(before, after []byte, m *contentalias.RequestMap) error {
	if m != nil && !bytes.Equal(before, after) {
		return contentalias.Error("post_alias_mutation")
	}
	return nil
}

type dpxAliasHTTPError struct{ code int }

func (e dpxAliasHTTPError) Error() string         { return "contentalias:upstream_failure" }
func (e dpxAliasHTTPError) StatusCode() int       { return e.code }
func (e dpxAliasHTTPError) IsRequestScoped() bool { return true }
