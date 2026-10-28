package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// sopTestService supplies only the selected occurrence's known links.
type sopTestService struct{ snapshot opsalerts.Snapshot }

// Snapshot returns fixed source membership for the HTTP boundary tests.
func (s *sopTestService) Snapshot() opsalerts.Snapshot { return s.snapshot }

// Act is unused by read-only SOP requests.
func (s *sopTestService) Act(context.Context, string, opsalerts.ActionRequest) (opsalerts.Occurrence, error) {
	return opsalerts.Occurrence{}, nil
}

// sopTestClient records the destination and supplies deterministic upstream failures.
type sopTestClient struct {
	status    int
	body      string
	err       error
	request   *http.Request
	calls     int
	readError bool
}

// Do avoids network access while retaining the exact outgoing request.
func (c *sopTestClient) Do(r *http.Request) (*http.Response, error) {
	c.request = r
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	body := io.NopCloser(strings.NewReader(c.body))
	if c.readError {
		body = io.NopCloser(sopErrorReader{})
	}
	return &http.Response{StatusCode: c.status, Body: body}, nil
}

// sopErrorReader exposes a failed body read without a network connection.
type sopErrorReader struct{}

// Read always fails to model an interrupted response.
func (sopErrorReader) Read([]byte) (int, error) { return 0, errors.New("private upstream failure") }

// TestDashboardSOP exercises membership and fetching behind the existing authentication.
func TestDashboardSOP(t *testing.T) {
	const source = "https://github.com/openshift/release/blob/main/docs/dptp-triage-sop/infrastructure-jobs.md#periodic-ci-secret-bootstrap"
	const raw = "https://raw.githubusercontent.com/openshift/release/main/docs/dptp-triage-sop/infrastructure-jobs.md"
	cases := []struct {
		name, id, link, body    string
		unauth                  bool
		upstream, status, calls int
		err                     error
		readError               bool
		code                    string
	}{
		{name: "success with fragment", id: "C:1", link: source, body: "# SOP\nSteps", upstream: 200, status: 200, calls: 1},
		{name: "authentication", id: "C:1", link: source, unauth: true, status: 403, code: "forbidden"},
		{name: "missing occurrence", id: "missing", link: source, status: 404, code: "not_found"},
		{name: "missing ID", link: source, status: 404, code: "not_found"},
		{name: "unlinked document", id: "C:1", link: strings.ReplaceAll(source, "infrastructure-jobs", "misc"), status: 404, code: "not_found"},
		{name: "missing URL", id: "C:1", status: 404, code: "not_found"},
		{name: "unsafe stored URL", id: "C:unsafe", link: "https://example.test/sop.md", status: 404, code: "not_found"},
		{name: "HTTP missing", id: "C:1", link: source, upstream: 404, status: 502, calls: 1, code: "sop_unavailable"},
		{name: "redirect response", id: "C:1", link: source, upstream: 302, status: 502, calls: 1, code: "sop_unavailable"},
		{name: "network error", id: "C:1", link: source, err: errors.New("private token"), status: 502, calls: 1, code: "sop_unavailable"},
		{name: "body error", id: "C:1", link: source, upstream: 200, readError: true, status: 502, calls: 1, code: "sop_unavailable"},
		{name: "oversize", id: "C:1", link: source, body: strings.Repeat("a", maxSOPBytes+1), upstream: 200, status: 502, calls: 1, code: "sop_unavailable"},
		{name: "size limit", id: "C:1", link: source, body: strings.Repeat("a", maxSOPBytes), upstream: 200, status: 200, calls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &sopTestService{snapshot: opsalerts.Snapshot{Occurrences: []opsalerts.Occurrence{
				{Identity: opsalerts.OccurrenceIdentity{ID: "C:1"}, Message: opsalerts.Message{SOPURLs: []string{source}}},
				{Identity: opsalerts.OccurrenceIdentity{ID: "C:unsafe"}, Message: opsalerts.Message{SOPURLs: []string{"https://example.test/sop.md"}}},
			}}}
			d, err := newDashboard(service, testAccess(), false)
			if err != nil {
				t.Fatal(err)
			}
			client := &sopTestClient{status: tc.upstream, body: tc.body, err: tc.err, readError: tc.readError}
			d.sopClient = client
			query := url.Values{"occurrence_id": {tc.id}, "url": {tc.link}}
			request := httptest.NewRequest(http.MethodGet, "/api/sop?"+query.Encode(), nil)
			request.RemoteAddr = "127.0.0.1:1234"
			if !tc.unauth {
				request.Header.Set("X-Forwarded-Email", "operator@example.test")
			}
			response := httptest.NewRecorder()
			d.ServeHTTP(response, request)
			if diff := cmp.Diff(tc.status, response.Code); diff != "" {
				t.Fatalf("unexpected status (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.calls, client.calls); diff != "" {
				t.Errorf("unexpected fetch count (-want +got):\n%s", diff)
			}
			if tc.calls > 0 {
				if diff := cmp.Diff(raw, client.request.URL.String()); diff != "" {
					t.Errorf("unexpected destination (-want +got):\n%s", diff)
				}
				if client.request.Method != http.MethodGet {
					t.Errorf("method = %s", client.request.Method)
				}
				if _, ok := client.request.Context().Deadline(); !ok {
					t.Error("fetch has no deadline")
				}
			}
			if tc.status == 200 {
				var got sopResponse
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(sopResponse{URL: source, Markdown: tc.body}, got); diff != "" {
					t.Errorf("unexpected result (-want +got):\n%s", diff)
				}
				return
			}
			var got apiError
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.code, got.Code); diff != "" {
				t.Errorf("unexpected code (-want +got):\n%s", diff)
			}
			if strings.Contains(got.Error, "private") {
				t.Error("upstream details leaked")
			}
		})
	}
}

// TestSOPClientPolicy checks redirect rejection without contacting a remote service.
func TestSOPClientPolicy(t *testing.T) {
	cases := []struct{ name string }{{name: "production policy"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newSOPClient()
			if client.Timeout.Seconds() != 8 {
				t.Errorf("timeout = %s", client.Timeout)
			}
			if err := client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
				t.Errorf("redirect error = %v", err)
			}
		})
	}
}
