package opsproxy

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const stateKey = "state"

// State is the single JSON blob stored in the ops-proxy ConfigMap.
// Mute fields are a display cache; Alertmanager silences are authoritative.
type State struct {
	BoardTS   string                   `json:"board_ts,omitempty"`
	Channel   string                   `json:"channel,omitempty"`
	Incidents map[string]IncidentState `json:"incidents,omitempty"`
}

// IncidentState is ConfigMap bookkeeping for one identity.
type IncidentState struct {
	SlackTS    string            `json:"slack_ts,omitempty"`
	Channel    string            `json:"channel,omitempty"`
	SilenceID  string            `json:"silence_id,omitempty"`
	AckedBy    string            `json:"acked_by,omitempty"`
	EndsAt     string            `json:"ends_at,omitempty"`
	NeedsHuman bool              `json:"needs_human,omitempty"`
	Identity   Identity          `json:"identity"`
	Labels     map[string]string `json:"labels,omitempty"`
}

func emptyState() State {
	return State{Incidents: map[string]IncidentState{}}
}

func (s *State) ensureIncidents() {
	if s.Incidents == nil {
		s.Incidents = map[string]IncidentState{}
	}
}

// Store persists State as one ConfigMap data key (incident ids contain '/').
type Store struct {
	kube      ctrlruntimeclient.Client
	namespace string
	name      string
}

func NewStore(kube ctrlruntimeclient.Client, namespace, name string) *Store {
	return &Store{kube: kube, namespace: namespace, name: name}
}

func (s *Store) Load(ctx context.Context) (State, error) {
	cm, err := s.get(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return emptyState(), nil
		}
		return State{}, err
	}
	return decodeState(cm)
}

func (s *Store) Mutate(ctx context.Context, fn func(*State) error) (State, error) {
	var out State
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := s.get(ctx)
		create := false
		if apierrors.IsNotFound(err) {
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      s.name,
					Namespace: s.namespace,
				},
				Data: map[string]string{},
			}
			create = true
			err = nil
		}
		if err != nil {
			return err
		}
		st, err := decodeState(cm)
		if err != nil {
			return err
		}
		if err := fn(&st); err != nil {
			return err
		}
		st.ensureIncidents()
		raw, err := json.Marshal(st)
		if err != nil {
			return fmt.Errorf("marshal ops-proxy state: %w", err)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[stateKey] = string(raw)
		if create {
			if err := s.kube.Create(ctx, cm); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return apierrors.NewConflict(corev1.Resource("configmaps"), s.name, err)
				}
				return fmt.Errorf("create configmap %s/%s: %w", s.namespace, s.name, err)
			}
			out = st
			return nil
		}
		if err := s.kube.Update(ctx, cm); err != nil {
			return err
		}
		out = st
		return nil
	})
	return out, err
}

func (s *Store) get(ctx context.Context) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	if err := s.kube.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: s.name}, cm); err != nil {
		return nil, err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	return cm, nil
}

func decodeState(cm *corev1.ConfigMap) (State, error) {
	if cm == nil || cm.Data == nil || cm.Data[stateKey] == "" {
		return emptyState(), nil
	}
	var st State
	if err := json.Unmarshal([]byte(cm.Data[stateKey]), &st); err != nil {
		return State{}, fmt.Errorf("unmarshal ops-proxy state: %w", err)
	}
	st.ensureIncidents()
	return st, nil
}
