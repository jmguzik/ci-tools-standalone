package main

import (
	"testing"
)

func TestValidateOptions(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		o       options
		wantErr bool
	}{
		{
			name: "valid",
			o: options{
				logLevel:        "info",
				slackTokenPath:  "slack",
				hookTokenPath:   "hook",
				apiTokenPath:    "api",
				alertmanagerURL: "http://alertmanager.example",
			},
		},
		{
			name: "missing alertmanager-url",
			o: options{
				logLevel:       "info",
				slackTokenPath: "slack",
				hookTokenPath:  "hook",
				apiTokenPath:   "api",
			},
			wantErr: true,
		},
		{
			name: "missing slack-token-path",
			o: options{
				logLevel:        "info",
				hookTokenPath:   "hook",
				apiTokenPath:    "api",
				alertmanagerURL: "http://am",
			},
			wantErr: true,
		},
		{
			name: "bad log-level",
			o: options{
				logLevel:        "nope",
				slackTokenPath:  "slack",
				hookTokenPath:   "hook",
				apiTokenPath:    "api",
				alertmanagerURL: "http://am",
			},
			wantErr: true,
		},
	}
	for i := range testCases {
		tc := testCases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateOptions(tc.o)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
		})
	}
}

func TestTrimSecretBytes(t *testing.T) {
	t.Parallel()
	if got := string(trimSecretBytes([]byte("hook-token\n"))); got != "hook-token" {
		t.Fatalf("newline: got %q", got)
	}
	if got := string(trimSecretBytes([]byte("  hook-token\r\n"))); got != "hook-token" {
		t.Fatalf("spaces: got %q", got)
	}
	if len(trimSecretBytes([]byte(" \n\t"))) != 0 {
		t.Fatal("whitespace-only secret must be empty after trim")
	}
}
