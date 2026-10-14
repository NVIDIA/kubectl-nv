# Usage

kubectl-nv is a kubectl plugin. Installed on your `PATH` it runs as
`kubectl nv <command>`; the binary also runs on its own as
`kubectl-nv <command>`. No command writes to the cluster, and `explain`,
`schema` and `version` work without one. Two commands, `debug` and
`adm must-gather`, run commands inside the NVIDIA driver pods to collect a bug
report and the GPU device usage, unless `--no-exec` says not to.

`kubectl nv --help` lists the commands, and `kubectl nv <command> --help`
prints the flags of one.

## Connecting to a cluster

Every command takes kubectl's connection flags and reads the same kubeconfig
kubectl does:

| Flag | Meaning |
|---|---|
| `-k`, `--kubeconfig` | kubeconfig file to use (default: `KUBECONFIG`, then `~/.kube/config`) |
| `--context`, `--cluster`, `--user` | kubeconfig context, cluster or user to use |
| `-n`, `--namespace` | namespace scope of the request |
| `-s`, `--server`, `--token` | API server address and bearer token |
| `--as`, `--as-group`, `--as-uid`, `--as-user-extra` | impersonation |
| `--certificate-authority`, `--client-certificate`, `--client-key`, `--insecure-skip-tls-verify`, `--tls-server-name` | TLS |
| `--request-timeout`, `--proxy-url`, `--disable-compression`, `--cache-dir` | transport |

Two more flags apply to every command: `-d`, `--debug` turns on debug-level
logging, and `--no-color` keeps colour out of the progress output.

## Output

`list`, `info`, `debug`, `health` and `topology` print a table by default and
take `-o` to choose the format:

| `-o` | Prints |
|---|---|
| `table` | the default columns |
| `wide` | the table with more columns |
| `json`, `yaml` | the full object, with `apiVersion: kubectl-nv.nvidia.com/v1alpha1` and a `kind` |
| `name` | one `<kind>/<name>` per line, the kind in lower case, for instance `gpunode/gpu-worker-1` |

The kinds are `GPUNodeList` (`list`), `GPUNode` (`info`), `DebugReport`
(`debug`), `HealthReport` (`health`) and `TopologyReport` (`topology`);
`adm must-gather` writes a `BundleManifest` into the bundle it collects.

Two flags bound what is printed. `--fields` keeps only the dotted JSON paths
it is given, for instance `--fields status.ready`; `apiVersion`, `kind` and
`metadata.name` are always kept. `--limit N` prints at most N items, after
sorting; 0 prints every item.

`list`, `info`, `debug`, `health` and `topology` print to standard output, and
progress and errors go to standard error. On this release `explain`,
`version` and `mcp --list-tools` print to standard error as well.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | The command ran and completed. |
| 1 | `health` found something at or above its `--fail-on` severity. |
| 2 | The command line or the configuration was wrong, for instance an unknown flag or a node the cluster does not have. |
| 3 | The command ran over less than it was asked for, for instance a read the cluster denied in part or a collector that failed; what it printed covers the rest. |
| 4 | The command could not run, for instance because the API server was unreachable. |

## Commands

### kubectl nv list

```console
kubectl nv list [flags]
```

Lists the GPU nodes of a cluster: what they advertise, which driver and CUDA
version they run, whether the device plugin or DRA serves them, and what is
already asking for their GPUs.

A node counts as a GPU node when the scheduler can place GPU work on it: it
advertises `nvidia.com/gpu`, `nvidia.com/gpu.shared` or a MIG profile, or a
`gpu.nvidia.com` ResourceSlice names it. A node that carries an NVIDIA label
and advertises nothing is a hinted node: `list` leaves it out, says on standard
error how many it left out, and lists it with `--include-hinted`.
[labels.md](labels.md) lists every label and resource kubectl-nv reads.

| Flag | Meaning |
|---|---|
| `--include-hinted` | also list the nodes that carry an NVIDIA label but advertise no GPUs |
| `--node NODE` | report on this node alone |
| `-l`, `--selector` | label selector the nodes must match |
| `--no-pods` | do not list the pods; the requested GPU count is then reported as unknown |
| `--from-bundle DIR` | read a must-gather directory instead of a cluster |
| `-o`, `--fields`, `--limit` | see [Output](#output) |

```console
kubectl nv list
kubectl nv list -o wide --include-hinted
kubectl nv list -l nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3 -o json
kubectl nv list --from-bundle /tmp/support
```

### kubectl nv info

```console
kubectl nv info NODE [flags]
```

Shows everything kubectl-nv knows about one node: its GPU labels, what it
advertises, its conditions and taints, the GPU Operator pods running on it with
their images and the validator's results, and the ResourceSlices published for
it. A hinted node is shown and marked as hinted, since that it should have GPUs
and does not is the answer.

| Flag | Meaning |
|---|---|
| `--from-bundle DIR` | read a must-gather directory instead of a cluster |
| `-o`, `--fields`, `--limit` | see [Output](#output) |

```console
kubectl nv info gpu-worker-1
kubectl nv info gpu-worker-1 -o yaml
```

### kubectl nv debug

```console
kubectl nv debug NODE [flags]
```

Collects a must-gather narrowed to one node and prints what kubectl-nv knows
about that node on one screen: its GPUs, its driver, its operands, and what the
gather could and could not collect. The bundle is the one
`kubectl nv adm must-gather --node NODE` writes. An unknown node, or one with
no GPUs, exits 2 before anything is collected.

| Flag | Meaning |
|---|---|
| `--artifacts-dir DIR` | where the bundle is written (default `/tmp/nvidia-gpu-operator_<timestamp>`) |
| `--no-exec` | never exec into a driver pod: no bug report and no GPU device usage |
| `--exec-timeout` | how long one command inside a driver pod, or one copy out of it, may take (default 2m) |
| `--since` | only collect container logs newer than this, for instance `2h` or `30m` |
| `-o`, `--fields`, `--limit` | see [Output](#output) |

```console
kubectl nv debug gpu-worker-1
kubectl nv debug gpu-worker-1 --no-exec --since 2h
```

### kubectl nv health

```console
kubectl nv health [flags]
```

Runs every GPU health check over the cluster and reports what it found: the
node labels and capacity, the GPU Operator's ClusterPolicy, and the operand
pods on each node. The report opens with a summary of the nodes checked and
their GPU capacity, allocatable and requested, then lists the findings. Each
finding carries a stable code such as `NV-NODE-002`, a severity (`error`,
`warning` or `info`), the node and component it is about, the evidence it was
concluded from and the command to run next. `kubectl nv explain <code>` prints
the code's catalog entry; [findings](findings/) holds all sixteen.

| Flag | Meaning |
|---|---|
| `--fail-on` | exit 1 when a finding is at or above this severity: `error`, `warning` or `info` (default `error`) |
| `--node NODE` | report on one node only; the cluster-scoped findings are kept |
| `--summary` | print the summary line only, without the findings |
| `--upgrade-stuck-after` | report a node stuck in a driver upgrade step once it has been there this long (default 30m) |
| `--from-bundle DIR` | read a must-gather directory instead of a cluster |
| `-o`, `--fields`, `--limit` | see [Output](#output) |

`--fail-on` decides only the exit code: `-o json` always carries every finding.

```console
kubectl nv health
kubectl nv health --fail-on info
kubectl nv health --node gpu-worker-1 -o json
kubectl nv health --from-bundle /tmp/support
```

### kubectl nv explain

```console
kubectl nv explain CODE
```

Prints the catalog entry for a finding code: what it means, what the evidence
is, which GPU Operator versions it applies to, what to run next, and how to
reproduce it. The catalog is built into the binary, so `explain` needs no
cluster and no network. Codes are not case sensitive, and an unknown code exits
2.

```console
kubectl nv explain NV-NODE-002
kubectl nv explain nv-dp-001
```

### kubectl nv topology

```console
kubectl nv topology [flags]
```

Groups the GPU nodes at every level the cluster is labelled with, narrowest
first: the node, the NVLink clique (`nvidia.com/gpu.clique`), Topograph's
accelerator sub-domain and domain (`accelerator.topograph.run/sub-domain` and
`/domain`), its fabric tiers (`fabric.topograph.run/tier-0` upwards), the
levels a Kueue Topology declares, and the cluster. A level no node carries is
left out. Each group reports its nodes, the GPUs it has, the GPUs already
requested and the GPUs left. [labels.md](labels.md) lists the labels it reads.

`--for job/NAME` names the narrowest level where every pod of that Job fits,
placing whole pods on nodes and leaving out cordoned and not-Ready ones. The
Job is read in the namespace `-n` names. A Job that does not exist exits 2, a
Kueue read the cluster denies exits 3, and any other failed Kueue read exits 4.

| Flag | Meaning |
|---|---|
| `--for job/NAME` | answer where every pod of this Job would fit |
| `-o`, `--fields`, `--limit` | see [Output](#output) |

```console
kubectl nv topology
kubectl nv topology -o wide
kubectl nv topology --for job/train -n team-a
```

### kubectl nv adm must-gather

```console
kubectl nv adm must-gather [flags]
```

Collects the information from the cluster that NVIDIA support is most likely
to need, in the order the GPU Operator's own `hack/must-gather.sh` collects it,
into a directory that ends with a `manifest.json` recording what was collected,
what failed and a sha256 of every file. One collector failing does not stop the
others; the run then exits 3. [must-gather.md](must-gather.md) describes the
bundle file by file.

| Flag | Meaning |
|---|---|
| `--artifacts-dir DIR` | where the bundle is written (default `/tmp/nvidia-gpu-operator_<timestamp>`); a directory that already holds something is refused |
| `--node NODE` | collect only this node: its node object, the operand pods on it, their logs, its bug report and its device usage; the cluster-scoped resources are collected either way |
| `--since` | only collect container logs newer than this, for instance `2h` or `30m` |
| `--no-exec` | never exec into a driver pod: no bug report and no GPU device usage |
| `--exec-timeout` | how long one command inside a driver pod, or one copy out of it, may take (default 2m) |

```console
kubectl nv adm must-gather --artifacts-dir /tmp/support
kubectl nv adm must-gather --node gpu-worker-1 --since 2h
kubectl nv adm must-gather --no-exec
```

### kubectl nv schema

```console
kubectl nv schema [KIND|COMMAND] [flags]
```

Prints the JSON Schema of one output kind, named by the kind or by the command
that prints it, and `--write-dir DIR` writes one `<kind>.json` per registered
kind. The kinds register their schemas with the contract wiring that follows
this release; until then the registry is empty and the command says
`no kinds are registered yet`.

### kubectl nv mcp

```console
kubectl nv mcp [flags]
```

Serves kubectl-nv as a Model Context Protocol server on standard input and
output, with one tool per read-only command. The cluster the tools talk to,
and as whom, is set by the flags given to `mcp` itself and is out of reach of a
tool call: a tool offers its command's own flags and `--namespace`, nothing
else. `debug` and `adm must-gather` are served with `--no-exec` forced on.
`--list-tools` prints the tools and exits. [agents.md](agents.md) describes the
contract.

```console
kubectl nv mcp --list-tools
kubectl nv mcp --context my-cluster
```

### kubectl nv version

```console
kubectl nv version
kubectl nv --version
```

Prints the version the binary was built with.
