package opsproxy

import (
	"testing"
	"time"
)

func TestNewSlackAPITimeout(t *testing.T) {
	t.Parallel()
	api, ok := NewSlackAPI(func() []byte { return []byte("tok") }).(*slackAPI)
	if !ok {
		t.Fatal("NewSlackAPI should return *slackAPI")
	}
	if api.httpClient == nil || api.httpClient.Timeout != slackHTTPTimeout {
		t.Fatalf("timeout=%v, want %v", api.httpClient.Timeout, slackHTTPTimeout)
	}
	if slackHTTPTimeout != 15*time.Second {
		t.Fatalf("slackHTTPTimeout=%s, want 15s", slackHTTPTimeout)
	}
}
