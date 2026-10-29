package opsalerts

import (
	"fmt"
	"regexp"
)

// PreviewRequest selects current identifier matches without saving a workflow action.
type PreviewRequest struct {
	OccurrenceID string `json:"occurrence_id"`
	Kind         string `json:"kind"`
	Pattern      string `json:"pattern"`
}

// Preview uses the same retained snapshot and RE2 semantics as scoped workflow actions.
func (s *Service) Preview(req PreviewRequest) ([]Occurrence, error) {
	if req.Kind != "snooze" && req.Kind != "acknowledge" && req.Kind != "close" {
		return nil, fmt.Errorf("%w: unsupported preview action", ErrValidation)
	}
	if len(req.Pattern) == 0 || len(req.Pattern) > 512 || len(req.OccurrenceID) == 0 || len(req.OccurrenceID) > 256 {
		return nil, fmt.Errorf("%w: preview identifier and expression are required", ErrValidation)
	}
	rx, err := regexp.Compile(req.Pattern)
	if err != nil || rx.MatchString("") {
		return nil, fmt.Errorf("%w: invalid or empty-matching expression", ErrValidation)
	}
	snapshot := s.Snapshot()
	if snapshot.Status.Phase != "ready" {
		return nil, ErrNotReady
	}
	var selected *Occurrence
	for i := range snapshot.Occurrences {
		if snapshot.Occurrences[i].Identity.ID == req.OccurrenceID {
			selected = &snapshot.Occurrences[i]
			break
		}
	}
	if selected == nil {
		return nil, ErrNotFound
	}
	if selected.View == "chai_reports" || selected.State.ClosedAt != nil {
		return nil, fmt.Errorf("%w: selected occurrence is not open", ErrConflict)
	}
	if !matchesPreviewIdentifier(selected.Message.Identifiers, rx) {
		return nil, fmt.Errorf("%w: expression must match a selected identifier", ErrValidation)
	}
	matched := []Occurrence{}
	for _, o := range snapshot.Occurrences {
		if o.Identity.Channel != selected.Identity.Channel || o.Message.Source != selected.Message.Source || o.View == "chai_reports" || o.State.ClosedAt != nil || !matchesPreviewIdentifier(o.Message.Identifiers, rx) {
			continue
		}
		matched = append(matched, o)
		if len(matched) > 200 {
			return nil, fmt.Errorf("%w: more than 200 occurrences match; narrow the expression", ErrValidation)
		}
	}
	return matched, nil
}

// matchesPreviewIdentifier avoids treating arbitrary message evidence as a rule identifier.
func matchesPreviewIdentifier(identifiers []string, rx *regexp.Regexp) bool {
	for _, identifier := range identifiers {
		if rx.MatchString(identifier) {
			return true
		}
	}
	return false
}
