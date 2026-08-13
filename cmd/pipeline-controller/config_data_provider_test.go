package main

import (
	"testing"

	"sigs.k8s.io/prow/pkg/config"
)

func TestConfigDataProviderKeepsEmptyConditionalAnnotationProtected(t *testing.T) {
	const orgRepo = "openshift/hypershift"
	cfg := &config.Config{JobConfig: config.JobConfig{PresubmitsStatic: map[string][]config.Presubmit{
		orgRepo: {
			{JobBase: config.JobBase{Name: "empty-dockerfile", Annotations: map[string]string{"pipeline_run_if_dockerfile_changed": ""}}},
			{JobBase: config.JobBase{Name: "empty-run-if", Annotations: map[string]string{"pipeline_run_if_changed": ""}}},
			{JobBase: config.JobBase{Name: "valid-dockerfile", Annotations: map[string]string{"pipeline_run_if_dockerfile_changed": `[{"path":"Dockerfile"}]`}}},
		},
	}}}
	provider := &ConfigDataProvider{
		configGetter:      func() *config.Config { return cfg },
		updatedPresubmits: make(map[string]presubmitTests),
	}

	provider.gatherDataForRepos([]string{orgRepo})
	got := provider.GetPresubmits(orgRepo)
	if len(got.protected) != 2 || got.protected[0].Name != "empty-dockerfile" || got.protected[1].Name != "empty-run-if" {
		t.Errorf("protected jobs = %#v, want both empty-condition jobs", got.protected)
	}
	if len(got.pipelineConditionallyRequired) != 1 || got.pipelineConditionallyRequired[0].Name != "valid-dockerfile" {
		t.Errorf("pipeline conditionally required jobs = %#v, want valid-dockerfile", got.pipelineConditionallyRequired)
	}
}
