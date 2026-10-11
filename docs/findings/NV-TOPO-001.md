# NV-TOPO-001: the node is missing Topograph labels its peers carry

Severity: warning. Component: `node`.

## Symptom

Some GPU nodes carry Topograph's fabric labels and this one carries only
part of the set.

A node half the fabric knows about is the worst case for a topology
aware scheduler: it is placeable, so work lands on it, and it is outside
the groups the placement was computed over, so the work runs across
whatever link happens to be there. Nothing else in a report shows it.

The labels counted are Topograph's own: `fabric.topograph.run/tier-<N>`,
`accelerator.topograph.run/domain` and
`accelerator.topograph.run/sub-domain`. The set a node is measured
against is the ones in use anywhere in the cluster, so a fleet where
every node agrees gives no finding whichever labels it uses.

The partial set is computed by `pkg/topology`, the same code that builds
`kubectl nv topology`'s report, rather than by a second definition here.
That matters for two decisions a second implementation would be free to
make differently, and both of them are the difference between a useful
rule and noise:

- A clique is not a Topograph label. A fleet of GB200 nodes with cliques
  beside H100 nodes without them is an ordinary mixed cluster, and
  counting `nvidia.com/gpu.clique` would report every such cluster as
  broken.
- A node carrying none of the labels is outside the fabric rather than
  half inside it. Saying it is missing every label would bury the nodes
  that are missing one, which are the nodes this rule exists for.

The usual causes are a Topograph run that has not reached the node yet,
a node added after the last run, and a node in a part of the fabric the
topology source does not describe.

### When the rule stays quiet

- Every GPU node carries the same Topograph labels.
- No GPU node carries any of them, which is every cluster Topograph has
  never run against.
- A node carrying none of them while others carry some, for the reason
  above.
- Hinted nodes.

## Evidence

| Key | Meaning |
|---|---|
| `present` | the Topograph labels the node does carry, sorted and comma joined |
| `missing` | the ones the rest of the cluster uses and it does not, sorted and comma joined |

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
`hack/mokka/faults/nv-topo-001.sh inject|recover <node>`: it labels
every GPU node of the lane with `fabric.topograph.run/tier-0` and
`tier-1`, then removes `tier-1` from the named node, and puts the labels
back as it found them on recover.
