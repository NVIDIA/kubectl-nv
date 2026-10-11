# NV-DP-001: the device plugin is missing or not ready on a GPU node

Severity: error. Component: `nvidia-device-plugin-daemonset`.

## Symptom

A GPU node has no device plugin pod, or has one that is not reporting
ready. The device plugin is what puts `nvidia.com/gpu` into a node's
allocatable, so a GPU node without a working one is a node on its way to
NV-NODE-002. This rule is what says why.

The usual causes are a validator that never passed, so the operator
never started the plugin; a plugin crash-looping against a driver that
did not load; and a configuration the plugin rejects.

### When the rule stays quiet

- No GPU Operator. It writes the deploy labels, so a cluster without it
  has none, and a device plugin installed by hand is nothing the
  operator promised to run.
- `nvidia.com/gpu.deploy.device-plugin` is false or paused for the node.
  That is NV-OP-002's finding, and the absence is then expected.
- The node advertises its GPUs through dynamic resource allocation
  alone. Such a node publishes a `gpu.nvidia.com` ResourceSlice and runs
  a kubelet plugin instead, so a missing device plugin there is the
  design. A node running both, which is what a migration looks like
  mid-flight, still needs one.
- Hinted nodes. A node that advertises no GPUs at all is NV-NODE-002.

## Evidence

| Key | Meaning |
|---|---|
| `nvidia.com/gpu.deploy.device-plugin` | what the operator was told to do with the operand, absent when the node carries no such label |
| `operands` | the operand components running on the node, sorted and comma joined, absent when there are none |
| `pod` | the device plugin pod's name, absent when there is no pod |
| `phase` | its pod phase |
| `ready` | `true` or `false` |
| `restarts` | the restart count summed over its containers, base 10 |

## Versions

Every GPU Operator version this plugin supports.

## Documentation

- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html>
- <https://github.com/NVIDIA/k8s-device-plugin>

## Next command

```shell
kubectl nv info <node>
```

## Reproduction

The state to produce is a GPU node whose device plugin pod is gone while
the node still has GPU capacity. Deleting the pod is not enough: the
DaemonSet puts it straight back, and the node keeps its capacity in the
meantime.

Switching the operand off with the deploy label removes the pod but also
suppresses this rule on purpose, so that path reproduces NV-NODE-002 and
NV-OP-002 instead. A reproduction that leaves the capacity in place
needs the plugin container held unready rather than the pod removed,
which is why this one is a description of the state and not a command
line. E2 automates it.
