package opsproxy

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
)

// WebhookPayload is the standard Alertmanager webhook JSON.
type WebhookPayload struct {
	Status       string            `json:"status"`
	CommonLabels map[string]string `json:"commonLabels"`
	GroupLabels  map[string]string `json:"groupLabels"`
	Alerts       []WebhookAlert    `json:"alerts"`
}

// WebhookAlert is one alert in an AM webhook.
type WebhookAlert struct {
	Status string            `json:"status"`
	Labels map[string]string `json:"labels"`
}

// ActionRequest is the in-cluster body for /ack /unack /needs-human.
type ActionRequest struct {
	IncidentID  string `json:"incident_id"`
	Duration    string `json:"duration"`
	SlackUserID string `json:"slack_user_id"`
}

// Config is HTTP and Slack board settings.
type Config struct {
	HookToken    func() []byte
	APIToken     func() []byte
	Allowlist    map[string]struct{}
	SlackChannel string
	SetTopic     bool
	Now          func() time.Time
}

// Server is the ops-proxy HTTP surface and reconcile loop.
type Server struct {
	log   *logrus.Entry
	cfg   Config
	store *Store
	am    Alertmanager
	slack SlackClient
	mu    sync.Mutex
}

func NewServer(log *logrus.Entry, cfg Config, store *Store, am Alertmanager, slack SlackClient) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Allowlist == nil {
		cfg.Allowlist = map[string]struct{}{}
	}
	if log == nil {
		log = logrus.NewEntry(logrus.StandardLogger())
	}
	return &Server{log: log, cfg: cfg, store: store, am: am, slack: slack}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/hook/alertmanager", s.handleHook)
	mux.HandleFunc("/ack", s.handleAck)
	mux.HandleFunc("/unack", s.handleUnack)
	mux.HandleFunc("/needs-human", s.handleNeedsHuman)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	page := map[string]bool{"ok": true}
	if err := json.NewEncoder(w).Encode(page); err != nil {
		s.log.WithError(err).WithField("page", page).Error("failed to encode page")
	}
}

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireBearer(w, r, s.cfg.HookToken) {
		return
	}
	var payload WebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ingestWebhook(r.Context(), payload); err != nil {
		if errors.Is(err, ErrNoIdentity) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.log.WithError(err).Error("webhook ingest failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]bool{"ok": true}); err != nil {
		s.log.WithError(err).Error("failed to encode webhook response")
	}
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, s.ack)
}

func (s *Server) handleUnack(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, s.unack)
}

func (s *Server) handleNeedsHuman(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, s.needsHuman)
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request, fn func(context.Context, ActionRequest) error) {
	if !s.requireBearer(w, r, s.cfg.APIToken) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req ActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if !s.requireAllowlist(w, req.SlackUserID) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(r.Context(), req); err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, errIncidentNotFound):
			code = http.StatusNotFound
		case errors.Is(err, errBadRequest):
			code = http.StatusBadRequest
		}
		s.log.WithError(err).WithField("incident_id", req.IncidentID).Error("action failed")
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]bool{"ok": true}); err != nil {
		s.log.WithError(err).Error("failed to encode action response")
	}
}

var (
	errIncidentNotFound = errors.New("incident not found")
	errBadRequest       = errors.New("bad request")
)

func (s *Server) requireBearer(w http.ResponseWriter, r *http.Request, tokenFn func() []byte) bool {
	var want []byte
	if tokenFn != nil {
		want = tokenFn()
	}
	got := []byte(extractBearer(r.Header.Get("Authorization")))
	if len(want) == 0 || !hmac.Equal(got, want) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) requireAllowlist(w http.ResponseWriter, user string) bool {
	if user == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	if _, ok := s.cfg.Allowlist[user]; !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func extractBearer(h string) string {
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func ParseAllowlist(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out[p] = struct{}{}
		}
	}
	return out
}

func (s *Server) ingestWebhook(ctx context.Context, payload WebhookPayload) error {
	events, err := eventsFromPayload(payload)
	if err != nil {
		return err
	}
	var resolved []IncidentState
	if _, err := s.store.Mutate(ctx, func(st *State) error {
		resolved = nil
		for _, ev := range events {
			if !ev.firing {
				if inc, ok := st.Incidents[ev.id.ID]; ok {
					resolved = append(resolved, inc)
				}
				delete(st.Incidents, ev.id.ID)
				continue
			}
			prev := st.Incidents[ev.id.ID]
			prev.Identity = ev.id
			prev.Labels = copyLabels(ev.labels)
			st.Incidents[ev.id.ID] = prev
		}
		return nil
	}); err != nil {
		return err
	}
	for _, inc := range resolved {
		s.markResolved(inc)
	}
	return s.renderLocked(ctx)
}

type hookEvent struct {
	id     Identity
	labels map[string]string
	firing bool
}

func eventsFromPayload(payload WebhookPayload) ([]hookEvent, error) {
	alerts := payload.Alerts
	if len(alerts) == 0 {
		alerts = []WebhookAlert{{Status: payload.Status, Labels: payload.CommonLabels}}
	}
	byID := map[string]hookEvent{}
	var firstErr error
	for _, a := range alerts {
		labels := a.Labels
		if len(labels) == 0 {
			labels = payload.CommonLabels
		}
		id, err := IdentityFromLabels(labels)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		status := a.Status
		if status == "" {
			status = payload.Status
		}
		firing := !strings.EqualFold(status, "resolved")
		prev, ok := byID[id.ID]
		if ok && prev.firing && !firing {
			continue
		}
		byID[id.ID] = hookEvent{id: id, labels: copyLabels(labels), firing: firing}
	}
	if len(byID) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("%w: webhook contained no usable alerts", ErrNoIdentity)
	}
	out := make([]hookEvent, 0, len(byID))
	for _, ev := range byID {
		out = append(out, ev)
	}
	return out, nil
}

func (s *Server) ack(ctx context.Context, req ActionRequest) error {
	if req.IncidentID == "" {
		return fmt.Errorf("%w: incident_id is required", errBadRequest)
	}
	endsAt, err := AckUntil(req.Duration, s.cfg.Now())
	if err != nil {
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	st, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	inc, ok := st.Incidents[req.IncidentID]
	if !ok {
		return fmt.Errorf("%w: %s", errIncidentNotFound, req.IncidentID)
	}
	matchersSets := inc.Identity.SilenceMatcherSets()
	firing := s.currentFiring(ctx, st)
	for _, matchers := range matchersSets {
		if err := ValidateProposedSilence(matchers, firing); err != nil {
			return fmt.Errorf("%w: %v", errBadRequest, err)
		}
	}
	silences, err := s.listSilences(ctx)
	if err != nil {
		return err
	}
	var silenceID string
	for _, matchers := range matchersSets {
		spec := SilenceSpec{
			ID:        equivalentSilenceID(silences, matchers),
			Matchers:  matchers,
			StartsAt:  s.cfg.Now().UTC(),
			EndsAt:    endsAt,
			CreatedBy: "ops-proxy",
			Comment:   fmt.Sprintf("acked by %s via ops-proxy", req.SlackUserID),
		}
		id, err := s.am.CreateSilence(ctx, spec)
		if err != nil {
			return fmt.Errorf("create silence: %w", err)
		}
		silenceID = id
		s.log.WithFields(logrus.Fields{
			"incident_id": req.IncidentID,
			"silence_id":  id,
			"ends_at":     endsAt.Format(time.RFC3339),
			"acked_by":    req.SlackUserID,
			"matchers":    matchers,
		}).Info("created alertmanager silence")
	}
	inc.SilenceID = silenceID
	inc.AckedBy = req.SlackUserID
	inc.EndsAt = endsAt.UTC().Format(time.RFC3339)
	st.Incidents[req.IncidentID] = inc
	if _, err := s.store.Mutate(ctx, func(cur *State) error {
		cur.ensureIncidents()
		cur.Incidents[req.IncidentID] = inc
		cur.BoardTS = st.BoardTS
		if cur.Channel == "" {
			cur.Channel = st.Channel
		}
		return nil
	}); err != nil {
		return err
	}
	return s.renderLocked(ctx)
}

func (s *Server) unack(ctx context.Context, req ActionRequest) error {
	if req.IncidentID == "" {
		return fmt.Errorf("%w: incident_id is required", errBadRequest)
	}
	st, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	inc, ok := st.Incidents[req.IncidentID]
	if !ok {
		return fmt.Errorf("%w: %s", errIncidentNotFound, req.IncidentID)
	}
	silences, err := s.listSilences(ctx)
	if err != nil {
		return err
	}
	expired := map[string]struct{}{}
	if inc.SilenceID != "" {
		if err := s.am.ExpireSilence(ctx, inc.SilenceID); err != nil {
			return fmt.Errorf("expire silence %s: %w", inc.SilenceID, err)
		}
		expired[inc.SilenceID] = struct{}{}
	}
	for _, sil := range silences {
		if !silenceIsActive(sil) || sil.ID == "" {
			continue
		}
		if _, ok := expired[sil.ID]; ok {
			continue
		}
		if silenceMatchesIncident(sil, inc) {
			if err := s.am.ExpireSilence(ctx, sil.ID); err != nil {
				return fmt.Errorf("expire silence %s: %w", sil.ID, err)
			}
			expired[sil.ID] = struct{}{}
		}
	}
	inc.SilenceID = ""
	inc.AckedBy = ""
	inc.EndsAt = ""
	if _, err := s.store.Mutate(ctx, func(cur *State) error {
		cur.ensureIncidents()
		cur.Incidents[req.IncidentID] = inc
		return nil
	}); err != nil {
		return err
	}
	s.log.WithFields(logrus.Fields{
		"incident_id": req.IncidentID,
		"slack_user":  req.SlackUserID,
	}).Info("expired alertmanager silence")
	return s.renderLocked(ctx)
}

func (s *Server) needsHuman(ctx context.Context, req ActionRequest) error {
	if req.IncidentID == "" {
		return fmt.Errorf("%w: incident_id is required", errBadRequest)
	}
	st, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	inc, ok := st.Incidents[req.IncidentID]
	if !ok {
		return fmt.Errorf("%w: %s", errIncidentNotFound, req.IncidentID)
	}
	inc.NeedsHuman = true
	if _, err := s.store.Mutate(ctx, func(cur *State) error {
		cur.ensureIncidents()
		cur.Incidents[req.IncidentID] = inc
		return nil
	}); err != nil {
		return err
	}
	s.log.WithFields(logrus.Fields{
		"incident_id": req.IncidentID,
		"slack_user":  req.SlackUserID,
	}).Info("marked needs-human")
	return s.renderLocked(ctx)
}

func (inc IncidentState) matchLabels() map[string]string {
	if len(inc.Labels) > 0 {
		return inc.Labels
	}
	return inc.Identity.Labels()
}

func equivalentSilenceID(silences []Silence, matchers []Matcher) string {
	for _, sil := range silences {
		if !silenceIsActive(sil) {
			continue
		}
		if matchersEquivalent(sil.Matchers, matchers) {
			return sil.ID
		}
	}
	return ""
}

func matchersEquivalent(a, b []Matcher) bool {
	if len(a) != len(b) {
		return false
	}
	type key struct {
		name, value string
		regex       bool
		equal       bool
	}
	counts := map[key]int{}
	for _, m := range a {
		counts[key{m.Name, m.Value, m.IsRegex, matcherIsEqual(m)}]++
	}
	for _, m := range b {
		k := key{m.Name, m.Value, m.IsRegex, matcherIsEqual(m)}
		if counts[k] == 0 {
			return false
		}
		counts[k]--
	}
	return true
}

func (s *Server) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	alerts, err := s.am.ListAlerts(ctx)
	if err != nil {
		return fmt.Errorf("reconcile list alerts: %w", err)
	}
	st, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	next := map[string]IncidentState{}
	for _, a := range alerts {
		if !alertIsFiring(a) {
			continue
		}
		id, err := IdentityFromLabels(a.Labels)
		if err != nil {
			s.log.WithError(err).WithField("labels", a.Labels).Error("firing alert has no incident identity")
			continue
		}
		prev := st.Incidents[id.ID]
		prev.Identity = id
		prev.Labels = copyLabels(a.Labels)
		next[id.ID] = prev
	}
	for _, inc := range next {
		if inc.SlackTS == "" {
			s.log.Error("ops-proxy ConfigMap had no Slack timestamps for firing incidents; posting new Slack roots (fail visible)")
			break
		}
	}
	st.Incidents = next
	if _, err := s.store.Mutate(ctx, func(cur *State) error {
		cur.Incidents = next
		cur.BoardTS = st.BoardTS
		cur.Channel = st.Channel
		return nil
	}); err != nil {
		return err
	}
	return s.renderLocked(ctx)
}

func (s *Server) listSilences(ctx context.Context) ([]Silence, error) {
	if s.am == nil {
		return nil, nil
	}
	return s.am.ListSilences(ctx)
}

func (s *Server) currentFiring(ctx context.Context, st State) []FiringIncident {
	seen := map[string]FiringIncident{}
	for id, inc := range st.Incidents {
		seen[id] = FiringIncident{ID: id, Labels: inc.matchLabels()}
	}
	if s.am != nil {
		alerts, err := s.am.ListAlerts(ctx)
		if err != nil {
			s.log.WithError(err).Error("list alerts failed; using ConfigMap incidents for matcher check")
		} else {
			for _, a := range alerts {
				if !alertIsFiring(a) {
					continue
				}
				id, err := IdentityFromLabels(a.Labels)
				if err != nil {
					continue
				}
				seen[id.ID] = FiringIncident{ID: id.ID, Labels: a.Labels}
			}
		}
	}
	out := make([]FiringIncident, 0, len(seen))
	for _, fi := range seen {
		out = append(out, fi)
	}
	return out
}

func (s *Server) matchingSilences(silences []Silence, inc IncidentState) []Silence {
	var out []Silence
	seen := map[string]struct{}{}
	for _, sil := range silences {
		if !silenceIsActive(sil) {
			continue
		}
		if !silenceMatchesIncident(sil, inc) {
			continue
		}
		key := sil.ID
		if key == "" {
			key = fmt.Sprintf("%s-%d", sil.CreatedBy, len(out))
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, sil)
	}
	return out
}

func silenceMatchesIncident(sil Silence, inc IncidentState) bool {
	for _, labels := range inc.muteLabelSets() {
		if MatchersCover(sil.Matchers, labels) {
			return true
		}
	}
	return false
}

func (inc IncidentState) muteLabelSets() []map[string]string {
	sets := inc.Identity.SilenceLabelSets()
	if labels := inc.matchLabels(); len(labels) > 0 {
		sets = append(sets, labels)
	}
	return sets
}

func (s *Server) renderLocked(ctx context.Context) error {
	st, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	st.ensureIncidents()
	silences, silencesErr := s.listSilences(ctx)
	if silencesErr != nil {
		s.log.WithError(silencesErr).Error("list silences failed; treating mute as unknown (cards OPEN, ConfigMap silence cache unchanged)")
	}
	muted := map[string]Silence{}
	for id, inc := range st.Incidents {
		if silencesErr != nil {
			// ACKED iff a matching AM silence exists. List failure is fail-visible: do
			// not copy ConfigMap silence_id into muted.
			continue
		}
		matches := s.matchingSilences(silences, inc)
		if len(matches) == 0 {
			inc.SilenceID = ""
			inc.EndsAt = ""
			inc.AckedBy = ""
			st.Incidents[id] = inc
			continue
		}
		sil := matches[0]
		inc.SilenceID = sil.ID
		if !sil.EndsAt.IsZero() {
			inc.EndsAt = sil.EndsAt.UTC().Format(time.RFC3339)
		}
		muted[id] = sil
		st.Incidents[id] = inc
	}

	channel := s.cfg.SlackChannel
	if st.Channel != "" {
		channel = st.Channel
	}

	for id, inc := range st.Incidents {
		sil, isMuted := muted[id]
		status := statusOf(inc, isMuted)
		until := inc.EndsAt
		if isMuted && !sil.EndsAt.IsZero() {
			until = sil.EndsAt.UTC().Format(time.RFC3339)
		}
		text := fmt.Sprintf("%s: %s", status, inc.Identity.ID)
		ch, ts, err := s.upsertMessage(channel, inc.Channel, inc.SlackTS, text, cardBlocks(inc, status, until, inc.AckedBy))
		if err != nil {
			s.log.WithError(err).WithField("incident_id", id).Error("failed to upsert Slack card")
			continue
		}
		if ch != "" {
			inc.Channel = ch
			if st.Channel == "" {
				st.Channel = ch
			}
			channel = ch
		}
		inc.SlackTS = ts
		st.Incidents[id] = inc
	}

	boardText := boardFallback
	ch, ts, err := s.upsertMessage(channel, st.Channel, st.BoardTS, boardText, boardBlocks(st.Incidents, muted))
	if err != nil {
		s.log.WithError(err).Error("failed to upsert CURRENT INCIDENTS board")
	} else {
		if ch != "" {
			st.Channel = ch
			channel = ch
		}
		st.BoardTS = ts
		if err := s.slack.PinMessage(channel, ts); err != nil {
			s.log.WithError(err).Error("failed to pin CURRENT INCIDENTS board")
		}
	}

	if s.cfg.SetTopic {
		topic := FormatTopic(openNames(st.Incidents, muted))
		if err := s.slack.SetTopic(channel, topic); err != nil {
			s.log.WithError(err).Error("failed to set channel topic")
		}
	}

	_, err = s.store.Mutate(ctx, func(cur *State) error {
		cur.BoardTS = st.BoardTS
		cur.Channel = st.Channel
		cur.Incidents = st.Incidents
		return nil
	})
	return err
}

func (s *Server) upsertMessage(configured, storedChannel, ts, text string, blocks []slack.Block) (string, string, error) {
	channel := storedChannel
	if channel == "" {
		channel = configured
	}
	if ts != "" {
		if err := s.slack.UpdateMessage(channel, ts, text, blocks); err != nil {
			s.log.WithError(err).Error("slack update failed; posting new root (fail visible)")
		} else {
			return channel, ts, nil
		}
	}
	ch, newTS, err := s.slack.PostMessage(channel, text, blocks)
	if err != nil {
		return "", "", err
	}
	if ch == "" {
		ch = channel
	}
	return ch, newTS, nil
}

func (s *Server) markResolved(inc IncidentState) {
	channel := inc.Channel
	if channel == "" {
		channel = s.cfg.SlackChannel
	}
	if inc.SlackTS == "" {
		return
	}
	text := fmt.Sprintf("%s: %s", cardResolved, inc.Identity.ID)
	if err := s.slack.UpdateMessage(channel, inc.SlackTS, text, cardBlocks(inc, cardResolved, "", "")); err != nil {
		s.log.WithError(err).WithField("incident_id", inc.Identity.ID).Error("failed to mark Slack card resolved")
	}
}

func copyLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
