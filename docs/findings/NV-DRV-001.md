# NV-DRV-001: a driver pod on the node is not ready

Severity: error. Component: the driver pod's own `app` label, usually
`nvidia-driver-daemonset`.

## Symptom

A node runs an NVIDIA driver pod that is not reporting ready. The driver
is what every other operand is waiting for: without it the toolkit
validation never passes, the device plugin never advertises anything,
and the node ends up in NV-NODE-002 with nothing saying why.

A pod in `CrashLoopBackOff` is this finding. A container in that state
is never ready, so it fails the readiness test whatever the pod phase
says, and no separate condition is needed for it.

The usual causes are a kernel the driver has no module for, a driver
container that cannot reach the compile toolchain on a node whose
kernel changed, a GPU the installed driver branch does not support, and
a host that already has a driver loaded.

Which pods count as driver pods is the list `pkg/mustgather` collects a
bug report through: `app=nvidia-driver-daemonset`,
`app.kubernetes.io/component=nvidia-driver`,
`app=nvidia-vgpu-manager-daemonset` and `openshift.driver-toolkit`.
OpenShift builds one DaemonSet per RHCOS version and names it
`nvidia-driver-daemonset-<version>-<rhcos>`, so those pods are matched
on the base name plus a hyphen. A driver this rule did not recognise
would be a driver nobody collects a bug report from either, so the two
lists are held together by a test rather than by a comment.

### When the rule stays quiet

- The node runs no driver pod at all. Mokka runs simulated GPUs and no
  driver, and a node that should have one and does not is NV-OP-002's
  finding.
- No GPU Operator. It writes the deploy labels, and a driver installed
  by hand is nothing it promised to run.
- `nvidia.com/gpu.deploy.driver` or `nvidia.com/gpu.deploy.vgpu-manager`
  is false or paused for the node. The operator parked the operand on
  purpose, which is NV-OP-002's finding.
- Hinted nodes, which is NV-NODE-002's business.
- A pod that is ready now and has restarted. `inventory.OperandPod`
  carries a restart count and no termination time, so "restarted ten
  minutes ago" and "restarted in March" are the same input here, and a
  rule firing on a non-zero count would report every long lived node in
  the cluster for ever. The count is in the evidence of the findings
  that do fire. Dating the last termination needs a field on the operand
  model, which this rule cannot add on its own.

## Evidence

| Key | Meaning |
|---|---|
| `nvidia.com/gpu.deploy.driver` | whether the operator is currently running the driver on this node, absent when the node carries no such label. The vGPU manager carries `nvidia.com/gpu.deploy.vgpu-manager` instead |
| `operands` | the operand components running on the node, sorted and comma joined, absent when there are none |
| `pod` | the driver pod's name |
| `phase` | its pod phase |
| `ready` | `true` or `false`, always `false` here |
| `restarts` | the restart count summed over its containers, base 10 |

## Versions

Every GPU Operator version this plugin supports.

## Documentation

- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html>
- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/troubleshooting.html>

## Next command

```shell
kubectl nv info <node>
```

## Reproduction

E3 scripts both halves as
`hack/mokka/faults/nv-drv-001.sh inject|recover <node>`: it puts a
driver DaemonSet on the lane whose image tag does not exist, so the pod
lands on the node and never reaches ready while the deploy label still
says the driver belongs there, and takes it away again on recover.
