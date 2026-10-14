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

`hack/mokka/faults/nv-node-001.sh inject|recover <worker>` does this
against a Mokka lane, and does three things rather than two.

Removing the label with the operator running is not enough: on a t4 lane
on 2026-10-11 the ClusterPolicy controller wrote it back within five
seconds. Scaling the operator to zero first keeps the label off, and
then runs into the other half of the rule, which only fires where the
operator is installed. `kubectl nv health` reads "installed" as "a pod
matches `app=gpu-operator`", so with the Deployment at zero replicas the
rule returns nothing at all: the same lane, with the Deployment drained
and the label gone, reported 0 errors, 0 warnings and 0 infos.

So the script scales the Deployment to zero, waits for its pods to go,
plants a stand-in pod carrying `app=gpu-operator` and the operator's own
image, and then clears the label. That is the fault this code is about:
an operator that is present and is claiming nothing.

```shell
kubectl -n gpu-operator scale deploy/gpu-operator --replicas=0
# wait for the operator pods to go, then plant a stand-in pod labelled
# app=gpu-operator in the same namespace
kubectl label node <worker> nvidia.com/gpu.present-
kubectl nv health --node <worker>
```

Observed on a t4 lane on 2026-10-11: the finding appears within 3
seconds of the label going, and clears within 21 seconds of the label
being written back and the Deployment scaled up again. The PCI label is
untouched throughout, because node feature discovery wrote that one and
node feature discovery is not an operand.
