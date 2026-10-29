package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// TestViewOptions checks labels and explicit author selection without credentials.
func TestViewOptions(t *testing.T) {
	cases := []struct {
		name, channel, authors string
		want                   []string
		invalid                bool
	}{
		{name: "channel and exact authors", channel: "ops-testplatform", authors: "redhat-chai-bot, openshift-bot", want: []string{"redhat-chai-bot", "openshift-bot"}},
		{name: "display hash accepted", channel: "#ops-testplatform"},
		{name: "another channel", channel: "alerts-testplatform-build-farms"},
		{name: "empty channel rejected", invalid: true},
		{name: "invalid channel rejected", channel: "ops channel", invalid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateChannelName(tc.channel)
			if (err != nil) != tc.invalid {
				t.Fatalf("unexpected channel validation: %v", err)
			}
			if diff := cmp.Diff(tc.want, splitAuthors(tc.authors)); diff != "" {
				t.Errorf("unexpected authors (-want +got):\n%s", diff)
			}
		})
	}
}

// TestActionPreviewBoundary keeps bulk selection read-only behind identity and CSRF checks.
func TestActionPreviewBoundary(t *testing.T) {
	const body = `{"occurrence_id":"C:123.000001","kind":"close","pattern":"^ci-alert$"}`
	cases := []struct {
		name, body              string
		origin, token, identity bool
		want, calls             int
	}{
		{name: "preview does not write", body: body, origin: true, token: true, identity: true, want: 200, calls: 1},
		{name: "identity required", body: body, origin: true, token: true, want: 403},
		{name: "origin required", body: body, token: true, identity: true, want: 403},
		{name: "CSRF required", body: body, origin: true, identity: true, want: 403},
		{name: "unknown fields rejected", body: `{"actor":"spoofed"}`, origin: true, token: true, identity: true, want: 400},
		{name: "trailing JSON rejected", body: body + ` {}`, origin: true, token: true, identity: true, want: 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &dashboardTestService{}
			d, err := newDashboard(svc, testAccess(), false)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/api/action-preview", strings.NewReader(tc.body))
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("Content-Type", "application/json")
			if tc.identity {
				r.Header.Set("X-Forwarded-Email", "operator@example.test")
			}
			if tc.origin {
				r.Header.Set("Origin", "https://alerts.example.test")
			}
			if tc.token {
				r.Header.Set("X-CSRF-Token", d.csrfToken("operator@example.test"))
			}
			w := httptest.NewRecorder()
			d.ServeHTTP(w, r)
			if w.Code != tc.want || svc.previewCalls != tc.calls || svc.calls != 0 {
				t.Fatalf("status=%d preview calls=%d writes=%d", w.Code, svc.previewCalls, svc.calls)
			}
			if tc.calls != 0 {
				want := opsalerts.PreviewRequest{OccurrenceID: "C:123.000001", Kind: "close", Pattern: "^ci-alert$"}
				if diff := cmp.Diff(want, svc.previewRequest); diff != "" {
					t.Errorf("unexpected preview (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestPRViewAuthorization protects GitHub cache reads with the alert SSO boundary.
func TestPRViewAuthorization(t *testing.T) {
	cases := []struct {
		name     string
		identity bool
		want     int
	}{
		{name: "authenticated disabled view", identity: true, want: 200},
		{name: "anonymous blocked", want: 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := newDashboard(&dashboardTestService{}, testAccess(), false)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "/api/unmerged-prs", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			if tc.identity {
				r.Header.Set("X-Forwarded-Email", "operator@example.test")
			}
			w := httptest.NewRecorder()
			d.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d", w.Code, tc.want)
			}
			if tc.want == 200 {
				var got pullRequestSnapshot
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.Enabled || got.Phase != "disabled" || got.PRs == nil {
					t.Fatalf("unexpected disabled snapshot: %+v", got)
				}
			}
		})
	}
}
