package opsalerts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
)

// ThreadReplyRequest targets only a retained alert; reconciliation never initiates delivery.
type ThreadReplyRequest struct {
	OccurrenceID  string `json:"occurrence_id"`
	RequestID     string `json:"request_id"`
	Text          string `json:"text"`
	ReconcileOnly bool   `json:"reconcile_only,omitempty"`
}

// ThreadPerson identifies the actual Slack sender, not the operator using the dashboard.
type ThreadPerson struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	IsBot     bool   `json:"is_bot"`
}

// ThreadMessage contains detached, allowlisted Slack presentation data.
type ThreadMessage struct {
	Timestamp   string               `json:"timestamp"`
	Text        string               `json:"text"`
	Author      ThreadPerson         `json:"author"`
	Blocks      []map[string]any     `json:"blocks"`
	Attachments []map[string]any     `json:"attachments"`
	Reactions   []slack.ItemReaction `json:"reactions"`
	Edited      bool                 `json:"edited"`
	Actor       string               `json:"actor"`
	RequestID   string               `json:"request_id"`
	Unsupported []string             `json:"unsupported"`
}

// ThreadReplyConfirmation proves one uniquely accepted submission independently of the visible page.
type ThreadReplyConfirmation struct {
	OccurrenceID string `json:"occurrence_id"`
	RequestID    string `json:"request_id"`
	Actor        string `json:"actor"`
}

// AlertThread is one cache-only page of an original alert discussion.
type AlertThread struct {
	OccurrenceID string                   `json:"occurrence_id"`
	ChannelID    string                   `json:"channel_id"`
	RootTS       string                   `json:"root_ts"`
	SlackURL     string                   `json:"slack_url"`
	FetchedAt    time.Time                `json:"fetched_at"`
	Root         ThreadMessage            `json:"root"`
	Messages     []ThreadMessage          `json:"messages"`
	NextCursor   string                   `json:"next_cursor"`
	TotalReplies int                      `json:"total_replies"`
	People       map[string]ThreadPerson  `json:"people"`
	Emojis       map[string]string        `json:"emojis"`
	Warnings     []string                 `json:"warnings"`
	Confirmation *ThreadReplyConfirmation `json:"confirmation,omitempty"`
}

// ThreadDirectory optionally enriches only the loaded page using a shared metadata cache.
type ThreadDirectory interface {
	EnrichThread(context.Context, *AlertThread)
}

// EnrichThread delegates optional presentation metadata without expanding the archive interface.
func (s *Service) EnrichThread(ctx context.Context, thread *AlertThread) {
	if directory, ok := s.archive.(ThreadDirectory); ok {
		directory.EnrichThread(ctx, thread)
	}
}

// SafeThreadImage rejects arbitrary image hosts and credential-bearing URLs without proxying them.
func SafeThreadImage(raw string) string {
	if strings.IndexFunc(raw, unsafeThreadImageRune) != -1 {
		return ""
	}
	if len(raw) <= 65536 && regexp.MustCompile(`^data:image/(png|gif|jpeg|webp);base64,[A-Za-z0-9+/=]+$`).MatchString(raw) {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || strings.Contains(u.Host, ":") || strings.ContainsAny(raw, "\r\n\\") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	for _, family := range []string{"slack-edge.com", "slack-imgs.com", "slack.com"} {
		if host == family || strings.HasSuffix(host, "."+family) {
			return raw
		}
	}
	if host == "secure.gravatar.com" {
		return raw
	}
	return ""
}

// unsafeThreadImageRune matches the browser's control, whitespace and backslash rejection.
func unsafeThreadImageRune(r rune) bool { return r <= ' ' || unicode.IsControl(r) || r == '\\' }

// AlertThread reads retained messages without archive requests or background polling interest.
func (s *Service) AlertThread(occurrenceID, before string) (AlertThread, error) {
	return s.alertThread(s.currentCache(), occurrenceID, before)
}

// alertThread projects messages and their receipt fields from the same published cache snapshot.
func (s *Service) alertThread(cache serviceCache, occurrenceID, before string) (AlertThread, error) {
	if before != "" {
		if _, err := slackTime(before); err != nil {
			return AlertThread{}, fmt.Errorf("%w: invalid thread cursor", ErrValidation)
		}
	}
	o, exists := cache.Occurrences[occurrenceID]
	if !exists || !s.retained(o, s.clock.Now(), 0) {
		return AlertThread{}, ErrNotFound
	}
	thread, exists := cache.Threads[o.Identity.Timestamp]
	if !exists {
		return AlertThread{}, ErrNotReady
	}
	result := AlertThread{OccurrenceID: occurrenceID, ChannelID: s.cfg.ChannelID, RootTS: o.Identity.Timestamp, SlackURL: o.Message.SlackURL, FetchedAt: cache.Coverage.LastSync, Messages: []ThreadMessage{}, People: map[string]ThreadPerson{}, Emojis: map[string]string{}, Warnings: []string{}}
	root := visibleThreadMessage(thread.Root)
	result.Root = s.threadMessage(root, cache.ConversationRecords)
	byTimestamp := map[string]slack.Message{}
	hidden := sets.New[string]()
	for _, raw := range thread.Replies {
		m := visibleThreadMessage(raw)
		if m.SubType == slack.MsgSubTypeMessageDeleted {
			ts := m.DeletedTimestamp
			if ts == "" {
				ts = m.Timestamp
			}
			hidden.Insert(ts)
		} else if m.Hidden {
			hidden.Insert(m.Timestamp)
		}
	}
	for _, raw := range thread.Replies {
		m := visibleThreadMessage(raw)
		if m.Timestamp == result.RootTS || hidden.Has(m.Timestamp) || m.SubType == slack.MsgSubTypeMessageDeleted || m.Timestamp == "" || m.ThreadTimestamp != "" && m.ThreadTimestamp != result.RootTS {
			continue
		}
		if _, err := slackTime(m.Timestamp); err != nil {
			continue
		}
		if previous, exists := byTimestamp[m.Timestamp]; exists && previous.Edited != nil && (m.Edited == nil || !timestampLess(previous.Edited.Timestamp, m.Edited.Timestamp)) {
			continue
		}
		byTimestamp[m.Timestamp] = m
	}
	result.TotalReplies = len(byTimestamp)
	replies := make([]slack.Message, 0, len(byTimestamp))
	for _, m := range byTimestamp {
		if before == "" || timestampLess(m.Timestamp, before) {
			replies = append(replies, m)
		}
	}
	sort.Slice(replies, func(i, j int) bool { return timestampLess(replies[i].Timestamp, replies[j].Timestamp) })
	if len(replies) > 30 {
		replies = replies[len(replies)-30:]
		result.NextCursor = replies[0].Timestamp
	}
	result.People[result.Root.Author.ID] = result.Root.Author
	for _, m := range replies {
		item := s.threadMessage(m, cache.ConversationRecords)
		result.Messages = append(result.Messages, item)
		result.People[item.Author.ID] = item.Author
	}
	return result, nil
}

// ReplyToAlert uses the same durable write path as Chai without mentioning or requiring Chai.
func (s *Service) ReplyToAlert(ctx context.Context, actor string, req ThreadReplyRequest) (AlertThread, error) {
	if !req.ReconcileOnly && strings.Contains(req.Text, strings.TrimSpace(ActionMarker)) {
		return AlertThread{}, fmt.Errorf("%w: reply contains a reserved workflow control string", ErrValidation)
	}
	if req.OccurrenceID == "" || len(req.Text) > 4000 || !utf8.ValidString(req.Text) {
		return AlertThread{}, fmt.Errorf("%w: occurrence and valid UTF-8 reply of at most 4000 bytes required", ErrValidation)
	}
	for _, r := range req.Text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return AlertThread{}, fmt.Errorf("%w: reply contains control characters", ErrValidation)
		}
	}
	request := ConversationRequest{OccurrenceID: req.OccurrenceID, RequestID: req.RequestID, Text: req.Text}
	_, err := s.postConversation(ctx, actor, request, true, req.ReconcileOnly)
	if err != nil {
		return AlertThread{}, err
	}
	return s.confirmedAlertThread(actor, request)
}

// confirmedAlertThread refuses to invent delivery proof if replay changed after the serialized write path.
func (s *Service) confirmedAlertThread(actor string, req ConversationRequest) (AlertThread, error) {
	cache := s.currentCache()
	record, exists := cache.ConversationRecords[req.RequestID]
	occurrence, occurrenceExists := cache.Occurrences[req.OccurrenceID]
	if !exists || !occurrenceExists || !matchesConversationRequest(record, actor, req, true) || record.Root != occurrence.Identity.Timestamp || record.SourceOccurrenceID != req.OccurrenceID {
		return AlertThread{}, ErrDeliveryUncertain
	}
	thread, err := s.alertThread(cache, req.OccurrenceID, "")
	if err != nil {
		return AlertThread{}, ErrDeliveryUncertain
	}
	thread.Confirmation = &ThreadReplyConfirmation{OccurrenceID: record.SourceOccurrenceID, RequestID: record.Request.RequestID, Actor: record.Actor}
	return thread, nil
}

// visibleThreadMessage unwraps edits while preserving deletion visibility.
func visibleThreadMessage(m slack.Message) slack.Message {
	if m.SubType == slack.MsgSubTypeMessageChanged && m.SubMessage != nil {
		inner := slack.Message{Msg: *m.SubMessage}
		if inner.ThreadTimestamp == "" {
			inner.ThreadTimestamp = m.ThreadTimestamp
		}
		return inner
	}
	return m
}

// threadMessage detaches Slack objects through JSON before stripping interactive surfaces.
func (s *Service) threadMessage(m slack.Message, records map[string]conversationRecord) ThreadMessage {
	result := ThreadMessage{Timestamp: m.Timestamp, Text: m.Text, Edited: m.Edited != nil, Blocks: []map[string]any{}, Attachments: []map[string]any{}, Reactions: []slack.ItemReaction{}, Unsupported: []string{}}
	data, err := json.Marshal(m)
	if err != nil {
		result.Unsupported = append(result.Unsupported, "message")
		return result
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		result.Unsupported = append(result.Unsupported, "message")
		return result
	}
	result.Author = threadAuthor(m, raw)
	result.Blocks = threadBlocks(raw["blocks"], &result.Unsupported, 0)
	if attachments, ok := raw["attachments"].([]any); ok {
		for _, value := range attachments {
			attachment, ok := value.(map[string]any)
			if !ok {
				continue
			}
			clean := map[string]any{}
			for _, key := range []string{"title", "title_link", "pretext", "text", "fallback", "author_name", "author_link", "footer", "color"} {
				if text, ok := attachment[key].(string); ok {
					clean[key] = text
				}
			}
			for _, key := range []string{"author_icon", "footer_icon", "image_url", "thumb_url"} {
				if text, ok := attachment[key].(string); ok {
					clean[key] = SafeThreadImage(text)
				}
			}
			fields := []map[string]any{}
			if values, ok := attachment["fields"].([]any); ok {
				for _, value := range values {
					if field, ok := value.(map[string]any); ok {
						fields = append(fields, threadKeys(field, "title", "value", "short"))
					}
				}
			}
			clean["fields"] = fields
			clean["blocks"] = threadBlocks(attachment["blocks"], &result.Unsupported, 0)
			for _, key := range []string{"actions", "files", "video_url", "audio_url"} {
				if _, exists := attachment[key]; exists {
					result.Unsupported = append(result.Unsupported, key)
				}
			}
			result.Attachments = append(result.Attachments, clean)
		}
	}
	for _, reaction := range m.Reactions {
		reaction.Users = append([]string{}, reaction.Users...)
		result.Reactions = append(result.Reactions, reaction)
	}
	if _, exists := raw["files"]; exists {
		result.Unsupported = append(result.Unsupported, "files")
	}
	if matchesIdentity(m, s.cfg.Writer) && m.Metadata.EventType == ThreadReplyMetadataEvent {
		if record, err := s.decodeConversation(m); err == nil {
			if accepted, exists := records[record.Request.RequestID]; exists && accepted.Mode == "reply" && sameConversationRecord(accepted, record) {
				result.Text, result.Actor, result.RequestID = record.Request.Text, record.Actor, record.Request.RequestID
			}
		}
	}
	return result
}

// threadAuthor preserves bot profile and username fallbacks when directory scopes are unavailable.
func threadAuthor(m slack.Message, raw map[string]any) ThreadPerson {
	id := m.User
	if id == "" {
		id = m.BotID
	}
	p := ThreadPerson{ID: id, Name: id, IsBot: m.BotID != "" || m.SubType == slack.MsgSubTypeBotMessage}
	if profile, ok := raw["bot_profile"].(map[string]any); ok {
		if name, ok := profile["name"].(string); ok && name != "" {
			p.Name = name
		}
		if icons, ok := profile["icons"].(map[string]any); ok {
			p.AvatarURL = threadIcon(icons)
		}
	}
	if m.Username != "" {
		p.Name = m.Username
	}
	if icons, ok := raw["icons"].(map[string]any); ok {
		if avatar := threadIcon(icons); avatar != "" {
			p.AvatarURL = avatar
		}
	}
	return p
}

// threadIcon prefers an existing small raster avatar over fetching a new image.
func threadIcon(icons map[string]any) string {
	for _, key := range []string{"image_72", "image_48", "image_36", "image_24", "image_32", "image_64", "icon_url"} {
		if value, ok := icons[key].(string); ok && SafeThreadImage(value) != "" {
			return value
		}
	}
	return ""
}

// threadKeys copies only explicitly supported presentation fields from detached JSON.
func threadKeys(raw map[string]any, keys ...string) map[string]any {
	clean := map[string]any{}
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			switch value.(type) {
			case string, bool, float64:
				clean[key] = value
			case map[string]any:
				if key == "style" {
					styles := map[string]any{}
					for _, name := range []string{"bold", "italic", "strike", "code"} {
						if enabled, ok := value.(map[string]any)[name].(bool); ok {
							styles[name] = enabled
						}
					}
					clean[key] = styles
				}
			}
		}
	}
	return clean
}

// threadBlocks whitelists inert blocks and rich text recursively, with a depth bound.
func threadBlocks(value any, unsupported *[]string, depth int) []map[string]any {
	result := []map[string]any{}
	values, ok := value.([]any)
	if !ok || depth > 8 {
		if ok && len(values) > 0 && depth > 8 {
			*unsupported = append(*unsupported, "deeply nested blocks")
		}
		return result
	}
	for _, value := range values {
		raw, ok := value.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := raw["type"].(string)
		var clean map[string]any
		switch kind {
		case "section", "header", "context", "divider", "image", "rich_text", "rich_text_section", "rich_text_list", "rich_text_quote", "rich_text_preformatted":
			clean = threadKeys(raw, "type", "style", "indent", "offset", "border", "alt_text")
			for _, key := range []string{"text", "title"} {
				if obj, ok := raw[key].(map[string]any); ok {
					clean[key] = threadKeys(obj, "type", "text", "emoji", "verbatim")
				}
			}
			if image, ok := raw["image_url"].(string); ok {
				clean["image_url"] = SafeThreadImage(image)
			}
			for _, key := range []string{"elements", "fields"} {
				if nested, ok := raw[key]; ok {
					clean[key] = threadBlocks(nested, unsupported, depth+1)
				}
			}
			if accessory, ok := raw["accessory"].(map[string]any); ok {
				if accessory["type"] == "image" {
					image, _ := accessory["image_url"].(string)
					alt, _ := accessory["alt_text"].(string)
					clean["accessory"] = map[string]any{"type": "image", "image_url": SafeThreadImage(image), "alt_text": alt}
				} else {
					*unsupported = append(*unsupported, "accessory")
				}
			}
			if _, exists := raw["slack_file"]; exists {
				*unsupported = append(*unsupported, "slack_file")
			}
		case "color":
			clean = threadKeys(raw, "type", "value")
		case "plain_text", "mrkdwn", "text", "link", "emoji", "user", "channel", "broadcast", "date", "usergroup":
			clean = threadKeys(raw, "type", "text", "url", "name", "user_id", "channel_id", "usergroup_id", "range", "timestamp", "format", "fallback", "style", "unicode")
		default:
			if kind != "" {
				*unsupported = append(*unsupported, kind)
			}
			continue
		}
		result = append(result, clean)
	}
	return result
}
