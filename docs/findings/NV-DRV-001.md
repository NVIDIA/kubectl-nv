# NV-DRV-001: a driver pod on the node is not ready or is restarting

Severity: error. Component: the driver pod's own `app` label, usually
`nvidia-driver-daemonset`.

## Symptom

A node runs an NVIDIA driver pod that is not reporting ready, or one
that is ready and has died within the last ten minutes. The driver
is what every other operand is waiting for: without it the toolkit
validation never passes, the device plugin never advertises anything,
and the node ends up in NV-NODE-002 with nothing saying why.

Two conditions, because they are different faults with the same cause.

The pod is not ready. A container in `CrashLoopBackOff` is never ready,
so a pod stuck in one fails this test whatever the pod phase says.

The pod is ready and has restarted inside the window. This is the driver
that comes up, runs for a minute and dies again, which a readiness check
sampled at the wrong moment calls healthy. It needs both halves: a
restart count on its own cannot tell "restarted ten minutes ago" from
"restarted in March", so the finding is raised only when the pod has
restarted at least once *and* its most recent container exit is inside
`DriverRestartWindow`, which is ten minutes. Ten, because the question
the window answers is "is this pod dying now": a driver that crashed
during this morning's rollout and has been up since is not a finding,
and a driver in `CrashLoopBackOff` produces a new exit every few minutes
at the longest back-off.

A restart that cannot be dated is not reported. The alternative is
reading an absent exit time as now, which reports every pod that ever
restarted.

### Hinted nodes are checked too

A node whose driver is broken does not stay a GPU node. The device
plugin's init container waits on toolkit-ready and never starts, so the
plugin never advertises `nvidia.com/gpu`, and the kubelet zeroes the
capacity of a resource whose plugin has gone. The node keeps its hint
labels and moves out of the GPU inventory into the hinted one, which is
to say that by the time this rule has something to report, the node is
no longer one a GPU-nodes-only rule would look at.

The Mokka control plane is not affected: it carries every GPU hint label
and every deploy label and runs no driver pod at all, so there is
nothing on it for this rule to match.

### During an upgrade it drops to info

While the node's `nvidia.com/gpu-driver-upgrade-state` says an upgrade
is in progress, that is, anything other than empty or `upgrade-done`,
the finding is reported at `info`. The upgrade controller takes the
driver down and puts it back on purpose, and an error there would make
every rolling driver upgrade in a fleet look like an outage. Whether the
upgrade itself is wedged is NV-UPG-001's finding.

The usual causes are a kernel the driver has no module for, a driver
container that cannot reach the compile toolchain on a node whose
kernel changed, a GPU the installed driver branch does not support, and
a host that already has a driver loaded.

### Which pods count as driver pods

The four selectors `pkg/mustgather` collects a bug report through:
`app=nvidia-driver-daemonset`,
`app.kubernetes.io/component=nvidia-driver`,
`app=nvidia-vgpu-manager-daemonset` and `openshift.driver-toolkit`. They
live in `pkg/inventory` as the single list, so a selector added there
reaches the collection and this rule at once.

The classification cannot be done on the pod's own `app` label, and this
is the trap. The GPU Operator names every DaemonSet the `NVIDIADriver`
CR produces `nvidia-<driverType>-driver-<osTag>-<hash>` and puts that
name in the pod's `app` label (`internal/state/driver.go`
`getDriverAppName`, and `manifests/state-driver/0500_daemonset.yaml`,
which sets `app` to `.Driver.AppName`); OpenShift's driver toolkit pods
carry a per-RHCOS name of their own. Neither is ever the literal
`nvidia-driver-daemonset`. `pkg/inventory` matches the selectors against
the pod's labels and reports the pod as `nvidia-driver-daemonset` or
`nvidia-vgpu-manager-daemonset` whatever it is called, which is what the
finding's Component then names.

What is deliberately not claimed is the vGPU host manager DaemonSet the
`NVIDIADriver` CR produces, which carries
`app.kubernetes.io/component=nvidia-vgpu-host-manager`. No selector in
`pkg/mustgather`'s list matches it either, so adding one would change
what a bug report collects.

### When the rule stays quiet

- The node runs no driver pod at all. Mokka runs simulated GPUs and no
  driver, and a node that should have one and does not is NV-OP-002's
  finding.
- No GPU Operator. It writes the deploy labels, and a driver installed
  by hand is nothing it promised to run.
- `nvidia.com/gpu.deploy.driver` or `nvidia.com/gpu.deploy.vgpu-manager`
  is false or paused for the node. The operator parked the operand on
  purpose, which is NV-OP-002's finding.
- A pod that is ready and whose last exit is outside the window, or
  which has never restarted, or whose last exit carries no time.

## Evidence

| Key | Meaning |
|---|---|
| `nvidia.com/gpu.deploy.driver` | whether the operator is currently running the driver on this node, absent when the node carries no such label. The vGPU manager carries `nvidia.com/gpu.deploy.vgpu-manager` instead |
| `operands` | the operand components running on the node, sorted and comma joined, absent when there are none |
| `pod` | the driver pod's name |
| `phase` | its pod phase |
| `ready` | `true` or `false`: `false` on the not-ready finding, `true` on the restarting one |
| `restarts` | the restart count summed over its containers, base 10 |
| `lastTerminationAt` | when the pod's containers last exited, RFC3339 in UTC. Present only on the restarting finding |
| `lastExitCode` | the code of that exit, base 10. Present only on the restarting finding |
| `restartWindow` | the window the exit was measured against, Go's duration form. Present only on the restarting finding |
| `nvidia.com/gpu-driver-upgrade-state` | present only when an upgrade is in progress, which is why the finding is at `info` |

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
