package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/prow/pkg/github"
)

func TestAgenticBackoffSaturates(t *testing.T) {
	if got := nextAgenticBackoff(time.Duration(1<<63 - 1)); got != agenticMaxBackoff {
		t.Fatalf("oversized backoff overflowed: %v", got)
	}
	if got := nextAgenticBackoff(-time.Second); got != agenticInitialBackoff {
		t.Fatalf("negative starting backoff produced %v", got)
	}
}

func TestAgenticRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
		after time.Duration
	}{
		{name: "request timeout", err: fmt.Errorf("request: %w", context.DeadlineExceeded), retry: true},
		{name: "cancelled", err: context.Canceled},
		{name: "short body", err: io.ErrUnexpectedEOF, retry: true},
		{name: "connection reset", err: &url.Error{Op: "GET", URL: "https://github.example", Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}, retry: true},
		{name: "temporary DNS", err: &net.DNSError{Err: "temporary lookup failure", IsTemporary: true}, retry: true},
		{name: "bad hostname", err: &net.DNSError{Err: "no such host", IsNotFound: true}},
		{name: "bad URL", err: &url.Error{Op: "GET", Err: errors.New("unsupported protocol scheme")}},
		{name: "bad certificate", err: &url.Error{Op: "GET", Err: x509.UnknownAuthorityError{}}},
		{name: "unavailable", err: apierrors.NewServiceUnavailable("unavailable"), retry: true},
		{name: "internal", err: apierrors.NewInternalError(errors.New("server failure")), retry: true},
		{name: "conflict", err: apierrors.NewConflict(schema.GroupResource{Resource: "prowjobs"}, "test", errors.New("version conflict")), retry: true},
		{name: "server delay", err: apierrors.NewTooManyRequests("throttled", 600), retry: true, after: 10 * time.Minute},
		{name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{Resource: "prowjobs"}, "test", errors.New("RBAC"))},
		{name: "not found", err: github.NewNotFound()},
		{name: "invalid config", err: errors.New("agentic selection requires GitHub App authentication")},
		{name: "invalid journal", err: errors.New("invalid persisted execution")},
		{name: "invalid plan", err: agenticPlanPendingError{io.ErrUnexpectedEOF}},
		{name: "unknown", err: errors.New("temporarily unavailable")},
		{name: "not a request error", err: errors.New("status code 503 not one of [200], body: text")},
		{name: "rate-limit prose", err: errors.New("invalid field: sleep time for token reset exceeds max sleep time (10m0s > 1m0s)")},
		{name: "malformed rate wait", err: errors.New("sleep time for token reset exceeds max sleep time (-1m0s > 1m0s)")},
		{name: "joined failed write", err: errors.Join(apierrors.NewForbidden(schema.GroupResource{Resource: "prowjobs"}, "test", errors.New("RBAC")), fmt.Errorf("recording failure: %w", apierrors.NewTooManyRequests("throttled", 20))), retry: true, after: 20 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := agenticRetryFor(tc.err)
			if got.transient != tc.retry || got.after != tc.after {
				t.Fatalf("retry(%v)=%+v, want transient=%v, after=%v", tc.err, got, tc.retry, tc.after)
			}
		})
	}
}

type agenticRetryTransport func(*http.Request) (*http.Response, error)

func (f agenticRetryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// Exercise the real pinned SDK, including its private requestError and plain
// paginated/rate-limit errors, without HTTP listeners or network access.
func agenticProwError(t *testing.T, status int, headers http.Header, list bool) error {
	t.Helper()
	_, _, client, err := github.NewClientFromOptions(nil, github.ClientOptions{
		Bases: []string{"https://github.invalid"}, GraphqlEndpoint: "https://github.invalid/graphql",
		Censor: func(data []byte) []byte { return data }, MaxRetries: 1, InitialDelay: time.Nanosecond, MaxSleepTime: time.Millisecond,
		BaseRoundTripper: agenticRetryTransport(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: headers.Clone(),
				Body: io.NopCloser(strings.NewReader(`{"message":"test failure"}`)), Request: request}, nil
		}),
	})
	require.NoError(t, err)
	if list {
		_, err = client.GetPullRequests("org", "repo")
	} else {
		_, err = client.GetPullRequest("org", "repo", 42)
	}
	if err == nil {
		t.Fatal("expected SDK request failure")
	}
	return err
}

func TestAgenticProwHTTPRetryClassification(t *testing.T) {
	for _, list := range []bool{false, true} {
		for _, tc := range []struct {
			status int
			retry  bool
		}{
			{400, false}, {401, false}, {403, false}, {404, false}, {422, false},
			{408, true}, {429, true}, {500, true}, {502, true}, {503, true}, {504, true},
		} {
			t.Run(fmt.Sprintf("list=%v/status=%d", list, tc.status), func(t *testing.T) {
				err := agenticProwError(t, tc.status, nil, list)
				got := agenticRetryFor(fmt.Errorf("API operation: %w", err))
				if got.transient != tc.retry || got.githubRateLimit != (tc.status == 429) {
					t.Fatalf("SDK error %T(%v) classified as %+v", err, err, got)
				}
				if tc.status == 429 && got.after < time.Minute {
					t.Fatal("rate limiting without an exposed hint must wait at least a minute")
				}
			})
		}
	}
}

func TestAgenticProwRateLimitHints(t *testing.T) {
	for _, list := range []bool{false, true} {
		for _, limit := range []string{"primary", "secondary"} {
			t.Run(fmt.Sprintf("list=%v/%s", list, limit), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					headers := http.Header{}
					if limit == "primary" {
						headers.Set("X-RateLimit-Remaining", "0")
						headers.Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(11*time.Minute).Unix()))
					} else {
						headers.Set("Retry-After", "660")
					}
					err := agenticProwError(t, http.StatusForbidden, headers, list)
					got := agenticRetryFor(err)
					if !got.transient || !got.githubRateLimit || got.after != 11*time.Minute+time.Second {
						t.Fatalf("lost server wait in SDK error %T(%v): %+v", err, err, got)
					}
				})
			})
		}
	}
}

func TestAgenticFailureReporting(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, tc := range []struct {
			name               string
			failure            error
			failWrite, publish bool
		}{
			{"failed write preserves terminal cause", errors.New("invalid configuration"), true, true},
			{"Kubernetes throttling permits GitHub writes", apierrors.NewTooManyRequests("Kubernetes throttled", 60), false, true},
			{"GitHub cooldown prevents writes", errors.New("sleep time for token reset exceeds max sleep time (11m0s > 1m0s)"), false, false},
		} {
			t.Run(fmt.Sprintf("existing=%v/%s", existing, tc.name), func(t *testing.T) {
				f := newAgenticFixture(t, "manual")
				f.reconcile(t, nil)
				gate, state := f.gate(t)
				attempts := f.gh.checkAttempts
				f.gh.failCheck = tc.failWrite
				fail := f.a.failState
				if existing {
					fail = f.a.failExistingGate
				}
				err := fail(&gate, state, tc.failure)
				require.ErrorIs(t, err, tc.failure)
				require.Equal(t, tc.publish, f.gh.checkAttempts > attempts)
				require.True(t, agenticRetryFor(err).transient)
				if tc.failWrite {
					require.ErrorIs(t, err, io.ErrUnexpectedEOF, "failed gate write was masked by the terminal error")
				}
			})
		}
	}
}
