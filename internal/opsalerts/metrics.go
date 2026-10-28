package opsalerts

import (
	"bytes"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/slack-go/slack"
)

// prometheusAlerts preserves complete rule names independently of classification identifiers.
func prometheusAlerts(m slack.Message, fallback []string) []string {
	var names []string
	surfaces := flatten(m)
	for _, surface := range []any{m.Attachments, m.Blocks} {
		var data bytes.Buffer
		encoder := json.NewEncoder(&data)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(surface); err == nil {
			surfaces += "\n" + data.String()
		}
	}
	links := regexp.MustCompile(`https?://[^\s<>|"\\]+`)
	for _, raw := range links.FindAllString(html.UnescapeString(surfaces), -1) {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		name := strings.TrimSpace(u.Query().Get("alerting-rule-name"))
		if name == "" {
			name = strings.TrimSpace(u.Query().Get("rule-name"))
		}
		if name != "" && len(name) <= 512 && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	titles := regexp.MustCompile(`(?m)\[(?:FIRING|RESOLVED)(?::[0-9]+)?\]\s*([^\n]+)`)
	for _, match := range titles.FindAllStringSubmatch(flatten(m), -1) {
		name, _, _ := strings.Cut(match[1], " (")
		name, _, _ = strings.Cut(name, " [")
		name = strings.TrimSpace(name)
		if name != "" && len(name) <= 512 && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, identifier := range fallback {
		covered := false
		for _, name := range names {
			if name == identifier || strings.HasPrefix(name, identifier+" ") {
				covered = true
				break
			}
		}
		if !covered {
			names = append(names, identifier)
		}
	}
	return names
}
