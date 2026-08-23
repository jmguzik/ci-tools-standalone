package opsproxy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

func boolPtr(v bool) *bool { return &v }

func equalMatcher(name, value string) Matcher {
	return Matcher{Name: name, Value: value, IsRegex: false, IsEqual: boolPtr(true)}
}

func matcherIsEqual(m Matcher) bool {
	if m.IsEqual == nil {
		return true
	}
	return *m.IsEqual
}

// MatchersCover reports whether every matcher matches the given labels.
func MatchersCover(matchers []Matcher, labels map[string]string) bool {
	if len(matchers) == 0 || labels == nil {
		return false
	}
	for _, m := range matchers {
		val, ok := labels[m.Name]
		if !ok {
			return false
		}
		if m.IsRegex {
			re, err := regexp.Compile("^(?:" + m.Value + ")$")
			if err != nil {
				return false
			}
			matched := re.MatchString(val)
			if matcherIsEqual(m) {
				if !matched {
					return false
				}
			} else if matched {
				return false
			}
			continue
		}
		equal := val == m.Value
		if matcherIsEqual(m) {
			if !equal {
				return false
			}
		} else if equal {
			return false
		}
	}
	return true
}

// FiringIncident is an incident currently considered firing, used to refuse overly broad silences.
type FiringIncident struct {
	ID     string
	Labels map[string]string
}

// ValidateProposedSilence refuses severity-only matchers, infrastructure-job-failures without
// job_name, and matchers that would cover more than one currently firing incident.
func ValidateProposedSilence(matchers []Matcher, firing []FiringIncident) error {
	if len(matchers) == 0 {
		return fmt.Errorf("refusing empty silence matchers")
	}
	if isSeverityOnly(matchers) {
		return fmt.Errorf("refusing severity-only silence")
	}
	if isBroadInfraJobFailures(matchers) {
		return fmt.Errorf("refusing alertname=%s without job_name", alertInfraJobFailures)
	}
	covered := map[string]struct{}{}
	for _, fi := range firing {
		if MatchersCover(matchers, fi.Labels) {
			covered[fi.ID] = struct{}{}
		}
	}
	if len(covered) > 1 {
		ids := make([]string, 0, len(covered))
		for id := range covered {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return fmt.Errorf("refusing silence that would cover %d currently firing incidents: %s", len(covered), strings.Join(ids, ", "))
	}
	return nil
}

func isSeverityOnly(matchers []Matcher) bool {
	if len(matchers) == 0 {
		return true
	}
	for _, m := range matchers {
		if m.Name != "severity" {
			return false
		}
	}
	return true
}

func isBroadInfraJobFailures(matchers []Matcher) bool {
	hasInfra := false
	hasJobName := false
	for _, m := range matchers {
		if m.Name == labelAlertName && !m.IsRegex && matcherIsEqual(m) && m.Value == alertInfraJobFailures {
			hasInfra = true
		}
		if m.Name == labelJobName {
			hasJobName = true
		}
	}
	return hasInfra && !hasJobName
}
