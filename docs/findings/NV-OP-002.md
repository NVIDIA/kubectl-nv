# NV-OP-002: an operand the operator is not running on the node

Severity: info. Component: the operand, or the deploy key when this
plugin has no name for it.

## Symptom

The node carries `nvidia.com/gpu.deploy.<operand>=false`, or a
`paused-for-something` value the operator writes while an upgrade or a
MIG reconfiguration is in flight. The operator will not run that operand
on this node until the label changes.

This is reported at info because an operand switched off is usually
somebody's decision rather than a fault. What it is for is the question
underneath most of the other findings: a reader looking at a node with
no GPUs wants to know whether the device plugin failed or was never
asked to start, and this is the finding that answers it.

Unlike the node rules, this one is not gated on whether the operator
targets the node. That gate's first question is whether every deploy
label is false, so gating this rule on it would silence the rule exactly
where it has the most to say.

A label of `true` is not the opposite of this finding. The operator
writes every `nvidia.com/gpu.deploy.<key>` as `true` on every node
carrying the NVIDIA PCI label, whatever the ClusterPolicy enables, so
`true` says only that the operator saw the node. Whether an operand was
asked for at all is a question for the ClusterPolicy's
`spec.<operand>.enabled`, which is what NV-DP-001 and NV-DCGM-001 read.

Every key the operator writes is reported, including the ones this
plugin has no component name for. The operator publishes more of them
than `pkg/inventory` maps and the list grows between versions, so an
unmapped key is reported under its own name rather than dropped: an
operand nobody can name is an operand nobody goes and looks at.

The keys seen in the GPU Operator source as of 2026-10-11 are `driver`,
`container-toolkit`, `device-plugin`, `gpu-feature-discovery`, `dcgm`,
`dcgm-exporter`, `operator-validator`, `mig-manager`,
`node-status-exporter`, `vgpu-manager`, `vgpu-device-manager`,
`vfio-manager`, `sandbox-device-plugin`, `sandbox-validator`,
`kata-manager`, `cc-manager`, `nvsm`, `dra-driver`, `dra-validator`,
`dcgm-dra`, `dcgm-exporter-dra` and `client`.

## Evidence

| Key | Meaning |
|---|---|
| `nvidia.com/gpu.deploy.<operand>` | the label and the value it carries, verbatim |

## Versions

Every GPU Operator version this plugin supports. The rule reads whatever
keys it finds rather than a fixed list, so a new operand needs no change
here.

## Documentation

- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html>
- <https://github.com/NVIDIA/gpu-operator>

## Next command

```shell
kubectl nv info <node>
```

## Reproduction

On a Mokka lane, switch one operand off for one worker.

```shell
kubectl label node <worker> nvidia.com/gpu.deploy.dcgm-exporter=false --overwrite
kubectl nv health --node <worker> --fail-on info
```

`--fail-on info` is what makes the run exit non-zero on it; at the
default threshold an info finding is printed and does not fail the run.
Put the label back with the value it had.

The exporter and not the device plugin: switching the device plugin off
takes the node's allocatable GPUs with it and reports NV-NODE-002 as
well, where the exporter's absence is only this finding.

`hack/mokka/faults/nv-op-002.sh inject|recover <worker>` scripts both
halves. Measured on a t4 lane on 2026-10-11: the finding appears on the
next run with no measurable delay, and clears within 56 seconds of the
label being restored, which is the operator putting the exporter pod
back.
