// Package opsalerts derives an operations queue from retained Slack threads.
package opsalerts

import (
	"context"
	"errors"
	"time"

	"github.com/slack-go/slack"
)

// ErrValidation identifies invalid configuration or action input.
var ErrValidation = errors.New("validation failed")

// ErrNotFound identifies a missing retained occurrence.
var ErrNotFound = errors.New("occurrence not found")

// ErrConflict identifies a stale revision or reused request ID.
var ErrConflict = errors.New("action conflict")

// ErrNotReady prevents writes before complete archive recovery.
var ErrNotReady = errors.New("archive coverage incomplete")

// ErrDeliveryUncertain means a write may have reached Slack and must be reconciled.
var ErrDeliveryUncertain = errors.New("delivery uncertain")

// ErrDeliveryFailed means Slack explicitly rejected a write.
var ErrDeliveryFailed = errors.New("delivery failed")

// DefaultHistoryWindow bounds the visible queue when no override is supplied.
const DefaultHistoryWindow = 48 * time.Hour

// Config fixes the archive scope and trusted producer identities.
type Config struct {
	WorkspaceID   string        `json:"workspace_id"`
	ChannelID     string        `json:"channel_id"`
	WorkspaceURL  string        `json:"workspace_url"`
	Cutover       time.Time     `json:"cutover"`
	HistoryWindow time.Duration `json:"-"`
	Writer        Identity      `json:"writer"`
	Sources       []Source      `json:"sources"`
}

// Identity contains Slack-issued author identifiers, never display names.
type Identity struct {
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
	AppID  string `json:"app_id"`
}

// Source recognizes one producer and optionally extracts its stable identifiers.
type Source struct {
	Name              string   `json:"name"`
	Kind              string   `json:"kind"`
	UserIDs           []string `json:"user_ids"`
	BotIDs            []string `json:"bot_ids"`
	AppIDs            []string `json:"app_ids"`
	IdentifierPattern string   `json:"identifier_pattern"`
}

// Archive is the complete paginated Slack boundary; Post must not retry uncertain writes.
type Archive interface {
	History(context.Context, string, time.Time) ([]slack.Message, error)
	Replies(context.Context, string, string) ([]slack.Message, error)
	Post(context.Context, string, string, string, string) (string, error)
	SetReaction(context.Context, string, string, string, bool) error
}

// Clock allows deterministic expiry and action timestamps.
type Clock interface{ Now() time.Time }

// Coverage states whether the queue is safe to mutate.
type Coverage struct {
	Phase    string    `json:"phase"`
	LastSync time.Time `json:"last_sync"`
	Cutover  time.Time `json:"cutover"`
	Error    string    `json:"error"`
}

// Snapshot is an immutable copy of the latest complete scan.
type Snapshot struct {
	Status      Coverage     `json:"status"`
	Occurrences []Occurrence `json:"occurrences"`
}

// OccurrenceIdentity distinguishes repeat firings by parent Slack timestamp.
type OccurrenceIdentity struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`
	Timestamp string `json:"timestamp"`
	Revision  string `json:"revision"`
}

// Message describes the normalized root and its evidence replies.
type Message struct {
	PrometheusAlerts []string  `json:"prometheus_alerts,omitempty"`
	Title            string    `json:"title"`
	Text             string    `json:"text"`
	Source           string    `json:"source"`
	ScheduledTask    string    `json:"scheduled_task,omitempty"`
	Identifiers      []string  `json:"identifiers"`
	ReportedAt       time.Time `json:"reported_at"`
	SlackURL         string    `json:"slack_url"`
	SOPURLs          []string  `json:"sop_urls,omitempty"`
	Replies          []Reply   `json:"replies"`
}

// Reply preserves evidence without creating another occurrence.
type Reply struct {
	Text      string `json:"text"`
	User      string `json:"user"`
	Timestamp string `json:"timestamp"`
	IsChai    bool   `json:"is_chai"`
}

// Classification records both presentation and provenance.
type Classification struct {
	Color    string `json:"color" yaml:"color"`
	Category string `json:"category" yaml:"category"`
	Origin   string `json:"origin" yaml:"-"`
}

// Workflow is local to a single firing and survives snooze expiry.
type Workflow struct {
	AcknowledgedBy string     `json:"acknowledged_by"`
	AcknowledgedAt *time.Time `json:"acknowledged_at"`
	ClosedBy       string     `json:"closed_by"`
	ClosedAt       *time.Time `json:"closed_at"`
}

// Snooze is a durable channel/source-scoped regular expression rule.
type Snooze struct {
	ID      string    `json:"id"`
	Pattern string    `json:"pattern"`
	Source  string    `json:"source"`
	Channel string    `json:"channel"`
	Actor   string    `json:"actor"`
	Until   time.Time `json:"until"`
}

// Investigation is one same-thread Chai question and its delivery/reply state.
type Investigation struct {
	RequestID   string    `json:"request_id"`
	Status      string    `json:"status"`
	Question    string    `json:"question"`
	RequestedBy string    `json:"requested_by"`
	Reply       string    `json:"reply"`
	RequestedAt time.Time `json:"requested_at"`
}

// Occurrence groups source evidence, classification, and operator-owned state.
type Occurrence struct {
	Identity       OccurrenceIdentity `json:"identity"`
	Message        Message            `json:"message"`
	Classification Classification     `json:"classification"`
	State          Workflow           `json:"state"`
	View           string             `json:"view"`
	Snooze         *Snooze            `json:"snooze"`
	Investigation  *Investigation     `json:"investigation"`
}

// ActionRequest uses a stable request ID and optimistic revision for safe retries.
type ActionRequest struct {
	OccurrenceID     string `json:"occurrence_id"`
	Kind             string `json:"kind"`
	ExpectedRevision string `json:"expected_revision"`
	RequestID        string `json:"request_id"`
	Pattern          string `json:"pattern"`
	Question         string `json:"question"`
}
