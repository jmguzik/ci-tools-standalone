package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// pullRequestLimits bound GitHub traffic and keep polling independent of browser requests.
const (
	pullRequestPoll    = 5 * time.Minute
	pullRequestTimeout = 20 * time.Second
	pullRequestMaxBody = 8 * 1024 * 1024
)

// pullRequest contains only public search evidence, without extra per-PR requests.
type pullRequest struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Draft     bool      `json:"draft"`
	Labels    []string  `json:"labels"`
}

// pullRequestSnapshot publishes a complete scan or explicitly stale last-good evidence.
type pullRequestSnapshot struct {
	Enabled  bool          `json:"enabled"`
	Phase    string        `json:"phase"`
	LastSync time.Time     `json:"last_sync"`
	Error    string        `json:"error"`
	PRs      []pullRequest `json:"prs"`
}

// pullRequestConfig keeps server-only credentials apart from the public cache.
type pullRequestConfig struct {
	authors []string
	token   string
	demo    bool
}

// pullRequestCache retains the last complete result and upstream retry boundary together.
type pullRequestCache struct {
	snapshot    pullRequestSnapshot
	nextAttempt time.Time
}

// pullRequests owns one shared cache and serializes complete search scans.
type pullRequests struct {
	config  pullRequestConfig
	client  metricsHTTPClient
	clock   opsalerts.Clock
	mu      sync.RWMutex
	syncMu  sync.Mutex
	cache   pullRequestCache
	running atomic.Bool
}

// newPullRequests validates author qualifiers before they can enter a GitHub query.
func newPullRequests(authors []string, tokenFile string, demo bool) (*pullRequests, error) {
	valid := regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?(?:\[bot\])?$`)
	normalized := []string{}
	for _, author := range authors {
		if !valid.MatchString(author) || strings.Contains(author, "--") {
			return nil, fmt.Errorf("pull request authors must be GitHub login names")
		}
		author = strings.ToLower(author)
		if !slices.Contains(normalized, author) {
			normalized = append(normalized, author)
		}
	}
	p := &pullRequests{config: pullRequestConfig{authors: normalized, demo: demo}, client: &http.Client{Timeout: pullRequestTimeout, CheckRedirect: refuseSOPRedirect}}
	p.cache.snapshot = pullRequestSnapshot{Enabled: len(normalized) > 0, Phase: "disabled", PRs: []pullRequest{}}
	if len(normalized) == 0 {
		return p, nil
	}
	p.cache.snapshot.Phase = "catching_up"
	if demo {
		p.cache.snapshot = pullRequestSnapshot{Enabled: true, Phase: "ready", LastSync: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC), PRs: []pullRequest{{Number: 999001, Title: "Demo: update CI image references", URL: "https://github.com/openshift/release/pull/999001", Author: normalized[0], CreatedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), Labels: []string{"area/ci-operator", "demo"}}}}
		return p, nil
	}
	if tokenFile != "" {
		file, err := os.Open(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading GitHub token file failed")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, 65537))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(data) > 65536 {
			return nil, fmt.Errorf("reading GitHub token file failed")
		}
		token := strings.TrimSpace(string(data))
		if token == "" || strings.ContainsAny(token, " \t\r\n") {
			return nil, fmt.Errorf("GitHub token file must contain one nonempty token")
		}
		p.config.token = token
	}
	return p, nil
}

// now permits deterministic cache and retry tests without changing production timers.
func (p *pullRequests) now() time.Time {
	if p.clock != nil {
		return p.clock.Now().UTC()
	}
	return time.Now().UTC()
}

// Snapshot returns isolated slices so callers cannot mutate shared evidence.
func (p *pullRequests) Snapshot() pullRequestSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := p.cache.snapshot
	result.PRs = append([]pullRequest{}, result.PRs...)
	for i := range result.PRs {
		result.PRs[i].Labels = append([]string{}, result.PRs[i].Labels...)
	}
	return result
}

// Run starts at most one poller, including the initial scan, regardless of request count.
func (p *pullRequests) Run(ctx context.Context) {
	if !p.Snapshot().Enabled || p.config.demo || !p.running.CompareAndSwap(false, true) {
		return
	}
	defer p.running.Store(false)
	ticker := time.NewTicker(pullRequestPoll)
	defer ticker.Stop()
	for {
		// Sync records a sanitized public failure and preserves all last-good evidence.
		_ = p.Sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sync atomically publishes only complete searches and suppresses attempts during backoff.
func (p *pullRequests) Sync(ctx context.Context) error {
	p.syncMu.Lock()
	defer p.syncMu.Unlock()
	if !p.Snapshot().Enabled || p.config.demo {
		return nil
	}
	now := p.now()
	p.mu.RLock()
	next := p.cache.nextAttempt
	p.mu.RUnlock()
	if now.Before(next) {
		return fmt.Errorf("GitHub pull request search is waiting before retrying")
	}
	scanCtx, cancel := context.WithTimeout(ctx, pullRequestTimeout)
	defer cancel()
	prs, retry, err := p.scan(scanCtx, now)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache.nextAttempt = retry
	if err != nil {
		p.cache.snapshot.Phase = "stale"
		p.cache.snapshot.Error = err.Error()
		if p.cache.nextAttempt.Before(now.Add(pullRequestPoll)) {
			p.cache.nextAttempt = now.Add(pullRequestPoll)
		}
		return err
	}
	p.cache.snapshot = pullRequestSnapshot{Enabled: true, Phase: "ready", LastSync: p.now(), PRs: prs}
	return nil
}

// pullRequestSearch decodes only the fields needed to prove complete search coverage.
type pullRequestSearch struct {
	Total      *int              `json:"total_count"`
	Incomplete *bool             `json:"incomplete_results"`
	Items      []pullRequestItem `json:"items"`
}

// pullRequestItem carries upstream state and identity for defensive result filtering.
type pullRequestItem struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Draft     bool      `json:"draft"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

// scan rejects incomplete, inconsistent or oversized results instead of publishing partial lists.
func (p *pullRequests) scan(ctx context.Context, now time.Time) ([]pullRequest, time.Time, error) {
	qualifiers := make([]string, 0, len(p.config.authors))
	for _, author := range p.config.authors {
		qualifiers = append(qualifiers, "author:"+author)
	}
	query := "repo:openshift/release is:pr is:open (" + strings.Join(qualifiers, " OR ") + ")"
	prs := []pullRequest{}
	numbers := []int{}
	total := -1
	var retry time.Time
	for page := 1; page <= 10; page++ {
		values := url.Values{"q": {query}, "per_page": {"100"}, "page": {strconv.Itoa(page)}, "sort": {"created"}, "order": {"asc"}, "advanced_search": {"true"}}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/search/issues?"+values.Encode(), nil)
		if err != nil {
			return nil, retry, fmt.Errorf("GitHub pull request search request failed")
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		request.Header.Set("User-Agent", "ops-alert-dashboard")
		if p.config.token != "" {
			request.Header.Set("Authorization", "Bearer "+p.config.token)
		}
		response, err := p.client.Do(request)
		if err != nil {
			return nil, retry, fmt.Errorf("GitHub pull request search is unavailable")
		}
		backoff := pullRequestRetry(response, now)
		if backoff.After(retry) {
			retry = backoff
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, pullRequestMaxBody+1))
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK {
			if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
				return nil, retry, fmt.Errorf("GitHub pull request search is rate limited or access denied")
			}
			return nil, retry, fmt.Errorf("GitHub pull request search failed (HTTP %d)", response.StatusCode)
		}
		var result pullRequestSearch
		if readErr != nil || closeErr != nil || len(data) > pullRequestMaxBody || json.Unmarshal(data, &result) != nil || result.Total == nil || result.Incomplete == nil || result.Items == nil {
			return nil, retry, fmt.Errorf("GitHub pull request search returned an invalid response")
		}
		if *result.Incomplete {
			return nil, retry, fmt.Errorf("GitHub pull request search returned incomplete results")
		}
		if *result.Total < 0 || *result.Total > 1000 {
			return nil, retry, fmt.Errorf("GitHub pull request search exceeds the 1000-result limit")
		}
		if total < 0 {
			total = *result.Total
		}
		expected := min(100, total-(page-1)*100)
		if total != *result.Total || len(result.Items) != expected {
			return nil, retry, fmt.Errorf("GitHub pull request search changed during pagination")
		}
		for _, item := range result.Items {
			if item.Number <= 0 || slices.Contains(numbers, item.Number) {
				return nil, retry, fmt.Errorf("GitHub pull request search returned invalid or repeated identities")
			}
			numbers = append(numbers, item.Number)
			if item.State != "open" || !slices.Contains(p.config.authors, strings.ToLower(item.User.Login)) || item.PullRequest == nil || item.URL != "https://github.com/openshift/release/pull/"+strconv.Itoa(item.Number) || item.PullRequest.URL != "https://api.github.com/repos/openshift/release/pulls/"+strconv.Itoa(item.Number) {
				continue
			}
			labels := []string{}
			for _, label := range item.Labels {
				labels = append(labels, label.Name)
			}
			prs = append(prs, pullRequest{Number: item.Number, Title: item.Title, URL: item.URL, Author: item.User.Login, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, Draft: item.Draft, Labels: labels})
		}
		if len(numbers) == total {
			return prs, retry, nil
		}
		if retry.After(now) {
			return nil, retry, fmt.Errorf("GitHub pull request search is rate limited before pagination completed")
		}
	}
	return nil, retry, fmt.Errorf("GitHub pull request search exceeds the 1000-result limit")
}

// pullRequestRetry honors both primary and secondary GitHub rate limit boundaries.
func pullRequestRetry(response *http.Response, now time.Time) time.Time {
	var retry time.Time
	value := response.Header.Get("Retry-After")
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		retry = now.Add(time.Duration(seconds) * time.Second)
	}
	if date, err := http.ParseTime(value); err == nil && date.After(retry) {
		retry = date
	}
	if response.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > 0 {
			date := time.Unix(reset, 0).Add(time.Second)
			if date.After(retry) {
				retry = date
			}
		}
	}
	if (response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests) && !retry.After(now) {
		retry = now.Add(pullRequestPoll)
	}
	return retry
}
