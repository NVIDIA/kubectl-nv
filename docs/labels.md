# Node labels kubectl-nv reads

`kubectl nv list` and `kubectl nv info` read almost everything they report off
node labels. None of those labels is written by kubectl-nv: every one comes
from GPU feature discovery (GFD), the GPU Operator, the mig-manager, Topograph
or node-feature-discovery (NFD), and each of those has renamed or added keys
over its releases. This page records which component writes which label, which
versions use which name, and where that was checked, so that a cluster
reporting a blank field can be traced to the component and the release that
explains it rather than guessed at.

Everything below was read with `git show` against the upstream remotes of the
repositories named, at these commits:

| Repository | Remote | Commit read |
|---|---|---|
| `NVIDIA/k8s-device-plugin` (GFD lives here since v0.15.0) | `upstream/main` | `bd98fbd6b` |
| `NVIDIA/gpu-operator` | `upstream/main` | `9eddbaa42` |
| `NVIDIA/mig-parted` (ships the k8s-mig-manager scripts) | `upstream/main` | `87f9e3c6` |
| `NVIDIA/topograph` | `upstream/main` | `44eb46d` |

## Membership is capacity, not labels

Before the table: no label on this page decides whether a node is a GPU node.
That is decided by what the scheduler can place, which is `nvidia.com/gpu`
capacity, any `nvidia.com/mig-<profile>` capacity, or a `gpu.nvidia.com`
ResourceSlice naming the node. `pkg/source/source.go` carries the rule and the
reasoning.

`nvidia.com/gpu.present` and `feature.node.kubernetes.io/pci-10de.present` are
hints. Both say something NVIDIA-shaped was seen on the node, both are written
by a controller that does not retract them when the thing stops working, and
treating either as membership puts a control plane with no allocatable GPU into
every GPU inventory. A node that carries a hint and advertises nothing is
reported by `kubectl nv list --include-hinted` and by `kubectl nv info`, marked
`hinted: true`.

## The labels

| Label | Written by | Read into |
|---|---|---|
| `nvidia.com/gpu.product` | GFD | `product` |
| `nvidia.com/gpu.count` | GFD | `count` |
| `nvidia.com/gpu.replicas` | GFD | `sharing.replicas` |
| `nvidia.com/gpu.sharing-strategy` | GFD | `sharing.strategy` |
| `nvidia.com/gpu.clique` | GFD | `clique` |
| `nvidia.com/cuda.driver-version.full` | GFD | `driverVersion` |
| `nvidia.com/cuda.driver.major` / `.minor` / `.rev` | GFD (deprecated) | `driverVersion`, when the full label is absent |
| `nvidia.com/cuda.runtime-version.full` | GFD | `cudaVersion` |
| `nvidia.com/cuda.runtime.major` / `.minor` | GFD (deprecated) | `cudaVersion`, when the full label is absent |
| `nvidia.com/mig.strategy` | GFD | `mig.strategy` |
| `nvidia.com/mig.capable` | GFD | `mig.capable` |
| `nvidia.com/mig.config` | GPU Operator | `mig.config` |
| `nvidia.com/mig.config.state` | k8s-mig-manager | `mig.configState` |
| `nvidia.com/gpu.deploy.<key>` | GPU Operator | `deploy[<key>]` |
| `nvidia.com/gpu-driver-upgrade-state` | GPU Operator | `upgradeState` |
| `fabric.topograph.run/tier-<N>` | Topograph | `topology[<label>]` |
| `accelerator.topograph.run/domain` | Topograph | `topology[<label>]` |
| `accelerator.topograph.run/sub-domain` | Topograph | `topology[<label>]` |
| `nvidia.com/gpu.present` | GPU Operator | hint only, never membership |
| `feature.node.kubernetes.io/pci-10de.present` | NFD | hint only, never membership |

Every label a node carries is also kept verbatim under `metadata.labels`, so a
key this page does not name is still in `-o json` output.

## GFD: the CUDA version labels were renamed

GFD writes the driver and CUDA runtime versions under two sets of names at
once. The deprecated set splits the version into parts; the current set adds a
`-version` infix and a `.full` key carrying the whole string.

`internal/lm/nvml.go` writes both in one map, the deprecated five first and the
current seven under a `// New labels` comment. The current names were added by
commit `eba41da40`, "align cuda driver and runtime labels with the NFD kernel
version label format", dated 2024-04-03. The first release tag containing that
commit is `v0.15.0`:

```console
$ git log --oneline -S'cuda.driver-version.full' upstream/main -- internal/lm/nvml.go
eba41da40 align cuda driver and runtime labels with the NFD kernel version label format
$ git tag --contains eba41da40 | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | head -3
v0.15.0
v0.15.1
v0.16.0
```

The deprecated names have not been removed: `bd98fbd6b`, the commit read for
this page, still writes them. So a cluster running GFD v0.15.0 or later carries
both sets, and one running anything older carries only the deprecated set.

kubectl-nv therefore prefers `.full` and falls back to the parts. The fallback
joins what is there and stops at the first empty part, because GFD writes
`nvidia.com/cuda.driver.rev` as an empty label when the driver version has only
two components, and `455.06.` is not a version. The GFD documentation's own
example carries exactly that case (`docs/gpu-feature-discovery/README.md`,
the node label example around line 135).

`nvidia.com/gpu.clique` is newer still: the first release tag containing the
commit that added it (`0719b2a45`, "Enable labels for ClusterUUID and CliqueId")
is `v0.17.0`. A cluster older than that reports an empty `CLIQUE` column, which
is not the same as a cluster with no NVLink fabric.

## Which GPU Operator ships which GFD

The operator pins the GFD image in `deployments/gpu-operator/values.yaml`,
under the `gfd` key. Read at the tags:

```console
$ for t in v23.9.2 v24.3.0 v24.6.0 v24.9.0 v25.3.0 v26.3.3; do \
    v=$(git show $t:deployments/gpu-operator/values.yaml | \
        awk '/^gfd:/{f=1} f&&/version:/{print $2; exit}'); echo "$t gfd=$v"; done
v23.9.2 gfd=v0.8.2-ubi8
v24.3.0 gfd=v0.15.0-ubi8
v24.6.0 gfd=v0.16.1-ubi8
v24.9.0 gfd=v0.17.0-ubi9
v25.3.0 gfd=v0.17.1
v26.3.3 gfd=v0.19.3
```

So:

- GPU Operator v24.3.0 and later write both the current and the deprecated
  CUDA version labels.
- GPU Operator v23.9.x and earlier ship GFD v0.8.x, from the separate
  `gpu-feature-discovery` repository that was merged into `k8s-device-plugin`,
  and write only the deprecated names. On those clusters the `.full` labels are
  absent and kubectl-nv reads the parts.
- GPU Operator v24.9.0 and later ship GFD v0.17.0 or newer and so write
  `nvidia.com/gpu.clique`.

The version in the table is the image the chart's default values pin. A cluster
whose `ClusterPolicy` overrides `gfd.version` is running whatever it was given,
which is one reason `kubectl nv info` prints each operand's image.

## GFD: where each label is written

| Label | File read, at `bd98fbd6b` |
|---|---|
| `nvidia.com/cuda.driver-version.*`, `nvidia.com/cuda.driver.*`, `nvidia.com/cuda.runtime-version.*`, `nvidia.com/cuda.runtime.*` | `internal/lm/nvml.go:125`, which builds both sets in one map |
| `nvidia.com/mig.capable` | `internal/lm/nvml.go:171` |
| `nvidia.com/gpu.clique` | `internal/lm/imex.go:39` |
| `nvidia.com/gpu.product`, `.count`, `.replicas`, `.sharing-strategy` | `internal/lm/resource.go:160`, `baseLabeler`, which prefixes each suffix with the resource name |
| `nvidia.com/mig.strategy` | `internal/lm/strategy.go:20`, `migStrategyLabeler`, merged in by `internal/lm/mig-strategy.go:106` |

The full published set, including the labels kubectl-nv does not read
(`gpu.family`, `gpu.memory`, `gpu.compute.*`, `gfd.timestamp`, the per-profile
`MIG_TYPE.*` labels), is tabulated in
`docs/gpu-feature-discovery/README.md`, lines 199 to 273.

## GPU Operator and mig-manager

| Label | File read, at `9eddbaa42` |
|---|---|
| `nvidia.com/gpu.present` | `controllers/state_manager.go:41` names it; `controllers/nodelabeling_controller.go:303` is where it is set, and line 308 where it is set to `false` |
| `nvidia.com/mig.config` | `controllers/state_manager.go:49` |
| `nvidia.com/gpu.deploy.<key>` | `controllers/nodelabeling_controller.go:314`, `updateGPUStateLabels`; the individual keys are the constants from `controllers/state_manager.go:73` onwards |
| `nvidia.com/gpu-driver-upgrade-state` | `vendor/github.com/NVIDIA/k8s-operator-libs/pkg/upgrade/consts.go:21`, which builds the key as `nvidia.com/%s-driver-upgrade-state` from the driver name, and `util.go:108`, which fills it with `gpu` |

`nvidia.com/mig.config.state` is written from outside the operator: the
mig-manager's reconfigure script sets it, at
`deployments/container/reconfigure-mig.sh:222` in `NVIDIA/mig-parted`
(`87f9e3c6`). That matters for a reader: the operator asks for a profile
through `mig.config`, and whether the ask succeeded is reported by a different
component through `mig.config.state`. `kubectl nv info` prints both.

The deploy keys kubectl-nv maps onto operand components, and the components
themselves, are the constants in `pkg/inventory/types_components.go`. The two
vocabularies are not derivable from each other (the driver is `driver` and
`nvidia-driver-daemonset`, while GFD is `gpu-feature-discovery` both times), so
both are written out there and `ComponentForDeployKey` is the only bridge.

## Topograph

Topograph writes a variable-depth fabric label family and a two-level
accelerator hierarchy, documented in `docs/reference/node-labels.md` at
`44eb46d`:

- `fabric.topograph.run/tier-N`, one per discovered tier, with tier 0 the
  switch closest to the node and no fixed maximum depth.
- `accelerator.topograph.run/domain`, the accelerator interconnect locality.
- `accelerator.topograph.run/sub-domain`, optional, written only when the
  domain label is also present.

They are additive: a node can carry every discovered tier plus both accelerator
levels. kubectl-nv keeps the keys verbatim in `topology` rather than parsing a
tier number out of them, because Topograph's own governance treats these three
keys as a contract that downstream schedulers read, and a reader of
kubectl-nv's output should see the same strings those schedulers do.

`kubectl nv list -o wide` grows one column per topology key the listed nodes
carry, so a cluster without Topograph gets no empty fabric columns.

## node-feature-discovery

`feature.node.kubernetes.io/pci-10de.present` is not written by NVIDIA code at
all. It comes from NFD's `pci` source, whose `deviceLabelFields` default makes
a key per PCI vendor, and `10de` is NVIDIA's PCI vendor id. The GPU Operator
bundles NFD as a subchart; at `9eddbaa42` that chart is NFD v0.19.0
(`deployments/gpu-operator/charts/node-feature-discovery/Chart.yaml`), and the
source's configuration surface is in that chart's `values.yaml`, at around
line 469. The label is used as a hint only, for the reason given at the top
of this page.
