package opsproxy

import (
	"errors"
	"fmt"
	"strings"
)

const (
	labelAlertName = "alertname"
	labelJobName   = "job_name"
	labelReason    = "reason"
	labelJobTail   = "job_tail"

	alertCIOperatorError      = "high-ci-operator-error-rate"
	alertCIOperatorInfraError = "high-ci-operator-infra-error-rate"
	alertPrivImageBuilding    = "openshift-priv-image-building-jobs-failing"
	alertInfraJobFailures     = "infrastructure-job-failures"

	ciOperatorErrorIDPrefix = "ci-operator-error/"
)

// ErrNoIdentity means the label set cannot be collapsed to a v1 incident id.
var ErrNoIdentity = errors.New("unable to determine incident identity from labels")

// Identity is one Slack card / board row. Matcher fields are what we send to Alertmanager
// (payload alertname + identity label). ID may coalesce multiple alertnames onto one card.
type Identity struct {
	ID           string `json:"id"`
	AlertName    string `json:"alertname"`
	MatcherName  string `json:"matcher_name,omitempty"`
	MatcherValue string `json:"matcher_value,omitempty"`
}

// IdentityFromLabels derives an incident identity. It never groups on scrape label `job` alone.
func IdentityFromLabels(labels map[string]string) (Identity, error) {
	if len(labels) == 0 {
		return Identity{}, fmt.Errorf("%w: no labels", ErrNoIdentity)
	}
	alertname := labels[labelAlertName]
	if alertname == "" {
		return Identity{}, fmt.Errorf("%w: missing alertname", ErrNoIdentity)
	}

	// Class identity: one card for the whole priv-image storm, not FIRING:50.
	if alertname == alertPrivImageBuilding {
		return Identity{
			ID:        alertPrivImageBuilding,
			AlertName: alertname,
		}, nil
	}

	if jobName := labels[labelJobName]; jobName != "" {
		return Identity{
			ID:           alertname + "/" + jobName,
			AlertName:    alertname,
			MatcherName:  labelJobName,
			MatcherValue: jobName,
		}, nil
	}

	if reason := labels[labelReason]; reason != "" {
		id := alertname + "/" + reason
		if alertname == alertCIOperatorError || alertname == alertCIOperatorInfraError {
			id = ciOperatorErrorIDPrefix + reason
		}
		return Identity{
			ID:           id,
			AlertName:    alertname,
			MatcherName:  labelReason,
			MatcherValue: reason,
		}, nil
	}

	if jobTail := labels[labelJobTail]; jobTail != "" {
		return Identity{
			ID:           alertname + "/" + jobTail,
			AlertName:    alertname,
			MatcherName:  labelJobTail,
			MatcherValue: jobTail,
		}, nil
	}

	return Identity{}, fmt.Errorf("%w: alertname=%q has no job_name, reason, or job_tail (scrape job is not identity)", ErrNoIdentity, alertname)
}

// Labels returns the identity matchers as a label map (alertname plus the identity label, if any).
func (id Identity) Labels() map[string]string {
	out := map[string]string{labelAlertName: id.AlertName}
	if id.MatcherName != "" {
		out[id.MatcherName] = id.MatcherValue
	}
	return out
}

// SilenceMatchers is alertname plus the identity label. Class identity is alertname only.
func (id Identity) SilenceMatchers() []Matcher {
	matchers := []Matcher{equalMatcher(labelAlertName, id.AlertName)}
	if id.MatcherName != "" {
		matchers = append(matchers, equalMatcher(id.MatcherName, id.MatcherValue))
	}
	return matchers
}

func (id Identity) coalescesCIOperatorError() bool {
	if id.MatcherName != labelReason || id.MatcherValue == "" {
		return false
	}
	switch id.AlertName {
	case alertCIOperatorError, alertCIOperatorInfraError:
		return true
	}
	return strings.HasPrefix(id.ID, ciOperatorErrorIDPrefix)
}

// SilenceMatcherSets is the Alertmanager silence(s) to POST on ack.
// Coalesced ci-operator error cards mute both alertnames (AM matchers are AND).
func (id Identity) SilenceMatcherSets() [][]Matcher {
	if id.coalescesCIOperatorError() {
		return [][]Matcher{
			{equalMatcher(labelAlertName, alertCIOperatorError), equalMatcher(labelReason, id.MatcherValue)},
			{equalMatcher(labelAlertName, alertCIOperatorInfraError), equalMatcher(labelReason, id.MatcherValue)},
		}
	}
	return [][]Matcher{id.SilenceMatchers()}
}

// SilenceLabelSets is the label maps used to find matching AM silences for this card.
func (id Identity) SilenceLabelSets() []map[string]string {
	sets := id.SilenceMatcherSets()
	out := make([]map[string]string, 0, len(sets))
	for _, matchers := range sets {
		labels := make(map[string]string, len(matchers))
		for _, m := range matchers {
			if m.IsRegex || !matcherIsEqual(m) {
				continue
			}
			labels[m.Name] = m.Value
		}
		out = append(out, labels)
	}
	return out
}

// ShortName is a compact label for the channel topic / board.
func (id Identity) ShortName() string {
	if id.MatcherValue != "" {
		return id.MatcherValue
	}
	if id.ID != "" {
		return id.ID
	}
	return id.AlertName
}
