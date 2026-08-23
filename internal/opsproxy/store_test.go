package opsproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestStoreCreateIfMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fakeKube := fakectrlruntimeclient.NewClientBuilder().Build()
	store := NewStore(fakeKube, "ci", "ops-proxy")
	got, err := store.Mutate(ctx, func(st *State) error {
		st.Incidents["infrastructure-job-failures/job-a"] = IncidentState{
			Identity: Identity{ID: "infrastructure-job-failures/job-a", AlertName: "infrastructure-job-failures", MatcherName: "job_name", MatcherValue: "job-a"},
			SlackTS:  "1.2",
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if _, ok := got.Incidents["infrastructure-job-failures/job-a"]; !ok {
		t.Fatalf("incident missing from returned state: %#v", got)
	}
	cm := &corev1.ConfigMap{}
	if err := fakeKube.Get(ctx, types.NamespacedName{Namespace: "ci", Name: "ops-proxy"}, cm); err != nil {
		t.Fatalf("expected ConfigMap to be created: %v", err)
	}
	if _, ok := cm.Data[stateKey]; !ok {
		t.Fatalf("expected single %q blob, data=%v", stateKey, cm.Data)
	}
	if _, hasSlashKey := cm.Data["infrastructure-job-failures/job-a"]; hasSlashKey {
		t.Fatal("ConfigMap must not use incident id as a key")
	}
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(got, loaded); diff != "" {
		t.Fatalf("Load mismatch (-want +got):\n%s", diff)
	}
}

func TestStoreUpsertConflictRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	initial := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-proxy", Namespace: "ci"},
		Data:       map[string]string{stateKey: `{"incidents":{}}`},
	}
	base := fakectrlruntimeclient.NewClientBuilder().WithRuntimeObjects(initial).Build()
	var updates atomic.Int32
	kube := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c ctrlruntimeclient.WithWatch, obj ctrlruntimeclient.Object, opts ...ctrlruntimeclient.UpdateOption) error {
			if updates.Add(1) == 1 {
				return apierrors.NewConflict(schema.GroupResource{Group: "", Resource: "configmaps"}, "ops-proxy", fmt.Errorf("conflict"))
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	store := NewStore(kube, "ci", "ops-proxy")
	got, err := store.Mutate(ctx, func(st *State) error {
		st.BoardTS = "board.ts"
		st.Incidents["id/one"] = IncidentState{SlackTS: "card.ts", Identity: Identity{ID: "id/one"}}
		return nil
	})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if updates.Load() < 2 {
		t.Fatalf("expected conflict retry, updates=%d", updates.Load())
	}
	if got.BoardTS != "board.ts" {
		t.Fatalf("BoardTS=%q", got.BoardTS)
	}
	if got.Incidents["id/one"].SlackTS != "card.ts" {
		t.Fatalf("incident not stored: %#v", got.Incidents)
	}
}

func TestStoreLoadMissing(t *testing.T) {
	t.Parallel()
	store := NewStore(fakectrlruntimeclient.NewClientBuilder().Build(), "ci", "ops-proxy")
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Incidents) != 0 {
		t.Fatalf("expected empty state, got %#v", got)
	}
}

func TestDecodeStateSingleBlob(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(State{
		BoardTS: "1.0",
		Incidents: map[string]IncidentState{
			"ci-operator-error/reason": {SlackTS: "2.0"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := decodeState(&corev1.ConfigMap{Data: map[string]string{stateKey: string(raw)}})
	if err != nil {
		t.Fatal(err)
	}
	if st.BoardTS != "1.0" {
		t.Fatalf("board_ts=%s", st.BoardTS)
	}
	if _, ok := st.Incidents["ci-operator-error/reason"]; !ok {
		t.Fatalf("missing slash identity: %#v", st.Incidents)
	}
}
