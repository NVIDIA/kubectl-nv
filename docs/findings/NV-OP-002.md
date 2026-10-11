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
Put the label back with `=true`.
