package main

import (
	"reflect"

	"sigs.k8s.io/controller-runtime/pkg/event"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/kube"
)

func (r *reconciler) shouldReconcileProwJobCreate(create event.CreateEvent) bool {
	pj, ok := create.Object.(*v1.ProwJob)
	if !create.IsInInitialList || !ok || pj == nil || pj.Spec.Refs == nil || pj.Spec.Type != v1.PresubmitJob {
		return true
	}
	_, agentic := r.agentic.repoConfig(pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.BaseRef)
	return !agentic // Restarts intentionally do not replay historical agentic jobs.
}

func (r *reconciler) shouldReconcileProwJobUpdate(update event.UpdateEvent) bool {
	old, oldOK := update.ObjectOld.(*v1.ProwJob)
	current, currentOK := update.ObjectNew.(*v1.ProwJob)
	if !oldOK || !currentOK || old == nil || current == nil || old.Spec.Refs == nil || current.Spec.Refs == nil {
		return true
	}
	oldRefs, refs := old.Spec.Refs, current.Spec.Refs
	if !r.agentic.hasRepo(oldRefs.Org, oldRefs.Repo) && !r.agentic.hasRepo(refs.Org, refs.Repo) {
		return true // Preserve normal-only repository behavior, including resyncs.
	}
	if old.ResourceVersion == current.ResourceVersion {
		return false // Includes stale normal-branch jobs in agentic-capable repositories.
	}
	_, oldAgentic := r.agentic.repoConfig(oldRefs.Org, oldRefs.Repo, oldRefs.BaseRef)
	_, currentAgentic := r.agentic.repoConfig(refs.Org, refs.Repo, refs.BaseRef)
	if !oldAgentic && !currentAgentic {
		return true // Legacy branches also use completion timestamps and other status fields.
	}
	if !reflect.DeepEqual(old.Spec, current.Spec) || old.Name != current.Name || old.Namespace != current.Namespace ||
		old.UID != current.UID || !old.CreationTimestamp.Equal(&current.CreationTimestamp) ||
		old.Status.State != current.Status.State {
		return true
	}
	for _, label := range []string{kube.OrgLabel, kube.RepoLabel, kube.PullLabel, kube.ProwJobTypeLabel} {
		if old.Labels[label] != current.Labels[label] {
			return true
		}
	}
	return false
}
