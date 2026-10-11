# NV-NODE-002: a GPU node that advertises no allocatable GPUs

Severity: error. Component: `node`.

## Symptom

The node carries `nvidia.com/gpu.present=true` and its allocatable
`nvidia.com/gpu` is zero or absent. Nothing the scheduler places will
ever land a GPU workload here, and nothing in the node's labels says so.

This is the most common thing `kubectl nv health` is run to find. The
usual causes are a device plugin pod that is not running or not ready, a
driver that failed to load, and a validator that never passed so the
device plugin never started.

### Why it does not fire on a control plane

A GPU hint label is written by a DaemonSet and never retracted, so a
node can carry `nvidia.com/gpu.present=true` forever while no operand
will ever land on it. On this repository's own Mokka lane that is
exactly the control plane: the simulated-GPU DaemonSet tolerates every
taint, so node feature discovery labels the control plane, and every GPU
Operator operand is kept off it by a taint none of them tolerate.

The rule therefore evaluates a node only where the operator is expected
to run. Three questions decide it, in this order:

1. Did the operator say not to? Every `nvidia.com/gpu.deploy.*` label
   false or paused means the operator was told to leave the node alone.
1. Is an operand already running there? That settles it, whatever the
   taints say.
1. Would every operand be blocked? A `NoSchedule` or `NoExecute` taint
   that no operand toleration matches keeps all of them off.

A node running MIG in the mixed strategy is excluded too. It advertises
one extended resource per profile and no `nvidia.com/gpu` at all, so a
zero whole-GPU allocatable is what a healthy mixed node looks like; its
real numbers are in `mig.resources`.

## Evidence

| Key | Meaning |
|---|---|
| `nvidia.com/gpu.present` | the hint label that fired, always `true` |
| `capacity` | the `nvidia.com/gpu` capacity, base 10 |
| `allocatable` | the `nvidia.com/gpu` allocatable, base 10, always `0` |
| `hinted` | `true` when the node advertises no GPUs at all, `false` when it has capacity but nothing allocatable |
| `operands` | the operand components running on the node, sorted and comma joined, absent when there are none |

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

On a Mokka lane, switch the device plugin off for one worker. The
operator drains the plugin from that node, its allocatable GPUs go to
zero, and GPU feature discovery keeps the hint label in place.

```shell
kubectl label node <worker> nvidia.com/gpu.deploy.device-plugin=false --overwrite
kubectl nv health --node <worker>
```

Only that one label changes, so the node is still one the operator
targets and the rule still evaluates it. Put it back with
`nvidia.com/gpu.deploy.device-plugin=true`.
