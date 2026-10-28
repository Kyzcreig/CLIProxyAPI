package auth

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// transientUpstreamError marks an upstream failure that says nothing about the
// request or the credential, such as the codex backend closing the stream before
// response.completed. Such errors may be request-scoped (no cooldown, no rotation
// inside a round) and still be safe to retry on another credential.
type transientUpstreamError interface {
	IsTransientUpstream() bool
}

// isTransientUpstreamRetryError reports failures that request-retry should retry on
// another credential even when the regular retry predicate declines them: a marked
// transient 408/5xx, or an upstream stream that closed before its first payload.
// Both happen before anything is written downstream.
func isTransientUpstreamRetryError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if isRequestStopError(err) || isRequestTerminatedError(err) {
		return false
	}
	var marker transientUpstreamError
	if errors.As(err, &marker) && marker != nil && marker.IsTransientUpstream() {
		switch statusCodeFromError(err) {
		case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	var authErr *Error
	return errors.As(err, &authErr) && authErr != nil && authErr.Code == "empty_stream"
}

// transientRetryState is the per-request record of transient retries: the budget
// (request-retry, shared with the regular retry rounds), the credentials that
// already failed, and the pending log line.
type transientRetryState struct {
	max      int
	retries  int
	excluded map[string]struct{}
	label    string
	failed   string
	logged   bool
}

type transientRetryContextKey struct{}

func withTransientRetryState(ctx context.Context, requestRetry int) (context.Context, *transientRetryState) {
	if requestRetry < 0 {
		requestRetry = 0
	}
	st := &transientRetryState{max: requestRetry, excluded: make(map[string]struct{}), logged: true}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, transientRetryContextKey{}, st), st
}

func transientRetryStateFromContext(ctx context.Context) *transientRetryState {
	if ctx == nil {
		return nil
	}
	st, _ := ctx.Value(transientRetryContextKey{}).(*transientRetryState)
	return st
}

// seedTransientExclusions adds the credentials that already failed transiently for
// this request to tried, so the next pick moves to another credential. It reports
// whether anything was added.
func seedTransientExclusions(ctx context.Context, tried map[string]struct{}) bool {
	st := transientRetryStateFromContext(ctx)
	if st == nil || len(st.excluded) == 0 {
		return false
	}
	seeded := false
	for id := range st.excluded {
		if _, already := tried[id]; !already {
			tried[id] = struct{}{}
			seeded = true
		}
	}
	return seeded
}

// releaseTransientExclusions undoes seedTransientExclusions when no other credential
// can be picked, so the retry may reuse a failed one instead of failing the pick.
func releaseTransientExclusions(ctx context.Context, tried, base map[string]struct{}) {
	st := transientRetryStateFromContext(ctx)
	if st == nil {
		return
	}
	for id := range st.excluded {
		if _, keep := base[id]; !keep {
			delete(tried, id)
		}
	}
}

// logTransientRetryPick writes the one retry line once the retry has picked its credential.
func logTransientRetryPick(ctx context.Context, entry *log.Entry, auth *Auth) {
	st := transientRetryStateFromContext(ctx)
	if st == nil || st.logged || auth == nil {
		return
	}
	st.logged = true
	if entry == nil {
		entry = logEntryWithRequestID(ctx)
	}
	entry.Infof("retrying after transient %s on %s (attempt %d/%d, next %s)", st.label, st.failed, st.retries, st.max, transientAuthLogName(auth))
}

func transientAuthLogName(auth *Auth) string {
	if auth == nil {
		return "unknown"
	}
	if auth.FileName != "" {
		return filepath.Base(auth.FileName)
	}
	return auth.ID
}

// transientRetryDecision decides whether a transient upstream failure that the
// regular retry predicate declined is retried, and how long to wait first. attempt
// is the caller's retry-round counter, so transient retries and regular retry
// rounds share one request-retry budget. The credentials attempted in the failed
// round are excluded from the next pick.
func (m *Manager) transientRetryDecision(ctx context.Context, opts cliproxyexecutor.Options, st *transientRetryState, err error, attempt int, attempted map[string]struct{}, providers []string, model string, maxWait time.Duration) (time.Duration, bool) {
	if m == nil || st == nil || m.HomeEnabled() || attempt < 0 || attempt >= st.max || !isTransientUpstreamRetryError(err) {
		return 0, false
	}
	failed := make([]string, 0, len(attempted))
	for id := range attempted {
		st.excluded[id] = struct{}{}
		name := id
		if auth, ok := m.GetByID(id); ok {
			name = transientAuthLogName(auth)
		}
		failed = append(failed, name)
	}
	sort.Strings(failed)
	wait, ok := m.transientRetryWait(ctx, opts, providers, model, st.excluded, maxWait)
	if !ok {
		return 0, false
	}
	st.retries = attempt + 1
	if status := statusCodeFromError(err); status != 0 {
		st.label = strconv.Itoa(status)
	} else {
		st.label = "empty_stream"
	}
	st.failed = strings.Join(failed, ",")
	if st.failed == "" {
		st.failed = "unknown"
	}
	st.logged = false
	return wait, true
}

// transientRetryWait returns 0 when a credential outside excluded is usable now,
// 0 when only an excluded one is (same-credential retry), otherwise the shortest
// cooldown left, as long as it fits in maxWait.
func (m *Manager) transientRetryWait(ctx context.Context, opts cliproxyexecutor.Options, providers []string, model string, excluded map[string]struct{}, maxWait time.Duration) (time.Duration, bool) {
	providerSet := make(map[string]struct{}, len(providers))
	for _, p := range providers {
		if key := strings.TrimSpace(strings.ToLower(p)); key != "" {
			providerSet[key] = struct{}{}
		}
	}
	if len(providerSet) == 0 {
		return 0, false
	}
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	registryRef := registry.GetGlobalRegistry()
	now := time.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	freeExcluded := false
	var minNext time.Time
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if pinnedAuthID != "" && auth.ID != pinnedAuthID {
			continue
		}
		if !eligibility.allows(auth) {
			continue
		}
		if _, ok := providerSet[executorKeyFromAuth(auth)]; !ok {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(registryRef, auth, model) {
			continue
		}
		checkModel := model
		if strings.TrimSpace(model) != "" {
			checkModel = m.selectionModelForAuth(auth, model)
		}
		blocked, reason, next := isAuthBlockedForModel(auth, checkModel, now)
		if !blocked {
			if _, isExcluded := excluded[auth.ID]; !isExcluded {
				return 0, true
			}
			freeExcluded = true
			continue
		}
		if reason == blockReasonDisabled || next.IsZero() {
			continue
		}
		if minNext.IsZero() || next.Before(minNext) {
			minNext = next
		}
	}
	if freeExcluded {
		return 0, true
	}
	if minNext.IsZero() {
		return 0, false
	}
	wait := minNext.Sub(now)
	if wait <= 0 {
		return 0, true
	}
	if maxWait <= 0 || wait > maxWait {
		return 0, false
	}
	return wait, true
}
