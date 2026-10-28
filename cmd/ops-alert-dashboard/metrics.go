package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// metricsLimits constrain every on-demand upstream read independently of server timeouts.
const (
	metricsTimeout   = 12 * time.Second
	metricsMaxBody   = 4 * 1024 * 1024
	metricsMaxSeries = 20
	metricsMaxPoints = 121
	metricsMaxPages  = 20
)

// monitoringConfig selects one deployment-owned namespace and query endpoint.
type monitoringConfig struct {
	Enabled        bool   `json:"enabled"`
	RulesNamespace string `json:"rules_namespace"`
	QueryURL       string `json:"query_url"`
}

// validate checks configuration without requiring cluster credentials or network access.
func (c *monitoringConfig) validate() error {
	if c.RulesNamespace == "" {
		c.RulesNamespace = "ci-monitoring"
	}
	if len(validation.IsDNS1123Label(c.RulesNamespace)) != 0 {
		return fmt.Errorf("monitoring.rules_namespace must be a namespace name")
	}
	if !c.Enabled && c.QueryURL == "" {
		return nil
	}
	u, err := url.Parse(c.QueryURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("monitoring.query_url must be an http or https origin without credentials, query or path")
	}
	return nil
}

// monitoringStatus makes availability explicit without exposing infrastructure URLs.
type monitoringStatus struct {
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"`
}

// metricsProvider resolves only server-linked alert names and owns the fixed query window.
type metricsProvider interface {
	Read(context.Context, string, time.Time) (metricsResponse, error)
}

// metricsHTTPClient allows credential and request boundaries to be tested independently.
type metricsHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// metricsRule records rule provenance, never inferred live firing state.
type metricsRule struct {
	Namespace  string `json:"namespace"`
	Resource   string `json:"resource"`
	Group      string `json:"group"`
	Expression string `json:"expression"`
	For        string `json:"for"`
}

// metricsWindow records the actual bounded query range.
type metricsWindow struct {
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	StepSeconds int       `json:"step_seconds"`
}

// metricsPoint uses nil for absent or nonfinite samples so graphs retain gaps.
type metricsPoint struct {
	At    time.Time `json:"at"`
	Value *float64  `json:"value"`
}

// metricsSeries keeps the label set that identifies one returned time series.
type metricsSeries struct {
	Labels map[string]string `json:"labels"`
	Points []metricsPoint    `json:"points"`
}

// metricsResponse is bounded evidence from the explicitly selected core-ci source.
type metricsResponse struct {
	Source    string          `json:"source"`
	Demo      bool            `json:"demo"`
	Alert     string          `json:"alert"`
	Rule      metricsRule     `json:"rule"`
	Window    metricsWindow   `json:"window"`
	Series    []metricsSeries `json:"series"`
	Warnings  []string        `json:"warnings"`
	Truncated bool            `json:"truncated"`
}

// metricsErrors remain private to prevent upstream credentials and bodies leaking to browsers.
var (
	errMetricsMissing     = errors.New("rule not found")
	errMetricsAmbiguous   = errors.New("conflicting rule definitions")
	errMetricsUnavailable = errors.New("metrics unavailable")
)

// coreMetrics separates Kubernetes authentication from unauthenticated internal query traffic.
type coreMetrics struct {
	namespace string
	kubeURL   string
	queryURL  string
	kube      metricsHTTPClient
	query     metricsHTTPClient
	clock     opsalerts.Clock
}

// newMetricsProvider avoids any in-cluster configuration when disabled or synthetic.
func newMetricsProvider(cfg monitoringConfig, demo bool) (metricsProvider, error) {
	if demo {
		return demoMetrics{}, nil
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("loading monitoring in-cluster credentials: %w", err)
	}
	config.Timeout = metricsTimeout
	kube, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, fmt.Errorf("configuring monitoring Kubernetes client: %w", err)
	}
	kube.CheckRedirect = refuseSOPRedirect
	query := &http.Client{Timeout: metricsTimeout, CheckRedirect: refuseSOPRedirect}
	return &coreMetrics{namespace: cfg.RulesNamespace, kubeURL: strings.TrimRight(config.Host, "/"), queryURL: strings.TrimRight(cfg.QueryURL, "/"), kube: kube, query: query}, nil
}

// metrics checks retained occurrence membership before any provider or upstream call.
func (d *dashboard) metrics(w http.ResponseWriter, r *http.Request) {
	id, alert := r.URL.Query().Get("occurrence_id"), r.URL.Query().Get("alert")
	var reported time.Time
	linked := false
	for _, occurrence := range d.service.Snapshot().Occurrences {
		if occurrence.Identity.ID == id && slices.Contains(occurrence.Message.PrometheusAlerts, alert) {
			reported = occurrence.Message.ReportedAt
			linked = true
			break
		}
	}
	if id == "" || alert == "" || !linked {
		writeJSON(w, 404, apiError{Error: "alert not found for this occurrence", Code: "not_found"})
		return
	}
	if d.metricsProvider == nil {
		writeJSON(w, 503, apiError{Error: "core-ci metrics are not configured", Code: "not_configured"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), metricsTimeout)
	defer cancel()
	result, err := d.metricsProvider.Read(ctx, alert, reported)
	if err != nil {
		status, code := 502, "metrics_unavailable"
		message := "Core-ci metrics are temporarily unavailable."
		if errors.Is(err, errMetricsMissing) {
			status, code = 404, "not_found"
			message = "No matching rule was found in core-ci monitoring."
		}
		if errors.Is(err, errMetricsAmbiguous) {
			status, code = 409, "ambiguous_rule"
			message = "More than one rule matches this alert in core-ci."
		}
		writeJSON(w, status, apiError{Error: message, Code: code})
		return
	}
	writeJSON(w, 200, result)
}

// readMetricsJSON bounds all upstream bodies and rejects non-JSON success responses.
func readMetricsJSON(ctx context.Context, client metricsHTTPClient, raw string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return errMetricsUnavailable
	}
	response, err := client.Do(request)
	if err != nil {
		return errMetricsUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errMetricsUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, metricsMaxBody+1))
	if err != nil || len(data) > metricsMaxBody || json.Unmarshal(data, target) != nil {
		return errMetricsUnavailable
	}
	return nil
}

// prometheusRuleList uses only the fields needed to match one exact alert definition.
type prometheusRuleList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Groups []struct {
				Name  string `json:"name"`
				Rules []struct {
					Alert string          `json:"alert"`
					Expr  json.RawMessage `json:"expr"`
					For   string          `json:"for"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"spec"`
	} `json:"items"`
}

// rule exhausts bounded namespace pagination before returning an unambiguous exact match.
func (m *coreMetrics) rule(ctx context.Context, alert string) (metricsRule, error) {
	var found *metricsRule
	continuation := ""
	for page := 0; page < metricsMaxPages; page++ {
		query := url.Values{"limit": {"100"}}
		if continuation != "" {
			query.Set("continue", continuation)
		}
		raw := m.kubeURL + "/apis/monitoring.coreos.com/v1/namespaces/" + url.PathEscape(m.namespace) + "/prometheusrules?" + query.Encode()
		var list prometheusRuleList
		if err := readMetricsJSON(ctx, m.kube, raw, &list); err != nil {
			return metricsRule{}, err
		}
		if list.Items == nil {
			return metricsRule{}, errMetricsUnavailable
		}
		for _, item := range list.Items {
			if item.Metadata.Namespace != m.namespace {
				return metricsRule{}, errMetricsUnavailable
			}
			for _, group := range item.Spec.Groups {
				for _, rule := range group.Rules {
					if rule.Alert != alert {
						continue
					}
					expression, err := metricsExpression(rule.Expr)
					if err != nil {
						return metricsRule{}, err
					}
					candidate := metricsRule{Namespace: m.namespace, Resource: item.Metadata.Name, Group: group.Name, Expression: expression, For: rule.For}
					if candidate.Expression == "" {
						return metricsRule{}, errMetricsUnavailable
					}
					if found != nil && (found.Expression != candidate.Expression || found.For != candidate.For) {
						return metricsRule{}, errMetricsAmbiguous
					}
					if found == nil {
						found = &candidate
					}
				}
			}
		}
		if list.Metadata.Continue == "" {
			if found == nil {
				return metricsRule{}, errMetricsMissing
			}
			return *found, nil
		}
		if list.Metadata.Continue == continuation {
			return metricsRule{}, errMetricsUnavailable
		}
		continuation = list.Metadata.Continue
	}
	return metricsRule{}, errMetricsUnavailable
}

// metricWindow chooses recent evidence or a two-hour window around an older notification.
func metricWindow(now, reported time.Time) metricsWindow {
	end := now.UTC().Truncate(time.Minute)
	if !reported.IsZero() && reported.Before(end.Add(-90*time.Minute)) {
		end = reported.UTC().Add(30 * time.Minute).Truncate(time.Minute)
	}
	return metricsWindow{Start: end.Add(-2 * time.Hour), End: end, StepSeconds: 60}
}

// prometheusRange is the upstream matrix envelope, including upstream failure status.
type prometheusRange struct {
	Status   string   `json:"status"`
	Warnings []string `json:"warnings"`
	Data     struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string   `json:"metric"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// Read queries the expression verbatim with fixed server-owned limits and timestamps.
func (m *coreMetrics) Read(ctx context.Context, alert string, reported time.Time) (metricsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, metricsTimeout)
	defer cancel()
	rule, err := m.rule(ctx, alert)
	if err != nil {
		return metricsResponse{}, err
	}
	now := time.Now()
	if m.clock != nil {
		now = m.clock.Now()
	}
	result := metricsResponse{Source: "core-ci", Alert: alert, Rule: rule, Window: metricWindow(now, reported), Series: []metricsSeries{}, Warnings: []string{}}
	query := url.Values{"query": {rule.Expression}, "start": {strconv.FormatInt(result.Window.Start.Unix(), 10)}, "end": {strconv.FormatInt(result.Window.End.Unix(), 10)}, "step": {"60"}, "timeout": {"8s"}}
	var upstream prometheusRange
	if err := readMetricsJSON(ctx, m.query, m.queryURL+"/api/v1/query_range?"+query.Encode(), &upstream); err != nil {
		return metricsResponse{}, err
	}
	if upstream.Status != "success" || upstream.Data.ResultType != "matrix" {
		return metricsResponse{}, errMetricsUnavailable
	}
	// Upstream warning text is untrusted and can include query internals; retain a bounded generic indication.
	if len(upstream.Warnings) > 0 {
		result.Warnings = append(result.Warnings, "The query service reported warnings.")
	}
	for index, series := range upstream.Data.Result {
		if index >= metricsMaxSeries {
			result.Truncated = true
			break
		}
		output := metricsSeries{Labels: series.Metric, Points: []metricsPoint{}}
		for pointIndex, pair := range series.Values {
			if pointIndex >= metricsMaxPoints {
				result.Truncated = true
				break
			}
			if len(pair) != 2 {
				return metricsResponse{}, errMetricsUnavailable
			}
			var seconds float64
			if json.Unmarshal(pair[0], &seconds) != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
				return metricsResponse{}, errMetricsUnavailable
			}
			at := time.Unix(0, int64(seconds*float64(time.Second))).UTC()
			if at.Before(result.Window.Start) || at.After(result.Window.End) {
				result.Truncated = true
				continue
			}
			point := metricsPoint{At: at}
			var value string
			if string(pair[1]) != "null" {
				if json.Unmarshal(pair[1], &value) != nil {
					return metricsResponse{}, errMetricsUnavailable
				}
				number, parseErr := strconv.ParseFloat(value, 64)
				if parseErr != nil && !math.IsNaN(number) && !math.IsInf(number, 0) {
					return metricsResponse{}, errMetricsUnavailable
				}
				if !math.IsNaN(number) && !math.IsInf(number, 0) {
					point.Value = &number
				}
			}
			output.Points = append(output.Points, point)
		}
		result.Series = append(result.Series, output)
	}
	if result.Truncated {
		result.Warnings = append(result.Warnings, "Metrics were truncated to the bounded series, point or time limits.")
	}
	return result, nil
}

// metricsExpression accepts Kubernetes IntOrString expressions without changing their text.
func metricsExpression(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) != "" {
		return text, nil
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil && number.String() != "" {
		return number.String(), nil
	}
	return "", errMetricsUnavailable
}
