# NV-TOPO-001: the node is missing Topograph tiers its peers carry

Severity: info. Component: `node`.

Informational until the Phase 2 false-positive baseline. The finding is
real, and nothing yet says how often a healthy fleet produces one; a rule
that fails a pipeline before anybody has measured that is a rule
operators switch off rather than read. The severity is raised once the
baseline says what a clean cluster looks like.

## Symptom

Some GPU nodes carry Topograph's fabric tier labels and this one does
not carry all of them. Carrying none of them is the common case and is
reported: that is usually how Topograph misses a node.

A node half the fabric knows about is the worst case for a topology
aware scheduler: it is placeable, so work lands on it, and it is outside
the groups the placement was computed over, so the work runs across
whatever link happens to be there. Nothing else in a report shows it.

The labels counted are the fabric tiers alone,
`fabric.topograph.run/tier-<N>`. The set a node is measured against is
the tiers in use on any GPU node of the cluster, so a fleet where every
node agrees gives no finding whichever tiers it uses.

Three decisions define the rule, and each is the difference between a
useful finding and noise:

- Tiers only. The accelerator domain and sub-domain are optional per
  provider, so a fleet whose provider publishes tiers and no domain is
  complete rather than partial, and counting the domain would report
  every such cluster.
- A clique is not a Topograph label at all. A fleet of GB200 nodes with
  cliques beside H100 nodes without them is an ordinary mixed cluster,
  and counting `nvidia.com/gpu.clique` would report every one of them.
- A node carrying none of the tiers its peers carry is reported. That is
  what "present on some GPU nodes and absent on others" means, and it is
  the common shape: Topograph reaches a fleet and misses a node, and the
  node it missed has no tier labels whatsoever rather than some of them.

The last is where this parts company with the `partialNodes` of
`kubectl nv topology`'s report, which skips a node carrying none of the
labels. That is right there, where such a node is still shown and a
reader can see it; it is wrong here, where being left out means nothing
is reported at all. The two keep their own definitions on purpose.

The usual causes are a Topograph run that has not reached the node yet,
a node added after the last run, and a node in a part of the fabric the
topology source does not describe.

### When the rule stays quiet

- Every GPU node carries the same tiers.
- No GPU node carries any tier, which is every cluster Topograph has
  never run against.
- Hinted nodes. The question is which nodes the fabric describes, and a
  node that advertises no GPUs is not one work is placed on.

## Evidence

| Key | Meaning |
|---|---|
| `present` | the fabric tiers the node does carry, sorted and comma joined, absent when it carries none |
| `missing` | the tiers the rest of the cluster uses and it does not, sorted and comma joined |

## Versions

Every GPU Operator version this plugin supports. Topograph is not a GPU
Operator component, so no operator version decides whether its labels
mean anything.

## Documentation

- <https://github.com/NVIDIA/topograph>
- <https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/>

## Next command

```shell
kubectl nv info <node>
```

## Reproduction

E3 scripts both halves as
`hack/mokka/faults/nv-topo-001.sh inject|recover <node>`: it puts
`fabric.topograph.run/tier-0` on one worker of the lane and not on the
named node, so the named node is missing a tier its peer carries, and
removes the label again on recover.
