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

### The ignored duplicate, reported at warning

The operator reconciles exactly one ClusterPolicy and marks every other
one ignored, and it does that in memory: the ignored objects never get a
status at all. So a ClusterPolicy with no status sitting beside one that
is `ready` is a duplicate somebody left in the cluster, not an operator
that failed, and this entry reports it at warning with
`ignoredPolicy: true` rather than at error. Failing a run over an object
that decides nothing would be failing a cluster that works.

A duplicate that does carry a state the operator does not call `ready`
is a real failure and stays an error, and so does a statusless policy
when nothing else is ready: the exclusion is about the absent status
beside a ready one, not about there being more than one policy.

## Evidence

| Key | Meaning |
|---|---|
| `name` | the ClusterPolicy's name |
| `status.state` | the state the operator reported, absent when the policy has no status yet |
| `reconciling` | the name of the ready policy, on an ignored duplicate only |
| `ignoredPolicy` | `true`, on an ignored duplicate only |

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

On a Mokka lane, point the device plugin at an image tag that does not
exist. The operand never becomes ready, so the policy leaves `ready`.

```shell
kubectl patch clusterpolicy cluster-policy --type=merge \
  -p '{"spec":{"devicePlugin":{"version":"v0.0.0-absent"}}}'
kubectl nv health
```

ClusterPolicy is cluster scoped, so the command carries no `-n`. Save
`spec.devicePlugin.version` before the patch and write the saved value
back to recover.

This writes to the lane, so it belongs in a lane fixture and never in
anything `kubectl nv` itself runs: the plugin is read-only toward a
cluster. E3 scripts both halves as
`hack/mokka/faults/nv-op-001.sh inject|recover <node>`.
