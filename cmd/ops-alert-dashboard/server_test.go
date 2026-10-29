package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// dashboardTestService records the authenticated boundary without contacting Slack.
type dashboardTestService struct {
	actor          string
	request        opsalerts.ActionRequest
	calls          int
	err            error
	previewRequest opsalerts.PreviewRequest
	previewCalls   int
}

// Snapshot supplies a ready cache for HTTP unit tests.
func (s *dashboardTestService) Snapshot() opsalerts.Snapshot {
	return opsalerts.Snapshot{Status: opsalerts.Coverage{Phase: "ready"}}
}

// Act records exactly what the HTTP boundary authorizes.
func (s *dashboardTestService) Act(_ context.Context, actor string, request opsalerts.ActionRequest) (opsalerts.Occurrence, error) {
	s.actor, s.request = actor, request
	s.calls++
	return opsalerts.Occurrence{}, s.err
}

// Preview records the read-only request independently of action writes.
func (s *dashboardTestService) Preview(request opsalerts.PreviewRequest) ([]opsalerts.Occurrence, error) {
	s.previewRequest = request
	s.previewCalls++
	return []opsalerts.Occurrence{}, s.err
}

// testAccess fixes the external origin and authenticated operator for requests.
func testAccess() accessConfig {
	return accessConfig{ExternalURL: "https://alerts.example.test", Operators: []string{"operator@example.test"}}
}

// proxyAccess delegates group membership to an explicitly trusted authorizing proxy.
func proxyAccess() accessConfig {
	return accessConfig{Authorization: "proxy", ExternalURL: "https://alerts.example.test", TrustedProxies: []string{"127.0.0.1/32", "::1/128"}}
}

// TestDashboardAuthorization ensures browser identity cannot bypass the proxy boundary.
func TestDashboardAuthorization(t *testing.T) {
	cases := []struct {
		name, path, remote string
		users              []string
		proxy              bool
		status             int
		user               string
	}{
		{name: "proxy authorized identity", proxy: true, path: "/api/snapshot", remote: "127.0.0.1:1234", users: []string{" Group.Member@Example.Test "}, status: 200, user: "group.member@example.test"},
		{name: "proxy IPv6 source", proxy: true, path: "/api/snapshot", remote: "[::1]:1234", users: []string{"group.member@example.test"}, status: 200, user: "group.member@example.test"},
		{name: "proxy untrusted remote", proxy: true, path: "/api/snapshot", remote: "192.0.2.10:1234", users: []string{"group.member@example.test"}, status: 403},
		{name: "proxy missing identity", proxy: true, path: "/api/snapshot", remote: "127.0.0.1:1234", status: 403},
		{name: "proxy empty identity", proxy: true, path: "/api/snapshot", remote: "127.0.0.1:1234", users: []string{"  "}, status: 403},
		{name: "proxy duplicate identity", proxy: true, path: "/api/snapshot", remote: "127.0.0.1:1234", users: []string{"one", "two"}, status: 403},
		{name: "proxy comma identity", proxy: true, path: "/api/snapshot", remote: "127.0.0.1:1234", users: []string{"one,two"}, status: 403},
		{name: "proxy whitespace identity", proxy: true, path: "/api/snapshot", remote: "127.0.0.1:1234", users: []string{"one two"}, status: 403},
		{name: "proxy control identity", proxy: true, path: "/api/snapshot", remote: "127.0.0.1:1234", users: []string{"one\x00two"}, status: 403},
		{name: "proxy newline identity", proxy: true, path: "/api/snapshot", remote: "127.0.0.1:1234", users: []string{"one\r\n"}, status: 403},
		{name: "proxy invalid UTF8 identity", proxy: true, path: "/api/snapshot", remote: "127.0.0.1:1234", users: []string{"one\xfftwo"}, status: 403},
		{name: "proxy oversized identity", proxy: true, path: "/api/snapshot", remote: "127.0.0.1:1234", users: []string{strings.Repeat("a", 257)}, status: 403},
		{name: "proxy assets require identity", proxy: true, path: "/app.js", remote: "127.0.0.1:1234", status: 403},
		{name: "trusted operator", path: "/api/snapshot", remote: "127.0.0.1:1234", users: []string{" Operator@Example.Test "}, status: 200, user: "operator@example.test"},
		{name: "untrusted remote", path: "/api/snapshot", remote: "192.0.2.10:1234", users: []string{"operator@example.test"}, status: 403},
		{name: "missing identity", path: "/api/snapshot", remote: "127.0.0.1:1234", status: 403},
		{name: "unlisted identity", path: "/api/snapshot", remote: "127.0.0.1:1234", users: []string{"other@example.test"}, status: 403},
		{name: "duplicate identity header", path: "/api/snapshot", remote: "127.0.0.1:1234", users: []string{"operator@example.test", "operator@example.test"}, status: 403},
		{name: "invalid remote", path: "/api/snapshot", remote: "localhost", users: []string{"operator@example.test"}, status: 403},
		{name: "assets require identity", path: "/app.js", remote: "127.0.0.1:1234", status: 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testAccess()
			if tc.proxy {
				cfg = proxyAccess()
			}
			d, err := newDashboard(&dashboardTestService{}, cfg, false)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			request.RemoteAddr = tc.remote
			for _, user := range tc.users {
				request.Header.Add("X-Forwarded-Email", user)
			}
			response := httptest.NewRecorder()
			d.ServeHTTP(response, request)
			if diff := cmp.Diff(tc.status, response.Code); diff != "" {
				t.Fatalf("unexpected status (-want +got):\n%s", diff)
			}
			if tc.user != "" {
				var got snapshotResponse
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(tc.user, got.User); diff != "" {
					t.Errorf("unexpected user (-want +got):\n%s", diff)
				}
				if got.CSRFToken == "" || got.CSRFToken == d.csrfToken("other@example.test") {
					t.Error("CSRF token must be nonempty and user-bound")
				}
			}
		})
	}
}

// TestDashboardActions checks provenance, strict JSON and distinguishable write outcomes.
func TestDashboardActions(t *testing.T) {
	const body = `{"occurrence_id":"C:123.000001","kind":"acknowledge","expected_revision":"123.000001","request_id":"retry-id"}`
	cases := []struct {
		name, body, origin, token, media string
		proxy                            bool
		serviceError                     error
		status                           int
		code                             string
		calls                            int
	}{
		{name: "proxy authenticated actor", proxy: true, body: body, origin: "valid", token: "valid", media: "application/json", status: 200, calls: 1},
		{name: "proxy missing origin", proxy: true, body: body, token: "valid", media: "application/json", status: 403, code: "forbidden"},
		{name: "proxy cross origin", proxy: true, body: body, origin: "https://evil.example.test", token: "valid", media: "application/json", status: 403, code: "forbidden"},
		{name: "proxy missing CSRF", proxy: true, body: body, origin: "valid", media: "application/json", status: 403, code: "forbidden"},
		{name: "proxy another user CSRF", proxy: true, body: body, origin: "valid", token: "other", media: "application/json", status: 403, code: "forbidden"},
		{name: "server authenticated actor", body: body, origin: "valid", token: "valid", media: "application/json", status: 200, calls: 1},
		{name: "missing origin", body: body, token: "valid", media: "application/json", status: 403, code: "forbidden"},
		{name: "cross origin", body: body, origin: "https://evil.example.test", token: "valid", media: "application/json", status: 403, code: "forbidden"},
		{name: "missing CSRF", body: body, origin: "valid", media: "application/json", status: 403, code: "forbidden"},
		{name: "another operator CSRF", body: body, origin: "valid", token: "other", media: "application/json", status: 403, code: "forbidden"},
		{name: "wrong content type", body: body, origin: "valid", token: "valid", media: "text/plain", status: 415, code: "validation"},
		{name: "malformed JSON", body: `{`, origin: "valid", token: "valid", media: "application/json", status: 400, code: "validation"},
		{name: "client supplied actor", body: `{"actor":"impersonated"}`, origin: "valid", token: "valid", media: "application/json", status: 400, code: "validation"},
		{name: "trailing JSON", body: body + ` {}`, origin: "valid", token: "valid", media: "application/json", status: 400, code: "validation"},
		{name: "oversize body", body: `{"question":"` + strings.Repeat("a", 9000) + `"}`, origin: "valid", token: "valid", media: "application/json", status: 400, code: "validation"},
		{name: "invalid action", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: opsalerts.ErrValidation, status: 400, code: "validation", calls: 1},
		{name: "missing occurrence", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: opsalerts.ErrNotFound, status: 404, code: "not_found", calls: 1},
		{name: "stale revision", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: fmt.Errorf("wrapped: %w", opsalerts.ErrConflict), status: 409, code: "conflict", calls: 1},
		{name: "incomplete coverage", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: opsalerts.ErrNotReady, status: 503, code: "not_ready", calls: 1},
		{name: "rejected delivery", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: opsalerts.ErrDeliveryFailed, status: 502, code: "delivery_failed", calls: 1},
		{name: "uncertain delivery", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: opsalerts.ErrDeliveryUncertain, status: 502, code: "delivery_uncertain", calls: 1},
		{name: "internal error", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: errors.New("unexpected"), status: 500, code: "internal", calls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &dashboardTestService{err: tc.serviceError}
			cfg := testAccess()
			actor := "operator@example.test"
			if tc.proxy {
				cfg = proxyAccess()
				actor = "group.member@example.test"
			}
			d, err := newDashboard(service, cfg, false)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/actions", strings.NewReader(tc.body))
			request.RemoteAddr = "127.0.0.1:1234"
			request.Header.Set("X-Forwarded-Email", actor)
			request.Header.Set("Content-Type", tc.media)
			origin := tc.origin
			if origin == "valid" {
				origin = "https://alerts.example.test"
			}
			request.Header.Set("Origin", origin)
			token := tc.token
			if token == "valid" {
				token = d.csrfToken(actor)
			}
			if token == "other" {
				token = d.csrfToken("other@example.test")
			}
			request.Header.Set("X-CSRF-Token", token)
			response := httptest.NewRecorder()
			d.ServeHTTP(response, request)
			if diff := cmp.Diff(tc.status, response.Code); diff != "" {
				t.Fatalf("unexpected status (-want +got):\n%s\n%s", diff, response.Body.String())
			}
			if diff := cmp.Diff(tc.calls, service.calls); diff != "" {
				t.Fatalf("unexpected action count (-want +got):\n%s", diff)
			}
			if tc.calls == 1 {
				if diff := cmp.Diff(actor, service.actor); diff != "" {
					t.Errorf("unexpected actor (-want +got):\n%s", diff)
				}
				want := opsalerts.ActionRequest{OccurrenceID: "C:123.000001", Kind: "acknowledge", ExpectedRevision: "123.000001", RequestID: "retry-id"}
				if diff := cmp.Diff(want, service.request); diff != "" {
					t.Errorf("unexpected action (-want +got):\n%s", diff)
				}
			}
			if tc.code != "" {
				var got apiError
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(tc.code, got.Code); diff != "" {
					t.Errorf("unexpected error code (-want +got):\n%s", diff)
				}
				if got.Error == "" {
					t.Error("error response must explain rejection")
				}
			}
		})
	}
}
