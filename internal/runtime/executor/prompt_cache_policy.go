package executor

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// resolvedPromptCacheKey returns the routing key an executor attaches for a request whose
// body carries no prompt_cache_key:
//   - enforce mode: the key the manager resolved (session hash or stable-prefix hash), or
//     "" when the caller opted out (passthrough) - nothing is attached.
//   - shadow mode, or a request that never went through the policy (direct executor use):
//     the legacy per-API-key UUID, i.e. the wire behaviour before the policy existed.
func resolvedPromptCacheKey(ctx context.Context, opts cliproxyexecutor.Options, provider string) string {
	if key := cliproxyexecutor.WirePromptCacheKeyFromMetadata(opts.Metadata); key != "" {
		return key
	}
	if cliproxyexecutor.PromptCacheKeyEnforced(opts.Metadata) {
		return ""
	}
	return legacyPerAPIKeyPromptCacheKey(ctx, provider)
}

// legacyPerAPIKeyPromptCacheKey is the pre-policy default: one stable UUID per proxy API
// key. Kept for shadow mode (the A/B control arm) and for callers of the executors that
// bypass the manager.
func legacyPerAPIKeyPromptCacheKey(ctx context.Context, provider string) string {
	apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx))
	if apiKey == "" {
		return ""
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:"+provider+":prompt-cache:"+apiKey)).String()
}
