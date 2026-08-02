/*
 * Copyright 2025-2026 NVIDIA CORPORATION
 * SPDX-License-Identifier: Apache-2.0
 */

package k8s

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGetDaemonSetPod(t *testing.T) {
	controller := true
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gpu-plugin",
			Namespace: "gpu-operator",
			UID:       types.UID("gpu-plugin-uid"),
		},
		Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": "gpu-plugin"},
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "track",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"stable"},
			}},
		}},
	}
	owner := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "DaemonSet",
		Name:       ds.Name,
		UID:        ds.UID,
		Controller: &controller,
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "gpu-plugin-node-1",
			Namespace:       "gpu-operator",
			Labels:          map[string]string{"app": "gpu-plugin", "track": "stable"},
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: corev1.PodSpec{NodeName: "node-1"},
	}
	unowned := pod.DeepCopy()
	unowned.Name = "unowned-node-1"
	unowned.OwnerReferences = nil
	wrongExpression := pod.DeepCopy()
	wrongExpression.Name = "gpu-plugin-canary-node-1"
	wrongExpression.Labels["track"] = "canary"
	client := fake.NewSimpleClientset(ds, pod, unowned, wrongExpression)

	actual, err := GetDaemonSetPod(context.Background(), client, "gpu-plugin", "gpu-operator", "node-1")
	require.NoError(t, err)
	require.Equal(t, pod.Name, actual.Name)

	unownedClient := fake.NewSimpleClientset(ds, unowned)
	actual, err = GetDaemonSetPod(context.Background(), unownedClient, "gpu-plugin", "gpu-operator", "node-1")
	require.NoError(t, err)
	require.Nil(t, actual)

	noPodClient := fake.NewSimpleClientset(ds)
	actual, err = GetDaemonSetPod(context.Background(), noPodClient, "gpu-plugin", "gpu-operator", "node-2")
	require.NoError(t, err)
	require.Nil(t, actual)

	duplicate := pod.DeepCopy()
	duplicate.Name = "gpu-plugin-node-1-duplicate"
	duplicateClient := fake.NewSimpleClientset(ds, pod, duplicate)
	_, err = GetDaemonSetPod(context.Background(), duplicateClient, "gpu-plugin", "gpu-operator", "node-1")
	require.ErrorContains(t, err, "expected 1 gpu-plugin pod on node node-1, got 2")
}

func TestIsPodReady(t *testing.T) {
	testCases := []struct {
		name  string
		pod   *corev1.Pod
		ready bool
	}{
		{
			name: "Case 1: ready",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.DisruptionTarget,
							Status: corev1.ConditionUnknown,
						},
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			ready: true,
		},
		{
			name: "Case 2: implicit not ready",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.DisruptionTarget,
							Status: corev1.ConditionUnknown,
						},
						{
							Type:   corev1.ContainersReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			ready: false,
		},
		{
			name: "Case 3: explicit not ready",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.DisruptionTarget,
							Status: corev1.ConditionUnknown,
						},
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			ready: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.ready, IsPodReady(tc.pod))
		})
	}
}

func TestNodeListOptions(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]any
		want   *metav1.ListOptions
		err    string
	}{
		{name: "no parameters"},
		{
			name:   "other provider parameters are ignored",
			params: map[string]any{"accelerator": map[string]any{"source": "none"}},
		},
		{
			name:   "node selector",
			params: map[string]any{"nodeSelector": map[string]string{"key": "value"}},
			want:   &metav1.ListOptions{LabelSelector: "key=value"},
		},
		{
			name:   "invalid node selector",
			params: map[string]any{"nodeSelector": 0.1},
			err:    "could not decode configuration",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NodeListOptions(test.params)
			if test.err != "" {
				require.ErrorContains(t, err, test.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestValidateLabelKey(t *testing.T) {
	testCases := []struct {
		name string
		key  string
		err  string
	}{
		{name: "valid", key: "example.com/accelerator-domain"},
		{
			name: "empty",
			err:  `acceleratorDomainSourceLabel "" is not a valid Kubernetes label key`,
		},
		{
			name: "invalid",
			key:  "not a label",
			err:  `acceleratorDomainSourceLabel "not a label" is not a valid Kubernetes label key`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLabelKey("acceleratorDomainSourceLabel", tc.key)
			if tc.err == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.err)
		})
	}
}
