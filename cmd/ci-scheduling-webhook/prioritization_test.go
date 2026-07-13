package main

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAvoidanceUpdateAllowed(t *testing.T) {
	cordoned := &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}}
	schedulable := &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: false}}

	tests := []struct {
		name          string
		node          *corev1.Node
		desiredEffect corev1.TaintEffect
		want          bool
	}{
		{name: "clear avoidance on cordoned node", node: cordoned, desiredEffect: TaintEffectNone, want: true},
		{name: "block prefer-no-schedule on cordoned node", node: cordoned, desiredEffect: corev1.TaintEffectPreferNoSchedule, want: false},
		{name: "block no-schedule on cordoned node", node: cordoned, desiredEffect: corev1.TaintEffectNoSchedule, want: false},
		{name: "allow updates on schedulable node", node: schedulable, desiredEffect: corev1.TaintEffectPreferNoSchedule, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := avoidanceUpdateAllowed(tt.node, tt.desiredEffect); got != tt.want {
				t.Fatalf("avoidanceUpdateAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodServingDNS(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "ready pod",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{{
						Type:   corev1.PodReady,
						Status: corev1.ConditionTrue,
					}},
				},
			},
			want: true,
		},
		{
			name: "terminating pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
				},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{{
						Type:   corev1.PodReady,
						Status: corev1.ConditionTrue,
					}},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, podServingDNS(tt.pod)); diff != "" {
				t.Errorf("podServingDNS() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
