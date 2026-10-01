package cmd

import (
	"testing"
	"time"
)

func TestResolveLoginTimeout(t *testing.T) {
	cases := []struct {
		name    string
		flag    time.Duration
		env     string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset keeps provider default", want: 0},
		{name: "flag wins", flag: 30 * time.Minute, env: "10m", want: 30 * time.Minute},
		{name: "env used when flag unset", env: "45m", want: 45 * time.Minute},
		{name: "env whitespace trimmed", env: " 20m ", want: 20 * time.Minute},
		{name: "bad env", env: "soon", wantErr: true},
		{name: "negative env", env: "-1m", wantErr: true},
		{name: "negative flag", flag: -time.Minute, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveLoginTimeout(tc.flag, tc.env)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveLoginTimeout(%v, %q) = %v, want error", tc.flag, tc.env, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveLoginTimeout(%v, %q) error: %v", tc.flag, tc.env, err)
			}
			if got != tc.want {
				t.Fatalf("ResolveLoginTimeout(%v, %q) = %v, want %v", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}
