# NV-CD-001: a ComputeDomain covering the node is not serving it

Severity: error for the daemon, warning for the missing clique.
Component: `compute-domain-daemon`, or `node` for the missing clique.

## Symptom

A ComputeDomain names this node and one of two things is wrong: the IMEX
daemon on it is not ready, or the node carries no
`nvidia.com/gpu.clique`.

A ComputeDomain is what a multi-node NVLink workload asks for, and it is
the one object whose failure mode is silent: the workload pods stay in
`ContainerCreating` and nothing in the namespace says why.

### The daemon is not ready

The ComputeDomain's own status is the authority. `status.nodes` names
each node the domain covers, with that node's IMEX daemon status, `Ready`
or `NotReady`, alongside its `cliqueID` and `ipAddress`. An entry with no
status at all reads as not ready, which is the CRD's own default for the
field: reading an absent status as ready would report a domain nobody
has joined as working.

The daemon pod on the node is read as well, because a domain whose
status has not caught up still has a pod that is not reporting ready,
and that is the same fault a few seconds earlier. The pod is identified
by component `compute-domain-daemon`: the upstream template labels it
with `resource.nvidia.com/computeDomain` and with no `app` key of any
kind, so that label is the only way to recognise it, and the component
name is the template's own name for the workload.

### The node has no clique

A node outside any NVLink clique cannot be placed in an IMEX domain
however healthy its daemon looks. That is a fact about the node rather
than about one domain, so a node covered by two domains gets one finding
naming both, sorted and comma joined.

### Where these facts come from

NVIDIA DRA driver `fb8b9674` (`main` at 2026-10-08):

- `api/nvidia.com/resource/v1beta1/computedomain.go` for the status
  shape: `ComputeDomainStatus.Nodes`, each a `ComputeDomainNode` with
  `name`, `ipAddress`, `cliqueID`, `index` and `status`, and the
  `Ready`/`NotReady` enum with `NotReady` as the default.
- `deployments/helm/dra-driver-nvidia-gpu/crds/resource.nvidia.com_computedomains.yaml`
  for the served version: `v1beta1`, served and storage, and the only
  one. `api/nvidia.com/resource` holds `v1beta1` and nothing else at
  v25.3.0, v25.8.0 and v25.12.0 alike.
- `templates/compute-domain-daemon.tmpl.yaml` and
  `cmd/compute-domain-daemon/common.go:44` for the daemon pod's one
  label, `resource.nvidia.com/computeDomain`, whose value is the
  domain's UID.

### When the rule stays quiet

- The cluster has no ComputeDomains, which is every cluster not running
  a multi-node IMEX workload.
- A node the ComputeDomain does not name. Reporting one would make every
  GPU node in the cluster a member of every domain.
- A ComputeDomain naming a node this run never saw. A finding nobody can
  look up gives a reader no command to run next.
- A ready domain entry on a node whose daemon pod is ready, or has no
  daemon pod yet while the domain itself is content.
- Hinted nodes.

## Evidence

The daemon finding:

| Key | Meaning |
|---|---|
| `computeDomain` | the domain, as `<namespace>/<name>`, because ComputeDomains are namespaced |
| `computeDomainStatus` | `status.status`, the domain's global verdict |
| `nodeStatus` | this node's entry in `status.nodes`, `Ready` or `NotReady` |
| `cliqueID` | the clique the domain recorded for the node, absent when it recorded none |
| `pod` | the IMEX daemon pod on the node, absent when there is none |
| `ready` | whether that pod reports ready, absent for the same reason |

The clique finding:

| Key | Meaning |
|---|---|
| `computeDomains` | every domain covering the node, each as `<namespace>/<name>`, sorted and comma joined |

## Versions

GPU Operator v25.3.0 and above, the first release that ships the NVIDIA
DRA driver and so the first where a cluster can serve a ComputeDomain at
all. An unknown operator version still runs the rule.

## Documentation

- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/gpu-operator-dra.html>
- <https://github.com/NVIDIA/k8s-dra-driver-gpu>

## Next command

```shell
kubectl nv info <node>
```

## Reproduction

Object fixtures on the faults lane, which is what Carlos settled on for
this rule: real ComputeDomains come with Phase 2. E3 scripts both halves
as `hack/mokka/faults/nv-cd-001.sh inject|recover <node>`: it applies the
ComputeDomain CRD, one ComputeDomain whose `status.nodes` names the node
at `NotReady`, and a not-ready IMEX daemon pod fixture on it, and deletes
all three on recover.
