# must-gather

`kubectl nv adm must-gather` collects what NVIDIA support asks for from a
cluster running the GPU Operator, into one directory. The directory is the
bundle: it is meant to be archived and sent on, and `manifest.json` inside it
says what was collected, what was not, and what every file should hash to.

The bundle is written with mode 0600 and the directory with 0700, because the
default location is under `/tmp`, where every account on the machine can look.

## Flags

- `--artifacts-dir <dir>`: where the bundle goes. Defaults to
  `/tmp/nvidia-gpu-operator_<timestamp>_<suffix>`, the gpu-operator script's
  name with seconds and a short random suffix so two runs a minute apart do not
  share a directory, or to `$ARTIFACT_DIR` when that is set. A directory that
  already holds something is refused with exit 2 and left alone: mixing two
  runs produces a bundle nothing can take apart afterwards, because the
  manifest indexes both runs' files and its digests check out.
- `--node <name>`: collect one node instead of the whole cluster. The node
  object, the operand pods on it, their logs, its bug report and its device
  usage are collected; the cluster-scoped resources are collected either way,
  because a node's state cannot be read without them. A node that carries no
  NVIDIA PCI label leaves the `gpu_nodes` files out of the bundle, with the
  reason in the manifest; the rest is still collected. A name the cluster does
  not have is a usage error, exit 2, rather than a bundle that looks complete
  and says nothing about any node.

  Three collectors stay cluster-wide under `--node`, because narrowing them
  would remove what the node's state has to be read against: the cluster-scoped
  resources (ClusterPolicy, NVIDIADriver, GPUCluster, DeviceClass,
  HyperConverged, KubeVirt), the GPU operator pod and its logs, which are one
  pod's record of what the operator did to every node, and the operand
  daemonsets, which say what was supposed to be running on the node.
- `--since <duration>`: only collect container logs newer than this, for
  instance `2h` or `30m`. The window is not applied to the previous log of a
  container: that log ended when the container died, so a window measured from
  now would empty exactly the file a crash loop is diagnosed from.
- `--no-exec`: never run a command inside a driver pod. The bug report and the
  device usage are skipped and recorded as such.
- `--exec-timeout <duration>`: how long one command inside a driver pod, or one
  copy out of it, may take. Defaults to 2m. A pod that accepts the connection
  and never answers then costs one artifact rather than the whole bundle; it is
  recorded as `error:timeout after <duration>` and the gather moves on.
- `--redact`: rewrite the identities and the secret material out of the bundle
  before it is indexed. What it covers is below. `kubectl nv debug` takes it
  too: both commands run the same gather and register the same flags.

`--since` and `--exec-timeout` are rejected below one second, exit 2: a window
that short collects nothing and a deadline that short cannot finish a bug
report.

## Exit codes

- `0`: the gather ran. Collectors skipped by `--no-exec`, or because the
  cluster does not have what they collect, are a complete run.
- `2`: the command line was wrong. A `--node` the cluster does not have, a
  window below a second, or an artifact directory that already holds a bundle.
- `3`: the gather ran but could not do part of what was asked. The common cases
  are RBAC refusing `pods/exec`, which leaves the bundle without the driver
  evidence support reads first, and a collector that failed while the others
  succeeded. The error names them.
- `4`: nothing could be collected, so the bundle says nothing about the
  cluster. The `.missing` markers do not count as something collected: a bundle
  holding only those has reached the cluster and found nothing.

One collector failing does not stop the rest. Its entry in the manifest carries
`error:<text>` and everything behind it is still collected, because a bundle
that stops at the first refused list is a bundle missing evidence nobody asked
it to leave out.

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
- `flags`: `node`, `since`, `noExec` and `redacted`, so that a bundle missing a
  node's evidence can be read as narrowed rather than as incomplete, and a
  bundle whose nodes are called `node-3f91c2a7b4` is not read as a cluster
  whose nodes are genuinely called that.
- `collectors`: one entry per collector, with `name`, `status` and the
  `artifacts` it wrote. The status is `ok`, `skipped:<reason>` or
  `error:<text>`.
- `files`: `path`, `size` and `sha256` for every file of the bundle except
  `manifest.json` itself.

The digests are what a bundle is checked against after it has been through an
archive and an upload. A file that arrived truncated and a collector that was
never allowed to run look the same from the outside without them: a file that
is not there.

## Reading a bundle back

`kubectl nv list --from-bundle` and `kubectl nv health --from-bundle` answer
from a bundle instead of from a cluster. For either to exit 0, the bundle has
to hold what they read:

- `gpu_pods.yaml`, every pod of the cluster that requests `nvidia.com/gpu` or
  `nvidia.com/gpu.shared`, in every namespace. The inventory needs it to say
  how many of a node's GPUs are in use; the operand pods are the operator
  namespace alone, and a tenant's training job is not in it. Only the pods that
  request GPUs, because the alternative is the whole cluster's pod list in a
  file somebody mails to support. What counts as a request is
  `inventory.PodSpecGPURequest`, the same rule the live read uses.
- `node_events.yaml`, the events whose involved object is a node. `NV-UPG-001`
  dates a driver upgrade step from them: a node sitting in `cordon-required` is
  not a finding and a node that has been sitting there for an hour is, and only
  the event says which.
- `compute_domains.yaml`, the NVIDIA DRA driver's ComputeDomains, which
  `NV-CD-001` reads. A cluster without that driver serves none, and the gather
  writes `compute_domains.missing` saying so; a reader takes that as the answer
  it is rather than as a read that was refused.

A `<name>.missing` marker is how the bundle says why it holds no such file. Its
one line is `not installed` when the cluster served no such collection,
`forbidden` when the read was refused, and the error otherwise. Only the first
is an answer; the rest are partial reads.

Both honour `--node`. A bundle written before they existed, or one whose read
was refused, is still read as partial and exits 3, naming the file that is not
there: a count of zero requested GPUs read as complete would report an idle
cluster.

## Redaction

`--redact` rewrites every file of the bundle after the last collector has
finished and before the manifest is written, so the digests in the manifest are
the redacted files'.

### What is rewritten

**Identities.** Every node name, every `kubernetes.io/hostname` label value and
every `Hostname`, `InternalDNS` or `ExternalDNS` address, wherever it appears:
the node objects, the pod specs that name a node, the events, the
ResourceSlices, OpenShift's `machines.yaml`, the descriptions and the logs. The
names are learned from all of those and from the cluster's whole node list, so
a gather narrowed with `--node` still takes the other nodes out of the logs
that name them.

File names are rewritten too. `nvidia-bug-report_<node>.log.gz` and
`gpu_device_usage_<node>.log` carry the node in the directory listing, where no
rewrite of the contents reaches it.

A node name is replaced where it is a name and not part of a longer one, so a
cluster with a node called `gpu` keeps `nvidia.com/gpu` intact.

**Addresses.** Every IPv4 and IPv6 address, including one ending in `::` and
the address half of a CIDR such as `fd00:10:244:1::/64`, whose prefix length is
kept. Every candidate is parsed before it is replaced, so a version written
`1.2.3.4` is left alone unless it is also a valid address.

**Credential values**, in each of these shapes:

| Shape | Example |
|---|---|
| `name: value` in YAML | `password: s3cr3t` |
| `NAME=value` | `DB_PASSWORD=s3cr3t`, `password=s3cr3t` in a log line |
| `"name": "value"` in JSON | `{"registryToken":"s3cr3t"}` |
| a JSON env object | `{"name":"HF_TOKEN","value":"s3cr3t"}` |
| the same escaped inside `last-applied-configuration` | `'{"env":[{"name":"NGC_API_KEY","value":"s3cr3t"}]}'` |
| a command line flag | `--hf-token=s3cr3t`, `--password s3cr3t` |
| a URL's userinfo | `https://robot:s3cr3t@registry.example.com` |
| an `Authorization` header, anywhere in a line | `Authorization: Basic ...`, `Bearer ...` |
| a kubeconfig `*-data:` field | `client-key-data: ...` |
| a YAML block scalar | `password: \|` and everything indented under it |
| a PEM block | `-----BEGIN ... PRIVATE KEY-----` to `-----END` |
| the value map of a `Secret` | everything under `data:` or `stringData:` |

A name counts as a credential when it contains `pass`, `secret`, `token`,
`key`, `credential`, `cred`, `auth`, `licence`, `license`, `signature` or
`session`, case insensitively.

Only string values are rewritten. A boolean and a number are left as they are,
so `automountServiceAccountToken: false` keeps its value, and the field
literally called `key` is left alone, so a taint and a toleration keyed by
`nvidia.com/gpu` survive.

An identity becomes a pseudonym rather than a blank: `node-` or `host-` or
`ip-` followed by twenty hexadecimal digits, for instance
`node-3f91c2a7b4e81d05c9a2`. Secret material becomes `REDACTED`, because
nothing is gained by being able to tell two passwords apart.

### Compressed and binary files

A file beginning with the gzip magic bytes is decompressed, rewritten and
compressed again, so `nvidia-bug-report_<node>.log.gz` is redacted inside and
still unpacks. Rewriting the compressed bytes would change nothing a reader can
see and corrupt the file.

Any other file that is not text is removed rather than shipped unrewritten,
because a byte rewrite would corrupt it. `manifest.json` lists each one under
`dropped`, with the reason, so a file that is not there is not mistaken for a
bad upload.

### When it cannot finish

A redaction that fails takes the bundle with it and exits 4. Nothing is left in
the artifacts directory and no `manifest.json` is written. Half a redaction is
a directory the user believes is redacted and is not, and they send it on.

### Consistent within a bundle, not across bundles

The pseudonym is an HMAC of the value under a key drawn at random for that one
run and never written anywhere. So the same node name is the same pseudonym in
every file of one bundle, which is what lets a reader follow a node from the
node list to the pods on it; and the same node name in two bundles is two
different pseudonyms, so nobody holding both can line them up or confirm a
guess at a name. Nothing in or alongside the bundle can undo it, including for
whoever produced it.

### What is not rewritten

- **Container images**, registry hostname included. The driver image is the
  single most useful line in a bundle, and a bundle that will not say which
  driver it ran is a bundle nobody can answer from.
- **Namespaces, pod names, DaemonSet names, labels and annotations other than
  the hostname.** A pod name carries the node it is on only by accident, and
  these are what a reader navigates by.
- **Versions, conditions, events, exit codes and log text** other than the
  addresses and credentials in them.
- **Hostnames nothing in the cluster reported as a node identity.** One that
  appears in a log alone is left.

The rules are shapes rather than schemas, because the files of a bundle are not
one format. `--redact` reduces what a bundle discloses; it is not a guarantee
that nothing identifying survives, and a bundle leaving a regulated environment
should still be read before it is sent.

`--redact` applies to the bundle on disk. It does not apply to what a command
prints: `kubectl nv debug` writes a redacted bundle and prints a report naming
the real node, because that report is for the person who ran it. Over MCP the
flag is offered like any other and is not forced on, so a tool call that wants
a redacted bundle asks for one.

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
| `resourceslices-<driver>.yaml` | `resourceslices.yaml` | one file, both NVIDIA drivers |
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
both of them: `gpu.nvidia.com` for the GPUs and `compute-domain.nvidia.com` for
the compute domains, which is how IMEX is allocated across a fabric.

The version of `resource.k8s.io` is discovered rather than pinned. The group
has been through `v1beta1`, `v1beta2` and `v1` in four releases, so a client
fixed at one of them reports a cluster that is using DRA as not having it. Only
a cluster serving none of them has no DRA.

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

`.status` and `.descr` files are produced in process rather than by shelling
out to kubectl. The pod `.status` files are the Table the API server itself
renders, which is what `kubectl get -o wide` prints: the server knows pod
states that cannot be re-derived correctly from the objects alone, so an init
container in a crash loop reads `Init:CrashLoopBackOff` rather than `Pending`.
A server that will not serve a Table falls back to a local renderer with the
same columns, because an empty `.status` is worse. A `.descr` comes from the
describers in `k8s.io/kubectl/pkg/describe`, which is the same code
`kubectl describe` runs. Neither needs kubectl on the machine the gather runs
from.

The GPU nodes of `gpu_nodes.status` and `gpu_nodes.descr` are the nodes
carrying an NVIDIA PCI label, the same set as `gpu_nodes.labels` and
`gpu_nodes.yaml`. The script selects those two files on
`nvidia.com/gpu.present=true` instead, which is a label the GPU feature
discovery operand adds: on a cluster where that operand has not settled, the
script writes two empty files for nodes it has just listed in a third.

## Driver pods

The bug report and the device usage are collected from the driver pods, found
under four labels. Three are the ones the gpu-operator script uses (lines 337
to 339); the fourth is kubectl-nv's own and predates this work:

- `app.kubernetes.io/component=nvidia-driver`, the form newer charts use
  (script)
- `app=nvidia-vgpu-manager-daemonset`, the vGPU manager (script)
- `openshift.driver-toolkit`, the OpenShift driver toolkit (script)
- `app=nvidia-driver-daemonset`, the operator's own driver daemonset, which
  kubectl-nv has always looked under and the script does not

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
whose collector failed. Driver pods that are all there and none of which can be
asked, because every one is pending, is recorded `skipped:no driver pod could
be reached` instead: that is a different cluster.

Being refused the exec is different again. An exec is an HTTP upgrade, so a
denial fails the upgrade before any status is decoded and arrives as plain
text; it is recognised all the same, recorded `skipped:forbidden`, and exits 3.
