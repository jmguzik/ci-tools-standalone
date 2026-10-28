package opsalerts

import (
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v2"
)

// classificationRule matches a whole alert name in file order.
type classificationRule struct {
	Alertname string `yaml:"alertname"`
	Glob      string `yaml:"alertname_glob"`
	Color     string `yaml:"color"`
	Category  string `yaml:"category"`
}

// sourcePolicy defines fixed classifications before producer markers.
type sourcePolicy struct {
	Kind       string              `yaml:"kind"`
	Color      string              `yaml:"color"`
	Category   string              `yaml:"category"`
	View       string              `yaml:"view"`
	Components []componentCategory `yaml:"component_categories"`
}

// componentCategory specializes SHIP outage categories.
type componentCategory struct {
	Component    string `yaml:"component"`
	Subcomponent string `yaml:"subcomponent"`
	Category     string `yaml:"category"`
}

// Classifier is the validated release-owned classification file.
type Classifier struct {
	Version  int                     `yaml:"version"`
	Defaults Classification          `yaml:"defaults"`
	Policies map[string]sourcePolicy `yaml:"source_policies"`
	Platform []classificationRule    `yaml:"platform_alerts"`
	Legacy   []classificationRule    `yaml:"legacy_alerts"`
}

// LoadClassification accepts the release v1 file without copying its catalog.
func LoadClassification(data []byte) (*Classifier, error) {
	var c Classifier
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, fmt.Errorf("%w: classification: %v", ErrValidation, err)
	}
	if c.Version != 1 || c.Defaults.Color != "orange" || c.Defaults.Category != "other" {
		return nil, fmt.Errorf("%w: version 1 and orange/other defaults required", ErrValidation)
	}
	for name, p := range c.Policies {
		if name != "chai_reports" && name != "ship_outages" {
			return nil, fmt.Errorf("%w: unknown source policy %q", ErrValidation, name)
		}
		if name == "chai_reports" && (p.Kind != "report" || p.Color != "green" || p.Category != "chai_reports" || p.View != "chai_reports" || len(p.Components) != 0) {
			return nil, fmt.Errorf("%w: invalid chai_reports policy", ErrValidation)
		}
		if name == "ship_outages" && (p.Kind != "alert" || p.Color != "yellow" || p.Category != "other" || p.View != "") {
			return nil, fmt.Errorf("%w: invalid ship_outages policy", ErrValidation)
		}
		for _, component := range p.Components {
			if component.Component == "" || !validCategory(component.Category) {
				return nil, fmt.Errorf("%w: invalid component category", ErrValidation)
			}
		}
	}
	if len(c.Policies) != 2 {
		return nil, fmt.Errorf("%w: both fixed source policies required", ErrValidation)
	}
	rules := append(append([]classificationRule(nil), c.Platform...), c.Legacy...)
	for _, r := range rules {
		if (r.Alertname == "") == (r.Glob == "") || !validColor(r.Color) || !validCategory(r.Category) {
			return nil, fmt.Errorf("%w: invalid alert classification", ErrValidation)
		}
		if r.Glob != "" {
			if _, err := path.Match(r.Glob, ""); err != nil {
				return nil, fmt.Errorf("%w: invalid alertname glob: %v", ErrValidation, err)
			}
			if strings.Contains(r.Glob, "/") {
				return nil, fmt.Errorf("%w: alertname globs cannot contain slash", ErrValidation)
			}
		}
	}
	return &c, nil
}

// validColor keeps green exclusive to scheduled Chai reports.
func validColor(s string) bool { return s == "red" || s == "orange" || s == "yellow" }

// validCategory accepts only published alert categories.
func validCategory(s string) bool {
	switch s {
	case "cluster_fleet", "cloud_cleanup", "ci_platform", "repo_automation", "other":
		return true
	}
	return false
}

// fallback returns the first release-owned matching rule.
func (c *Classifier) fallback(identifier string) Classification {
	for _, rules := range [][]classificationRule{c.Platform, c.Legacy} {
		for _, r := range rules {
			matched := r.Alertname == identifier
			if r.Glob != "" {
				matched, _ = path.Match(r.Glob, identifier)
			} // Globs were validated at load.
			if matched {
				return Classification{Color: r.Color, Category: r.Category, Origin: "fallback"}
			}
		}
	}
	return Classification{Color: c.Defaults.Color, Category: c.Defaults.Category, Origin: "default"}
}
