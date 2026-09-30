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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/prow/pkg/github"
)

func TestAgenticBackoffSaturates(t *testing.T) {
	var backoff time.Duration
	for _, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute} {
		backoff = nextAgenticBackoff(backoff)
		if backoff != want {
			t.Fatalf("backoff=%v, want %v", backoff, want)
		}
	}
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
	if err != nil {
		t.Fatal(err)
	}
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
		for _, status := range []int{400, 401, 403, 404, 408, 422, 429, 500, 502, 503, 504} {
			t.Run(fmt.Sprintf("list=%v/status=%d", list, status), func(t *testing.T) {
				err := agenticProwError(t, status, nil, list)
				got := agenticRetryFor(fmt.Errorf("API operation: %w", err))
				if got.transient != retryableAgenticHTTPStatus(status) || got.githubRateLimit != (status == 429) {
					t.Fatalf("SDK error %T(%v) classified as %+v", err, err, got)
				}
				if status == 429 && got.after < time.Minute {
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

func TestAgenticFailureReportingPreservesRetryCauses(t *testing.T) {
	for _, existing := range []bool{false, true} {
		f := newAgenticFixture(t, "manual")
		f.reconcile(t, nil)
		gate, state := f.gate(t)
		failure := errors.New("invalid configuration")
		f.gh.failCheck = true
		var err error
		if existing {
			err = f.a.failExistingGate(&gate, state, failure)
		} else {
			err = f.a.failState(&gate, state, failure)
		}
		if !errors.Is(err, failure) || !errors.Is(err, io.ErrUnexpectedEOF) || !agenticRetryFor(err).transient {
			t.Fatalf("failed gate write was masked by the original terminal error: %v", err)
		}
	}
}

func TestAgenticFailureReportingRespectsOnlyGitHubCooldown(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, githubLimit := range []bool{false, true} {
			f := newAgenticFixture(t, "manual")
			f.reconcile(t, nil)
			gate, state := f.gate(t)
			attempts := f.gh.checkAttempts
			var failure error = apierrors.NewTooManyRequests("Kubernetes throttled", 60)
			if githubLimit {
				failure = errors.New("sleep time for token reset exceeds max sleep time (11m0s > 1m0s)")
			}
			if existing {
				_ = f.a.failExistingGate(&gate, state, failure)
			} else {
				_ = f.a.failState(&gate, state, failure)
			}
			if got := f.gh.checkAttempts > attempts; got == githubLimit {
				t.Fatalf("GitHub failure-report attempt=%v for GitHub rate limit=%v", got, githubLimit)
			}
		}
	}
}
