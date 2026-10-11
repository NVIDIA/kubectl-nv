# NV-DCGM-001: dcgm-exporter is missing or not ready where it should run

Severity: warning. Component: `nvidia-dcgm-exporter`.

## Symptom

A GPU node whose `nvidia.com/gpu.deploy.dcgm-exporter` label says the
exporter belongs there has no dcgm-exporter pod, or has one that is not
ready. The node still runs GPU work; it runs it unobserved, so nothing
is collecting utilisation, temperature or error counters from it, and a
Prometheus target is quietly empty.

A warning rather than an error for that reason: the workloads are fine
and the view of them is not.

### Why this rule needs the label and NV-DP-001 does not

dcgm-exporter is optional. Plenty of installs switch it off in the chart
values, and this repository's own Mokka lane switches off the standalone
host engine, the container toolkit, the mig-manager and the node status
exporter exactly that way. So the rule waits for the node's own deploy
label to say the operand belongs there before its absence is worth
reporting. The device plugin needs no such evidence, because it is what
makes a GPU node a GPU node.

The cost of the stricter test is stated plainly: on a cluster whose
operator writes no deploy label for an exporter it does run, a dead
exporter goes unreported here. The alternative is a warning on every
cluster that chose not to run one, and a check that fires on a healthy
cluster is a check nobody reads.

The rule also stays quiet when the GPU Operator is not installed, when
the label is false or paused, and on hinted nodes, which advertise no
GPUs to collect metrics about.

## Evidence

| Key | Meaning |
|---|---|
| `nvidia.com/gpu.deploy.dcgm-exporter` | what the operator was told to do with the operand |
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
cluster. E2 automates it.

## Next command

```shell
kubectl nv info <node>
```
