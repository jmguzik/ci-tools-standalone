package opsalerts

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
)

// realClock supplies production time.
type realClock struct{}

// Now implements Clock.
func (realClock) Now() time.Time { return time.Now().UTC() }

// threadCache retains complete threads when history metadata is unchanged.
type threadCache struct {
	Root    slack.Message
	Replies []slack.Message
}

// pendingDelivery prevents blind retries after an ambiguous transport failure.
type pendingDelivery struct {
	Request       ActionRequest
	Actor         string
	Status        string
	Investigation *Investigation
}

// serviceCache is published atomically after a complete replay.
type serviceCache struct {
	Coverage    Coverage
	Threads     map[string]threadCache
	Occurrences map[string]Occurrence
	Records     map[string]actionRecord
	Rules       map[string]Snooze
	Pending     map[string]pendingDelivery
	Decorations map[string]string
}

// Service serializes recovery and mutations while keeping snapshots nonblocking.
type Service struct {
	cfg        Config
	classifier *Classifier
	archive    Archive
	clock      Clock
	serial     chan struct{}
	mu         sync.RWMutex
	cache      serviceCache
}

// NewService validates trusted identities and starts with incomplete coverage.
func NewService(cfg Config, classifier *Classifier, archive Archive, clock Clock) (*Service, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if classifier == nil || archive == nil {
		return nil, fmt.Errorf("%w: classification and archive required", ErrValidation)
	}
	if clock == nil {
		clock = realClock{}
	}
	s := &Service{cfg: cfg, classifier: classifier, archive: archive, clock: clock, serial: make(chan struct{}, 1)}
	s.cache = serviceCache{Coverage: Coverage{Phase: "catching_up", Cutover: cfg.Cutover}, Threads: map[string]threadCache{}, Occurrences: map[string]Occurrence{}, Records: map[string]actionRecord{}, Rules: map[string]Snooze{}, Pending: map[string]pendingDelivery{}}
	return s, nil
}

// validateConfig permits the SHIP writer identity while keeping producers distinct.
func validateConfig(cfg Config) error {
	u, err := url.Parse(cfg.WorkspaceURL)
	if cfg.WorkspaceID == "" || cfg.ChannelID == "" || cfg.Cutover.IsZero() || cfg.Writer.UserID == "" && cfg.Writer.BotID == "" || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: workspace, channel, HTTPS workspace URL, fixed cutover and writer user/bot identity required", ErrValidation)
	}
	writerIDs := sets.New[string]()
	for kind, id := range map[string]string{"user:": cfg.Writer.UserID, "bot:": cfg.Writer.BotID, "app:": cfg.Writer.AppID} {
		if id != "" {
			writerIDs.Insert(kind + id)
		}
	}
	owners := map[string]string{}
	names := sets.New[string]()
	for _, source := range cfg.Sources {
		if source.Name == "" || source.Name == "unknown" {
			return fmt.Errorf("%w: source name required and cannot be unknown", ErrValidation)
		}
		if names.Has(source.Name) {
			return fmt.Errorf("%w: duplicate source name", ErrValidation)
		}
		names.Insert(source.Name)
		switch source.Kind {
		case "alertmanager", "prow", "ship", "chai":
		default:
			return fmt.Errorf("%w: unknown source kind", ErrValidation)
		}
		if len(source.UserIDs)+len(source.BotIDs)+len(source.AppIDs) == 0 {
			return fmt.Errorf("%w: source identity required", ErrValidation)
		}
		for kind, ids := range map[string][]string{"user:": source.UserIDs, "bot:": source.BotIDs, "app:": source.AppIDs} {
			for _, id := range ids {
				if id == "" || strings.TrimSpace(id) != id {
					return fmt.Errorf("%w: blank or padded source identity", ErrValidation)
				}
				if writerIDs.Has(kind+id) && source.Kind != "ship" {
					return fmt.Errorf("%w: identity shared by writer and %s", ErrValidation, source.Name)
				}
				if owner, exists := owners[kind+id]; exists {
					return fmt.Errorf("%w: identity shared by %s and %s", ErrValidation, owner, source.Name)
				}
				owners[kind+id] = source.Name
			}
		}
		if len(source.IdentifierPattern) > 512 {
			return fmt.Errorf("%w: identifier pattern too long", ErrValidation)
		}
		if source.IdentifierPattern != "" {
			rx, err := regexp.Compile(source.IdentifierPattern)
			if err != nil || rx.MatchString("") {
				return fmt.Errorf("%w: invalid or empty-matching identifier pattern", ErrValidation)
			}
		}
	}
	return nil
}

// currentCache borrows an immutable published cache.
func (s *Service) currentCache() serviceCache {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cache
}

// publish swaps complete cache state without holding a lock across I/O.
func (s *Service) publish(cache serviceCache) {
	s.mu.Lock()
	s.cache = cache
	s.mu.Unlock()
}

// degrade preserves the last complete queue while disabling writes.
func (s *Service) degrade(cache serviceCache, err error) error {
	cache.Coverage.Phase = "degraded"
	if cache.Coverage.LastSync.IsZero() {
		cache.Coverage.Phase = "catching_up"
	}
	cache.Coverage.Error = err.Error()
	s.publish(cache)
	return err
}

// Sync scans every retained parent and publishes only after complete thread replay.
func (s *Service) Sync(ctx context.Context) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	previous := s.currentCache()
	catchingUp := previous
	catchingUp.Coverage.Phase = "catching_up"
	s.publish(catchingUp)
	history, err := s.archive.History(ctx, s.cfg.ChannelID, s.cfg.Cutover)
	if err != nil {
		return s.degrade(previous, fmt.Errorf("reading retained history: %w", err))
	}
	threads := map[string]threadCache{}
	for _, root := range history {
		if root.SubType == slack.MsgSubTypeMessageChanged && root.SubMessage != nil {
			root = slack.Message{Msg: *root.SubMessage}
		}
		_, admitted, normalizeErr := s.normalize(root)
		if normalizeErr != nil {
			return s.degrade(previous, normalizeErr)
		}
		if !admitted {
			continue
		}
		if existing, exists := threads[root.Timestamp]; exists {
			if root.Edited == nil || existing.Root.Edited != nil && !timestampLess(existing.Root.Edited.Timestamp, root.Edited.Timestamp) {
				continue
			}
		}
		thread, exists := previous.Threads[root.Timestamp]
		if !exists || thread.Root.ReplyCount != root.ReplyCount || thread.Root.LatestReply != root.LatestReply {
			thread.Replies, err = s.archive.Replies(ctx, s.cfg.ChannelID, root.Timestamp)
			if err != nil {
				return s.degrade(previous, fmt.Errorf("reading thread %s: %w", root.Timestamp, err))
			}
		}
		thread.Root = root
		threads[root.Timestamp] = thread
	}
	cache, err := s.replay(threads, previous.Pending)
	if err != nil {
		return s.degrade(previous, err)
	}
	cache.Coverage = Coverage{Phase: "ready", LastSync: s.clock.Now(), Cutover: s.cfg.Cutover}
	cache.Decorations = previous.Decorations
	s.publish(cache)
	s.publish(s.decorate(ctx, cache))
	return nil
}

// replay treats trusted action records as truth and orders rules across all threads.
func (s *Service) replay(threads map[string]threadCache, pending map[string]pendingDelivery) (serviceCache, error) {
	cache := serviceCache{Threads: threads, Occurrences: map[string]Occurrence{}, Records: map[string]actionRecord{}, Rules: map[string]Snooze{}, Pending: maps.Clone(pending)}
	var records []timedRecord
	investigationTimestamps := map[string]string{}
	for _, thread := range threads {
		o, admitted, err := s.normalize(thread.Root)
		if err != nil {
			return cache, err
		}
		if !admitted {
			continue
		}
		seen := sets.New[string]()
		replies := append([]slack.Message(nil), thread.Replies...)
		sort.Slice(replies, func(i, j int) bool { return timestampLess(replies[i].Timestamp, replies[j].Timestamp) })
		for _, reply := range replies {
			if reply.Timestamp == thread.Root.Timestamp || reply.SubType == slack.MsgSubTypeMessageDeleted {
				continue
			}
			if reply.ThreadTimestamp != "" && reply.ThreadTimestamp != thread.Root.Timestamp {
				return cache, fmt.Errorf("reply outside requested thread")
			}
			if seen.Has(reply.Timestamp) {
				continue
			}
			seen.Insert(reply.Timestamp)
			if _, err := slackTime(reply.Timestamp); err != nil {
				return cache, err
			}
			if matchesIdentity(reply, s.cfg.Writer) {
				record, exists, err := decodeRecord(reply.Text, o.Identity.ID)
				if err != nil {
					return cache, fmt.Errorf("trusted action at %s: %w", reply.Timestamp, err)
				}
				if exists {
					records = append(records, timedRecord{Timestamp: reply.Timestamp, Record: record})
					continue
				}
			}
			source, err := s.source(reply)
			if err != nil {
				return cache, err
			}
			o.Message.Replies = append(o.Message.Replies, Reply{Text: flatten(reply), User: reply.User, Timestamp: reply.Timestamp, IsChai: source != nil && source.Kind == "chai"})
		}
		cache.Occurrences[o.Identity.ID] = o
	}
	sort.Slice(records, func(i, j int) bool { return timestampLess(records[i].Timestamp, records[j].Timestamp) })
	for _, timed := range records {
		r := timed.Record
		if previous, exists := cache.Records[r.Request.RequestID]; exists {
			if !sameRecord(previous, r) {
				return cache, fmt.Errorf("conflicting durable request ID %s", r.Request.RequestID)
			}
			continue
		}
		o := cache.Occurrences[r.Request.OccurrenceID]
		o = enrichInvestigation(o, investigationTimestamps[o.Identity.ID], timed.Timestamp)
		if err := validateTransition(o, r, cache.Rules); err != nil {
			return cache, fmt.Errorf("trusted action at %s: %w", timed.Timestamp, err)
		}
		o.State, o.Investigation, o.Identity.Revision = r.State, r.Investigation, timed.Timestamp
		cache.Occurrences[o.Identity.ID] = o
		cache.Records[r.Request.RequestID] = r
		delete(cache.Pending, r.Request.RequestID)
		if r.Request.Kind == "investigate" {
			investigationTimestamps[o.Identity.ID] = timed.Timestamp
			for requestID, attempt := range cache.Pending {
				if attempt.Request.OccurrenceID == r.Request.OccurrenceID && attempt.Request.Kind == "investigate" {
					delete(cache.Pending, requestID)
				}
			}
		}
		if r.Snooze != nil {
			cache.Rules[r.Snooze.ID] = *r.Snooze
		}
		if r.Unsnooze != "" {
			delete(cache.Rules, r.Unsnooze)
		}
	}
	for id, o := range cache.Occurrences {
		cache.Occurrences[id] = enrichInvestigation(o, investigationTimestamps[o.Identity.ID], "")
	}
	return cache, nil
}

// acquire lets timed-out requests leave the serialized archive queue immediately.
func (s *Service) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.serial <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release returns the archive serialization token.
func (s *Service) release() { <-s.serial }

// enrichInvestigation ties trusted replies to the most recent preceding request.
func enrichInvestigation(o Occurrence, requestTS, beforeTS string) Occurrence {
	if o.Investigation == nil || requestTS == "" {
		return o
	}
	investigation := *o.Investigation
	for _, reply := range o.Message.Replies {
		if reply.IsChai && timestampLess(requestTS, reply.Timestamp) && (beforeTS == "" || timestampLess(reply.Timestamp, beforeTS)) {
			investigation.Status, investigation.Reply = "reply_received", reply.Text
		}
	}
	o.Investigation = &investigation
	return o
}

// Snapshot evaluates rule expiry at read time and returns detached values.
func (s *Service) Snapshot() Snapshot {
	cache := s.currentCache()
	snapshot := Snapshot{Status: cache.Coverage, Occurrences: []Occurrence{}}
	now := s.clock.Now()
	for _, occurrence := range cache.Occurrences {
		snapshot.Occurrences = append(snapshot.Occurrences, effectiveOccurrence(occurrence, cache, now))
	}
	sort.Slice(snapshot.Occurrences, func(i, j int) bool {
		return timestampLess(snapshot.Occurrences[j].Identity.Timestamp, snapshot.Occurrences[i].Identity.Timestamp)
	})
	return snapshot
}

// effectiveOccurrence overlays active rules without changing the workflow history.
func effectiveOccurrence(o Occurrence, cache serviceCache, now time.Time) Occurrence {
	o.Message.Identifiers = append([]string{}, o.Message.Identifiers...)
	o.Message.PrometheusAlerts = append([]string(nil), o.Message.PrometheusAlerts...)
	o.Message.SOPURLs = append([]string(nil), o.Message.SOPURLs...)
	o.Message.Replies = append([]Reply{}, o.Message.Replies...)
	if o.State.AcknowledgedAt != nil {
		value := *o.State.AcknowledgedAt
		o.State.AcknowledgedAt = &value
	}
	if o.State.ClosedAt != nil {
		value := *o.State.ClosedAt
		o.State.ClosedAt = &value
	}
	if o.Investigation != nil {
		value := *o.Investigation
		o.Investigation = &value
	}
	for _, pending := range cache.Pending {
		if pending.Request.OccurrenceID == o.Identity.ID && pending.Investigation != nil {
			value := *pending.Investigation
			o.Investigation = &value
		}
	}
	o.View, o.Snooze = "feed", nil
	if o.State.AcknowledgedAt != nil {
		o.View = "acknowledged"
	}
	for _, rule := range cache.Rules {
		if !now.Before(rule.Until) || rule.Channel != o.Identity.Channel || rule.Source != o.Message.Source {
			continue
		}
		rx, err := regexp.Compile(rule.Pattern)
		if err != nil {
			continue
		} // Durable rules were validated during replay.
		for _, identifier := range o.Message.Identifiers {
			if rx.MatchString(identifier) && (o.Snooze == nil || rule.ID > o.Snooze.ID) {
				value := rule
				o.Snooze = &value
				o.View = "snoozed"
			}
		}
	}
	if o.State.ClosedAt != nil {
		o.View = "closed"
	}
	if o.Classification.Category == "chai_reports" {
		o.View = "chai_reports"
	}
	return o
}

// decorate reconciles only this writer's optional icons, never durable state.
func (s *Service) decorate(ctx context.Context, cache serviceCache) serviceCache {
	decorations := maps.Clone(cache.Decorations)
	if decorations == nil {
		decorations = map[string]string{}
	}
	for id, occurrence := range cache.Occurrences {
		o := effectiveOccurrence(occurrence, cache, s.clock.Now())
		if o.View == "chai_reports" {
			continue
		}
		desired := ""
		switch o.View {
		case "acknowledged":
			desired = "eyes"
		case "snoozed":
			desired = "zzz"
		case "closed":
			desired = "white_check_mark"
		}
		previous, known := decorations[id]
		if known && previous == desired {
			continue
		}
		complete := true
		for _, emoji := range []string{"eyes", "zzz", "white_check_mark"} {
			if known && emoji != previous && emoji != desired {
				continue
			}
			if err := s.archive.SetReaction(ctx, o.Identity.Channel, o.Identity.Timestamp, emoji, emoji == desired); err != nil {
				complete = false
				logrus.WithError(err).WithFields(logrus.Fields{"channel": o.Identity.Channel, "timestamp": o.Identity.Timestamp, "emoji": emoji, "present": emoji == desired}).Warn("could not reconcile dashboard reaction")
			}
		}
		if complete {
			decorations[id] = desired
		}
	}
	cache.Decorations = decorations
	return cache
}
