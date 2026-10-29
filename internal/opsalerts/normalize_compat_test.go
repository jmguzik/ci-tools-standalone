package opsalerts

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/slack-go/slack"
)

// TestProducerCompatibility keeps historical formats bounded by trusted headers.
func TestProducerCompatibility(t *testing.T) {
	cases := []struct {
		name, bot, user, text string
		blockHeading          string
		want                  bool
		classification        Classification
		identifiers           []string
	}{
		{name: "wire outage alias", bot: "B_SHIP", text: ":rotating_light: Outage Detected: Build Farm/Build03\n[ops_color=red ops_category=ci_platform]", want: true, classification: Classification{Color: "yellow", Category: "cluster_fleet", Origin: "policy"}, identifiers: []string{"Build Farm/Build03"}},
		{name: "wire CI config alias", bot: "B_SHIP", text: ":rotating_light: Outage Detected: Downstream CI/CI-Config", want: true, classification: Classification{Color: "yellow", Category: "repo_automation", Origin: "policy"}, identifiers: []string{"Downstream CI/CI-Config"}},
		{name: "outage body mention", bot: "B_SHIP", text: "An update\n:rotating_light: Outage Detected: Build Farm/Build03", want: true, classification: Classification{Color: "orange", Category: "other", Origin: "default"}},
		{name: "outage prefixed comment", bot: "B_SHIP", text: "Comment: :rotating_light: Outage Detected: Build Farm/Build03", want: true, classification: Classification{Color: "orange", Category: "other", Origin: "default"}},
		{name: "outage missing subcomponent", bot: "B_SHIP", text: ":rotating_light: Outage Detected: Build Farm/", want: true, classification: Classification{Color: "orange", Category: "other", Origin: "default"}},
		{name: "branch report identifier", bot: "B_PROW", text: "Job branch-ci-openshift-release-master-report failed [ops_color=yellow ops_category=ci_platform]", want: true, classification: Classification{Color: "yellow", Category: "ci_platform", Origin: "source"}, identifiers: []string{"branch-ci-openshift-release-master-report"}},
		{name: "legacy cleanup job", bot: "B_PROW", text: "Job periodic-ci-cleanup-aws failed", want: true, classification: Classification{Color: "orange", Category: "cloud_cleanup", Origin: "fallback"}, identifiers: []string{"periodic-ci-cleanup-aws"}},
		{name: "grouped legacy cleanup and unknown", bot: "B_PROW", text: "Job periodic-ci-cleanup-aws failed\nJob periodic-ci-unclassified failed", want: true, classification: Classification{Color: "orange", Category: "other", Origin: "fallback"}, identifiers: []string{"periodic-ci-cleanup-aws", "periodic-ci-unclassified"}},
		{name: "cleanup marker priority", bot: "B_PROW", text: "Job periodic-ci-cleanup-aws failed [ops_color=red ops_category=ci_platform]", want: true, classification: Classification{Color: "red", Category: "ci_platform", Origin: "source"}, identifiers: []string{"periodic-ci-cleanup-aws"}},
		{name: "cleanup separate line marker priority", bot: "B_PROW", text: "Job periodic-ci-cleanup-aws failed\n[ops_color=red ops_category=ci_platform]", want: true, classification: Classification{Color: "red", Category: "ci_platform", Origin: "source"}, identifiers: []string{"periodic-ci-cleanup-aws"}},
		{name: "platform glob excludes Prow", bot: "B_PROW", text: "Job periodic-ci-etcd-test failed", want: true, classification: Classification{Color: "orange", Category: "other", Origin: "default"}, identifiers: []string{"periodic-ci-etcd-test"}},
		{name: "DPTP firing wrapper", bot: "B_SHIP", text: "[FIRING] high-ci-operator-error-rate: CI operator error rate", want: true, classification: Classification{Color: "yellow", Category: "ci_platform", Origin: "fallback"}, identifiers: []string{"high-ci-operator-error-rate"}},
		{name: "DPTP grouped ghproxy wrapper", bot: "B_SHIP", text: "[FIRING] ghproxy-too-many-pending-alerts ghproxy 2 alerts in this notification", want: true, classification: Classification{Color: "yellow", Category: "ci_platform", Origin: "fallback"}, identifiers: []string{"ghproxy-too-many-pending-alerts"}},
		{name: "DPTP no longer firing wrapper", bot: "B_SHIP", text: "[NO LONGER FIRING IN ALERTMANAGER] high-ci-operator-infra-error-rate", want: true, classification: Classification{Color: "orange", Category: "ci_platform", Origin: "fallback"}, identifiers: []string{"high-ci-operator-infra-error-rate"}},
		{name: "DPTP wrapper marker priority", bot: "B_SHIP", text: "[FIRING] high-ci-operator-error-rate\n[ops_color=red ops_category=repo_automation]", want: true, classification: Classification{Color: "red", Category: "repo_automation", Origin: "source"}, identifiers: []string{"high-ci-operator-error-rate"}},
		{name: "DPTP invalid marker fallback", bot: "B_SHIP", text: "[FIRING] high-ci-operator-error-rate\n[ops_color=green ops_category=chai_reports]", want: true, classification: Classification{Color: "yellow", Category: "ci_platform", Origin: "fallback"}, identifiers: []string{"high-ci-operator-error-rate"}},
		{name: "Repo brancher summary", bot: "B_SHIP", text: "Repo brancher has 1 active fast-forward failure(s). See details below.", want: true, classification: Classification{Color: "orange", Category: "repo_automation", Origin: "fallback"}, identifiers: []string{"repo-brancher-fast-forward-failures"}},
		{name: "Repo brancher block heading", bot: "B_SHIP", blockHeading: "Repo brancher fast-forward failures", want: true, classification: Classification{Color: "orange", Category: "repo_automation", Origin: "fallback"}, identifiers: []string{"repo-brancher-fast-forward-failures"}},
		{name: "Repo brancher marked heading", bot: "B_SHIP", text: "Repo brancher fast-forward failures\n[ops_color=yellow ops_category=ci_platform]", want: true, classification: Classification{Color: "yellow", Category: "ci_platform", Origin: "source"}, identifiers: []string{"repo-brancher-fast-forward-failures"}},
		{name: "unknown DPTP wrapper", bot: "B_SHIP", text: "[FIRING] unknown-alert", want: true, classification: Classification{Color: "orange", Category: "other", Origin: "default"}, identifiers: []string{"unknown-alert"}},
		{name: "platform glob excludes DPTP", bot: "B_SHIP", text: "[FIRING] etcdDown", want: true, classification: Classification{Color: "orange", Category: "other", Origin: "default"}, identifiers: []string{"etcdDown"}},
		{name: "body wrapper mention", bot: "B_SHIP", text: "An update\n[FIRING] high-ci-operator-error-rate", want: true, classification: Classification{Color: "orange", Category: "other", Origin: "default"}},
		{name: "body brancher mention", bot: "B_SHIP", text: "An update\nRepo brancher fast-forward failures", want: true, classification: Classification{Color: "orange", Category: "other", Origin: "default"}},
		{name: "unknown bot spoof", bot: "B_UNKNOWN", text: "[FIRING] high-ci-operator-error-rate [ops_color=red ops_category=ci_platform]", want: true, classification: Classification{Color: "orange", Category: "other", Origin: "default"}},
		{name: "human wrapper spoof", user: "U_HUMAN", text: "[FIRING] high-ci-operator-error-rate [ops_color=red ops_category=ci_platform]"},
		{name: "human outage spoof", user: "U_HUMAN", text: ":rotating_light: Outage Detected: Build Farm/Build03"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, archive, _ := fixture(t)
			classifier, err := LoadClassification([]byte(testClassification + `
- {alertname: high-ci-operator-error-rate, color: yellow, category: ci_platform}
- {alertname: ghproxy-too-many-pending-alerts, color: yellow, category: ci_platform}
- {alertname: high-ci-operator-infra-error-rate, color: orange, category: ci_platform}
- {alertname: repo-brancher-fast-forward-failures, color: orange, category: repo_automation}
- {alertname_glob: 'periodic-ci-cleanup-*', color: orange, category: cloud_cleanup}
`))
			if err != nil {
				t.Fatal(err)
			}
			s.classifier = classifier
			message := slack.Message{Msg: slack.Msg{Timestamp: archive.roots[0].Timestamp, BotID: tc.bot, User: tc.user, Text: tc.text}}
			if tc.blockHeading != "" {
				message.Blocks = slack.Blocks{BlockSet: []slack.Block{slack.NewHeaderBlock(slack.NewTextBlockObject("plain_text", tc.blockHeading, false, false))}}
			}
			got, admitted, err := s.normalize(message)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, admitted); diff != "" {
				t.Fatalf("admission (-want +got):\n%s", diff)
			}
			if !admitted {
				return
			}
			if diff := cmp.Diff(tc.classification, got.Classification); diff != "" {
				t.Errorf("classification (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.identifiers, got.Message.Identifiers); diff != "" {
				t.Errorf("identifiers (-want +got):\n%s", diff)
			}
		})
	}
}
