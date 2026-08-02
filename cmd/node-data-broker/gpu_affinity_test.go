/*
 * Copyright 2026 NVIDIA CORPORATION
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/NVIDIA/topograph/pkg/deviceaffinity"
)

type fakeGPUCommandRunner struct {
	outputs map[string]string
	found   bool
	err     error
}

func (r *fakeGPUCommandRunner) Run(_ context.Context, _ string, command []string) (string, bool, error) {
	return r.outputs[strings.Join(command, " ")], r.found, r.err
}

func TestBuildNodeTopologyMappingDisabled(t *testing.T) {
	broker := &nodeBroker{config: nodeDataBrokerConfig{NICRailsConfigMap: &configMapReference{}}}
	topology, err := broker.buildNodeTopology(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", UID: types.UID("uid-1")},
	}, map[string][]string{"0000:3B:00.0": {"rail0"}})
	require.NoError(t, err)
	require.Equal(t, deviceaffinity.SchemaVersion, topology.SchemaVersion)
	require.Equal(t, "uid-1", topology.NodeUID)
	require.Equal(t, map[string][]string{"0000:3b:00.0": {"rail0"}}, topology.NICs)
	require.Nil(t, topology.GPUs)
}

func TestBuildNodeTopologyNonGPUNode(t *testing.T) {
	broker := newGPUAffinityTestBroker(&fakeGPUCommandRunner{err: fmt.Errorf("must not run")})
	topology, err := broker.buildNodeTopology(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", UID: types.UID("uid-1")},
	}, map[string][]string{"0000:3b:00.0": {"rail0"}})
	require.NoError(t, err)
	require.NotNil(t, topology.GPUs)
	require.Empty(t, *topology.GPUs)
}

func TestBuildNodeTopologyEmptyRailsSkipsGPUDiscovery(t *testing.T) {
	broker := newGPUAffinityTestBroker(&fakeGPUCommandRunner{err: fmt.Errorf("must not run")})
	topology, err := broker.buildNodeTopology(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			UID:    types.UID("uid-1"),
			Labels: map[string]string{"nvidia.com/gpu.present": "true"},
		},
	}, map[string][]string{})
	require.NoError(t, err)
	require.Empty(t, topology.NICs)
	require.NotNil(t, topology.GPUs)
	require.Empty(t, *topology.GPUs)
}

func TestBuildNodeTopologyGPUNode(t *testing.T) {
	runner := &fakeGPUCommandRunner{
		found: true,
		outputs: map[string]string{
			strings.Join(gpuInventoryCommand, " "): "0,GPU-a,00000000:17:00.0\n",
			strings.Join(gpuTopologyCommand, " "): `GPU0 NIC0 NIC1 CPU Affinity
GPU0 X PIX SYS 0-7
NIC0 PIX X SYS
NIC1 SYS SYS X
NIC Legend:
NIC0: mlx5_0
NIC1: mlx5_1
`,
		},
	}
	broker := newGPUAffinityTestBroker(runner)
	broker.resolveIB = func(device string) (string, error) {
		return map[string]string{"mlx5_0": "0000:3b:00.0", "mlx5_1": "0000:af:00.0"}[device], nil
	}
	topology, err := broker.buildNodeTopology(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			UID:    types.UID("uid-1"),
			Labels: map[string]string{"nvidia.com/gpu.present": "true"},
		},
	}, map[string][]string{
		"0000:3b:00.0": {"rail0"},
		"0000:af:00.0": {"rail1"},
	})
	require.NoError(t, err)
	require.Equal(t, map[string]deviceaffinity.GPUAffinity{
		"GPU-a": {
			PCIAddress: "0000:17:00.0",
			Rails: map[string][]deviceaffinity.PreferredNIC{
				"rail0": {{NIC: "0000:3b:00.0", Path: deviceaffinity.PathPIX}},
				"rail1": {{NIC: "0000:af:00.0", Path: deviceaffinity.PathSYS}},
			},
		},
	}, *topology.GPUs)
}

func TestBuildNodeTopologyMissingGPUOperatorPod(t *testing.T) {
	broker := newGPUAffinityTestBroker(&fakeGPUCommandRunner{found: false})
	_, err := broker.buildNodeTopology(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			UID:    types.UID("uid-1"),
			Labels: map[string]string{"nvidia.com/gpu.present": "true"},
		},
	}, map[string][]string{"0000:3b:00.0": {"rail0"}})
	require.ErrorContains(t, err, "has no gpu-operator/nvidia-device-plugin-daemonset pod")
}

func TestIsGPUNode(t *testing.T) {
	require.False(t, isGPUNode(&corev1.Node{}))
	require.True(t, isGPUNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"nvidia.com/gpu.present": "TRUE"},
	}}))
	require.True(t, isGPUNode(&corev1.Node{Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		"nvidia.com/gpu": resource.MustParse("1"),
	}}}))
	require.True(t, isGPUNode(&corev1.Node{Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		"nvidia.com/mig-1g.10gb": resource.MustParse("1"),
	}}}))
}

func newGPUAffinityTestBroker(runner gpuCommandRunner) *nodeBroker {
	return &nodeBroker{
		gpuRunner: runner,
		config: nodeDataBrokerConfig{NICRailsConfigMap: &configMapReference{
			Name:      "topograph-nic-rails",
			Namespace: "topograph",
			GPUMapping: gpuMappingConfig{
				Enabled:              true,
				GPUOperatorNamespace: "gpu-operator",
				DaemonSet:            "nvidia-device-plugin-daemonset",
			},
		}},
	}
}
