package main

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestParseOptions guards deliberate production startup and local-only demo mode.
func TestParseOptions(t *testing.T) {
	cases := []struct {
		name             string
		args             []string
		wantError        string
		demo, validation bool
	}{
		{name: "local demo", args: []string{"--demo"}, demo: true},
		{name: "IPv6 local demo", args: []string{"--demo", "--listen=[::1]:8080"}, demo: true},
		{name: "public demo rejected", args: []string{"--demo", "--listen=0.0.0.0:8080"}, wantError: "loopback"},
		{name: "demo credentials rejected", args: []string{"--demo", "--slack-token-file=secret"}, wantError: "cannot be combined"},
		{name: "production requires config", wantError: "required"},
		{name: "production requires credentials", args: []string{"--config=settings.yaml", "--classification=classes.yaml"}, wantError: "required"},
		{name: "production configured", args: []string{"--config=settings.yaml", "--classification=classes.yaml", "--slack-token-file=secret"}},
		{name: "offline validation requires no token", args: []string{"--validate-only", "--config=settings.yaml", "--classification=classes.yaml"}, validation: true},
		{name: "short interval rejected", args: []string{"--demo", "--sync-interval=100ms"}, wantError: "at least 1s"},
		{name: "positional argument rejected", args: []string{"--demo", "unexpected"}, wantError: "no positional arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOptions(tc.args)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want error containing %q, got %v", tc.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff([]bool{tc.demo, tc.validation}, []bool{got.demo, got.validateOnly}); diff != "" {
				t.Errorf("unexpected startup modes (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCompileAccess rejects configurations that weaken authenticated request provenance.
func TestCompileAccess(t *testing.T) {
	cases := []struct {
		name      string
		cfg       accessConfig
		demo      bool
		wantError string
	}{
		{name: "production", cfg: testAccess()},
		{name: "missing allowlist", cfg: accessConfig{ExternalURL: "https://alerts.example.test"}, wantError: "at least one"},
		{name: "insecure production", cfg: accessConfig{ExternalURL: "http://alerts.example.test", Operators: []string{"operator"}}, wantError: "must use https"},
		{name: "origin cannot contain path", cfg: accessConfig{ExternalURL: "https://alerts.example.test/dashboard"}, wantError: "without a path"},
		{name: "origin cannot contain credentials", cfg: accessConfig{ExternalURL: "https://user@alerts.example.test"}, wantError: "without a path"},
		{name: "universal proxy rejected", cfg: accessConfig{ExternalURL: "https://alerts.example.test", Operators: []string{"operator"}, TrustedProxies: []string{"0.0.0.0/0"}}, wantError: "non-universal"},
		{name: "malformed proxy rejected", cfg: accessConfig{ExternalURL: "https://alerts.example.test", Operators: []string{"operator"}, TrustedProxies: []string{"127.0.0.1"}}, wantError: "CIDRs"},
		{name: "ambiguous operator rejected", cfg: accessConfig{ExternalURL: "https://alerts.example.test", Operators: []string{"one,two"}}, wantError: "invalid identity"},
		{name: "header injection rejected", cfg: accessConfig{ExternalURL: "https://alerts.example.test", UserHeader: "X-User\r\nInjected"}, wantError: "user_header"},
		{name: "local demo", cfg: accessConfig{ExternalURL: "http://127.0.0.1:8080"}, demo: true},
		{name: "remote insecure demo rejected", cfg: accessConfig{ExternalURL: "http://192.0.2.1:8080"}, demo: true, wantError: "must use https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileAccess(tc.cfg, tc.demo)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want error containing %q, got %v", tc.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
