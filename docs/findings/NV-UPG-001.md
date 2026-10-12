# NV-UPG-001: a driver upgrade failed or is stuck on the node

Severity: warning, and see below. Component: `node`.

## Symptom

The node carries `nvidia.com/gpu-driver-upgrade-state` saying either
that its driver upgrade failed, or that it is part way through one and
has not moved for longer than `--upgrade-stuck-after`.

The label is written by the GPU Operator's upgrade controller as it
walks a node through an upgrade. Three of its states are ones a node can
sit in indefinitely when whatever the controller is waiting for never
happens:

| State | What the controller is waiting for |
|---|---|
| `cordon-required` | the node to be made unschedulable |
| `pod-restart-required` | the GPU pods on it to go away and come back |
| `validation-required` | the new driver to pass validation |

A fourth, `upgrade-failed`, is terminal: the controller tried and could
not.

### Three severities, one per quality of evidence

The label carries no transition time. A node that entered
`cordon-required` a minute ago and one that entered it last Tuesday
carry exactly the same label, and an upgrade in progress is not a fault.
So the duration is read from the event the upgrade controller recorded
when it set the state, and a node with no such event is reported at
`info` rather than given a duration nobody measured.

| Case | Severity |
|---|---|
| `upgrade-failed` | error |
| a stuck state, with an event dating it past the window | warning |
| a stuck state, with no event to date it | info |

The event is matched on three things at once, and each one of them has
silenced a wrong answer: its involved object is this node, its reason is
`GPUDriverUpgrade`, and its message ends with the state. An event about
another node would date the state from somebody else's upgrade, an event
from another controller would date it from the kubelet's last
heartbeat, and the event announcing the state the node passed through on
its way here is older than the one that matters. The newest match wins.

The reason and the message shape are the upgrade controller's own:
`strings.ToUpper(DriverName) + "DriverUpgrade"` and
`"Successfully updated node state label to %s"`, with `DriverName` being
`gpu` for this label (k8s-operator-libs `pkg/upgrade/util.go:158`,
`pkg/upgrade/node_upgrade_state_provider.go:129`, `pkg/upgrade/consts.go`).

### When the rule stays quiet

- Any other state, the empty label included. `upgrade-done` is the
  ordinary condition of every node of a cluster that is not upgrading,
  and the remaining states are steps the controller drives itself within
  seconds. Reporting those would be reporting every node of every
  cluster mid-upgrade.
- A stuck state entered more recently than `--upgrade-stuck-after`. That
  is an upgrade, not a stuck node.

## Evidence

| Key | Meaning |
|---|---|
| `nvidia.com/gpu-driver-upgrade-state` | the label value, verbatim |
| `upgradeStuckAfter` | the window the state was measured against, as `--upgrade-stuck-after` was given: Go's duration form, `30m0s` |
| `since` | when the upgrade controller announced this state, RFC3339 in UTC, absent when no event dates it |
| `for` | how long ago that was, truncated to the second, absent for the same reason |

## Versions

Every GPU Operator version this plugin supports.

## Documentation

- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/gpu-driver-upgrades.html>
- <https://github.com/NVIDIA/k8s-operator-libs/blob/main/pkg/upgrade/consts.go>

## Flag

`--upgrade-stuck-after` is the window, and it defaults to `30m0s`. It
must be positive: a window of zero would report every node the instant it
entered an upgrade step and a negative one every node always, so either
is rejected as a usage error, exit 2, before the cluster is read.

## Next command

```shell
kubectl nv info <node>
```

## Reproduction

E3 scripts both halves as
`hack/mokka/faults/nv-upg-001.sh inject|recover <node>`: it labels the
node `nvidia.com/gpu-driver-upgrade-state=upgrade-failed` and removes
the label again on recover.

`upgrade-failed` and not one of the stuck states. That value is reported
at error and needs nothing dated, where the stuck states are measured
against `--upgrade-stuck-after` and an event saying when the node
entered them, and a lane that runs no upgrade controller writes no such
event.

Measured on a t4 lane on 2026-10-12: the finding appears within 1 second
and clears within 3.
