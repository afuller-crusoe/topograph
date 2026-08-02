/*
 * Copyright 2026 NVIDIA CORPORATION
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	internalk8s "github.com/NVIDIA/topograph/internal/k8s"
	"github.com/NVIDIA/topograph/pkg/deviceaffinity"
)

var (
	gpuInventoryCommand = []string{
		"nvidia-smi",
		"--query-gpu=index,uuid,pci.bus_id",
		"--format=csv,noheader",
	}
	gpuTopologyCommand = []string{"nvidia-smi", "topo", "-m"}
)

type gpuCommandRunner interface {
	Run(context.Context, string, []string) (string, bool, error)
}

type kubernetesGPUCommandRunner struct {
	client    kubernetes.Interface
	config    *rest.Config
	namespace string
	daemonSet string
}

func (r *kubernetesGPUCommandRunner) Run(ctx context.Context, nodeName string, command []string) (string, bool, error) {
	pod, err := internalk8s.GetDaemonSetPod(ctx, r.client, r.daemonSet, r.namespace, nodeName)
	if err != nil {
		return "", false, err
	}
	if pod == nil {
		return "", false, nil
	}
	output, err := internalk8s.ExecInPod(ctx, r.client, r.config, pod.Name, r.namespace, command)
	if err != nil {
		return "", true, err
	}
	return output.String(), true, nil
}

func (b *nodeBroker) buildNodeTopology(
	ctx context.Context,
	node *corev1.Node,
	nicRails map[string][]string,
) (*deviceaffinity.NodeTopology, error) {
	if node == nil {
		return nil, fmt.Errorf("Kubernetes node is required for NIC rail publishing")
	}
	if node.UID == "" {
		return nil, fmt.Errorf("Kubernetes node %q has no UID", node.Name)
	}

	normalizedNICs, err := deviceaffinity.NormalizeNICRails(nicRails)
	if err != nil {
		return nil, fmt.Errorf("invalid NIC rail data for node %q: %w", node.Name, err)
	}
	nodeTopology := &deviceaffinity.NodeTopology{
		SchemaVersion: deviceaffinity.SchemaVersion,
		NodeUID:       string(node.UID),
		NICs:          normalizedNICs,
	}

	ref := b.config.NICRailsConfigMap
	if ref == nil || !ref.GPUMapping.Enabled {
		return nodeTopology, nil
	}

	gpuAffinities := make(map[string]deviceaffinity.GPUAffinity)
	nodeTopology.GPUs = &gpuAffinities
	if len(normalizedNICs) == 0 {
		klog.InfoS("Skipping GPU-to-NIC discovery because LLDP found no usable rail data", "node", node.Name)
		return nodeTopology, nil
	}
	if !isGPUNode(node) {
		klog.InfoS("Skipping GPU-to-NIC discovery on non-GPU node", "node", node.Name)
		return nodeTopology, nil
	}

	runner := b.gpuRunner
	if runner == nil {
		runner = &kubernetesGPUCommandRunner{
			client:    b.clientset,
			config:    b.restConfig,
			namespace: ref.GPUMapping.GPUOperatorNamespace,
			daemonSet: ref.GPUMapping.DaemonSet,
		}
	}

	inventoryOutput, found, err := runner.Run(ctx, node.Name, gpuInventoryCommand)
	if err != nil {
		return nil, fmt.Errorf("failed to query GPU inventory on node %q: %w", node.Name, err)
	}
	if !found {
		return nil, fmt.Errorf(
			"GPU node %q has no %s/%s pod",
			node.Name,
			ref.GPUMapping.GPUOperatorNamespace,
			ref.GPUMapping.DaemonSet,
		)
	}
	inventory, err := deviceaffinity.ParseGPUInventory(inventoryOutput)
	if err != nil {
		return nil, fmt.Errorf("invalid GPU inventory on node %q: %w", node.Name, err)
	}
	if len(inventory) == 0 {
		return nil, fmt.Errorf("GPU inventory is empty on GPU node %q", node.Name)
	}

	topologyOutput, found, err := runner.Run(ctx, node.Name, gpuTopologyCommand)
	if err != nil {
		return nil, fmt.Errorf("failed to query GPU-to-NIC topology on node %q: %w", node.Name, err)
	}
	if !found {
		return nil, fmt.Errorf(
			"GPU node %q lost its %s/%s pod during topology discovery",
			node.Name,
			ref.GPUMapping.GPUOperatorNamespace,
			ref.GPUMapping.DaemonSet,
		)
	}
	resolveIB := b.resolveIB
	if resolveIB == nil {
		resolveIB = deviceaffinity.ResolveInfiniBandDevice
	}
	paths, err := deviceaffinity.ParseTopologyMatrix(
		topologyOutput,
		inventory,
		resolveIB,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid GPU-to-NIC topology on node %q: %w", node.Name, err)
	}

	gpuAffinities, normalizedNICs, err = deviceaffinity.SelectPreferredNICs(inventory, paths, normalizedNICs)
	if err != nil {
		return nil, fmt.Errorf("failed to select GPU-to-NIC rail affinity on node %q: %w", node.Name, err)
	}
	nodeTopology.NICs = normalizedNICs
	nodeTopology.GPUs = &gpuAffinities

	affinityCount := 0
	railSet := make(map[string]bool)
	for _, rails := range normalizedNICs {
		for _, rail := range rails {
			railSet[rail] = true
		}
	}
	for _, gpu := range gpuAffinities {
		for _, nics := range gpu.Rails {
			affinityCount += len(nics)
		}
	}
	klog.InfoS(
		"Collected GPU-to-NIC rail affinity",
		"node", node.Name,
		"gpus", len(gpuAffinities),
		"railNICs", len(normalizedNICs),
		"rails", len(railSet),
		"affinities", affinityCount,
	)
	return nodeTopology, nil
}

func isGPUNode(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(node.Labels["nvidia.com/gpu.present"]), "true") {
		return true
	}
	for resourceName, quantity := range node.Status.Allocatable {
		name := string(resourceName)
		if (name == "nvidia.com/gpu" || strings.HasPrefix(name, "nvidia.com/mig-")) && quantity.Sign() > 0 {
			return true
		}
	}
	return false
}
