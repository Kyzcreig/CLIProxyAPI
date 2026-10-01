package cmd

import (
	"fmt"
	"strings"
	"time"
)

// LoginTimeoutEnv names the environment variable read when -login-timeout is not set.
const LoginTimeoutEnv = "CLIPROXYAPI_LOGIN_TIMEOUT"

// ResolveLoginTimeout picks the callback/paste wait for -*-login flows.
// A positive flag value wins; otherwise envValue is parsed as a Go duration.
// Zero means "use the provider default" (5 minutes).
func ResolveLoginTimeout(flagValue time.Duration, envValue string) (time.Duration, error) {
	if flagValue < 0 {
		return 0, fmt.Errorf("-login-timeout must not be negative: %s", flagValue)
	}
	if flagValue > 0 {
		return flagValue, nil
	}
	envValue = strings.TrimSpace(envValue)
	if envValue == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(envValue)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", LoginTimeoutEnv, envValue, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must not be negative: %s", LoginTimeoutEnv, envValue)
	}
	return d, nil
}
