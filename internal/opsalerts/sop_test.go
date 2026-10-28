package opsalerts

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/slack-go/slack"
)

// TestSOPRawURL checks the fixed repository boundary before any network operation.
func TestSOPRawURL(t *testing.T) {
	const base = "https://github.com/openshift/release/blob/main/docs/dptp-triage-sop/"
	cases := []struct{ name, url, want string }{
		{name: "main", url: base + "qci-pruner-timeout.md", want: "https://raw.githubusercontent.com/openshift/release/main/docs/dptp-triage-sop/qci-pruner-timeout.md"},
		{name: "master and section", url: "https://github.com/openshift/release/blob/master/docs/dptp-triage-sop/infrastructure-jobs.md#periodic-ci-secret-bootstrap", want: "https://raw.githubusercontent.com/openshift/release/master/docs/dptp-triage-sop/infrastructure-jobs.md"},
		{name: "other repository", url: "https://github.com/other/release/blob/main/docs/dptp-triage-sop/a.md"},
		{name: "other host", url: "https://example.test/openshift/release/blob/main/docs/dptp-triage-sop/a.md"},
		{name: "http", url: "http://github.com/openshift/release/blob/main/docs/dptp-triage-sop/a.md"},
		{name: "userinfo", url: "https://user@github.com/openshift/release/blob/main/docs/dptp-triage-sop/a.md"},
		{name: "port", url: "https://github.com:443/openshift/release/blob/main/docs/dptp-triage-sop/a.md"},
		{name: "query", url: base + "a.md?token=secret"},
		{name: "empty query", url: base + "a.md?"},
		{name: "traversal", url: base + "../a.md"},
		{name: "encoded traversal", url: base + "%2e%2e/a.md"},
		{name: "encoded slash", url: base + "a%2fb.md"},
		{name: "encoded backslash", url: base + "a%5cb.md"},
		{name: "backslash", url: base + `a\b.md`},
		{name: "commit", url: "https://github.com/openshift/release/blob/123abc/docs/dptp-triage-sop/a.md"},
		{name: "other file", url: base + "a.txt"},
		{name: "malformed", url: "https://github.com/%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SOPRawURL(tc.url)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
			if ok != (tc.want != "") {
				t.Errorf("allowed = %t", ok)
			}
		})
	}
}

// TestSOPURLs covers the source message surfaces without altering report text.
func TestSOPURLs(t *testing.T) {
	const first = "https://github.com/openshift/release/blob/main/docs/dptp-triage-sop/qci-pruner-timeout.md"
	const section = "https://github.com/openshift/release/blob/main/docs/dptp-triage-sop/infrastructure-jobs.md#periodic-ci-secret-bootstrap"
	cases := []struct {
		name, message string
		want          []string
	}{
		{name: "Slack SOP label", message: `{"text":"<` + first + `|SOP>"}`, want: []string{first}},
		{name: "annotation", message: `{"text":"runbook_url: ` + section + `"}`, want: []string{section}},
		{name: "attachments and dedupe", message: `{"text":"<` + first + `|SOP>","attachments":[{"text":"<` + first + `|SOP>","fields":[{"title":"runbook_url","value":"<` + section + `|SOP>"}]}]}`, want: []string{first, section}},
		{name: "rich text URL", message: `{"blocks":[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"link","url":"` + section + `","text":"SOP"}]}]}]}`, want: []string{section}},
		{name: "attachment title link", message: `{"attachments":[{"title":"SOP","title_link":"` + first + `"}]}`, want: []string{first}},
		{name: "fragment punctuation", message: `{"attachments":[{"title_link":"` + first + `#a&b"}]}`, want: []string{first + "#a&b"}},
		{name: "section mrkdwn", message: `{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"<` + first + `|SOP>"}}]}`, want: []string{first}},
		{name: "unrelated and reply", message: `{"text":"https://example.test/sop.md","replies":[{"text":"` + first + `"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m slack.Message
			if err := json.Unmarshal([]byte(tc.message), &m); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, sopURLs(m)); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSOPSnapshotIsolation prevents callers from changing retained source membership.
func TestSOPSnapshotIsolation(t *testing.T) {
	cases := []struct {
		name string
		urls []string
	}{{name: "linked", urls: []string{"source"}}, {name: "absent"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := append([]string(nil), tc.urls...)
			original := Occurrence{Message: Message{SOPURLs: tc.urls}}
			copy := effectiveOccurrence(original, serviceCache{}, time.Time{})
			if len(copy.Message.SOPURLs) > 0 {
				copy.Message.SOPURLs[0] = "changed"
			}
			if diff := cmp.Diff(want, original.Message.SOPURLs); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNormalizeSOP retains source links while preserving classification and workflow.
func TestNormalizeSOP(t *testing.T) {
	const link = "https://github.com/openshift/release/blob/main/docs/dptp-triage-sop/etcd-slowness.md"
	cases := []struct {
		name, link string
		want       []string
	}{{name: "source SOP", link: " <" + link + "|SOP>", want: []string{link}}, {name: "no SOP"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, _ := fixture(t)
			root := archive.roots[0]
			root.Text += tc.link
			got, admitted, err := service.normalize(root)
			if err != nil || !admitted {
				t.Fatalf("normalize = %t, %v", admitted, err)
			}
			if diff := cmp.Diff(tc.want, got.Message.SOPURLs); diff != "" {
				t.Errorf("unexpected links (-want +got):\n%s", diff)
			}
			if got.View != "feed" || got.Classification.Color != "red" || got.Classification.Category != "cluster_fleet" {
				t.Errorf("SOP changed alert classification or workflow: %+v", got)
			}
		})
	}
}
