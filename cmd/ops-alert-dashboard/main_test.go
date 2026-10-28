package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
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
		window           time.Duration
	}{
		{name: "history override", args: []string{"--demo", "--history-window=12h"}, demo: true, window: 12 * time.Hour},
		{name: "zero history rejected", args: []string{"--demo", "--history-window=0"}, wantError: "must be positive"},
		{name: "negative history rejected", args: []string{"--demo", "--history-window=-1h"}, wantError: "must be positive"},
		{name: "malformed history rejected", args: []string{"--demo", "--history-window=yesterday"}, wantError: "invalid value"},
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
			wantWindow := opsalerts.DefaultHistoryWindow
			if tc.window != 0 {
				wantWindow = tc.window
			}
			if got.historyWindow != wantWindow {
				t.Errorf("history window: want %v, got %v", wantWindow, got.historyWindow)
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

// TestLoadSettingsWithoutCutover validates rolling configuration without contacting Slack.
func TestLoadSettingsWithoutCutover(t *testing.T) {
	cases := []struct {
		name   string
		window time.Duration
	}{
		{name: "default", window: opsalerts.DefaultHistoryWindow},
		{name: "override", window: 6 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile("examples/config.yaml")
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(data), "\n")
			var kept []string
			for _, line := range lines {
				if strings.HasPrefix(strings.TrimSpace(line), "cutover:") {
					continue
				}
				kept = append(kept, line)
			}
			config := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(config, []byte(strings.Join(kept, "\n")), 0600); err != nil {
				t.Fatal(err)
			}
			o, err := parseOptions([]string{"--validate-only", "--config=" + config, "--classification=examples/classification.yaml", "--history-window=" + tc.window.String()})
			if err != nil {
				t.Fatal(err)
			}
			cfg, _, err := loadSettings(o)
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.Slack.Cutover.IsZero() || cfg.Slack.HistoryWindow != tc.window {
				t.Fatalf("unexpected rolling settings: %+v", cfg.Slack)
			}
		})
	}
}
