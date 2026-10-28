package opsalerts

import (
	"bytes"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
)

// SOPRawURL confines document reads to the published release triage SOP directory.
// The returned raw URL omits the fragment, which remains part of the source link.
func SOPRawURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.RawPath != "" {
		return "", false
	}
	path := regexp.MustCompile(`^/openshift/release/blob/(main|master)/docs/dptp-triage-sop/[A-Za-z0-9][A-Za-z0-9._-]*\.md$`)
	if !path.MatchString(u.Path) || strings.Contains(u.Path, "..") {
		return "", false
	}
	return "https://raw.githubusercontent.com/openshift/release/" + strings.TrimPrefix(u.Path, "/openshift/release/blob/"), true
}

// sopURLs finds source links without changing the readable text used for classification.
func sopURLs(m slack.Message) []string {
	parts := []string{m.Text}
	// Marshaling the source surfaces includes link URLs in rich text and attachments.
	for _, surface := range []any{m.Attachments, m.Blocks} {
		var data bytes.Buffer
		encoder := json.NewEncoder(&data)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(surface); err == nil {
			parts = append(parts, data.String())
		}
	}
	seen := sets.New[string]()
	var links []string
	pattern := regexp.MustCompile(`https://[^\s<>|"\\]+`)
	for _, part := range parts {
		for _, candidate := range pattern.FindAllString(html.UnescapeString(part), -1) {
			if _, allowed := SOPRawURL(candidate); allowed && !seen.Has(candidate) {
				seen.Insert(candidate)
				links = append(links, candidate)
			}
		}
	}
	return links
}
