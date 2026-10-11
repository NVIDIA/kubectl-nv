# must-gather

`kubectl nv adm must-gather` collects what NVIDIA support asks for from a
cluster running the GPU Operator, into one directory. The directory is the
bundle: it is meant to be archived and sent on, and `manifest.json` inside it
says what was collected, what was not, and what every file should hash to.

The bundle is written with mode 0600 and the directory with 0700, because the
default location is under `/tmp`, where every account on the machine can look.

## Flags

- `--artifacts-dir <dir>`: where the bundle goes. Defaults to
  `/tmp/nvidia-gpu-operator_<timestamp>`, the same name the gpu-operator
  script uses, or to `$ARTIFACT_DIR` when that is set.
- `--node <name>`: collect one node instead of the whole cluster. The node
  object, the operand pods on it, their logs, its bug report and its device
  usage are collected; the cluster-scoped resources are collected either way,
  because a node's state cannot be read without them.
- `--since <duration>`: only collect container logs newer than this, for
  instance `2h` or `30m`. The window is not applied to the previous log of a
  container: that log ended when the container died, so a window measured from
  now would empty exactly the file a crash loop is diagnosed from.
- `--no-exec`: never run a command inside a driver pod. The bug report and the
  device usage are skipped and recorded as such.

## Exit codes

- `0`: the gather ran. Collectors skipped by `--no-exec`, or because the
  cluster does not have what they collect, are a complete run.
- `3`: the gather ran but could not do part of what was asked. The common case
  is RBAC refusing `pods/exec`, which leaves the bundle without the driver
  evidence support reads first. The error names the collectors that were
  skipped.
- `4`: nothing could be collected, so the bundle says nothing about the
  cluster.

Being told no and asking for no are different outcomes on purpose. A script
that files an exec-denied bundle as a complete one files an unanswerable
support ticket.

## manifest.json

Written last, in every bundle, whichever way the gather ended. It carries:

- `apiVersion` and `kind: BundleManifest`, the output contract every
  kubectl-nv payload is stamped with.
- `bundleSchemaVersion`, the version of the manifest's own shape. A reader
  checks it before trusting any other field. It moves when a field changes
  meaning, not when a collector is added.
- `toolVersion`, the kubectl-nv that produced the bundle.
- `context`, the kubeconfig context it was collected through.
- `timestamp`, RFC3339 in UTC.
- `flags`: `node`, `since` and `noExec`, so that a bundle missing a node's
  evidence can be read as narrowed rather than as incomplete.
- `collectors`: one entry per collector, with `name`, `status` and the
  `artifacts` it wrote. The status is `ok`, `skipped:<reason>` or
  `error:<text>`.
- `files`: `path`, `size` and `sha256` for every file of the bundle except
  `manifest.json` itself.

The digests are what a bundle is checked against after it has been through an
archive and an upload. A file that arrived truncated and a collector that was
never allowed to run look the same from the outside without them: a file that
is not there.

## Parity with the gpu-operator script

The reference collector is `hack/must-gather.sh` in NVIDIA/gpu-operator, read
at commit `f6d32bad44eec471ecbdccbfc4ce8e7fafa62228` (2026-09-30). Everything
it writes is written here, under the name below. The mapping is checked by
`TestPlannedArtifactsCoverTheScript` in `pkg/mustgather`, which computes the
set of artifacts the collectors plan without needing a cluster, so a collector
dropped or renamed fails a unit test rather than being found by a support
engineer opening a bundle.

| gpu-operator script | kubectl-nv | note |
| --- | --- | --- |
| `must-gather.log` | `must-gather.log` | |
| `must-gather.stderr.log` | `must-gather.stderr.log` | |
| `version` | (field of `manifest.json`) | see below |
| `openshift_version.yaml` | `openshift_version.yaml` | OpenShift only |
| `hyperconverged.yaml` | `hyperconverged.yaml` | when the CRD exists |
| `kubevirt.yaml` | `kubevirt.yaml` | when the CRD exists |
| `cluster_policy.yaml` | `cluster_policy.yaml` | |
| `cluster_policy.missing` | `cluster_policy.missing` | holds the reason |
| `nvidiadrivers.yaml` | `nvidia_driver.yaml` | list, across namespaces |
| (no equivalent) | `nvidia_driver.missing` | holds the reason |
| `gpu_cluster.yaml` | `gpu_cluster.yaml` | when the CRD exists |
| `deviceclasses.yaml` | `deviceclasses.yaml` | when the CRD exists |
| `resourceslices-<driver>.yaml` | `resourceslices.yaml` | one file, NVIDIA's slices |
| `all_machines.list` | `machines.yaml` | OpenShift only, YAML not a list |
| `gpu_nodes.labels` | `gpu_nodes.labels` | |
| `gpu_nodes.status` | `gpu_nodes.status` | |
| `gpu_nodes.descr` | `gpu_nodes.descr` | |
| (no equivalent) | `gpu_nodes.yaml` | the GPU nodes as YAML |
| (no equivalent) | `nodes.yaml` | every node, as YAML |
| `gpu_operator_pod.status` | `gpu_operator_pod.status` | |
| `gpu_operator_pod.yaml` | `gpu_operator_pod.yaml` | |
| `gpu_operator_pod.log` | `gpu_operator_logs_<container>.log` | one per container |
| `gpu_operator_pod.previous.log` | `gpu_operator_logs_<container>.previous.log` | one per container |
| `gpu_operand_pods.status` | `gpu_operand_pods.status` | |
| `gpu_operand_pods.yaml` | `gpu_operand_pods.yaml` | |
| `gpu_operand_pod_images.txt` | `gpu_operand_pod_images.txt` | |
| `gpu_operand_pod_<pod>.log` | `<pod>_<container>.log` | one per container |
| `gpu_operand_pod_<pod>.previous.log` | `<pod>_<container>.previous.log` | one per container |
| `gpu_operand_pod_<pod>.descr` | `<pod>.descr` | |
| `gpu_operand_ds.status` | `gpu_operand_ds.status` | |
| `gpu_operand_ds.yaml` | `daemonsets.yaml` | |
| `gpu_operand_ds_<ds>.descr` | `gpu_operand_ds_<daemonset>.descr` | |
| `nvidia-bug-report_<node>.log.gz` | `nvidia-bug-report_<node>.log.gz` | |
| `gpu_device_usage_<node>.log` | `gpu_device_usage_<node>.log` | |
| (no equivalent) | `manifest.json` | the bundle index |

### Where the names differ, and why

The per-container log names are the ones Track A and Track B chose. The script
asks for `kubectl logs --all-containers`, which puts every container of a pod
in one file; kubectl-nv asks for one container at a time, because a single
request that names no container is refused by the API server for any pod
running more than one, and because one bad container should not cost the
bundle the logs of the containers behind it. The per-line
`[pod/<pod>/<container>]` prefix is kept, so the file reads the way
`kubectl logs --prefix` does.

The script writes one `resourceslices-<driver>.yaml` per NVIDIA DRA driver.
kubectl-nv writes one `resourceslices.yaml` holding the slices published under
`gpu.nvidia.com`, which is what Track B chose.

`machines.yaml` is YAML where the script's `all_machines.list` is the plain
table `kubectl get machines` prints. The rest of the bundle is YAML a reader
can parse, and a table of machines is the one place the script is not.

`version` has no file of its own. The script writes it only when it runs as an
OpenShift must-gather plugin image, from that image's `VERSION`. kubectl-nv
reports its own version instead, in `manifest.json`, where a reader can parse
it rather than having to read a two-line text file.

### Where the content differs

`cluster_policy.yaml` is the single ClusterPolicy document, as the script's
`kubectl get -oyaml <name>` produces, because a cluster has one and every
reader reaches straight for its `.spec`. `nvidia_driver.yaml` is a list, as
the script's `kubectl get nvidiadrivers.nvidia.com -A -oyaml` produces,
because a cluster can run more than one NVIDIADriver.

`.status` and `.descr` files are rendered in process rather than by shelling
out to kubectl. A `.status` is the column set `kubectl get -o wide` prints,
built over the typed client; a `.descr` comes from the describers in
`k8s.io/kubectl/pkg/describe`, which is the same code `kubectl describe` runs.
Neither needs kubectl on the machine the gather runs from.

The GPU nodes of `gpu_nodes.status` and `gpu_nodes.descr` are the nodes
carrying an NVIDIA PCI label, the same set as `gpu_nodes.labels` and
`gpu_nodes.yaml`. The script selects those two files on
`nvidia.com/gpu.present=true` instead, which is a label the GPU feature
discovery operand adds: on a cluster where that operand has not settled, the
script writes two empty files for nodes it has just listed in a third.

## Driver pods

The bug report and the device usage are collected from the driver pods, found
under all four labels the script uses:

- `app=nvidia-driver-daemonset`, the operator's own driver daemonset
- `app.kubernetes.io/component=nvidia-driver`, the form newer charts use
- `app=nvidia-vgpu-manager-daemonset`, the vGPU manager
- `openshift.driver-toolkit`, the OpenShift driver toolkit

A pod carrying more than one of them is collected once. The command runs in
the container named by the pod's `kubectl.kubernetes.io/default-container`
annotation, falling back to the first container in spec order: a driver pod
carrying a sidecar has `nvidia-bug-report.sh` in only one of its containers,
and it is not always the first.

The device usage section runs `nvidia-smi`, and then `lsof`, `ps` and `fuser`
in the host's namespaces through `nsenter --target 1`, because the processes
holding a GPU open are the host's. It runs only when the container has
`nvidia-smi`; otherwise the collector is recorded
`skipped:no nvidia-smi in <pod>` rather than writing a file of error messages.

A cluster with no driver pod at all, which is what the Mokka lane is and what
`driver.enabled=false` gives anywhere, is a complete gather. The collector is
recorded `skipped:no driver pods` and the gather says so on its error stream,
so that a bundle with no `nvidia-bug-report` file is not mistaken for one
whose collector failed.
