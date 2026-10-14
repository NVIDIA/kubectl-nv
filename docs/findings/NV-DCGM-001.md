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

On a Mokka lane, point the exporter at an image version that does not
exist, through the ClusterPolicy, and then delete the exporter pod on
the node under test.

```shell
kubectl patch clusterpolicy cluster-policy --type=merge \
  -p '{"spec":{"dcgmExporter":{"version":"v0.0.0-absent"}}}'
kubectl -n gpu-operator delete pod -l app=nvidia-dcgm-exporter \
  --field-selector spec.nodeName=<node>
kubectl nv health --fail-on warning
```

Through the ClusterPolicy and not the DaemonSet. An earlier draft of
this entry patched a node selector onto `nvidia-dcgm-exporter`
directly; the operator owns that DaemonSet and reconciles it, so an
injection made there is undone on its own schedule and the test passes
or fails on timing.

The pod deletion is what puts the fault on the node that was asked for.
A DaemonSet rolling update takes one node at a time and stops at the
first replacement that never becomes ready, so the patch alone breaks
whichever node the controller reached first: on the first live run that
was the other worker, while the node under test kept its healthy pod for
the whole six minutes. A node with no pod gets one from the template as
it stands now, whatever the rollout is waiting on.

That is a write to the lane, so it belongs in a lane fixture and never
in anything `kubectl nv` itself runs: the plugin is read-only toward a
cluster. Recover by writing the saved version back and deleting the
broken pod.

`hack/mokka/faults/nv-dcgm-001.sh inject|recover <node>` scripts both
halves. Measured on a t4 lane on 2026-10-11: the finding appears within
5 seconds and clears within 58 seconds, the exporter's own image pull
being most of the second number.

## Next command

```shell
kubectl nv info <node>
```
