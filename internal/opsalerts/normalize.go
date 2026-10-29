package opsalerts

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// messageAppID reads the SDK's app identity without relying on a bot name.
func messageAppID(m slack.Message) string {
	if m.BotProfile != nil {
		return m.BotProfile.AppID
	}
	return ""
}

// matchesIdentity requires the writer's auth.test-verified user or bot ID.
func matchesIdentity(m slack.Message, identity Identity) bool {
	return identity.UserID != "" && identity.UserID == m.User || identity.BotID != "" && identity.BotID == m.BotID
}

// matchesSource never matches an empty author field against a configured blank.
func matchesSource(m slack.Message, source Source) bool {
	return m.User != "" && slices.Contains(source.UserIDs, m.User) || m.BotID != "" && slices.Contains(source.BotIDs, m.BotID) || messageAppID(m) != "" && slices.Contains(source.AppIDs, messageAppID(m))
}

// source identifies producers only from explicitly configured identities.
func (s *Service) source(m slack.Message) (*Source, error) {
	var found *Source
	for i := range s.cfg.Sources {
		if !matchesSource(m, s.cfg.Sources[i]) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("ambiguous trusted source at %s", m.Timestamp)
		}
		found = &s.cfg.Sources[i]
	}
	return found, nil
}

// flatten combines readable message surfaces and ignores unsupported block fields.
func flatten(m slack.Message) string {
	parts := []string{m.Text}
	for _, a := range m.Attachments {
		parts = append(parts, a.Pretext, a.Title, a.Text)
		if a.Pretext == "" && a.Title == "" && a.Text == "" {
			parts = append(parts, a.Fallback)
		}
		for _, field := range a.Fields {
			parts = append(parts, field.Title+": "+field.Value)
		}
	}
	data, err := json.Marshal(m.Blocks)
	if err == nil {
		var blocks any
		if json.Unmarshal(data, &blocks) == nil {
			parts = append(parts, blockTexts(blocks)...)
		}
	}
	var out []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	return html.UnescapeString(strings.Join(out, "\n"))
}

// blockTexts traverses Slack's heterogeneous blocks in their display order.
func blockTexts(v any) []string {
	var out []string
	switch value := v.(type) {
	case []any:
		for _, item := range value {
			out = append(out, blockTexts(item)...)
		}
	case map[string]any:
		if text, ok := value["text"].(string); ok {
			out = append(out, text)
		}
		for _, key := range []string{"text", "title", "fields", "elements", "accessory", "blocks"} {
			if _, ok := value[key].(string); !ok {
				out = append(out, blockTexts(value[key])...)
			}
		}
	}
	return out
}

// slackTime parses decimal Slack timestamps without floating-point rounding.
func slackTime(ts string) (time.Time, error) {
	parts := strings.Split(ts, ".")
	if len(parts) != 2 || len(parts[1]) != 6 {
		return time.Time{}, fmt.Errorf("invalid Slack timestamp %q", ts)
	}
	for _, digit := range parts[0] + parts[1] {
		if digit < '0' || digit > '9' {
			return time.Time{}, fmt.Errorf("invalid Slack timestamp %q", ts)
		}
	}
	seconds, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || seconds < 0 {
		return time.Time{}, fmt.Errorf("invalid Slack timestamp %q", ts)
	}
	micros, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil || micros < 0 {
		return time.Time{}, fmt.Errorf("invalid Slack timestamp %q", ts)
	}
	return time.Unix(seconds, micros*1000).UTC(), nil
}

// timestampLess compares Slack timestamps numerically, including differing seconds lengths.
func timestampLess(a, b string) bool {
	ta, ea := slackTime(a)
	tb, eb := slackTime(b)
	if ea == nil && eb == nil {
		return ta.Before(tb)
	}
	return a < b
}

// extractIdentifiers recognizes producer formats; arbitrary human text is never parsed.
func extractIdentifiers(source Source, text string) []string {
	patterns := []string{}
	var identifiers []string
	switch source.Kind {
	case "alertmanager":
		patterns = append(patterns, `(?m)(?:^|\n)\s*([A-Za-z_][A-Za-z0-9_:.-]*)\s+\[ops_color=`, `(?i)\balertname\s*[=:]\s*["\x60*]?([A-Za-z_][A-Za-z0-9_:.-]*)`, `(?im)\balert:\s*[*\x60]?([A-Za-z_][A-Za-z0-9_:.-]*)`, `(?m)\[(?:FIRING|RESOLVED)(?::[0-9]+)?\]\s*([A-Za-z_][A-Za-z0-9_:.-]*)`)
	case "prow":
		patterns = append(patterns, `(?i)\bjob(?:_name)?\s*[=:]?\s*[*\x60]?((?:periodic|pull|postsubmit|rehearse)-[A-Za-z0-9_.-]+)`, `\b((?:periodic|pull|postsubmit|rehearse)-[A-Za-z0-9_.-]+)`)
	case "ship":
		component, subcomponent, outage := shipParts(text)
		if outage {
			identifiers = append(identifiers, component+"/"+subcomponent)
		}
	}
	if source.IdentifierPattern != "" {
		patterns = append(patterns, source.IdentifierPattern)
	}
	for _, pattern := range patterns {
		rx, err := regexp.Compile(pattern)
		if err != nil {
			continue
		} // Configured expressions are validated by NewService.
		for _, match := range rx.FindAllStringSubmatch(text, -1) {
			identifier := match[0]
			if len(match) > 1 {
				identifier = match[1]
			}
			if identifier != "" && len(identifier) <= 512 && !slices.Contains(identifiers, identifier) {
				identifiers = append(identifiers, identifier)
			}
		}
	}
	return identifiers
}

// classify uses fixed source policies before valid published marker pairs.
func (s *Service) classify(source Source, m slack.Message, text string, identifiers []string) (Classification, bool) {
	if source.Kind == "chai" {
		if !strings.Contains(text, "[Scheduled task:") && m.Metadata.EventType != "scheduled_task" && m.Metadata.EventPayload["scheduled_task"] == nil {
			return Classification{}, false
		}
		p := s.classifier.Policies["chai_reports"]
		return Classification{Color: p.Color, Category: p.Category, Origin: "policy"}, true
	}
	if source.Kind == "ship" {
		return s.shipClassification(text), true
	}
	rx := regexp.MustCompile(`\[ops_color=([a-z_]+)\s+ops_category=([a-z_]+)\]`)
	var classifications []Classification
	for _, marker := range rx.FindAllStringSubmatch(text, -1) {
		if validColor(marker[1]) && validCategory(marker[2]) {
			classifications = append(classifications, Classification{Color: marker[1], Category: marker[2], Origin: "source"})
		}
	}
	if source.Kind == "alertmanager" {
		for _, identifier := range identifiers {
			supplied := false
			for _, line := range strings.Split(text, "\n") {
				if !slices.Contains(extractIdentifiers(source, line), identifier) {
					continue
				}
				for _, marker := range rx.FindAllStringSubmatch(line, -1) {
					if validColor(marker[1]) && validCategory(marker[2]) {
						supplied = true
					}
				}
			}
			if !supplied {
				classifications = append(classifications, s.classifier.fallback(identifier))
			}
		}
	}
	if len(classifications) == 0 {
		return Classification{Color: "orange", Category: "other", Origin: "default"}, true
	}
	result := classifications[0]
	for _, c := range classifications[1:] {
		if colorRank(c.Color) > colorRank(result.Color) {
			result.Color = c.Color
		}
		if c.Category != result.Category {
			result.Category = "other"
		}
	}
	return result, true
}

// colorRank chooses the severest color in grouped notifications.
func colorRank(color string) int {
	switch color {
	case "red":
		return 3
	case "orange":
		return 2
	case "yellow":
		return 1
	}
	return 0
}

// shipParts recognizes SHIP's structural root header, never description prose.
func shipParts(text string) (string, string, bool) {
	line, _, _ := strings.Cut(text, "\n")
	header, found := strings.CutPrefix(line, "🚨 Outage Detected: ")
	if !found {
		return "", "", false
	}
	component, subcomponent, found := strings.Cut(header, "/")
	component, subcomponent = strings.TrimSpace(component), strings.TrimSpace(subcomponent)
	return component, subcomponent, found && component != "" && subcomponent != ""
}

// shipClassification uses component names from the published SHIP root format.
func (s *Service) shipClassification(text string) Classification {
	componentName, subcomponentName, outage := shipParts(text)
	if !outage {
		return Classification{Color: "orange", Category: "other", Origin: "default"}
	}
	p := s.classifier.Policies["ship_outages"]
	c := Classification{Color: p.Color, Category: p.Category, Origin: "policy"}
	for _, component := range p.Components {
		if componentName == component.Component && (component.Subcomponent == "" || subcomponentName == component.Subcomponent) {
			c.Category = component.Category
			break
		}
	}
	return c
}

// normalize admits only parent producer messages, creating a stable occurrence per timestamp.
func (s *Service) normalize(m slack.Message) (Occurrence, bool, error) {
	if m.SubType == slack.MsgSubTypeMessageChanged && m.SubMessage != nil {
		m = slack.Message{Msg: *m.SubMessage}
	}
	if m.Hidden || m.Timestamp == "" || m.ThreadTimestamp != "" && m.ThreadTimestamp != m.Timestamp {
		return Occurrence{}, false, nil
	}
	if m.SubType != "" && m.SubType != slack.MsgSubTypeBotMessage {
		return Occurrence{}, false, nil
	}
	source, err := s.source(m)
	if err != nil {
		return Occurrence{}, false, err
	}
	if matchesIdentity(m, s.cfg.Writer) && (source == nil || source.Kind != "ship") {
		return Occurrence{}, false, nil
	}
	if source == nil && m.BotID == "" && messageAppID(m) == "" {
		return Occurrence{}, false, nil
	}
	reported, err := slackTime(m.Timestamp)
	if err != nil {
		return Occurrence{}, false, err
	}
	if reported.Before(s.cfg.Cutover) {
		return Occurrence{}, false, nil
	}
	text := flatten(m)
	if text == "" {
		return Occurrence{}, false, nil
	}
	if matchesIdentity(m, s.cfg.Writer) && (strings.Contains(text, strings.TrimSpace(ActionMarker)) || strings.HasPrefix(m.Text, ActionReceiptPrefix) || m.Metadata.EventType == ActionMetadataEvent) {
		return Occurrence{}, false, nil
	}
	src := Source{Name: "unknown", Kind: "unknown"}
	if source != nil {
		src = *source
	}
	identifiers := extractIdentifiers(src, text)
	classification := Classification{Color: "orange", Category: "other", Origin: "default"}
	if source != nil {
		var admitted bool
		classification, admitted = s.classify(src, m, text, identifiers)
		if !admitted {
			return Occurrence{}, false, nil
		}
	}
	title, _, _ := strings.Cut(text, "\n")
	if len(title) > 240 {
		title = string([]rune(title)[:min(240, len([]rune(title)))])
	}
	link := strings.TrimRight(s.cfg.WorkspaceURL, "/") + "/archives/" + url.PathEscape(s.cfg.ChannelID) + "/p" + strings.ReplaceAll(m.Timestamp, ".", "")
	o := Occurrence{Identity: OccurrenceIdentity{ID: s.cfg.ChannelID + ":" + m.Timestamp, Channel: s.cfg.ChannelID, Timestamp: m.Timestamp, Revision: m.Timestamp}, Message: Message{Title: title, Text: text, Source: src.Name, Identifiers: identifiers, ReportedAt: reported, SlackURL: link, Replies: []Reply{}}, Classification: classification, View: "feed"}
	o.Message.SOPURLs = sopURLs(m)
	if source != nil && src.Kind == "alertmanager" {
		o.Message.PrometheusAlerts = prometheusAlerts(m, identifiers)
	}
	if classification.Category == "chai_reports" {
		o.View = "chai_reports"
		if task, ok := m.Metadata.EventPayload["scheduled_task"].(string); ok {
			o.Message.ScheduledTask = strings.TrimSpace(task)
		}
	}
	return o, true, nil
}
