# NV-NODE-003: fewer allocatable GPUs than the node has

Severity: warning. Component: `node`.

## Symptom

The node's allocatable `nvidia.com/gpu` is above zero and below its
capacity. Some devices are present and not placeable: the kubelet has
taken them out of the allocatable pool, usually because the device
plugin reported them unhealthy, or because a system reserved or kube
reserved setting holds some back.

Allocatable zero is NV-NODE-002 rather than this rule, so the two
partition the space and a node with no placeable GPUs produces one
finding instead of two.

This rule reads the whole-GPU numbers only. A node running MIG in the
mixed strategy advertises one extended resource per profile and no
`nvidia.com/gpu`, so its per-profile shortfall is not covered here.

## Evidence

| Key | Meaning |
|---|---|
| `capacity` | the `nvidia.com/gpu` capacity, base 10 |
| `allocatable` | the `nvidia.com/gpu` allocatable, base 10 |
| `missing` | capacity minus allocatable, base 10 |

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

The state to produce is a node whose device plugin advertises a capacity
the kubelet does not fully offer: mark one simulated device unhealthy on
a Mokka worker and let the device plugin re-advertise. Reserving a GPU
through the kubelet's `--system-reserved` produces the same split on a
real node.

Both paths need the lane's device simulation driven directly rather than
a label edit, which is why this reproduction is a description of the
state and not a command line. E2 automates it.
