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

// conversationTestService records chat boundary calls independently of workflow actions.
type conversationTestService struct {
	dashboardTestService
	snapshot            opsalerts.ConversationSnapshot
	reads               int
	conversationCalls   int
	conversationActor   string
	conversationRequest opsalerts.ConversationRequest
}

// Conversations supplies only the published cache to the HTTP handler.
func (s *conversationTestService) Conversations() opsalerts.ConversationSnapshot {
	s.reads++
	return s.snapshot
}

// Converse records the server-derived identity and one authorized request.
func (s *conversationTestService) Converse(_ context.Context, actor string, request opsalerts.ConversationRequest) (opsalerts.Conversation, error) {
	s.conversationCalls++
	s.conversationActor, s.conversationRequest = actor, request
	return opsalerts.Conversation{ID: "C:200.000001", Title: "Question"}, s.err
}

// TestDashboardConversationsCache checks cached reads and user-bound CSRF under either access mode.
func TestDashboardConversationsCache(t *testing.T) {
	cases := []struct {
		name     string
		proxy    bool
		identity string
		remote   string
		status   int
	}{
		{name: "operator cache", identity: " Operator@Example.Test ", remote: "127.0.0.1:1234", status: 200},
		{name: "proxy cache", proxy: true, identity: " Group.Member@Example.Test ", remote: "127.0.0.1:1234", status: 200},
		{name: "missing identity", remote: "127.0.0.1:1234", status: 403},
		{name: "untrusted proxy", proxy: true, identity: "operator@example.test", remote: "192.0.2.1:1234", status: 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := opsalerts.ConversationSnapshot{Status: opsalerts.Coverage{Phase: "ready"}, Enabled: true, Conversations: []opsalerts.Conversation{{ID: "C:100.000001", Title: "Cached question"}}}
			service := &conversationTestService{snapshot: snapshot}
			cfg := testAccess()
			if tc.proxy {
				cfg = proxyAccess()
			}
			d, err := newDashboard(service, cfg, false)
			if err != nil {
				t.Fatal(err)
			}
			d.channelName = "ops-testplatform"
			request := httptest.NewRequest(http.MethodGet, "/api/conversations", nil)
			request.RemoteAddr = tc.remote
			request.Header.Set("X-Forwarded-Email", tc.identity)
			response := httptest.NewRecorder()
			d.ServeHTTP(response, request)
			if diff := cmp.Diff(tc.status, response.Code); diff != "" {
				t.Fatalf("unexpected status (-want +got):\n%s", diff)
			}
			reads := 0
			if tc.status == 200 {
				reads = 1
			}
			if diff := cmp.Diff(reads, service.reads); diff != "" {
				t.Errorf("unexpected cache reads (-want +got):\n%s", diff)
			}
			if service.conversationCalls != 0 || service.calls != 0 {
				t.Fatal("GET initiated a write")
			}
			if tc.status != 200 {
				return
			}
			var got conversationSnapshotResponse
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			actor := strings.ToLower(strings.TrimSpace(tc.identity))
			want := conversationSnapshotResponse{ConversationSnapshot: snapshot, User: actor, CSRFToken: d.csrfToken(actor), ChannelName: "ops-testplatform"}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("unexpected cached response (-want +got):\n%s", diff)
			}
			if got.CSRFToken == "" || got.CSRFToken == d.csrfToken("other@example.test") {
				t.Error("CSRF token must be user bound")
			}
		})
	}
}

// TestDashboardConverse checks strict request parsing, provenance, and distinguishable service failures.
func TestDashboardConverse(t *testing.T) {
	const body = `{"request_id":"chat-request-1","text":"Explain the controller backlog."}`
	cases := []struct {
		name, body, origin, token, media    string
		proxy, unauthenticated, unavailable bool
		serviceError                        error
		status, calls                       int
		code                                string
	}{
		{name: "new root", body: body, origin: "valid", token: "valid", media: "application/json", status: 200, calls: 1},
		{name: "proxy actor", proxy: true, body: body, origin: "valid", token: "valid", media: "application/json; charset=utf-8", status: 200, calls: 1},
		{name: "followup", body: `{"conversation_id":"C:100.000001","request_id":"chat-request-1","text":"Explain the controller backlog."}`, origin: "valid", token: "valid", media: "application/json", status: 200, calls: 1},
		{name: "unauthenticated", unauthenticated: true, body: body, origin: "valid", token: "valid", media: "application/json", status: 403, code: "forbidden"},
		{name: "missing origin", body: body, token: "valid", media: "application/json", status: 403, code: "forbidden"},
		{name: "foreign origin", body: body, origin: "https://evil.example.test", token: "valid", media: "application/json", status: 403, code: "forbidden"},
		{name: "missing CSRF", body: body, origin: "valid", media: "application/json", status: 403, code: "forbidden"},
		{name: "another user CSRF", body: body, origin: "valid", token: "other", media: "application/json", status: 403, code: "forbidden"},
		{name: "wrong media", body: body, origin: "valid", token: "valid", media: "text/plain", status: 415, code: "validation"},
		{name: "malformed media", body: body, origin: "valid", token: "valid", media: `application/json; charset="`, status: 415, code: "validation"},
		{name: "malformed JSON", body: `{`, origin: "valid", token: "valid", media: "application/json", status: 400, code: "validation"},
		{name: "unknown field", body: `{"text":"question","unexpected":true}`, origin: "valid", token: "valid", media: "application/json", status: 400, code: "validation"},
		{name: "client actor rejected", body: `{"text":"question","actor":"other@example.test"}`, origin: "valid", token: "valid", media: "application/json", status: 400, code: "validation"},
		{name: "extra JSON object", body: body + ` {}`, origin: "valid", token: "valid", media: "application/json", status: 400, code: "validation"},
		{name: "trailing invalid JSON", body: body + ` x`, origin: "valid", token: "valid", media: "application/json", status: 400, code: "validation"},
		{name: "oversized body", body: `{"text":"` + strings.Repeat("a", 17000) + `"}`, origin: "valid", token: "valid", media: "application/json", status: 400, code: "validation"},
		{name: "service unavailable", unavailable: true, body: body, origin: "valid", token: "valid", media: "application/json", status: 503, code: "not_ready"},
		{name: "validation", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: opsalerts.ErrValidation, status: 400, calls: 1, code: "validation"},
		{name: "missing thread", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: opsalerts.ErrNotFound, status: 404, calls: 1, code: "not_found"},
		{name: "conflict", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: fmt.Errorf("request changed: %w", opsalerts.ErrConflict), status: 409, calls: 1, code: "conflict"},
		{name: "not ready", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: opsalerts.ErrNotReady, status: 503, calls: 1, code: "not_ready"},
		{name: "delivery failed", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: opsalerts.ErrDeliveryFailed, status: 502, calls: 1, code: "delivery_failed"},
		{name: "delivery uncertain", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: opsalerts.ErrDeliveryUncertain, status: 502, calls: 1, code: "delivery_uncertain"},
		{name: "internal error", body: body, origin: "valid", token: "valid", media: "application/json", serviceError: errors.New("unexpected"), status: 500, calls: 1, code: "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &conversationTestService{dashboardTestService: dashboardTestService{err: tc.serviceError}}
			cfg := testAccess()
			actor := "operator@example.test"
			if tc.proxy {
				cfg = proxyAccess()
				actor = "group.member@example.test"
			}
			var boundary alertService = service
			if tc.unavailable {
				boundary = &dashboardTestService{}
			}
			d, err := newDashboard(boundary, cfg, false)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/conversations", strings.NewReader(tc.body))
			request.RemoteAddr = "127.0.0.1:1234"
			if !tc.unauthenticated {
				request.Header.Set("X-Forwarded-Email", actor)
			}
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
			request.Header.Set("Content-Type", tc.media)
			response := httptest.NewRecorder()
			d.ServeHTTP(response, request)
			if diff := cmp.Diff(tc.status, response.Code); diff != "" {
				t.Fatalf("unexpected status (-want +got):\n%s\n%s", diff, response.Body.String())
			}
			if diff := cmp.Diff(tc.calls, service.conversationCalls); diff != "" {
				t.Fatalf("unexpected writes (-want +got):\n%s", diff)
			}
			if service.calls != 0 {
				t.Error("conversation invoked workflow action")
			}
			if tc.calls == 1 {
				if diff := cmp.Diff(actor, service.conversationActor); diff != "" {
					t.Errorf("unexpected actor (-want +got):\n%s", diff)
				}
				var want opsalerts.ConversationRequest
				if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(want, service.conversationRequest); diff != "" {
					t.Errorf("unexpected request (-want +got):\n%s", diff)
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
					t.Error("error must explain rejection")
				}
				if tc.serviceError != nil {
					if diff := cmp.Diff(tc.serviceError.Error(), got.Error); diff != "" {
						t.Errorf("unexpected service error (-want +got):\n%s", diff)
					}
				}
			} else {
				var got conversationResponse
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff("C:200.000001", got.Conversation.ID); diff != "" {
					t.Errorf("unexpected conversation (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// conversationWatchTestService records foreground interest before the cache is read.
type conversationWatchTestService struct {
	conversationTestService
	touched      []string
	readsAtTouch int
}

// TouchConversation records cache-only foreground interest without dispatching any work.
func (s *conversationWatchTestService) TouchConversation(id string) bool {
	s.touched = append(s.touched, id)
	s.readsAtTouch = s.reads
	return true
}

// TestDashboardConversationInterest gates foreground interest on the authenticated read boundary.
func TestDashboardConversationInterest(t *testing.T) {
	cases := []struct {
		name, path          string
		authorized, watcher bool
		status              int
		wantTouched         []string
	}{
		{name: "authenticated active conversation", path: "/api/conversations?active=C%3A100.000001", authorized: true, watcher: true, status: 200, wantTouched: []string{"C:100.000001"}},
		{name: "authenticated without active conversation", path: "/api/conversations", authorized: true, watcher: true, status: 200, wantTouched: []string{""}},
		{name: "unauthorized active conversation", path: "/api/conversations?active=C%3A100.000001", watcher: true, status: 403},
		{name: "service without foreground support", path: "/api/conversations?active=C%3A100.000001", authorized: true, status: 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &conversationWatchTestService{}
			var boundary alertService = &service.conversationTestService
			if tc.watcher {
				boundary = service
			}
			d, err := newDashboard(boundary, testAccess(), false)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			request.RemoteAddr = "127.0.0.1:1234"
			if tc.authorized {
				request.Header.Set("X-Forwarded-Email", "operator@example.test")
			}
			response := httptest.NewRecorder()
			d.ServeHTTP(response, request)
			if diff := cmp.Diff(tc.status, response.Code); diff != "" {
				t.Fatalf("unexpected status (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantTouched, service.touched); diff != "" {
				t.Errorf("unexpected foreground touches (-want +got):\n%s", diff)
			}
			if service.readsAtTouch != 0 {
				t.Error("foreground interest was recorded after the cache read")
			}
			wantReads := 0
			if tc.authorized {
				wantReads = 1
			}
			if diff := cmp.Diff(wantReads, service.reads); diff != "" {
				t.Errorf("unexpected cache reads (-want +got):\n%s", diff)
			}
			if service.conversationCalls != 0 || service.calls != 0 {
				t.Error("foreground interest initiated a write")
			}
		})
	}
}
