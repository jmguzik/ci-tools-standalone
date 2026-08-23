package opsproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type fakeSlack struct {
	mu       sync.Mutex
	posts    int
	updates  int
	pins     int
	topics   []string
	lastTS   int
	messages []string
}

func (f *fakeSlack) PostMessage(channel, text string, blocks []slack.Block) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts++
	f.lastTS++
	f.messages = append(f.messages, text)
	return "C123", fmtTS(f.lastTS), nil
}

func (f *fakeSlack) UpdateMessage(channel, ts, text string, blocks []slack.Block) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	f.messages = append(f.messages, text)
	return nil
}

func (f *fakeSlack) PinMessage(channel, ts string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pins++
	return nil
}

func (f *fakeSlack) SetTopic(channel, topic string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.topics = append(f.topics, topic)
	return nil
}

func (f *fakeSlack) lastIncidentText(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	suffix := ": " + id
	for i := len(f.messages) - 1; i >= 0; i-- {
		if strings.HasSuffix(f.messages[i], suffix) {
			return f.messages[i]
		}
	}
	return ""
}

func fmtTS(n int) string { return "ts-" + itoa(n) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

type fakeAM struct {
	mu          sync.Mutex
	alerts      []Alert
	silences    []Silence
	created     []SilenceSpec
	expired     []string
	nextID      int
	createErr   error
	listErr     error
	silencesErr error
}

func (f *fakeAM) ListAlerts(ctx context.Context) ([]Alert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]Alert, len(f.alerts))
	copy(out, f.alerts)
	return out, nil
}

func (f *fakeAM) ListSilences(ctx context.Context) ([]Silence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.silencesErr != nil {
		return nil, f.silencesErr
	}
	out := make([]Silence, len(f.silences))
	copy(out, f.silences)
	return out, nil
}

func (f *fakeAM) CreateSilence(ctx context.Context, spec SilenceSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, spec)
	id := spec.ID
	if id == "" {
		f.nextID++
		id = "silence-" + itoa(f.nextID)
	}
	sil := Silence{
		ID:        id,
		State:     "active",
		Comment:   spec.Comment,
		CreatedBy: spec.CreatedBy,
		StartsAt:  spec.StartsAt,
		EndsAt:    spec.EndsAt,
		Matchers:  spec.Matchers,
	}
	replaced := false
	for i, existing := range f.silences {
		if existing.ID == id {
			f.silences[i] = sil
			replaced = true
			break
		}
	}
	if !replaced {
		f.silences = append(f.silences, sil)
	}
	return id, nil
}

func (f *fakeAM) ExpireSilence(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired = append(f.expired, id)
	for i, sil := range f.silences {
		if sil.ID == id {
			f.silences[i].State = "expired"
		}
	}
	return nil
}

func testLogger() *logrus.Entry {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return logrus.NewEntry(l)
}

func testServer(t *testing.T, allowlist string, hook, api string, am *fakeAM, slackClient *fakeSlack) *Server {
	t.Helper()
	store := NewStore(fakectrlruntimeclient.NewClientBuilder().Build(), "ci", "ops-proxy")
	cfg := Config{
		HookToken:    func() []byte { return []byte(hook) },
		APIToken:     func() []byte { return []byte(api) },
		Allowlist:    ParseAllowlist(allowlist),
		SlackChannel: "#dptp-robot-testing",
		SetTopic:     true,
		Now:          func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) },
	}
	return NewServer(testLogger(), cfg, store, am, slackClient)
}

func TestHealth(t *testing.T) {
	t.Parallel()
	s := testServer(t, "", "hook", "api", &fakeAM{}, &fakeSlack{})
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var page map[string]bool
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(map[string]bool{"ok": true}, page); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestHookUnauthorized(t *testing.T) {
	t.Parallel()
	s := testServer(t, "U1", "hook-token", "api-token", &fakeAM{}, &fakeSlack{})
	body := []byte(`{"status":"firing","commonLabels":{"alertname":"infrastructure-job-failures","job_name":"job-a"}}`)
	testCases := []struct {
		name   string
		header string
	}{
		{name: "missing"},
		{name: "wrong", header: "Bearer other"},
		{name: "not bearer", header: "hook-token"},
	}
	for i := range testCases {
		tc := testCases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/hook/alertmanager", bytes.NewReader(body))
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestAckDenyEmptyAllowlist(t *testing.T) {
	t.Parallel()
	s := testServer(t, "", "hook", "api-token", &fakeAM{}, &fakeSlack{})
	raw, _ := json.Marshal(ActionRequest{IncidentID: "x", Duration: "2h", SlackUserID: "U123"})
	req := httptest.NewRequest(http.MethodPost, "/ack", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer api-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func firingWebhook(alertname, jobName string) []byte {
	p := WebhookPayload{
		Status: "firing",
		CommonLabels: map[string]string{
			"alertname": alertname,
			"job_name":  jobName,
		},
		Alerts: []WebhookAlert{{
			Status: "firing",
			Labels: map[string]string{
				"alertname": alertname,
				"job_name":  jobName,
				"severity":  "critical",
			},
		}},
	}
	raw, _ := json.Marshal(p)
	return raw
}

func reasonWebhook(alertname, reason string) []byte {
	p := WebhookPayload{
		Status: "firing",
		CommonLabels: map[string]string{
			"alertname": alertname,
			"reason":    reason,
		},
		Alerts: []WebhookAlert{{
			Status: "firing",
			Labels: map[string]string{
				"alertname": alertname,
				"reason":    reason,
			},
		}},
	}
	raw, _ := json.Marshal(p)
	return raw
}

func postHook(t *testing.T, s *Server, token string, body []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hook/alertmanager", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("hook status=%d body=%s", rr.Code, rr.Body.String())
	}
	s.waitForIdle()
}

func postAction(t *testing.T, h http.Handler, path, token string, body ActionRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestAckHappyPath(t *testing.T) {
	t.Parallel()
	am := &fakeAM{}
	sl := &fakeSlack{}
	s := testServer(t, "U123", "hook-token", "api-token", am, sl)
	h := s.Handler()
	postHook(t, s, "hook-token", firingWebhook("infrastructure-job-failures", "periodic-foo"))

	incidentID := "infrastructure-job-failures/periodic-foo"
	ackBody, _ := json.Marshal(ActionRequest{IncidentID: incidentID, Duration: "24h", SlackUserID: "U123"})
	ackReq := httptest.NewRequest(http.MethodPost, "/ack", bytes.NewReader(ackBody))
	ackReq.Header.Set("Authorization", "Bearer api-token")
	ackRR := httptest.NewRecorder()
	h.ServeHTTP(ackRR, ackReq)
	if ackRR.Code != http.StatusOK {
		t.Fatalf("ack status=%d body=%s", ackRR.Code, ackRR.Body.String())
	}

	am.mu.Lock()
	defer am.mu.Unlock()
	if len(am.created) != 1 {
		t.Fatalf("created silences=%d", len(am.created))
	}
	spec := am.created[0]
	wantMatchers := []Matcher{
		equalMatcher("alertname", "infrastructure-job-failures"),
		equalMatcher("job_name", "periodic-foo"),
	}
	if diff := cmp.Diff(wantMatchers, spec.Matchers); diff != "" {
		t.Fatalf("matchers (-want +got):\n%s", diff)
	}
	wantEnd := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if diff := cmp.Diff(wantEnd, spec.EndsAt); diff != "" {
		t.Fatalf("endsAt (-want +got):\n%s", diff)
	}
	if spec.CreatedBy != "ops-proxy" {
		t.Fatalf("createdBy=%s", spec.CreatedBy)
	}
	st, err := s.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	inc := st.Incidents[incidentID]
	if inc.SilenceID == "" {
		t.Fatal("expected silence_id cache")
	}
	if inc.AckedBy != "U123" {
		t.Fatalf("acked_by=%s", inc.AckedBy)
	}
	if sl.posts < 2 {
		t.Fatalf("expected card+board posts, posts=%d", sl.posts)
	}
	if sl.pins < 1 {
		t.Fatalf("expected board pin, pins=%d", sl.pins)
	}
}

func TestWebhookFiringThenResolved(t *testing.T) {
	t.Parallel()
	am := &fakeAM{}
	sl := &fakeSlack{}
	s := testServer(t, "U123", "hook-token", "api-token", am, sl)
	h := s.Handler()
	postHook(t, s, "hook-token", firingWebhook("infrastructure-job-failures", "periodic-foo"))
	st, err := s.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id := "infrastructure-job-failures/periodic-foo"
	if _, ok := st.Incidents[id]; !ok {
		t.Fatalf("expected firing incident, got %#v", st.Incidents)
	}

	resolved := WebhookPayload{
		Status: "resolved",
		CommonLabels: map[string]string{
			"alertname": "infrastructure-job-failures",
			"job_name":  "periodic-foo",
		},
		Alerts: []WebhookAlert{{
			Status: "resolved",
			Labels: map[string]string{
				"alertname": "infrastructure-job-failures",
				"job_name":  "periodic-foo",
			},
		}},
	}
	raw, _ := json.Marshal(resolved)
	resReq := httptest.NewRequest(http.MethodPost, "/hook/alertmanager", bytes.NewReader(raw))
	resReq.Header.Set("Authorization", "Bearer hook-token")
	resRR := httptest.NewRecorder()
	h.ServeHTTP(resRR, resReq)
	if resRR.Code != http.StatusOK {
		t.Fatalf("resolved status=%d body=%s", resRR.Code, resRR.Body.String())
	}
	s.waitForIdle()
	st, err = s.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Incidents[id]; ok {
		t.Fatalf("incident still on board after resolved: %#v", st.Incidents)
	}
	if sl.updates < 1 {
		t.Fatalf("expected resolved card/board update, updates=%d", sl.updates)
	}
}

func TestFormatTopic(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name  string
		names []string
		want  string
	}{
		{name: "none", want: "RED 0 OPEN"},
		{name: "two", names: []string{"a", "b"}, want: "RED 2 OPEN · a, b"},
	}
	for i := range testCases {
		tc := testCases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FormatTopic(tc.names)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("(-want +got):\n%s", diff)
			}
		})
	}
	long := make([]string, 40)
	for i := range long {
		long[i] = "periodic-openshift-release-merge-blockers"
	}
	got := FormatTopic(long)
	if len(got) > maxTopicChars {
		t.Fatalf("topic len %d > %d: %s", len(got), maxTopicChars, got)
	}
	if got[:6] != "RED 40" {
		t.Fatalf("prefix=%q", got[:10])
	}
}

func TestAckActionIDsOnCard(t *testing.T) {
	t.Parallel()
	blocks := ackActionBlocks("inc/id")
	want := []string{
		actionAck2h, actionAck4h, actionAck8h, actionAck16h, actionAck24h,
		actionAck2d, actionAckMonday, actionUnack, actionNeedsHuman,
	}
	var got []string
	for _, b := range blocks {
		ab, ok := b.(*slack.ActionBlock)
		if !ok || ab.Elements == nil {
			continue
		}
		for _, el := range ab.Elements.ElementSet {
			btn, ok := el.(*slack.ButtonBlockElement)
			if !ok {
				continue
			}
			got = append(got, btn.ActionID)
			if btn.Value != "inc/id" {
				t.Fatalf("button value=%q", btn.Value)
			}
		}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("action ids (-want +got):\n%s", diff)
	}
}

func TestCMSilenceIDWithoutAMSilenceIsOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	am := &fakeAM{}
	sl := &fakeSlack{}
	s := testServer(t, "U123", "hook-token", "api-token", am, sl)
	postHook(t, s, "hook-token", firingWebhook("infrastructure-job-failures", "periodic-foo"))
	id := "infrastructure-job-failures/periodic-foo"
	if _, err := s.store.Mutate(ctx, func(st *State) error {
		inc := st.Incidents[id]
		inc.SilenceID = "cm-only-silence"
		inc.AckedBy = "U999"
		inc.EndsAt = "2099-01-01T00:00:00Z"
		st.Incidents[id] = inc
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.renderLocked(ctx); err != nil {
		t.Fatal(err)
	}
	got := sl.lastIncidentText(id)
	if got != "OPEN: "+id {
		t.Fatalf("card=%q, want OPEN (mute must not be ConfigMap-authored)", got)
	}
}

func TestAMSilenceWithEmptyCMCacheIsAcked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	am := &fakeAM{}
	sl := &fakeSlack{}
	s := testServer(t, "U123", "hook-token", "api-token", am, sl)
	postHook(t, s, "hook-token", firingWebhook("infrastructure-job-failures", "periodic-foo"))
	id := "infrastructure-job-failures/periodic-foo"
	if _, err := s.store.Mutate(ctx, func(st *State) error {
		inc := st.Incidents[id]
		inc.SilenceID = ""
		inc.AckedBy = ""
		inc.EndsAt = ""
		st.Incidents[id] = inc
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	am.mu.Lock()
	am.silences = []Silence{{
		ID:     "from-am",
		State:  "active",
		EndsAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		Matchers: []Matcher{
			equalMatcher("alertname", "infrastructure-job-failures"),
			equalMatcher("job_name", "periodic-foo"),
		},
	}}
	am.mu.Unlock()
	if err := s.renderLocked(ctx); err != nil {
		t.Fatal(err)
	}
	got := sl.lastIncidentText(id)
	if got != "ACKED: "+id {
		t.Fatalf("card=%q, want ACKED from AM silence with empty CM cache", got)
	}
}

func TestListSilencesFailureDoesNotAckFromConfigMap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	am := &fakeAM{}
	sl := &fakeSlack{}
	s := testServer(t, "U123", "hook-token", "api-token", am, sl)
	postHook(t, s, "hook-token", firingWebhook("infrastructure-job-failures", "periodic-foo"))
	id := "infrastructure-job-failures/periodic-foo"
	if _, err := s.store.Mutate(ctx, func(st *State) error {
		inc := st.Incidents[id]
		inc.SilenceID = "cm-only-silence"
		inc.AckedBy = "U999"
		inc.EndsAt = "2099-01-01T00:00:00Z"
		st.Incidents[id] = inc
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	am.mu.Lock()
	am.silencesErr = errors.New("am down")
	am.mu.Unlock()
	if err := s.renderLocked(ctx); err != nil {
		t.Fatal(err)
	}
	got := sl.lastIncidentText(id)
	if got != "OPEN: "+id {
		t.Fatalf("card=%q, want OPEN when ListSilences fails", got)
	}
	st, err := s.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Incidents[id].SilenceID != "cm-only-silence" {
		t.Fatalf("must not wipe CM silence cache when list fails, silence_id=%q", st.Incidents[id].SilenceID)
	}
}

func TestUnackExpiresAMSilenceAndCardOpens(t *testing.T) {
	t.Parallel()
	am := &fakeAM{}
	sl := &fakeSlack{}
	s := testServer(t, "U123", "hook-token", "api-token", am, sl)
	h := s.Handler()
	postHook(t, s, "hook-token", firingWebhook("infrastructure-job-failures", "periodic-foo"))
	id := "infrastructure-job-failures/periodic-foo"
	ackRR := postAction(t, h, "/ack", "api-token", ActionRequest{IncidentID: id, Duration: "24h", SlackUserID: "U123"})
	if ackRR.Code != http.StatusOK {
		t.Fatalf("ack status=%d body=%s", ackRR.Code, ackRR.Body.String())
	}
	if got := sl.lastIncidentText(id); got != "ACKED: "+id {
		t.Fatalf("after ack card=%q", got)
	}
	unackRR := postAction(t, h, "/unack", "api-token", ActionRequest{IncidentID: id, SlackUserID: "U123"})
	if unackRR.Code != http.StatusOK {
		t.Fatalf("unack status=%d body=%s", unackRR.Code, unackRR.Body.String())
	}
	am.mu.Lock()
	expired := append([]string(nil), am.expired...)
	am.mu.Unlock()
	if len(expired) == 0 {
		t.Fatal("POST /unack must expire an Alertmanager silence")
	}
	if got := sl.lastIncidentText(id); got != "OPEN: "+id {
		t.Fatalf("after unack card=%q", got)
	}

	ackRR = postAction(t, h, "/ack", "api-token", ActionRequest{IncidentID: id, Duration: "2h", SlackUserID: "U123"})
	if ackRR.Code != http.StatusOK {
		t.Fatalf("second ack status=%d body=%s", ackRR.Code, ackRR.Body.String())
	}
	am.mu.Lock()
	am.silences = nil
	am.mu.Unlock()
	if err := s.renderLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sl.lastIncidentText(id); got != "OPEN: "+id {
		t.Fatalf("after silence gone card=%q", got)
	}
}

func TestNeedsHumanDoesNotCreateSilence(t *testing.T) {
	t.Parallel()
	am := &fakeAM{}
	sl := &fakeSlack{}
	s := testServer(t, "U123", "hook-token", "api-token", am, sl)
	h := s.Handler()
	postHook(t, s, "hook-token", firingWebhook("infrastructure-job-failures", "periodic-foo"))
	id := "infrastructure-job-failures/periodic-foo"
	rr := postAction(t, h, "/needs-human", "api-token", ActionRequest{IncidentID: id, SlackUserID: "U123"})
	if rr.Code != http.StatusOK {
		t.Fatalf("needs-human status=%d body=%s", rr.Code, rr.Body.String())
	}
	am.mu.Lock()
	created := len(am.created)
	am.mu.Unlock()
	if created != 0 {
		t.Fatalf("needs-human created %d silences", created)
	}
	if got := sl.lastIncidentText(id); got != "NEEDS HUMAN: "+id {
		t.Fatalf("card=%q", got)
	}
}

func TestReconcileRebuildsFromFiringAndSilences(t *testing.T) {
	t.Parallel()
	am := &fakeAM{
		alerts: []Alert{
			{
				Labels: map[string]string{"alertname": "infrastructure-job-failures", "job_name": "periodic-foo"},
				State:  "active",
			},
			{
				Labels: map[string]string{"alertname": "infrastructure-job-failures", "job_name": "periodic-bar"},
				State:  "active",
			},
		},
		silences: []Silence{{
			ID:     "from-am",
			State:  "active",
			EndsAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
			Matchers: []Matcher{
				equalMatcher("alertname", "infrastructure-job-failures"),
				equalMatcher("job_name", "periodic-foo"),
			},
		}},
	}
	sl := &fakeSlack{}
	s := testServer(t, "U123", "hook-token", "api-token", am, sl)
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := s.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Incidents) != 2 {
		t.Fatalf("incidents=%d, want 2 rebuilt from firing", len(st.Incidents))
	}
	foo := "infrastructure-job-failures/periodic-foo"
	bar := "infrastructure-job-failures/periodic-bar"
	if got := sl.lastIncidentText(foo); got != "ACKED: "+foo {
		t.Fatalf("foo card=%q", got)
	}
	if got := sl.lastIncidentText(bar); got != "OPEN: "+bar {
		t.Fatalf("bar card=%q", got)
	}
}

func TestHookUnusableAlertsOK(t *testing.T) {
	t.Parallel()
	s := testServer(t, "U123", "hook-token", "api-token", &fakeAM{}, &fakeSlack{})
	body := []byte(`{"status":"firing","commonLabels":{"alertname":"Watchdog"},"alerts":[{"status":"firing","labels":{"alertname":"Watchdog"}}]}`)
	postHook(t, s, "hook-token", body)
	st, err := s.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Incidents) != 0 {
		t.Fatalf("unusable webhook created incidents: %#v", st.Incidents)
	}
}

func TestReconcileResolvesDroppedCards(t *testing.T) {
	t.Parallel()
	am := &fakeAM{}
	sl := &fakeSlack{}
	s := testServer(t, "U123", "hook-token", "api-token", am, sl)
	postHook(t, s, "hook-token", firingWebhook("infrastructure-job-failures", "periodic-foo"))
	id := "infrastructure-job-failures/periodic-foo"
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := s.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Incidents[id]; ok {
		t.Fatalf("dropped incident still stored: %#v", st.Incidents)
	}
	if got := sl.lastIncidentText(id); got != "RESOLVED: "+id {
		t.Fatalf("card=%q", got)
	}
}

func TestRequireBearerTrimsToken(t *testing.T) {
	t.Parallel()
	s := testServer(t, "U123", "hook-token\n", "api-token", &fakeAM{}, &fakeSlack{})
	postHook(t, s, "hook-token", firingWebhook("infrastructure-job-failures", "periodic-foo"))
}

func TestGetAckWithoutBearerUnauthorized(t *testing.T) {
	t.Parallel()
	s := testServer(t, "U123", "hook-token", "api-token", &fakeAM{}, &fakeSlack{})
	req := httptest.NewRequest(http.MethodGet, "/ack", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAckRejectsHookToken(t *testing.T) {
	t.Parallel()
	s := testServer(t, "U123", "hook-token", "api-token", &fakeAM{}, &fakeSlack{})
	raw, _ := json.Marshal(ActionRequest{IncidentID: "x", Duration: "2h", SlackUserID: "U123"})
	req := httptest.NewRequest(http.MethodPost, "/ack", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer hook-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAckHTTPRefuseDuration1hAndForever(t *testing.T) {
	t.Parallel()
	s := testServer(t, "U123", "hook-token", "api-token", &fakeAM{}, &fakeSlack{})
	h := s.Handler()
	for _, spec := range []string{"1h", "forever"} {
		rr := postAction(t, h, "/ack", "api-token", ActionRequest{IncidentID: "x", Duration: spec, SlackUserID: "U123"})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("duration %q status=%d body=%s", spec, rr.Code, rr.Body.String())
		}
	}
}

func TestAckCoalescedCIOperatorErrorPostsBothSilences(t *testing.T) {
	t.Parallel()
	am := &fakeAM{}
	sl := &fakeSlack{}
	s := testServer(t, "U123", "hook-token", "api-token", am, sl)
	h := s.Handler()
	postHook(t, s, "hook-token", reasonWebhook("high-ci-operator-error-rate", "creating_release_images"))
	postHook(t, s, "hook-token", reasonWebhook("high-ci-operator-infra-error-rate", "creating_release_images"))
	id := "ci-operator-error/creating_release_images"
	st, err := s.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Incidents) != 1 {
		t.Fatalf("want 1 coalesced card, got %d: %#v", len(st.Incidents), st.Incidents)
	}
	if sl.posts != 2 {
		t.Fatalf("posts=%d, want 2 (one card + board)", sl.posts)
	}
	rr := postAction(t, h, "/ack", "api-token", ActionRequest{IncidentID: id, Duration: "24h", SlackUserID: "U123"})
	if rr.Code != http.StatusOK {
		t.Fatalf("ack status=%d body=%s", rr.Code, rr.Body.String())
	}
	am.mu.Lock()
	created := append([]SilenceSpec(nil), am.created...)
	am.mu.Unlock()
	if len(created) != 2 {
		t.Fatalf("created silences=%d, want 2", len(created))
	}
	var names []string
	for _, spec := range created {
		for _, m := range spec.Matchers {
			if m.Name == "alertname" {
				names = append(names, m.Value)
			}
		}
	}
	sort.Strings(names)
	want := []string{"high-ci-operator-error-rate", "high-ci-operator-infra-error-rate"}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Fatalf("alertnames (-want +got):\n%s", diff)
	}
	if got := sl.lastIncidentText(id); got != "ACKED: "+id {
		t.Fatalf("card=%q", got)
	}
}

func TestResolvedSlackOutsideConfigMapMutate(t *testing.T) {
	t.Parallel()
	base := fakectrlruntimeclient.NewClientBuilder().Build()
	var allowConflict atomic.Bool
	var conflicted atomic.Bool
	kube := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c ctrlruntimeclient.WithWatch, obj ctrlruntimeclient.Object, opts ...ctrlruntimeclient.UpdateOption) error {
			if allowConflict.Load() && conflicted.CompareAndSwap(false, true) {
				return apierrors.NewConflict(schema.GroupResource{Group: "", Resource: "configmaps"}, "ops-proxy", fmt.Errorf("conflict"))
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	store := NewStore(kube, "ci", "ops-proxy")
	sl := &fakeSlack{}
	s := NewServer(testLogger(), Config{
		HookToken:    func() []byte { return []byte("hook-token") },
		APIToken:     func() []byte { return []byte("api-token") },
		Allowlist:    ParseAllowlist("U123"),
		SlackChannel: "#dptp-robot-testing",
		SetTopic:     true,
		Now:          func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) },
	}, store, &fakeAM{}, sl)
	postHook(t, s, "hook-token", firingWebhook("infrastructure-job-failures", "periodic-foo"))
	allowConflict.Store(true)

	resolved := WebhookPayload{
		Status: "resolved",
		CommonLabels: map[string]string{
			"alertname": "infrastructure-job-failures",
			"job_name":  "periodic-foo",
		},
		Alerts: []WebhookAlert{{
			Status: "resolved",
			Labels: map[string]string{
				"alertname": "infrastructure-job-failures",
				"job_name":  "periodic-foo",
			},
		}},
	}
	raw, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	postHook(t, s, "hook-token", raw)
	if !conflicted.Load() {
		t.Fatal("expected ConfigMap conflict retry")
	}
	var resolvedMsgs int
	sl.mu.Lock()
	for _, msg := range sl.messages {
		if strings.HasPrefix(msg, "RESOLVED:") {
			resolvedMsgs++
		}
	}
	sl.mu.Unlock()
	if resolvedMsgs != 1 {
		t.Fatalf("RESOLVED Slack updates=%d, want 1 (Slack I/O must not run inside Mutate retries)", resolvedMsgs)
	}
}
