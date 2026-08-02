# Software Design Document: GPU-to-NIC Rail Mapping

## Status

Draft.

## Summary

Discover each Kubernetes node's physical NIC-to-rail membership and
GPU-to-NIC PCIe locality, combine both dimensions by PCI address, and publish
the result as one versioned JSON document in a shared ConfigMap.

The `lldp-k8s` node-data-broker performs both discoveries during the same
startup reconciliation:

1. LLDP and host network sysfs identify physical NICs and their rails.
2. `nvidia-smi`, executed in a same-node GPU Operator pod, identifies physical
   GPUs and their locality to qualified NICs.
3. The broker selects the closest NIC candidates independently for every GPU
   and rail and atomically publishes the combined node entry.

This is Kubernetes-only, node-local device metadata. It does not change
`topology.Graph`, provider output consumed by engines, or scheduler topology
labels.

## Goals

- Publish normalized physical NIC PCI addresses and their rail IDs.
- Publish the closest NIC or tied NICs for each physical GPU on every rail.
- Use GPU UUIDs and PCI addresses rather than unstable GPU indices.
- Preserve the `nvidia-smi` path class so selections are explainable.
- Reuse the GPU Operator driver environment instead of adding NVIDIA devices
  and libraries to the Topograph container.
- Fail rather than publish incomplete affinity on a GPU node.
- Keep permissions, mounts, and collection Kubernetes-specific and opt-in.

## Non-Goals

- Enabling or validating GPUDirect RDMA.
- Measuring bandwidth, congestion, or NIC health.
- Allocating GPU, RDMA, or SR-IOV resources to Pods.
- Replacing kubelet Topology Manager or a device allocator.
- Adding rails or device affinity to the canonical topology graph.
- Publishing the complete GPU-by-NIC matrix.
- Publishing separate mappings for MIG device UUIDs.
- Supporting the bare-metal `lldp-bm` provider.

## Configuration

Rail discovery requires `lldp-k8s`, an interface regular expression, and a rail
ID expansion template. GPU mapping is enabled under the rail ConfigMap
settings:

```yaml
provider:
  name: lldp-k8s
  params:
    interfaceRegex: '^eth_r([0-9]+)_p[0-9]+$'
    railID: 'rail$1'

nodeDataBroker:
  nicRailsConfigMap:
    name: topograph-nic-rails
    gpuMapping:
      enabled: true
      gpuOperatorNamespace: gpu-operator
      daemonSet: nvidia-device-plugin-daemonset
```

`gpuOperatorNamespace` and `daemonSet` identify the GPU Operator workload used
for command execution. Their defaults match the GPU Operator device-plugin
DaemonSet already used by Topograph's Kubernetes `nvidia-smi` accelerator
discovery.

Helm rejects GPU mapping unless the node-data-broker is enabled,
`provider.name` is `lldp-k8s`, and `provider.params.railID` is configured.

## ConfigMap Contract

The chart creates one shared ConfigMap, `topograph-nic-rails` by default. Each
`data` key is a Kubernetes node name, and its value is a versioned JSON
document:

```json
{
  "schemaVersion": "v1alpha1",
  "nodeUID": "3e931c2b-6a5d-4c17-93bc-bc3a34132bd2",
  "nics": {
    "0000:3b:00.0": ["rail0"],
    "0000:af:00.0": ["rail1"]
  },
  "gpus": {
    "GPU-acde1234-acde-1234-acde-1234abcdef01": {
      "pciAddress": "0000:17:00.0",
      "rails": {
        "rail0": [
          {"nic": "0000:3b:00.0", "path": "PIX"}
        ],
        "rail1": [
          {"nic": "0000:af:00.0", "path": "SYS"}
        ]
      }
    }
  }
}
```

Contract semantics:

- `schemaVersion` is required; consumers reject unknown versions.
- `nodeUID` lets consumers reject data left by a deleted node whose name is
  later reused.
- `nics` maps normalized physical NIC PCI addresses to sorted, deduplicated
  rail ID arrays.
- `gpus` is keyed by physical GPU UUID. Each GPU records its PCI address and
  the preferred NIC candidates for every discovered rail.
- NIC ties at the best path rank are preserved and sorted by PCI address.
- `path` is the winning NVIDIA topology class: `PIX`, `PXB`, `PHB`, `NODE`, or
  `SYS`.
- If GPU mapping is disabled, `gpus` is omitted. If mapping succeeds on a
  non-GPU node, `gpus` is an empty object.

## Discovery and Join

### NIC rails

The broker runs `lldpctl -f json`, selects interfaces matching
`provider.params.interfaceRegex`, and expands each match through
`provider.params.railID`.

For every selected interface, it resolves:

```text
/sys/class/net/<interface>/device
```

The symlink target basename is normalized to lowercase `dddd:bb:ss.f` form.
Interfaces resolving to the same physical PCI function contribute to one
deduplicated rail array.

### GPU inventory and NIC locality

On GPU nodes, the broker executes two commands in the configured same-node GPU
Operator pod:

```text
nvidia-smi --query-gpu=index,uuid,pci.bus_id --format=csv,noheader
nvidia-smi topo -m
```

The query output maps temporary matrix labels such as `GPU0` to stable GPU UUID
and PCI identities. The topology matrix supplies each GPU-to-NIC path class.

`topo -m` commonly maps aliases such as `NIC0` to InfiniBand devices such as
`mlx5_0`. The broker resolves those devices through:

```text
/sys/class/infiniband/<ibdev>/device
```

Both discovery paths therefore produce normalized physical PCI addresses,
which form the join key. NICs reported by `nvidia-smi` but absent from LLDP rail
discovery are ignored because they are outside the configured data-plane
rails.

The design uses `topo -m` because the focused `topo -nic` command requires
R610, while the supported GPU Operator matrix includes pre-R610 drivers.

## Affinity Selection

Path classes are ranked closest-first:

| Rank | Class | Path |
|---:|---|---|
| 1 | `PIX` | At most one PCIe bridge |
| 2 | `PXB` | Multiple PCIe bridges without a host bridge |
| 3 | `PHB` | Crosses a PCIe host bridge |
| 4 | `NODE` | Crosses host bridges within one NUMA node |
| 5 | `SYS` | Crosses NUMA nodes |

For each GPU and rail, the broker:

1. Finds physical NICs assigned to that rail.
2. Looks up each NIC's path from the GPU.
3. Retains every NIC tied at the minimum path rank.
4. Sorts retained NICs by normalized PCI address.

Selection is performed independently per rail. A globally closest NIC may
belong only to one rail and must not suppress the best candidate on another.

Unknown path classes are rejected rather than assigned an implicit rank. If a
GPU has no qualified NIC candidate for a discovered rail, the entire node
update fails.

## Broker Behavior

A node is considered a GPU node when it has `nvidia.com/gpu.present=true`, a
positive allocatable `nvidia.com/gpu` resource, or an allocatable
`nvidia.com/mig-*` resource.

| Condition | Result |
|---|---|
| Non-GPU node | Publish NIC rails with an empty `gpus` object |
| Exactly one configured GPU Operator pod on a GPU node | Execute discovery |
| No GPU Operator pod on a GPU node | Fail startup |
| Multiple matching pods on one node | Fail as ambiguous |
| Command, parsing, PCI join, or affinity selection fails | Fail startup |
| `nvidia-smi` reports a NIC without an LLDP rail | Ignore that NIC |
| LLDP discovers no usable rail data | Skip GPU discovery and remove the node's ConfigMap entry |

The broker evaluates the configured DaemonSet's complete Kubernetes label
selector, including `matchLabels` and `matchExpressions`, and then retains only
Pods controlled by that DaemonSet. Unrelated Pods with overlapping labels are
not candidates for command execution.

LLDP data and GPU affinity are collected before the ConfigMap is patched, so a
successful patch replaces the complete node document atomically. The broker
serves `/healthz` only after node annotations and the ConfigMap update succeed,
preserving the node-observer readiness gate.

Empty LLDP rail data takes precedence over GPU discovery. Because there is no
rail affinity to publish, the broker removes any existing node entry without
requiring the GPU Operator pod or either `nvidia-smi` command to succeed. This
prevents stale rail mappings from surviving an LLDP attachment change.

The broker reconciles once at startup. `nodeUID` protects against node-name
reuse, but in-place hardware changes require a broker restart.

## Component Boundaries

- `pkg/providers/lldp` collects LLDP attachment, interface rails, and physical
  NIC identities.
- `pkg/deviceaffinity` parses GPU inventory and `topo -m`, normalizes PCI
  addresses, ranks paths, and selects preferred NICs.
- `internal/k8s` locates the same-node DaemonSet pod and supports pod exec.
- `cmd/node-data-broker` composes the discoveries and publishes the ConfigMap.

The LLDP provider does not depend on GPU Operator. Engines do not discover
hardware, and the canonical graph remains unchanged.

## Security

GPU mapping conditionally adds these node-data-broker permissions:

```yaml
- apiGroups: [apps]
  resources: [daemonsets]
  verbs: [get]
- apiGroups: [""]
  resources: [pods]
  verbs: [list]
- apiGroups: [""]
  resources: [pods/exec]
  verbs: [create]
```

ConfigMap patch permission remains restricted by `resourceNames`. Host
`/sys/class/net` and `/sys/class/infiniband` are mounted read-only. Commands are
passed as argument arrays without a shell or interpolated command text.

## Scale and Limitations

The shared ConfigMap has an approximately 1 MiB limit. Publishing only the
preferred NIC candidates rather than the complete GPU-by-NIC matrix reduces
growth, but large clusters can still exceed the limit. Representative
8-GPU/8-NIC nodes are expected to require roughly 1-3 KiB each, depending on
rail and tie counts. Larger deployments may require one object per node or a
custom resource.

`nvidia-smi topo -m` describes physical GPUs. MIG devices inherit their parent
GPU's affinity and are not published by MIG UUID. vGPU and passthrough
environments are outside the validation scope.

The topology matrix describes locality, not proof that GPUDirect RDMA is
configured or operational.

## Validation

Automated coverage includes:

- GPU inventory, topology matrix, NIC legend, and PCI normalization parsing;
- malformed, duplicate, incomplete, and unknown-path input;
- per-rail selection, ties, unmatched NICs, and missing candidates;
- GPU and non-GPU node behavior;
- empty-rail cleanup without GPU Operator discovery;
- atomic per-node ConfigMap patches and deterministic JSON;
- full DaemonSet selector and controller-ownership filtering;
- conditional Helm configuration, validation, RBAC, and sysfs mounts; and
- full Go, race-enabled Go, vet, Helm lint, and chart unit tests.

Hardware validation must cover a supported pre-R610 GPU Operator driver, a
multi-GPU/multi-rail node, equal-distance ties, a non-GPU node, MIG mode, and
additional non-rail or bonded NICs.

## Open Questions

1. Does the intended consumer allocate full GPU UUIDs, MIG UUIDs, PCI
   addresses, or CDI device names?
2. What maximum cluster size must the shared ConfigMap support?
