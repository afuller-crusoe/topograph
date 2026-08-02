/*
 * Copyright 2026 NVIDIA CORPORATION
 * SPDX-License-Identifier: Apache-2.0
 */

package deviceaffinity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseGPUInventory(t *testing.T) {
	inventory, err := ParseGPUInventory("0, GPU-a, 00000000:17:00.0\n1,GPU-b,0000:65:00.0\n")
	require.NoError(t, err)
	require.Equal(t, GPUInventory{
		"GPU0": {UUID: "GPU-a", PCIAddress: "0000:17:00.0"},
		"GPU1": {UUID: "GPU-b", PCIAddress: "0000:65:00.0"},
	}, inventory)

	for _, test := range []struct {
		name   string
		output string
		err    string
	}{
		{name: "fields", output: "0,GPU-a\n", err: "expected GPU index, UUID, and PCI address"},
		{name: "index", output: "x,GPU-a,0000:17:00.0\n", err: `invalid GPU index "x"`},
		{name: "duplicate index", output: "0,GPU-a,0000:17:00.0\n0,GPU-b,0000:65:00.0\n", err: "duplicate GPU index 0"},
		{name: "UUID", output: "0,MIG-a,0000:17:00.0\n", err: "invalid physical GPU UUID"},
		{name: "duplicate UUID", output: "0,GPU-a,0000:17:00.0\n1,GPU-a,0000:65:00.0\n", err: `duplicate GPU UUID "GPU-a"`},
		{name: "PCI", output: "0,GPU-a,invalid\n", err: "invalid PCI address"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseGPUInventory(test.output)
			require.ErrorContains(t, err, test.err)
		})
	}
}

func TestParseTopologyMatrix(t *testing.T) {
	const output = `
        GPU0    GPU1    NIC0    NIC1    CPU Affinity    NUMA Affinity
GPU0     X      NV4     PIX     SYS     0-31            0
GPU1    NV4      X      PXB     NODE    0-31            0
NIC0    PIX     PXB      X      SYS
NIC1    SYS     NODE    SYS      X

Legend:
  X = Self

NIC Legend:
  NIC0: mlx5_0
  NIC1: mlx5_1
`
	inventory := GPUInventory{
		"GPU0": {UUID: "GPU-a", PCIAddress: "0000:17:00.0"},
		"GPU1": {UUID: "GPU-b", PCIAddress: "0000:65:00.0"},
	}
	addresses := map[string]string{
		"mlx5_0": "0000:3B:00.0",
		"mlx5_1": "af:00.0",
	}
	paths, err := ParseTopologyMatrix(output, inventory, func(device string) (string, error) {
		return addresses[device], nil
	})
	require.NoError(t, err)
	require.Equal(t, GPUPaths{
		"GPU-a": {"0000:3b:00.0": PathPIX, "0000:af:00.0": PathSYS},
		"GPU-b": {"0000:3b:00.0": PathPXB, "0000:af:00.0": PathNODE},
	}, paths)
}

func TestParseTopologyMatrixDirectIBDeviceColumns(t *testing.T) {
	const output = `GPU0 mlx5_0 CPU Affinity
GPU0 X PHB 0-7
mlx5_0 PHB X
`
	paths, err := ParseTopologyMatrix(output, GPUInventory{
		"GPU0": {UUID: "GPU-a", PCIAddress: "0000:17:00.0"},
	}, func(device string) (string, error) {
		require.Equal(t, "mlx5_0", device)
		return "0000:3b:00.0", nil
	})
	require.NoError(t, err)
	require.Equal(t, GPUPaths{"GPU-a": {"0000:3b:00.0": PathPHB}}, paths)
}

func TestParseTopologyMatrixErrors(t *testing.T) {
	inventory := GPUInventory{"GPU0": {UUID: "GPU-a", PCIAddress: "0000:17:00.0"}}
	resolve := func(string) (string, error) { return "0000:3b:00.0", nil }
	for _, test := range []struct {
		name   string
		output string
		err    string
	}{
		{name: "header", output: "no matrix", err: "missing nvidia-smi topology matrix header"},
		{name: "NIC columns", output: "GPU0 CPU Affinity\nGPU0 X 0\n", err: "topology matrix contains no NIC columns"},
		{name: "legend", output: "GPU0 NIC0\nGPU0 X PIX\n", err: "missing NIC legend entry for NIC0"},
		{name: "unknown GPU", output: "GPU0 NIC0\nGPU1 X PIX\nNIC Legend:\nNIC0: mlx5_0\n", err: "unknown GPU row GPU1"},
		{name: "short row", output: "GPU0 NIC0\nGPU0 X\nNIC Legend:\nNIC0: mlx5_0\n", err: "has 1 cells, expected at least 2"},
		{name: "self path", output: "GPU0 NIC0\nGPU0 X X\nNIC Legend:\nNIC0: mlx5_0\n", err: "invalid self path"},
		{name: "path", output: "GPU0 NIC0\nGPU0 X SOC\nNIC Legend:\nNIC0: mlx5_0\n", err: "unsupported GPU-to-NIC path"},
		{name: "missing row", output: "GPU0 NIC0\nNIC0 PIX X\nNIC Legend:\nNIC0: mlx5_0\n", err: "missing GPU row GPU0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseTopologyMatrix(test.output, inventory, resolve)
			require.ErrorContains(t, err, test.err)
		})
	}
}

func TestResolveInfiniBandDeviceAt(t *testing.T) {
	dir := t.TempDir()
	deviceDir := filepath.Join(dir, "mlx5_0")
	require.NoError(t, os.Mkdir(deviceDir, 0o755))
	require.NoError(t, os.Symlink("../../../devices/pci0000:00/0000:3b:00.0", filepath.Join(deviceDir, "device")))

	address, err := ResolveInfiniBandDeviceAt(dir, "mlx5_0")
	require.NoError(t, err)
	require.Equal(t, "0000:3b:00.0", address)

	_, err = ResolveInfiniBandDeviceAt(dir, "../mlx5_0")
	require.ErrorContains(t, err, "invalid InfiniBand device name")
}

func TestNormalizePCIAddress(t *testing.T) {
	for input, expected := range map[string]string{
		"3B:00.0":          "0000:3b:00.0",
		"0000:3b:00.0":     "0000:3b:00.0",
		"00000000:3B:00.0": "0000:3b:00.0",
	} {
		actual, err := NormalizePCIAddress(input)
		require.NoError(t, err)
		require.Equal(t, expected, actual)
	}
	for _, input := range []string{"", "0000:3b:20.0", "0000:3b:00.8", "00010000:3b:00.0"} {
		_, err := NormalizePCIAddress(input)
		require.Error(t, err, input)
	}
}

func TestSelectPreferredNICs(t *testing.T) {
	inventory := GPUInventory{
		"GPU0": {UUID: "GPU-a", PCIAddress: "0000:17:00.0"},
		"GPU1": {UUID: "GPU-b", PCIAddress: "0000:65:00.0"},
	}
	paths := GPUPaths{
		"GPU-a": {
			"0000:3b:00.0": PathPIX,
			"0000:4b:00.0": PathPIX,
			"0000:af:00.0": PathSYS,
			"0000:bf:00.0": PathNODE,
		},
		"GPU-b": {
			"0000:3b:00.0": PathSYS,
			"0000:4b:00.0": PathNODE,
			"0000:af:00.0": PathPXB,
			"0000:bf:00.0": PathPXB,
		},
	}
	affinities, nics, err := SelectPreferredNICs(inventory, paths, map[string][]string{
		"0000:3B:00.0": {"rail0"},
		"0000:4b:00.0": {"rail0", "rail0"},
		"0000:af:00.0": {"rail1"},
		"0000:bf:00.0": {"rail1"},
	})
	require.NoError(t, err)
	require.Equal(t, map[string][]string{
		"0000:3b:00.0": {"rail0"},
		"0000:4b:00.0": {"rail0"},
		"0000:af:00.0": {"rail1"},
		"0000:bf:00.0": {"rail1"},
	}, nics)
	require.Equal(t, map[string]GPUAffinity{
		"GPU-a": {
			PCIAddress: "0000:17:00.0",
			Rails: map[string][]PreferredNIC{
				"rail0": {{NIC: "0000:3b:00.0", Path: PathPIX}, {NIC: "0000:4b:00.0", Path: PathPIX}},
				"rail1": {{NIC: "0000:bf:00.0", Path: PathNODE}},
			},
		},
		"GPU-b": {
			PCIAddress: "0000:65:00.0",
			Rails: map[string][]PreferredNIC{
				"rail0": {{NIC: "0000:4b:00.0", Path: PathNODE}},
				"rail1": {{NIC: "0000:af:00.0", Path: PathPXB}, {NIC: "0000:bf:00.0", Path: PathPXB}},
			},
		},
	}, affinities)
}

func TestSelectPreferredNICsErrors(t *testing.T) {
	inventory := GPUInventory{"GPU0": {UUID: "GPU-a", PCIAddress: "0000:17:00.0"}}
	_, _, err := SelectPreferredNICs(inventory, GPUPaths{}, map[string][]string{"0000:3b:00.0": {"rail0"}})
	require.ErrorContains(t, err, "missing topology paths for GPU GPU-a")

	_, _, err = SelectPreferredNICs(inventory, GPUPaths{"GPU-a": {}}, map[string][]string{"0000:3b:00.0": {"rail0"}})
	require.ErrorContains(t, err, `GPU GPU-a has no qualified NIC for rail "rail0"`)
}
