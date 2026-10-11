# NV-NODE-001: an NVIDIA PCI device with no GPU labels

Severity: warning. Component: `node`.

## Symptom

Node feature discovery found a PCI device with NVIDIA's vendor id on the
node and wrote `feature.node.kubernetes.io/pci-10de.present=true`, but
`nvidia.com/gpu.present` is absent. That second label is written by GPU
feature discovery, which is a GPU Operator operand, so its absence means
the operator has not reached this node: the hardware is there and
nothing has claimed it.

The usual causes are a GPU feature discovery pod that never started on
the node, a node selector or taint that keeps the operand off it, and an
operator installed into a namespace that does not cover the node.

The rule only looks at nodes the GPU Operator is expected to run on. A
node whose every `nvidia.com/gpu.deploy.*` label is false, or that
carries a taint no operand DaemonSet tolerates, is left alone: see
NV-NODE-002 for why that exclusion exists.

## Evidence

| Key | Meaning |
|---|---|
| `feature.node.kubernetes.io/pci-10de.present` | the hint label that fired, always `true` |
| `operands` | the operand components running on the node, sorted and comma joined, absent when there are none |

## Versions

Every GPU Operator version this plugin supports.

## Documentation

- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html>
- <https://github.com/kubernetes-sigs/node-feature-discovery>

## Next command

```shell
kubectl nv info <node>
```

## Reproduction

On a Mokka lane, remove the label GPU feature discovery wrote from one
worker. The PCI label stays, because node feature discovery wrote that
one.

```shell
kubectl label node <worker> nvidia.com/gpu.present-
kubectl nv health --node <worker>
```

GPU feature discovery rewrites the label on its next pass, so the
finding clears on its own.
