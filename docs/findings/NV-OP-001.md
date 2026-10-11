# NV-OP-001: the ClusterPolicy is not ready

Severity: error. Component: `gpu-operator`.

## Symptom

A ClusterPolicy's `status.state` is something other than `ready`. The
ClusterPolicy is the GPU Operator's own verdict on itself, so this is
the one check that infers nothing from labels or pods: the operator says
it has not got everything it was asked for running.

A policy that is not ready usually explains the node findings underneath
it. Read this one first.

The finding is cluster scoped and names no node, so `--node` does not
hide it. A node reported clean while the policy it depends on is broken
is a false clean, and a false clean is the worst answer this tool can
give.

The state is matched without regard to case, because it is a free-form
string the operator writes and nothing guarantees its spelling. A policy
with no status at all is a policy the operator has not reconciled yet,
which is this finding too: the `status.state` key is then left out of
the evidence rather than written as an empty string.

A cluster with no ClusterPolicy produces nothing here. The operator not
being installed is an answer rather than a failure.

## Evidence

| Key | Meaning |
|---|---|
| `name` | the ClusterPolicy's name |
| `status.state` | the state the operator reported, absent when the policy has no status yet |

## Versions

Every GPU Operator version this plugin supports.

## Documentation

- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html>
- <https://github.com/NVIDIA/gpu-operator>

## Next command

```shell
kubectl describe clusterpolicy <name>
```

## Reproduction

On a Mokka lane, make one operand unschedulable and the policy leaves
`ready` while the operator waits for it.

```shell
kubectl -n gpu-operator patch clusterpolicy cluster-policy --type=merge \
  -p '{"spec":{"devicePlugin":{"nodeSelector":{"nv.example/absent":"yes"}}}}'
kubectl nv health
```

Remove the node selector to put it back. This writes to the lane, so it
belongs in a lane fixture and never in anything `kubectl nv` itself
runs: the plugin is read-only toward a cluster.
