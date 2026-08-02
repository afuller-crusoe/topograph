/*
 * Copyright 2026 NVIDIA CORPORATION
 * SPDX-License-Identifier: Apache-2.0
 */

// Package deviceaffinity discovers and represents node-local GPU-to-NIC
// locality independently of Topograph's canonical cluster topology graph.
package deviceaffinity

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	SchemaVersion          = "v1alpha1"
	SysClassInfiniBandPath = "/sys/class/infiniband"
)

var (
	gpuLabelPattern   = regexp.MustCompile(`^GPU([0-9]+)$`)
	nicAliasPattern   = regexp.MustCompile(`^NIC[0-9]+$`)
	pciAddressPattern = regexp.MustCompile(
		`^(?:([[:xdigit:]]{4}|[[:xdigit:]]{8}):)?([[:xdigit:]]{2}):([[:xdigit:]]{2})\.([[:xdigit:]])$`,
	)
)

type GPU struct {
	UUID       string
	PCIAddress string
}

// GPUInventory maps nvidia-smi matrix labels such as GPU0 to stable GPU
// identities returned by the query interface.
type GPUInventory map[string]GPU

type PathClass string

const (
	PathPIX  PathClass = "PIX"
	PathPXB  PathClass = "PXB"
	PathPHB  PathClass = "PHB"
	PathNODE PathClass = "NODE"
	PathSYS  PathClass = "SYS"
)

var pathRanks = map[PathClass]int{
	PathPIX:  1,
	PathPXB:  2,
	PathPHB:  3,
	PathNODE: 4,
	PathSYS:  5,
}

// GPUPaths maps stable GPU UUIDs to physical NIC PCI addresses and their
// nvidia-smi path class.
type GPUPaths map[string]map[string]PathClass

type PreferredNIC struct {
	NIC  string    `json:"nic"`
	Path PathClass `json:"path"`
}

type GPUAffinity struct {
	PCIAddress string                    `json:"pciAddress"`
	Rails      map[string][]PreferredNIC `json:"rails"`
}

// NodeTopology is serialized into one node-named data entry in the shared
// rail ConfigMap. GPUs is nil when collection is disabled. A non-nil pointer
// to an empty map means collection succeeded on a non-GPU node.
type NodeTopology struct {
	SchemaVersion string                  `json:"schemaVersion"`
	NodeUID       string                  `json:"nodeUID"`
	NICs          map[string][]string     `json:"nics"`
	GPUs          *map[string]GPUAffinity `json:"gpus,omitempty"`
}

func ParseGPUInventory(output string) (GPUInventory, error) {
	reader := csv.NewReader(strings.NewReader(output))
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1

	inventory := make(GPUInventory)
	uuids := make(map[string]bool)
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to parse GPU inventory CSV: %w", err)
		}
		if len(record) == 1 && strings.TrimSpace(record[0]) == "" {
			continue
		}
		if len(record) != 3 {
			return nil, fmt.Errorf("expected GPU index, UUID, and PCI address, got %q", record)
		}

		indexText := strings.TrimSpace(record[0])
		index, err := strconv.Atoi(indexText)
		if err != nil || index < 0 {
			return nil, fmt.Errorf("invalid GPU index %q", indexText)
		}
		label := fmt.Sprintf("GPU%d", index)
		if _, exists := inventory[label]; exists {
			return nil, fmt.Errorf("duplicate GPU index %d", index)
		}

		uuid := strings.TrimSpace(record[1])
		if !strings.HasPrefix(uuid, "GPU-") || len(uuid) == len("GPU-") {
			return nil, fmt.Errorf("invalid physical GPU UUID %q", uuid)
		}
		if uuids[uuid] {
			return nil, fmt.Errorf("duplicate GPU UUID %q", uuid)
		}

		pciAddress, err := NormalizePCIAddress(record[2])
		if err != nil {
			return nil, fmt.Errorf("GPU %s: %w", label, err)
		}
		inventory[label] = GPU{UUID: uuid, PCIAddress: pciAddress}
		uuids[uuid] = true
	}

	return inventory, nil
}

// ParseTopologyMatrix parses nvidia-smi topo -m output. resolveIBDevice maps
// an InfiniBand device from the NIC legend to its physical PCI address.
func ParseTopologyMatrix(
	output string,
	inventory GPUInventory,
	resolveIBDevice func(string) (string, error),
) (GPUPaths, error) {
	lines := strings.Split(output, "\n")
	headers, headerLine, err := topologyHeaders(lines)
	if err != nil {
		return nil, err
	}

	nicHeaders := make([]string, 0)
	for _, header := range headers {
		if !gpuLabelPattern.MatchString(header) {
			nicHeaders = append(nicHeaders, header)
		}
	}
	if len(nicHeaders) == 0 {
		return nil, fmt.Errorf("topology matrix contains no NIC columns")
	}

	legend, err := parseNICLegend(lines)
	if err != nil {
		return nil, err
	}
	if resolveIBDevice == nil {
		return nil, fmt.Errorf("InfiniBand device resolver is required")
	}

	nicAddresses := make(map[string]string, len(nicHeaders))
	for _, header := range nicHeaders {
		ibDevice := header
		if nicAliasPattern.MatchString(header) {
			var ok bool
			ibDevice, ok = legend[header]
			if !ok {
				return nil, fmt.Errorf("missing NIC legend entry for %s", header)
			}
		}
		pciAddress, err := resolveIBDevice(ibDevice)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve NIC %s (%s): %w", header, ibDevice, err)
		}
		pciAddress, err = NormalizePCIAddress(pciAddress)
		if err != nil {
			return nil, fmt.Errorf("NIC %s (%s): %w", header, ibDevice, err)
		}
		nicAddresses[header] = pciAddress
	}

	paths := make(GPUPaths, len(inventory))
	seenRows := make(map[string]bool)
	for _, line := range lines[headerLine+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Legend:") || strings.HasPrefix(trimmed, "NIC Legend:") {
			break
		}

		fields := strings.Fields(line)
		if len(fields) == 0 || !gpuLabelPattern.MatchString(fields[0]) {
			continue
		}
		gpu, ok := inventory[fields[0]]
		if !ok {
			return nil, fmt.Errorf("topology matrix contains unknown GPU row %s", fields[0])
		}
		if seenRows[fields[0]] {
			return nil, fmt.Errorf("duplicate topology row %s", fields[0])
		}
		if len(fields) < len(headers)+1 {
			return nil, fmt.Errorf("topology row %s has %d cells, expected at least %d", fields[0], len(fields)-1, len(headers))
		}

		gpuPaths := make(map[string]PathClass, len(nicHeaders))
		for column, header := range headers {
			if gpuLabelPattern.MatchString(header) {
				continue
			}
			path := PathClass(strings.ToUpper(strings.TrimSpace(fields[column+1])))
			if path == "X" {
				return nil, fmt.Errorf("invalid self path in GPU-to-NIC cell %s/%s", fields[0], header)
			}
			if _, ok := pathRanks[path]; !ok {
				return nil, fmt.Errorf("unsupported GPU-to-NIC path %q in cell %s/%s", path, fields[0], header)
			}

			pciAddress := nicAddresses[header]
			if previous, exists := gpuPaths[pciAddress]; !exists || pathRanks[path] < pathRanks[previous] {
				gpuPaths[pciAddress] = path
			}
		}
		paths[gpu.UUID] = gpuPaths
		seenRows[fields[0]] = true
	}

	for label := range inventory {
		if !seenRows[label] {
			return nil, fmt.Errorf("topology matrix is missing GPU row %s", label)
		}
	}
	return paths, nil
}

func topologyHeaders(lines []string) ([]string, int, error) {
	for lineNumber, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 || !gpuLabelPattern.MatchString(fields[0]) {
			continue
		}

		headers := make([]string, 0, len(fields))
		for _, field := range fields {
			if field == "CPU" || field == "NUMA" {
				break
			}
			headers = append(headers, field)
		}
		if len(headers) != 0 {
			return headers, lineNumber, nil
		}
	}
	return nil, 0, fmt.Errorf("missing nvidia-smi topology matrix header")
}

func parseNICLegend(lines []string) (map[string]string, error) {
	legend := make(map[string]string)
	inLegend := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "NIC Legend:" {
			inLegend = true
			continue
		}
		if !inLegend || trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		alias := strings.TrimSuffix(fields[0], ":")
		if !nicAliasPattern.MatchString(alias) {
			continue
		}
		device := strings.TrimSpace(fields[1])
		if device == "" {
			return nil, fmt.Errorf("empty NIC legend entry for %s", alias)
		}
		if previous, exists := legend[alias]; exists && previous != device {
			return nil, fmt.Errorf("conflicting NIC legend entries for %s", alias)
		}
		legend[alias] = device
	}
	return legend, nil
}

func ResolveInfiniBandDevice(device string) (string, error) {
	return ResolveInfiniBandDeviceAt(SysClassInfiniBandPath, device)
}

func ResolveInfiniBandDeviceAt(sysClassInfiniBandPath, device string) (string, error) {
	device = strings.TrimSpace(device)
	if device == "" || filepath.Base(device) != device {
		return "", fmt.Errorf("invalid InfiniBand device name %q", device)
	}
	deviceLink := filepath.Join(sysClassInfiniBandPath, device, "device")
	target, err := os.Readlink(deviceLink)
	if err != nil {
		return "", fmt.Errorf("failed to read %q: %w", deviceLink, err)
	}
	return NormalizePCIAddress(filepath.Base(target))
}

func NormalizePCIAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	matches := pciAddressPattern.FindStringSubmatch(value)
	if matches == nil {
		return "", fmt.Errorf("invalid PCI address %q", value)
	}

	domainText := matches[1]
	if domainText == "" {
		domainText = "0"
	}
	domain, err := strconv.ParseUint(domainText, 16, 32)
	if err != nil || domain > 0xffff {
		return "", fmt.Errorf("invalid PCI domain in address %q", value)
	}
	bus, _ := strconv.ParseUint(matches[2], 16, 8)
	device, _ := strconv.ParseUint(matches[3], 16, 8)
	function, _ := strconv.ParseUint(matches[4], 16, 8)
	if device > 0x1f || function > 7 {
		return "", fmt.Errorf("invalid PCI address %q", value)
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", domain, bus, device, function), nil
}

func NormalizeNICRails(nicRails map[string][]string) (map[string][]string, error) {
	normalizedSets := make(map[string]map[string]bool, len(nicRails))
	for address, rails := range nicRails {
		normalizedAddress, err := NormalizePCIAddress(address)
		if err != nil {
			return nil, fmt.Errorf("NIC rail key: %w", err)
		}
		if normalizedSets[normalizedAddress] == nil {
			normalizedSets[normalizedAddress] = make(map[string]bool)
		}
		for _, rail := range rails {
			rail = strings.TrimSpace(rail)
			if rail == "" {
				return nil, fmt.Errorf("NIC %s has an empty rail ID", normalizedAddress)
			}
			normalizedSets[normalizedAddress][rail] = true
		}
	}

	normalized := make(map[string][]string, len(normalizedSets))
	for address, railSet := range normalizedSets {
		rails := make([]string, 0, len(railSet))
		for rail := range railSet {
			rails = append(rails, rail)
		}
		sort.Strings(rails)
		normalized[address] = rails
	}
	return normalized, nil
}

func SelectPreferredNICs(
	inventory GPUInventory,
	paths GPUPaths,
	nicRails map[string][]string,
) (map[string]GPUAffinity, map[string][]string, error) {
	normalizedNICs, err := NormalizeNICRails(nicRails)
	if err != nil {
		return nil, nil, err
	}

	railNICs := make(map[string][]string)
	for address, rails := range normalizedNICs {
		for _, rail := range rails {
			railNICs[rail] = append(railNICs[rail], address)
		}
	}
	for rail := range railNICs {
		sort.Strings(railNICs[rail])
	}

	affinities := make(map[string]GPUAffinity, len(inventory))
	for _, gpu := range inventory {
		gpuPaths, ok := paths[gpu.UUID]
		if !ok {
			return nil, nil, fmt.Errorf("missing topology paths for GPU %s", gpu.UUID)
		}
		railAffinity := make(map[string][]PreferredNIC, len(railNICs))
		for rail, addresses := range railNICs {
			bestRank := 0
			preferred := make([]PreferredNIC, 0)
			for _, address := range addresses {
				path, exists := gpuPaths[address]
				if !exists {
					continue
				}
				rank, exists := pathRanks[path]
				if !exists {
					return nil, nil, fmt.Errorf("GPU %s NIC %s has unsupported path %q", gpu.UUID, address, path)
				}
				if bestRank == 0 || rank < bestRank {
					bestRank = rank
					preferred = []PreferredNIC{{NIC: address, Path: path}}
				} else if rank == bestRank {
					preferred = append(preferred, PreferredNIC{NIC: address, Path: path})
				}
			}
			if len(preferred) == 0 {
				return nil, nil, fmt.Errorf("GPU %s has no qualified NIC for rail %q", gpu.UUID, rail)
			}
			railAffinity[rail] = preferred
		}
		affinities[gpu.UUID] = GPUAffinity{
			PCIAddress: gpu.PCIAddress,
			Rails:      railAffinity,
		}
	}
	return affinities, normalizedNICs, nil
}
