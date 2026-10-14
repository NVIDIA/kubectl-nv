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
- The ClusterPolicy sets `spec.devicePlugin.enabled: false`. That is the
  cluster saying it never wanted one. A missing field means enabled,
  which is the CRD's own default.
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
| `nvidia.com/gpu.deploy.device-plugin` | whether the operator is currently running the operand on this node, absent when the node carries no such label. A value of `true` means the operator saw the node, not that the cluster asked for the operand: `spec.devicePlugin.enabled` is what says that |
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

On a Mokka lane, point the device plugin at an image version that does
not exist, then delete the plugin pod on the node under test. The
replacement never pulls, so it stays unready while the deploy label and
the ClusterPolicy both still say the operand belongs there.

```shell
kubectl patch clusterpolicy cluster-policy --type=merge \
  -p '{"spec":{"devicePlugin":{"version":"v0.0.0-absent"}}}'
kubectl -n gpu-operator delete pod -l app=nvidia-device-plugin-daemonset \
  --field-selector spec.nodeName=<node>
kubectl nv health
```

ClusterPolicy is cluster scoped, so that command carries no `-n`. Save
`spec.devicePlugin.version` before the patch and write the saved value
back to recover.

The pod deletion is not optional. A DaemonSet rolling update takes one
node at a time and stops at the first replacement that never becomes
ready, so the patch on its own breaks whichever node the controller
reached first: on the first live run against this lane that was the
other worker, and the node under test kept its healthy pod for the whole
six minutes the run polled for the finding.

The same patch reproduces NV-OP-001, because the policy leaves `ready`
while an operand it enables is not. Switching the operand off with the
deploy label instead would suppress this rule on purpose, and reproduce
NV-NODE-002 and NV-OP-002 rather than this one.

`hack/mokka/faults/nv-dp-001.sh inject|recover <node>` scripts both
halves. Measured on a t4 lane on 2026-10-11: the finding appears within
5 seconds and clears within 11 seconds.
