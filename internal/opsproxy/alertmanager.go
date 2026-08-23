package opsproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Matcher is an Alertmanager v2 silence matcher.
type Matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual *bool  `json:"isEqual,omitempty"`
}

// Alert is a firing (or suppressed) alert from GET /api/v2/alerts.
type Alert struct {
	Labels map[string]string
	State  string
}

// Silence is an Alertmanager v2 silence.
type Silence struct {
	ID        string
	State     string
	Comment   string
	CreatedBy string
	StartsAt  time.Time
	EndsAt    time.Time
	Matchers  []Matcher
}

// SilenceSpec is POST /api/v2/silences.
type SilenceSpec struct {
	ID        string
	Matchers  []Matcher
	StartsAt  time.Time
	EndsAt    time.Time
	CreatedBy string
	Comment   string
}

// Alertmanager is the AM v2 subset ops-proxy needs.
type Alertmanager interface {
	ListAlerts(ctx context.Context) ([]Alert, error)
	ListSilences(ctx context.Context) ([]Silence, error)
	CreateSilence(ctx context.Context, spec SilenceSpec) (string, error)
	ExpireSilence(ctx context.Context, id string) error
}

type amClient struct {
	baseURL    string
	token      func() []byte
	httpClient *http.Client
}

func NewAlertmanagerClient(baseURL string, token func() []byte, caPath string) (Alertmanager, error) {
	httpClient, err := alertmanagerHTTPClient(caPath)
	if err != nil {
		return nil, err
	}
	return &amClient{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:      token,
		httpClient: httpClient,
	}, nil
}

func alertmanagerHTTPClient(caPath string) (*http.Client, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	if strings.TrimSpace(caPath) == "" {
		return client, nil
	}
	pemBytes, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read alertmanager CA %s: %w", caPath, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("alertmanager CA %s contained no certificates", caPath)
	}
	tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := tr.Clone()
		cloned.TLSClientConfig = tlsCfg
		client.Transport = cloned
		return client, nil
	}
	client.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsCfg}
	return client, nil
}

type amStatusError struct {
	method, path, body string
	status             int
}

func (e *amStatusError) Error() string {
	return fmt.Sprintf("alertmanager %s %s: status %d: %s", e.method, e.path, e.status, e.body)
}

type gettableAlert struct {
	Labels map[string]string `json:"labels"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

type gettableSilence struct {
	ID     string `json:"id"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
	Comment   string    `json:"comment"`
	CreatedBy string    `json:"createdBy"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	Matchers  []Matcher `json:"matchers"`
}

type postableSilence struct {
	ID        string    `json:"id,omitempty"`
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
}

type postSilenceResponse struct {
	SilenceID string `json:"silenceID"`
}

func (c *amClient) ListAlerts(ctx context.Context) ([]Alert, error) {
	q := url.Values{}
	q.Set("active", "true")
	q.Set("silenced", "true")
	q.Set("inhibited", "true")
	q.Set("unprocessed", "false")
	var raw []gettableAlert
	if err := c.doJSON(ctx, http.MethodGet, "/api/v2/alerts?"+q.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Alert, 0, len(raw))
	for _, a := range raw {
		out = append(out, Alert{Labels: a.Labels, State: a.Status.State})
	}
	return out, nil
}

func (c *amClient) ListSilences(ctx context.Context) ([]Silence, error) {
	var raw []gettableSilence
	if err := c.doJSON(ctx, http.MethodGet, "/api/v2/silences", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Silence, 0, len(raw))
	for _, s := range raw {
		out = append(out, Silence{
			ID:        s.ID,
			State:     s.Status.State,
			Comment:   s.Comment,
			CreatedBy: s.CreatedBy,
			StartsAt:  s.StartsAt,
			EndsAt:    s.EndsAt,
			Matchers:  s.Matchers,
		})
	}
	return out, nil
}

func (c *amClient) CreateSilence(ctx context.Context, spec SilenceSpec) (string, error) {
	body := postableSilence{
		ID:        spec.ID,
		Matchers:  spec.Matchers,
		StartsAt:  spec.StartsAt,
		EndsAt:    spec.EndsAt,
		CreatedBy: spec.CreatedBy,
		Comment:   spec.Comment,
	}
	var resp postSilenceResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v2/silences", body, &resp); err != nil {
		return "", err
	}
	if resp.SilenceID == "" {
		return "", fmt.Errorf("alertmanager returned empty silenceID")
	}
	return resp.SilenceID, nil
}

func (c *amClient) ExpireSilence(ctx context.Context, id string) error {
	err := c.doJSON(ctx, http.MethodDelete, "/api/v2/silence/"+url.PathEscape(id), nil, nil)
	var se *amStatusError
	if errors.As(err, &se) && se.status == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *amClient) doJSON(ctx context.Context, method, path string, body any, dest any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal alertmanager request: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != nil {
		if tok := c.token(); len(tok) > 0 {
			req.Header.Set("Authorization", "Bearer "+string(tok))
		}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("alertmanager %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read alertmanager response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &amStatusError{method: method, path: path, status: resp.StatusCode, body: truncate(string(respBody), 512)}
	}
	if dest == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, dest); err != nil {
		return fmt.Errorf("decode alertmanager response: %w", err)
	}
	return nil
}

func alertIsFiring(a Alert) bool {
	switch strings.ToLower(a.State) {
	case "unprocessed":
		return false
	case "active", "suppressed", "":
		return true
	default:
		return false
	}
}

func silenceIsActive(s Silence) bool {
	return strings.EqualFold(s.State, "active")
}
