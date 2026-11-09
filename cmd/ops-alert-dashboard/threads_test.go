package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// threadTestService isolates HTTP authorization and strict parsing from Slack transport.
type threadTestService struct {
	dashboardTestService
	request       opsalerts.ThreadReplyRequest
	actor         string
	reads, writes int
}

// AlertThread returns only a synthetic cached discussion.
func (s *threadTestService) AlertThread(id, before string) (opsalerts.AlertThread, error) {
	s.reads++
	return opsalerts.AlertThread{OccurrenceID: id, NextCursor: before}, s.err
}

// ReplyToAlert records the authenticated actor and exact transport safety flag.
func (s *threadTestService) ReplyToAlert(_ context.Context, actor string, req opsalerts.ThreadReplyRequest) (opsalerts.AlertThread, error) {
	s.writes++
	s.actor, s.request = actor, req
	return opsalerts.AlertThread{OccurrenceID: req.OccurrenceID}, s.err
}

// TestDashboardAlertThread tests both endpoints at the existing trusted-proxy boundary.
func TestDashboardAlertThread(t *testing.T) {
	const body = `{"occurrence_id":"C:100.000001","request_id":"plain","text":"hello","reconcile_only":true}`
	cases := []struct {
		name, method, body       string
		noOrigin, noCSRF, unauth bool
		reconcile                bool
		serviceErr               error
		status                   int
		code                     string
		writes                   int
	}{
		{name: "cached read", method: "GET", status: 200},
		{name: "reconciliation flag", method: "POST", body: body, status: 200, writes: 1, reconcile: true},
		{name: "initial submission omits flag", method: "POST", body: strings.Replace(body, `,"reconcile_only":true`, "", 1), status: 200, writes: 1},
		{name: "explicit false flag", method: "POST", body: strings.Replace(body, `"reconcile_only":true`, `"reconcile_only":false`, 1), status: 200, writes: 1},
		{name: "no origin", method: "POST", body: body, noOrigin: true, status: 403, code: "forbidden"},
		{name: "no CSRF", method: "POST", body: body, noCSRF: true, status: 403, code: "forbidden"},
		{name: "unauthenticated", method: "GET", unauth: true, status: 403, code: "forbidden"},
		{name: "arbitrary target", method: "POST", body: `{"channel_id":"C","text":"hello"}`, status: 400, code: "validation"},
		{name: "actor override", method: "POST", body: `{"actor":"other","text":"hello"}`, status: 400, code: "validation"},
		{name: "extra object", method: "POST", body: body + `{}`, status: 400, code: "validation"},
		{name: "duplicate reconcile flag", method: "POST", body: strings.TrimSuffix(body, "}") + `,"reconcile_only":false}`, status: 400, code: "validation"},
		{name: "case variant reconcile flag", method: "POST", body: strings.TrimSuffix(body, "}") + `,"Reconcile_only":false}`, status: 400, code: "validation"},
		{name: "null reconcile flag", method: "POST", body: strings.Replace(body, `"reconcile_only":true`, `"reconcile_only":null`, 1), status: 400, code: "validation"},
		{name: "numeric reconcile flag", method: "POST", body: strings.Replace(body, `"reconcile_only":true`, `"reconcile_only":1`, 1), status: 400, code: "validation"},
		{name: "string reconcile flag", method: "POST", body: strings.Replace(body, `"reconcile_only":true`, `"reconcile_only":"true"`, 1), status: 400, code: "validation"},
		{name: "case variant text", method: "POST", body: strings.Replace(body, `"text":`, `"Text":`, 1), status: 400, code: "validation"},
		{name: "duplicate text", method: "POST", body: strings.TrimSuffix(body, "}") + `,"text":"different"}`, status: 400, code: "validation"},
		{name: "null required string", method: "POST", body: strings.Replace(body, `"text":"hello"`, `"text":null`, 1), status: 400, code: "validation"},
		{name: "oversized", method: "POST", body: `{"text":"` + strings.Repeat("a", 17000) + `"}`, status: 400, code: "validation"},
		{name: "uncertain redacted", method: "POST", body: body, serviceErr: errors.Join(opsalerts.ErrDeliveryUncertain, errors.New("secret-token-upstream-body")), status: 502, code: "delivery_uncertain", writes: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &threadTestService{}
			service.err = tc.serviceErr
			d, err := newDashboard(service, testAccess(), false)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(tc.method, "/api/alert-thread?occurrence_id=C:100.000001&before=100.000002", strings.NewReader(tc.body))
			r.RemoteAddr = "127.0.0.1:1234"
			if !tc.unauth {
				r.Header.Set("X-Forwarded-Email", "operator@example.test")
			}
			if !tc.noOrigin {
				r.Header.Set("Origin", d.access.origin)
			}
			if !tc.noCSRF {
				r.Header.Set("X-CSRF-Token", d.csrfToken("operator@example.test"))
			}
			r.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			d.ServeHTTP(response, r)
			if diff := cmp.Diff(tc.status, response.Code); diff != "" {
				t.Fatalf("unexpected status (-want +got):\n%s\n%s", diff, response.Body.String())
			}
			if diff := cmp.Diff(tc.writes, service.writes); diff != "" {
				t.Errorf("unexpected writes (-want +got):\n%s", diff)
			}
			if strings.Contains(response.Body.String(), "secret-token") {
				t.Fatal("upstream error leaked")
			}
			if tc.status == 200 {
				var got alertThreadResponse
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.User != "operator@example.test" || got.CSRFToken != d.csrfToken(got.User) {
					t.Fatalf("identity response %#v", got)
				}
				if tc.method == "POST" && (service.request.ReconcileOnly != tc.reconcile || service.actor != got.User) {
					t.Fatalf("request %#v actor %q", service.request, service.actor)
				}
			} else {
				var got apiError
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(tc.code, got.Code); diff != "" {
					t.Errorf("unexpected code (-want +got):\n%s", diff)
				}
			}
		})
	}
}
