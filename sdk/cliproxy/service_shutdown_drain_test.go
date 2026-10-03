package cliproxy

import (
	"testing"
	"time"
)

func TestShutdownDrainTimeout(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", 0},
		{"1300", 1300 * time.Second},
		{" 0 ", 0},
		{"-5", 0},
		{"abc", 0},
	}
	for _, tc := range cases {
		t.Setenv(shutdownDrainEnv, tc.env)
		if got := shutdownDrainTimeout(); got != tc.want {
			t.Errorf("%s=%q: got %s, want %s", shutdownDrainEnv, tc.env, got, tc.want)
		}
	}
}
