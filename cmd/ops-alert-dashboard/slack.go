package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// slackArchive keeps Slack credentials and pagination outside the workflow model.
type slackArchive struct {
	reader *slack.Client
	writer *slack.Client
}

// History fetches every retained parent, including old parents with newer replies.
func (a *slackArchive) History(ctx context.Context, channel string, since time.Time) ([]slack.Message, error) {
	params := slack.GetConversationHistoryParameters{ChannelID: channel, Oldest: protocolTimestamp(since), Inclusive: true, Limit: 200, IncludeAllMetadata: true}
	var messages []slack.Message
	seen := sets.New[string]()
	for {
		page, err := a.historyPage(ctx, &params)
		if err != nil {
			return nil, fmt.Errorf("reading channel history: %w", err)
		}
		messages = append(messages, page.Messages...)
		next := page.ResponseMetaData.NextCursor
		if next != "" {
			if seen.Has(next) {
				return nil, fmt.Errorf("channel history repeated a cursor")
			}
			seen.Insert(next)
			params.Cursor = next
			continue
		}
		if !page.HasMore {
			return messages, nil
		}
		oldest := pageBoundary(page.Messages, false)
		if oldest == "" || (params.Latest != "" && oldest >= params.Latest) {
			return nil, fmt.Errorf("channel history pagination did not advance")
		}
		params.Cursor, params.Latest, params.Inclusive = "", oldest, false
	}
}

// historyPage retries only rate-limited reads and honors cancellation during backoff.
func (a *slackArchive) historyPage(ctx context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	for {
		page, err := a.reader.GetConversationHistoryContext(ctx, params)
		if err == nil {
			return page, nil
		}
		if err := waitRateLimit(ctx, err); err != nil {
			return nil, err
		}
	}
}

// Replies fetches the complete thread so recovery never loses actions on older alerts.
func (a *slackArchive) Replies(ctx context.Context, channel, rootTS string) ([]slack.Message, error) {
	params := slack.GetConversationRepliesParameters{ChannelID: channel, Timestamp: rootTS, Limit: 200, IncludeAllMetadata: true}
	var messages []slack.Message
	seen := sets.New[string]()
	for {
		page, more, next, err := a.repliesPage(ctx, &params)
		if err != nil {
			return nil, fmt.Errorf("reading alert thread: %w", err)
		}
		messages = append(messages, page...)
		if next != "" {
			if seen.Has(next) {
				return nil, fmt.Errorf("thread history repeated a cursor")
			}
			seen.Insert(next)
			params.Cursor = next
			continue
		}
		if !more {
			return messages, nil
		}
		latest := pageBoundary(page, true)
		if latest == "" || (params.Oldest != "" && latest <= params.Oldest) {
			return nil, fmt.Errorf("thread history pagination did not advance")
		}
		params.Cursor, params.Oldest, params.Inclusive = "", latest, false
	}
}

// repliesPage leaves failed reads uncommitted in the rebuildable cache.
func (a *slackArchive) repliesPage(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	for {
		messages, more, cursor, err := a.reader.GetConversationRepliesContext(ctx, params)
		if err == nil {
			return messages, more, cursor, nil
		}
		if err := waitRateLimit(ctx, err); err != nil {
			return nil, false, "", err
		}
	}
}

// Post makes exactly one write attempt; uncertain delivery must be reconciled first.
func (a *slackArchive) Post(ctx context.Context, channel, rootTS, requestID, text string) (string, error) {
	postedChannel, timestamp, err := a.writer.PostMessageContext(ctx, channel,
		slack.MsgOptionTS(rootTS), slack.MsgOptionText(text, false),
		slack.MsgOptionDisableLinkUnfurl(), slack.MsgOptionDisableMediaUnfurl(),
		slack.MsgOptionParse(false),
		slack.UnsafeMsgOptionEndpoint(slack.APIURL+"chat.postMessage", clientMessageID(requestID).set),
	)
	if err != nil {
		var rejected slack.SlackErrorResponse
		var rateLimit *slack.RateLimitedError
		var status slack.StatusCodeError
		if errors.As(err, &rejected) || errors.As(err, &rateLimit) || (errors.As(err, &status) && status.Code >= 400 && status.Code < 500) {
			return "", errors.Join(opsalerts.ErrDeliveryFailed, err)
		}
		return "", errors.Join(opsalerts.ErrDeliveryUncertain, err)
	}
	if postedChannel != channel || timestamp == "" {
		return "", fmt.Errorf("%w: Slack returned an incomplete write confirmation", opsalerts.ErrDeliveryUncertain)
	}
	return timestamp, nil
}

// SetReaction maintains decorative status without making reactions durable state.
func (a *slackArchive) SetReaction(ctx context.Context, channel, rootTS, emoji string, present bool) error {
	item := slack.ItemRef{Channel: channel, Timestamp: rootTS}
	if present {
		err := a.writer.AddReactionContext(ctx, emoji, item)
		return reactionError(err, "already_reacted")
	}
	err := a.writer.RemoveReactionContext(ctx, emoji, item)
	return reactionError(err, "no_reaction")
}

// reactionError makes repeated decoration updates harmless without hiding failures.
func reactionError(err error, harmless string) error {
	var response slack.SlackErrorResponse
	if err == nil || (errors.As(err, &response) && response.Err == harmless) {
		return nil
	}
	return fmt.Errorf("updating Slack status reaction: %w", err)
}

// clientMessageID supplies the SDK's missing typed client_msg_id option.
type clientMessageID string

// set binds an idempotency key to the pinned SDK's fixed chat.postMessage endpoint.
func (id clientMessageID) set(values url.Values) {
	values.Set("client_msg_id", string(id))
}

// protocolTimestamp formats the Slack wire identifier without exposing epoch API fields.
func protocolTimestamp(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// pageBoundary supports Slack's time pagination when a cursor is absent.
func pageBoundary(messages []slack.Message, newest bool) string {
	boundary := ""
	for _, message := range messages {
		if message.Timestamp == "" {
			continue
		}
		if boundary == "" || (newest && message.Timestamp > boundary) || (!newest && message.Timestamp < boundary) {
			boundary = message.Timestamp
		}
	}
	return boundary
}

// waitRateLimit obeys Retry-After for reads and returns every other failure immediately.
func waitRateLimit(ctx context.Context, err error) error {
	var limited *slack.RateLimitedError
	if !errors.As(err, &limited) {
		return err
	}
	timer := time.NewTimer(max(limited.RetryAfter, time.Second))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// verifySlackIdentity prevents a wrong workspace or personal token from writing actions.
func verifySlackIdentity(ctx context.Context, client *slack.Client, cfg *opsalerts.Config) error {
	identity, err := client.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("checking Slack identity: %w", err)
	}
	if identity.TeamID != cfg.WorkspaceID || identity.UserID != cfg.Writer.UserID || identity.BotID == "" {
		return fmt.Errorf("slack token does not belong to the configured workspace and DPTP-bot user")
	}
	if cfg.Writer.BotID != "" && cfg.Writer.BotID != identity.BotID {
		return fmt.Errorf("slack bot identity does not match configured writer")
	}
	if strings.TrimRight(identity.URL, "/") != strings.TrimRight(cfg.WorkspaceURL, "/") {
		return fmt.Errorf("slack workspace URL does not match configured workspace")
	}
	cfg.Writer.BotID = identity.BotID
	return nil
}

// newSlackClient keeps bounded network timeouts and tokens out of browser configuration.
func newSlackClient(token string) *slack.Client {
	return slack.New(token, slack.OptionHTTPClient(&http.Client{Timeout: 30 * time.Second}))
}
