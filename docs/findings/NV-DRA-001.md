# NV-DRA-001: the node advertises its GPUs twice over

Severity: warning. Component: `node`.

## Symptom

The node advertises `nvidia.com/gpu` through the device plugin and
publishes a `gpu.nvidia.com` ResourceSlice through the DRA driver at the
same time. `kubectl nv list` shows it in mode `both`.

The two advertisements are independent. The device plugin puts
`nvidia.com/gpu` into the node's allocatable and the kubelet schedules
against it; the DRA driver publishes a ResourceSlice and the scheduler
allocates claims against that. Neither knows about the other, so the
node can hand the same physical device to a pod holding a claim and to a
pod holding an extended resource, and the second one to start finds a
device already in use.

It is a warning rather than an error because this is also exactly what a
migration looks like mid-flight: a cluster moving from the device plugin
to DRA passes through it on purpose, node by node. What the finding says
is that the node has not finished moving, not that it is broken.

The usual causes are a device plugin DaemonSet still selecting a node
that has been moved to DRA, and a `nvidia.com/gpu.deploy.device-plugin`
label that was never set to false on the nodes the DRA driver took over.

### When the rule stays quiet

- A node in one mode. `device-plugin`, `dra` and `none` are each a node
  doing exactly one thing, which is the design.
- Hinted nodes.

## Evidence

| Key | Meaning |
|---|---|
| `mode` | always `both`, so that the evidence says which of the four modes was read |
| `capacity` | the `nvidia.com/gpu` capacity the device plugin advertises, base 10 |
| `allocatable` | the `nvidia.com/gpu` allocatable, base 10 |
| `resourceSlices` | the `gpu.nvidia.com` ResourceSlices published for the node, sorted and comma joined, absent when the run did not read them |

## Versions

GPU Operator v25.3.0 and above, the first release that ships the NVIDIA
DRA driver: below it no node can be in both modes. An unknown operator
version still runs the rule.

## Documentation

- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/gpu-operator-dra.html>
- <https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/>

## Next command

```shell
kubectl nv info <node>
```

## Reproduction

E3 scripts both halves as
`hack/mokka/faults/nv-dra-001.sh inject|recover <node>`: it publishes a
`gpu.nvidia.com` ResourceSlice naming a node that already advertises
`nvidia.com/gpu`, so the node reads as mode `both`, and deletes the
slice again on recover.
