package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// metricsTestClock makes historical and current-window selection deterministic.
type metricsTestClock struct{}

// Now returns a stable server time for metrics tests.
func (metricsTestClock) Now() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }

// metricsTestProvider records membership-gated calls without upstream connections.
type metricsTestProvider struct {
	calls    int
	alert    string
	deadline bool
	err      error
}

// Read returns deterministic evidence and captures the server's bounded context.
func (p *metricsTestProvider) Read(ctx context.Context, alert string, _ time.Time) (metricsResponse, error) {
	p.calls++
	p.alert = alert
	_, p.deadline = ctx.Deadline()
	return metricsResponse{Source: "core-ci", Alert: alert, Series: []metricsSeries{}, Warnings: []string{}}, p.err
}

// TestDashboardMetrics verifies authorization and retained occurrence membership precede upstream work.
func TestDashboardMetrics(t *testing.T) {
	cases := []struct {
		name, id, alert, code string
		unauth, disabled      bool
		err                   error
		status, calls         int
	}{
		{name: "linked alert", id: "C:1", alert: "ExactAlert", status: 200, calls: 1},
		{name: "authentication", id: "C:1", alert: "ExactAlert", unauth: true, status: 403, code: "forbidden"},
		{name: "unknown occurrence", id: "C:other", alert: "ExactAlert", status: 404, code: "not_found"},
		{name: "unlinked alert", id: "C:1", alert: "OtherAlert", status: 404, code: "not_found"},
		{name: "case sensitive", id: "C:1", alert: "exactalert", status: 404, code: "not_found"},
		{name: "identifier is insufficient", id: "C:2", alert: "ExactAlert", status: 404, code: "not_found"},
		{name: "disabled", id: "C:1", alert: "ExactAlert", disabled: true, status: 503, code: "not_configured"},
		{name: "rule missing", id: "C:1", alert: "ExactAlert", err: errMetricsMissing, status: 404, code: "not_found", calls: 1},
		{name: "rule conflict", id: "C:1", alert: "ExactAlert", err: errMetricsAmbiguous, status: 409, code: "ambiguous_rule", calls: 1},
		{name: "private upstream error", id: "C:1", alert: "ExactAlert", err: errors.New("secret-token"), status: 502, code: "metrics_unavailable", calls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &sopTestService{snapshot: opsalerts.Snapshot{Occurrences: []opsalerts.Occurrence{
				{Identity: opsalerts.OccurrenceIdentity{ID: "C:1"}, Message: opsalerts.Message{PrometheusAlerts: []string{"ExactAlert"}}},
				{Identity: opsalerts.OccurrenceIdentity{ID: "C:2"}, Message: opsalerts.Message{Identifiers: []string{"ExactAlert"}}},
			}}}
			d, err := newDashboard(service, testAccess(), false)
			if err != nil {
				t.Fatal(err)
			}
			provider := &metricsTestProvider{err: tc.err}
			if !tc.disabled {
				d.metricsProvider = provider
			}
			query := url.Values{"occurrence_id": {tc.id}, "alert": {tc.alert}, "query": {"malicious browser expression"}, "url": {"https://untrusted.example.test"}}
			request := httptest.NewRequest(http.MethodGet, "/api/metrics?"+query.Encode(), nil)
			request.RemoteAddr = "127.0.0.1:1234"
			if !tc.unauth {
				request.Header.Set("X-Forwarded-Email", "operator@example.test")
			}
			response := httptest.NewRecorder()
			d.ServeHTTP(response, request)
			if diff := cmp.Diff(tc.status, response.Code); diff != "" {
				t.Fatalf("unexpected status (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.calls, provider.calls); diff != "" {
				t.Errorf("unexpected calls (-want +got):\n%s", diff)
			}
			if provider.calls > 0 && (!provider.deadline || provider.alert != "ExactAlert") {
				t.Errorf("provider called without fixed identity or deadline: %#v", provider)
			}
			if strings.Contains(response.Body.String(), "secret-token") {
				t.Error("private upstream error leaked")
			}
			if tc.code != "" {
				var failure apiError
				if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(tc.code, failure.Code); diff != "" {
					t.Errorf("unexpected code (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// metricsPagesClient supplies paginated Kubernetes results or one deterministic matrix.
type metricsPagesClient struct {
	bodies   []string
	status   int
	err      error
	requests []*http.Request
	matrix   bool
}

// Do records credential separation and builds matrix timestamps from server-owned range parameters.
func (c *metricsPagesClient) Do(request *http.Request) (*http.Response, error) {
	c.requests = append(c.requests, request)
	if c.err != nil {
		return nil, c.err
	}
	index := len(c.requests) - 1
	body := "{}"
	if len(c.bodies) > 0 {
		body = c.bodies[min(index, len(c.bodies)-1)]
	}
	if c.matrix {
		start, err := strconv.ParseInt(request.URL.Query().Get("start"), 10, 64)
		if err != nil {
			return nil, err
		}
		body = fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"test"},"values":[[%d,"12"],[%d,"NaN"],[%d,"+Inf"],[%d,null]]}]}}`, start, start+60, start+120, start+180)
	}
	return &http.Response{StatusCode: c.status, Body: io.NopCloser(strings.NewReader(body))}, nil
}

// metricRulePage supplies only namespace-owned exact rule definitions for tests.
func metricRulePage(expression, continuation string) string {
	return fmt.Sprintf(`{"metadata":{"continue":%q},"items":[{"metadata":{"name":"ci-alerts","namespace":"ci-monitoring"},"spec":{"groups":[{"name":"ci","rules":[{"alert":"ExactAlert","expr":%q,"for":"10m"}]}]}}]}`, continuation, expression)
}

// TestMetricsRules requires complete pagination, exact matching and consistent provenance.
func TestMetricsRules(t *testing.T) {
	cases := []struct {
		name       string
		pages      []string
		status     int
		wantErr    error
		calls      int
		expression string
	}{
		{name: "exact rule", pages: []string{metricRulePage("sum(foo) > 5", "")}, status: 200, calls: 1, expression: "sum(foo) > 5"},
		{name: "paginated exact duplicate", pages: []string{metricRulePage("foo > 5", "next"), metricRulePage("foo > 5", "")}, status: 200, calls: 2, expression: "foo > 5"},
		{name: "conflicting expression", pages: []string{metricRulePage("foo > 5", "next"), metricRulePage("foo > 6", "")}, status: 200, calls: 2, wantErr: errMetricsAmbiguous},
		{name: "conflicting duration", pages: []string{metricRulePage("foo > 5", "next"), strings.Replace(metricRulePage("foo > 5", ""), "10m", "20m", 1)}, status: 200, calls: 2, wantErr: errMetricsAmbiguous},
		{name: "not exact", pages: []string{strings.Replace(metricRulePage("foo", ""), "ExactAlert", "ExactAlertLong", 1)}, status: 200, calls: 1, wantErr: errMetricsMissing},
		{name: "wrong namespace", pages: []string{strings.Replace(metricRulePage("foo", ""), "ci-monitoring", "other", 1)}, status: 200, calls: 1, wantErr: errMetricsUnavailable},
		{name: "forbidden", status: 403, calls: 1, wantErr: errMetricsUnavailable},
		{name: "invalid JSON", pages: []string{"private failure"}, status: 200, calls: 1, wantErr: errMetricsUnavailable},
		{name: "empty expression", pages: []string{metricRulePage("", "")}, status: 200, calls: 1, wantErr: errMetricsUnavailable},
		{name: "repeated pagination", pages: []string{metricRulePage("foo", "next")}, status: 200, calls: 2, wantErr: errMetricsUnavailable},
		{name: "numeric expression", pages: []string{strings.Replace(metricRulePage("foo", ""), `"expr":"foo"`, `"expr":1`, 1)}, status: 200, calls: 1, expression: "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &metricsPagesClient{status: tc.status, bodies: tc.pages}
			provider := &coreMetrics{clock: metricsTestClock{}, namespace: "ci-monitoring", kubeURL: "https://kubernetes.default.svc", kube: client}
			got, err := provider.rule(context.Background(), "ExactAlert")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v, want %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.calls, len(client.requests)); diff != "" {
				t.Errorf("unexpected calls (-want +got):\n%s", diff)
			}
			if err == nil {
				want := metricsRule{Namespace: "ci-monitoring", Resource: "ci-alerts", Group: "ci", Expression: tc.expression, For: "10m"}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("unexpected rule (-want +got):\n%s", diff)
				}
			}
			for _, request := range client.requests {
				if request.URL.Path != "/apis/monitoring.coreos.com/v1/namespaces/ci-monitoring/prometheusrules" || request.URL.Query().Get("limit") != "100" {
					t.Errorf("unbounded namespace request: %s", request.URL)
				}
			}
		})
	}
}

// TestMetricsRange checks fixed PromQL, bounds, finite samples and failure envelopes.
func TestMetricsRange(t *testing.T) {
	cases := []struct {
		name, body string
		matrix     bool
		status     int
		err        error
		wantErr    error
		points     int
		gaps       int
	}{
		{name: "verbatim expression and gaps", matrix: true, status: 200, points: 4, gaps: 3},
		{name: "empty evidence", body: `{"status":"success","data":{"resultType":"matrix","result":[]}}`, status: 200},
		{name: "upstream logical failure", body: `{"status":"error","error":"private token"}`, status: 200, wantErr: errMetricsUnavailable},
		{name: "wrong result type", body: `{"status":"success","data":{"resultType":"vector"}}`, status: 200, wantErr: errMetricsUnavailable},
		{name: "redirect", status: 302, wantErr: errMetricsUnavailable},
		{name: "network failure", err: errors.New("private network secret"), wantErr: errMetricsUnavailable},
		{name: "oversized body", status: 200, body: strings.Repeat(" ", metricsMaxBody+1), wantErr: errMetricsUnavailable},
		{name: "malformed sample", status: 200, body: `{"status":"success","data":{"resultType":"matrix","result":[{"values":[[123,"broken"]]}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kube := &metricsPagesClient{status: 200, bodies: []string{metricRulePage(`sum(rate(foo[5m])) > 10`, "")}}
			query := &metricsPagesClient{status: tc.status, bodies: []string{tc.body}, matrix: tc.matrix, err: tc.err}
			provider := &coreMetrics{clock: metricsTestClock{}, namespace: "ci-monitoring", kubeURL: "https://kubernetes.default.svc", queryURL: "http://query.ci-monitoring.svc:9090", kube: kube, query: query}
			reported := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
			ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
			defer cancel()
			got, err := provider.Read(ctx, "ExactAlert", reported)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v, want %v", err, tc.wantErr)
			}
			if len(query.requests) != 1 {
				t.Fatalf("query requests = %d", len(query.requests))
			}
			request := query.requests[0]
			wantQuery := url.Values{"query": {`sum(rate(foo[5m])) > 10`}, "start": {strconv.FormatInt(reported.Add(-90*time.Minute).Unix(), 10)}, "end": {strconv.FormatInt(reported.Add(30*time.Minute).Unix(), 10)}, "step": {"60"}, "timeout": {"8s"}}
			if diff := cmp.Diff(wantQuery, request.URL.Query()); diff != "" {
				t.Errorf("unexpected query (-want +got):\n%s", diff)
			}
			if request.URL.Host != "query.ci-monitoring.svc:9090" || request.Header.Get("Authorization") != "" {
				t.Error("query crossed credential or configured host boundary")
			}
			if _, ok := request.Context().Deadline(); !ok {
				t.Error("query missing deadline")
			}
			if err == nil && tc.points > 0 {
				if diff := cmp.Diff(tc.points, len(got.Series[0].Points)); diff != "" {
					t.Errorf("unexpected points (-want +got):\n%s", diff)
				}
				gaps := 0
				for _, point := range got.Series[0].Points {
					if point.Value == nil {
						gaps++
					}
				}
				if diff := cmp.Diff(tc.gaps, gaps); diff != "" {
					t.Errorf("unexpected gaps (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestMonitoringConfiguration validates optional setup entirely offline.
func TestMonitoringConfiguration(t *testing.T) {
	cases := []struct {
		name      string
		config    monitoringConfig
		wantError string
	}{
		{name: "disabled default"},
		{name: "enabled default namespace", config: monitoringConfig{Enabled: true, QueryURL: "http://thanos-query-frontend-ci-query.ci-monitoring.svc.cluster.local:9090"}},
		{name: "HTTPS", config: monitoringConfig{Enabled: true, RulesNamespace: "ci-monitoring", QueryURL: "https://query.example.test"}},
		{name: "missing enabled URL", config: monitoringConfig{Enabled: true}, wantError: "monitoring.query_url must be an http or https origin without credentials, query or path"},
		{name: "credentials", config: monitoringConfig{Enabled: true, QueryURL: "https://user:secret@query.example.test"}, wantError: "monitoring.query_url must be an http or https origin without credentials, query or path"},
		{name: "path", config: monitoringConfig{Enabled: true, QueryURL: "http://query.example.test/api"}, wantError: "monitoring.query_url must be an http or https origin without credentials, query or path"},
		{name: "browser query", config: monitoringConfig{Enabled: true, QueryURL: "http://query.example.test/?query=foo"}, wantError: "monitoring.query_url must be an http or https origin without credentials, query or path"},
		{name: "fragment", config: monitoringConfig{Enabled: true, QueryURL: "http://query.example.test/#foo"}, wantError: "monitoring.query_url must be an http or https origin without credentials, query or path"},
		{name: "bad scheme", config: monitoringConfig{Enabled: true, QueryURL: "file:///etc/passwd"}, wantError: "monitoring.query_url must be an http or https origin without credentials, query or path"},
		{name: "invalid namespace", config: monitoringConfig{RulesNamespace: "../kube-system"}, wantError: "monitoring.rules_namespace must be a namespace name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.validate()
			got := ""
			if err != nil {
				got = err.Error()
			}
			if diff := cmp.Diff(tc.wantError, got); diff != "" {
				t.Fatalf("unexpected error (-want +got):\n%s", diff)
			}
			if err == nil && tc.config.RulesNamespace == "" {
				t.Error("namespace default was not applied")
			}
		})
	}
}

// TestMetricWindow prevents client or occurrence timestamps from expanding the fixed range.
func TestMetricWindow(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 1, 45, 0, time.UTC)
	cases := []struct {
		name          string
		reported, end time.Time
	}{
		{name: "recent", reported: now.Add(-20 * time.Minute), end: now.Truncate(time.Minute)},
		{name: "old", reported: now.Add(-8 * time.Hour), end: now.Add(-8*time.Hour + 30*time.Minute).Truncate(time.Minute)},
		{name: "unknown", end: now.Truncate(time.Minute)},
		{name: "future", reported: now.Add(8 * time.Hour), end: now.Truncate(time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := metricsWindow{Start: tc.end.Add(-2 * time.Hour), End: tc.end, StepSeconds: 60}
			if diff := cmp.Diff(want, metricWindow(now, tc.reported)); diff != "" {
				t.Errorf("unexpected window (-want +got):\n%s", diff)
			}
		})
	}
}

// metricMatrixBody builds deterministic over-limit matrices without contacting Prometheus.
func metricMatrixBody(seriesCount, pointCount int) string {
	start := time.Date(2020, 1, 1, 10, 30, 0, 0, time.UTC).Unix()
	series := make([]string, 0, seriesCount)
	for i := 0; i < seriesCount; i++ {
		points := make([]string, 0, pointCount)
		for j := 0; j < pointCount; j++ {
			points = append(points, fmt.Sprintf(`[%d,"12"]`, start+int64(j*60)))
		}
		series = append(series, fmt.Sprintf(`{"metric":{"instance":%q},"values":[%s]}`, strconv.Itoa(i), strings.Join(points, ",")))
	}
	return fmt.Sprintf(`{"status":"success","warnings":["partial response"],"data":{"resultType":"matrix","result":[%s]}}`, strings.Join(series, ","))
}

// TestMetricTruncation reports every enforced output bound without converting missing values to zero.
func TestMetricTruncation(t *testing.T) {
	cases := []struct {
		name                                   string
		series, points, wantSeries, wantPoints int
		truncated                              bool
	}{
		{name: "exact limits", series: metricsMaxSeries, points: metricsMaxPoints, wantSeries: metricsMaxSeries, wantPoints: metricsMaxPoints},
		{name: "series limit", series: metricsMaxSeries + 1, points: 1, wantSeries: metricsMaxSeries, wantPoints: 1, truncated: true},
		{name: "point limit", series: 1, points: metricsMaxPoints + 1, wantSeries: 1, wantPoints: metricsMaxPoints, truncated: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &coreMetrics{clock: metricsTestClock{}, namespace: "ci-monitoring", kubeURL: "https://kube.example.test", queryURL: "http://query.example.test", kube: &metricsPagesClient{status: 200, bodies: []string{metricRulePage("foo > 5", "")}}, query: &metricsPagesClient{status: 200, bodies: []string{metricMatrixBody(tc.series, tc.points)}}}
			got, err := provider.Read(context.Background(), "ExactAlert", time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.wantSeries, len(got.Series)); diff != "" {
				t.Errorf("unexpected series (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantPoints, len(got.Series[0].Points)); diff != "" {
				t.Errorf("unexpected points (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.truncated, got.Truncated); diff != "" {
				t.Errorf("unexpected truncation (-want +got):\n%s", diff)
			}
			if len(got.Warnings) == 0 || (tc.truncated && len(got.Warnings) < 2) {
				t.Errorf("missing upstream or truncation warning: %#v", got.Warnings)
			}
		})
	}
}

// TestMetricsProviderModes keeps synthetic and disabled startup independent of cluster credentials.
func TestMetricsProviderModes(t *testing.T) {
	cases := []struct {
		name          string
		demo, enabled bool
	}{
		{name: "disabled"},
		{name: "synthetic", demo: true, enabled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := newMetricsProvider(monitoringConfig{}, tc.demo)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.enabled, provider != nil); diff != "" {
				t.Fatalf("unexpected mode (-want +got):\n%s", diff)
			}
			if provider == nil {
				return
			}
			reported := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			first, err := provider.Read(context.Background(), "ExactAlert", reported)
			if err != nil {
				t.Fatal(err)
			}
			second, err := provider.Read(context.Background(), "ExactAlert", reported)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(first, second); diff != "" {
				t.Errorf("synthetic evidence changed (-want +got):\n%s", diff)
			}
			if !first.Demo || first.Source != "core-ci" || len(first.Series) != 1 || len(first.Warnings) == 0 {
				t.Errorf("synthetic provenance missing: %#v", first)
			}
			for _, point := range first.Series[0].Points {
				if point.Value != nil && *point.Value <= 10 {
					t.Error("synthetic sample violates filtered expression")
				}
			}
		})
	}
}

// TestMetricsSnapshotStatus exposes only configured availability and fixed source identity.
func TestMetricsSnapshotStatus(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
	}{{name: "disabled"}, {name: "enabled", enabled: true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := newDashboard(&dashboardTestService{}, testAccess(), false)
			if err != nil {
				t.Fatal(err)
			}
			if tc.enabled {
				d.metricsProvider = &metricsTestProvider{}
			}
			request := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
			request.RemoteAddr = "127.0.0.1:1234"
			request.Header.Set("X-Forwarded-Email", "operator@example.test")
			response := httptest.NewRecorder()
			d.ServeHTTP(response, request)
			var got snapshotResponse
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(monitoringStatus{Enabled: tc.enabled, Source: "core-ci"}, got.Monitoring); diff != "" {
				t.Errorf("unexpected monitoring status (-want +got):\n%s", diff)
			}
		})
	}
}
