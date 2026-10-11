# NV-MIG-001: the node's MIG configuration has not been applied

Severity: warning, and see below. Component: `nvidia-mig-manager` for
the configuration half, `node` for the strategy half.

## Symptom

Two faults under one code, because a reader looking at a node whose MIG
is wrong does not yet know which of the two it is.

### The configuration was not applied

`nvidia.com/mig.config` names a profile and
`nvidia.com/mig.config.state` is not `success`.

| State | Severity | What it means |
|---|---|---|
| `failed` | error | the mig-manager tried and could not |
| `pending` | warning | it has not started yet |
| `rebooting` | warning | the node is being restarted for it |
| absent | warning | the mig-manager has not picked the label up |
| anything else | warning | a state this plugin has not seen |

The transitional ones are warnings because they are states a node passes
through: the reader is being told to look, not that the node is lost.

### The strategy cannot be serving what the node advertises

The three strategies publish different things, and getting this
backwards is the easy mistake:

| Strategy | What the node advertises | Fires when |
|---|---|---|
| `single` | every partition as `nvidia.com/gpu`, and no `nvidia.com/mig-*` at all | the node advertises a `nvidia.com/mig-*` resource with capacity above zero, which its own strategy does not serve |
| `mixed` | one `nvidia.com/mig-<profile>` per partition, and no `nvidia.com/gpu` | a partitioning config was applied and the node advertises no `nvidia.com/mig-*` with capacity above zero, so there is nothing a MIG workload can ask for |
| `none` | whole GPUs | never |

`none` on MIG-capable hardware is the Mokka main lane, gb300 with
`nvidia.com/mig.capable=true` and strategy `none`, and it is healthy:
nothing was asked for and nothing is wrong.

### Only a profile with capacity above zero counts

The kubelet does not drop a removed extended resource from a node's
status, it keeps it at zero. A node partitioned last week and running
whole GPUs today still carries its old `nvidia.com/mig-1g.10gb` entry
for ever, and counting that leftover reports a working single-strategy
node as advertising profiles its strategy does not serve. The same
predicate decides NV-NODE-002's "this node's zero `nvidia.com/gpu`
capacity is legitimate", so the two cannot disagree about what a
mixed-strategy node is.

### Hinted nodes are checked too

A node the mig-manager is reconfiguring advertises its profiles at zero
for a while, which is not GPU-node membership, so it is in the hinted
inventory exactly while its configuration is the thing that is wrong.
The Mokka control plane carries no MIG labels at all.

### When the rule stays quiet

- A healthy partitioned node of either strategy. A `single` node with
  `success` and whole-GPU capacity, and a `mixed` node with `success`
  and its `nvidia.com/mig-*` resources, both give no finding.
- `nvidia.com/mig.capable` is not `true`. Hardware that cannot do MIG
  cannot be misconfigured for it.
- `nvidia.com/mig.config` is `all-disabled`. That asks for no
  partitions, so a `mixed` node advertising none is doing exactly what
  it was told.
- `nvidia.com/mig.config` is absent. A node nobody has configured has
  nothing to disagree with.
- A strategy this plugin has not seen. That is not a claim about what
  the node advertises, and guessing would report a working node as
  broken.
- The configuration half already fired. A node mid-change is allowed to
  have resources that disagree with its strategy, and reporting both
  would be reporting the same fault twice.
- The node's GPUs are set up for virtual machines
  (`nvidia.com/gpu.workload.config` is `vm-passthrough` or `vm-vgpu`),
  and nodes the operator does not target.
- Hinted nodes.

## Evidence

| Key | Meaning |
|---|---|
| `nvidia.com/mig.capable` | `true` or `false` |
| `nvidia.com/mig.strategy` | `none`, `single` or `mixed`, absent when the label is not there |
| `nvidia.com/mig.config` | the profile the mig-manager was asked for, absent when the label is not there |
| `nvidia.com/mig.config.state` | where it got to, absent when the label is not there, which is itself the finding |
| `migResources` | the `nvidia.com/mig-*` resources the node advertises, sorted and comma joined, absent when there are none. Strategy half only |
| `capacity` | the `nvidia.com/gpu` capacity, base 10. Strategy half only |

## Versions

Every GPU Operator version this plugin supports.

## Documentation

- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/gpu-operator-mig.html>
- <https://docs.nvidia.com/datacenter/tesla/mig-user-guide/>

## Next command

```shell
kubectl nv info <node>
```

## Reproduction

E3 scripts both halves as
`hack/mokka/faults/nv-mig-001.sh inject|recover <node>`: it labels the
node `nvidia.com/mig.config` with a profile and
`nvidia.com/mig.config.state=failed`, and restores the labels it saved
on recover.

E4 runs the other direction on the MIG lane: a real partitioned node,
which must give no finding at all.
