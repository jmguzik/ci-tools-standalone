package main

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/kube"
)

func TestAgenticProwJobUpdateFields(t *testing.T) {
	f, _ := mixedAgenticFixture(t)
	r := &reconciler{agentic: f.a}
	old := &v1.ProwJob{
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "ci", ResourceVersion: "10", CreationTimestamp: metav1.NewTime(f.now), Labels: map[string]string{}},
		Spec: v1.ProwJobSpec{Type: v1.PresubmitJob, Job: "test", Context: "ci/test", Report: true,
			Refs: &v1.Refs{Org: "org", Repo: "repo", BaseRef: "main", Pulls: []v1.Pull{{Number: 42, SHA: "head"}}}},
		Status: v1.ProwJobStatus{State: v1.PendingState, PrevReportStates: map[string]v1.ProwJobState{}},
	}
	for _, test := range []struct {
		name   string
		change func(*v1.ProwJob)
		want   bool
	}{
		{"resource-version", func(*v1.ProwJob) {}, false},
		{"pod", func(pj *v1.ProwJob) { pj.Status.PodName = "pod"; pj.Status.PodRevivalCount++ }, false},
		{"timing", func(pj *v1.ProwJob) { pj.Status.CompletionTime = &metav1.Time{Time: f.now} }, false},
		{"description", func(pj *v1.ProwJob) { pj.Status.Description = "updated" }, false},
		{"build-id", func(pj *v1.ProwJob) { pj.Status.BuildID = "123" }, false},
		{"other-reporter", func(pj *v1.ProwJob) { pj.Status.PrevReportStates["other-reporter"] = v1.SuccessState }, false},
		{"unrelated-label", func(pj *v1.ProwJob) { pj.Labels["unrelated"] = "updated" }, false},
		{"annotation", func(pj *v1.ProwJob) { pj.Annotations = map[string]string{"unrelated": "updated"} }, false},
		{"state", func(pj *v1.ProwJob) { pj.Status.State = v1.SuccessState }, true},
		{"url", func(pj *v1.ProwJob) { pj.Status.URL = "https://example.com/job" }, false},
		{"github-ack", func(pj *v1.ProwJob) { pj.Status.PrevReportStates["github-reporter"] = v1.PendingState }, false},
		{"job", func(pj *v1.ProwJob) { pj.Spec.Job = "other" }, true},
		{"context", func(pj *v1.ProwJob) { pj.Spec.Context = "ci/other" }, true},
		{"report", func(pj *v1.ProwJob) { pj.Spec.Report = false }, true},
		{"type", func(pj *v1.ProwJob) { pj.Spec.Type = v1.BatchJob }, true},
		{"head", func(pj *v1.ProwJob) { pj.Spec.Refs.Pulls[0].SHA = "other" }, true},
		{"base", func(pj *v1.ProwJob) { pj.Spec.Refs.BaseRef = "release" }, true},
		{"uid", func(pj *v1.ProwJob) { pj.UID = types.UID("other") }, true},
		{"creation", func(pj *v1.ProwJob) { pj.CreationTimestamp = metav1.NewTime(f.now.Add(time.Second)) }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := old.DeepCopy()
			current.ResourceVersion = "11"
			test.change(current)
			if got := r.shouldReconcileProwJobUpdate(event.UpdateEvent{ObjectOld: old, ObjectNew: current}); got != test.want {
				t.Fatalf("accepted=%v, want %v", got, test.want)
			}
		})
	}
	for _, label := range []string{kube.OrgLabel, kube.RepoLabel, kube.PullLabel, kube.ProwJobTypeLabel} {
		t.Run(label, func(t *testing.T) {
			current := old.DeepCopy()
			current.ResourceVersion, current.Labels[label] = "11", "changed"
			if !r.shouldReconcileProwJobUpdate(event.UpdateEvent{ObjectOld: old, ObjectNew: current}) {
				t.Fatal("selector or dispatch identity change was dropped")
			}
		})
	}
}

func TestAgenticProwJobUpdateLegacyAndUnknownObjects(t *testing.T) {
	f, _ := mixedAgenticFixture(t)
	r := &reconciler{agentic: f.a}
	old := &v1.ProwJob{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "10"},
		Spec: v1.ProwJobSpec{Refs: &v1.Refs{Org: "org", Repo: "repo", BaseRef: "release"}}}
	current := old.DeepCopy()
	update := event.UpdateEvent{ObjectOld: old, ObjectNew: current}
	if r.shouldReconcileProwJobUpdate(update) {
		t.Fatal("mixed repository's stale normal-branch resync was accepted")
	}
	current.ResourceVersion, current.Status.PodName = "11", "legacy-pod"
	if !r.shouldReconcileProwJobUpdate(update) {
		t.Fatal("legacy branch's real update was filtered")
	}
	current.ResourceVersion = old.ResourceVersion
	r.agentic = nil
	if !r.shouldReconcileProwJobUpdate(update) {
		t.Fatal("normal-only controller update behavior changed")
	}
	r.agentic = f.a
	old.Spec.Refs.Repo, current.Spec.Refs.Repo = "normal", "normal"
	current.ResourceVersion = old.ResourceVersion
	if !r.shouldReconcileProwJobUpdate(update) {
		t.Fatal("normal-only repository behavior changed")
	}
	for _, update := range []event.UpdateEvent{
		{}, {ObjectOld: old}, {ObjectOld: (*v1.ProwJob)(nil), ObjectNew: current},
		{ObjectOld: old, ObjectNew: &v1.ProwJob{}}, {ObjectOld: old, ObjectNew: &metav1.PartialObjectMetadata{}},
	} {
		if !r.shouldReconcileProwJobUpdate(update) {
			t.Fatal("unknown or incomplete update was filtered")
		}
	}
}
