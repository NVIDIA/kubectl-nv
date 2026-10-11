# NV-NODE-001: the GPU Operator is not reconciling this node

Severity: warning. Component: `node`.

## Symptom

Node feature discovery found a PCI device with NVIDIA's vendor id on the
node and wrote `feature.node.kubernetes.io/pci-10de.present=true`, but
`nvidia.com/gpu.present` is absent.

That second label is written, and retracted, by the GPU Operator's own
ClusterPolicy controller as it decides which nodes it manages. So its
absence is a statement about the operator rather than about feature
discovery: the hardware is there and the operator has not claimed it.

The usual causes are a node selector in the ClusterPolicy that does not
cover the node, a taint the operands do not tolerate, and an operator
that has not finished its first reconcile.

The rule only fires where the operator is installed. On a cluster with
no operator nobody was ever going to write the label, so its absence
says nothing at all, and a node with four working GPUs and a device
plugin installed by hand would otherwise be reported as broken.

The rule also only looks at nodes the GPU Operator is expected to run
on. A node whose every `nvidia.com/gpu.deploy.*` label is false, or that
carries a taint no operand DaemonSet tolerates and runs no deploy-gated
operand, is left alone: see NV-NODE-002 for why that exclusion exists.

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

On a Mokka lane, remove the label the operator wrote from one worker.
The PCI label stays, because node feature discovery wrote that one and
node feature discovery is not an operand.

Stop the operator first, or it writes the label straight back.

```shell
kubectl -n gpu-operator scale deploy/gpu-operator --replicas=0
kubectl label node <worker> nvidia.com/gpu.present-
kubectl nv health --node <worker>
```

Recover by scaling the deployment back to 1 and waiting for the label to
return. Whether anything else restores it in the meantime is not
recorded here: E3 runs the injection against a live lane and writes down
what it observes, in
`hack/mokka/faults/nv-node-001.sh inject|recover <node>`.
