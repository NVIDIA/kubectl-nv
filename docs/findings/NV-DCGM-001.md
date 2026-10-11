# NV-DCGM-001: dcgm-exporter is missing or not ready where it should run

Severity: warning. Component: `nvidia-dcgm-exporter`.

## Symptom

A GPU node whose ClusterPolicy enables dcgm-exporter has no
dcgm-exporter pod, or has one that is not ready. The node still runs GPU
work; it runs it unobserved, so nothing is collecting utilisation,
temperature or error counters from it, and a Prometheus target is
quietly empty.

A warning rather than an error for that reason: the workloads are fine
and the view of them is not.

### Where "should run" comes from

Not from the deploy label. The operator writes
`nvidia.com/gpu.deploy.dcgm-exporter=true` on every node carrying the
NVIDIA PCI label whatever the ClusterPolicy enables, so the label says
only that the operator saw the node.

dcgm-exporter is optional, and plenty of installs switch it off in their
chart values: this repository's own Mokka lane switches off the
standalone host engine, the container toolkit, the mig-manager and the
node status exporter exactly that way. Reading the label as consent
would warn on every node of every one of those clusters.

So the rule asks both questions, and they are different ones.
`spec.dcgmExporter.enabled` in the ClusterPolicy says the cluster asked
for an exporter at all; the node's deploy label says whether the
operator is currently running it there, which is how a node parked
mid-upgrade stays quiet. A missing `enabled` field means enabled, which
is the CRD's own default.

The rule also stays quiet when the GPU Operator is not installed and on
hinted nodes, which advertise no GPUs to collect metrics about.

## Evidence

| Key | Meaning |
|---|---|
| `nvidia.com/gpu.deploy.dcgm-exporter` | whether the operator is currently running the operand on this node, absent when the node carries no such label |
| `operands` | the operand components running on the node, sorted and comma joined, absent when there are none |
| `pod` | the exporter pod's name, absent when there is no pod |
| `phase` | its pod phase |
| `ready` | `true` or `false` |
| `restarts` | the restart count summed over its containers, base 10 |

## Versions

Every GPU Operator version this plugin supports.

## Documentation

- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html>
- <https://github.com/NVIDIA/dcgm-exporter>

## Reproduction

On a Mokka lane, point the exporter DaemonSet at a node selector nothing
matches, leaving the deploy labels alone. The operator still says the
exporter belongs on the node and no pod is there.

```shell
kubectl -n gpu-operator patch daemonset nvidia-dcgm-exporter --type=merge \
  -p '{"spec":{"template":{"spec":{"nodeSelector":{"nv.example/absent":"yes"}}}}}'
kubectl nv health --fail-on warning
```

That is a write to the lane, so it belongs in a lane fixture and never
in anything `kubectl nv` itself runs: the plugin is read-only toward a
cluster. Recover by removing the node selector again.

E3 scripts both halves as
`hack/mokka/faults/nv-dcgm-001.sh inject|recover <node>`.

## Next command

```shell
kubectl nv info <node>
```
