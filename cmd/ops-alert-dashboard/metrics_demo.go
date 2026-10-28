package main

import (
	"context"
	"time"
)

// demoMetrics produces deterministic synthetic evidence without credentials or transports.
type demoMetrics struct{}

// Read uses a fixed notification-relative window and synthetic samples, including a gap.
func (demoMetrics) Read(ctx context.Context, alert string, reported time.Time) (metricsResponse, error) {
	if err := ctx.Err(); err != nil {
		return metricsResponse{}, err
	}
	if reported.IsZero() {
		reported = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	}
	window := metricWindow(reported, reported)
	result := metricsResponse{Source: "core-ci", Demo: true, Alert: alert, Rule: metricsRule{Namespace: "ci-monitoring", Resource: "synthetic-demo-rules", Group: "synthetic", Expression: `synthetic_demo_alert_value{alertname="` + alert + `"} > 10`, For: "10m"}, Window: window, Series: []metricsSeries{}, Warnings: []string{"Synthetic demo data; no core-ci metrics were queried."}}
	series := metricsSeries{Labels: map[string]string{"job": "synthetic-demo", "instance": "demo"}, Points: []metricsPoint{}}
	for i := 0; i < metricsMaxPoints; i++ {
		point := metricsPoint{At: window.Start.Add(time.Duration(i) * time.Minute)}
		if i < 45 || i > 50 {
			value := 12 + float64((i*7)%23)/2
			point.Value = &value
		}
		series.Points = append(series.Points, point)
	}
	result.Series = append(result.Series, series)
	return result, nil
}
