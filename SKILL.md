---
name: kubectl-nv
description: Diagnose NVIDIA GPU nodes in a Kubernetes cluster with the kubectl-nv plugin. Use it when a GPU workload will not schedule, a node advertises no GPUs, the GPU Operator operands are unhealthy, a driver or DCGM pod is failing, or someone asks which node a job can run on. It reads a live cluster or an offline must-gather bundle, reports findings as stable NV-<AREA>-<NNN> codes with the evidence behind each one, and never changes a Kubernetes object.
license: Apache-2.0
compatibility: Requires the kubectl-nv binary on PATH and either a kubeconfig for the cluster or a must-gather directory to read offline.
---

# kubectl-nv

A read-only diagnostic plugin for Kubernetes clusters with NVIDIA GPUs. It
answers three kinds of question: what GPU nodes exist and what they carry,
what is wrong with them, and what to do about it.

## Installing this skill

The Agent Skills specification requires the `name` above to equal the
directory the skill is installed under. This file lives at the root of the
`kubectl-nv` repository, and that root is the skill root, so the name is
`kubectl-nv` and the directory must be `kubectl-nv/`. Copying this file into a
directory under any other name makes the skill invalid.

## Which command answers which question

Every command takes `-o json` and prints one object on stdout. Run them with
`-o json` and read the fields; the default `table` output is for a person.

| Question | Command |
|---|---|
| What GPU nodes are there, and what do they advertise? | `kubectl nv list` |
| Everything about one node | `kubectl nv info NODE` |
| What is wrong with the GPU stack? | `kubectl nv health` |
| What does finding code X mean, and what do I run next? | `kubectl nv explain CODE` |
| How are the nodes wired together, and where does a job fit? | `kubectl nv topology` |
| What fields does a kind have? | `kubectl nv schema KIND` |
| Everything about one node, plus a bundle for support | `kubectl nv debug NODE` |
| Collect the bundle support asks for | `kubectl nv adm must-gather` |

### What each command prints, and what it may do

| Command | Takes `-o` | Runs anything in a pod | Prints |
|---|---|---|---|
| `kubectl nv list` | yes | no | A `GPUNodeList` |
| `kubectl nv info` | yes | no | One `GPUNode` |
| `kubectl nv health` | yes | no | A `HealthReport` |
| `kubectl nv topology` | yes | no | A `TopologyReport` |
| `kubectl nv debug` | yes | yes | A `DebugReport` |
| `kubectl nv explain` | no | no | The catalog entry, as Markdown |
| `kubectl nv schema` | no | no | A JSON Schema |
| `kubectl nv adm must-gather` | no | yes | Nothing; it writes a directory |
| `kubectl nv mcp` | no | no | Nothing; it serves the protocol |
| `kubectl nv version` | no | no | The version string |

Only the four in the first group take `-o`. Asking the others for `-o json`
is exit 2, not a silently ignored flag.

Start at `kubectl nv health`. It is the command that turns "the GPUs are
broken" into a finding code, a node and a component. Use `kubectl nv list` and
`kubectl nv info` to confirm what a finding says, not to find the problem.

### Narrowing the question

```console
kubectl nv health --node worker-1 -o json
kubectl nv list --selector nvidia.com/gpu.present=true -o json
kubectl nv list --include-hinted -o json
kubectl nv health --summary
```

`--node` on `kubectl nv health` and on `kubectl nv list` reports one node.
`--include-hinted` adds the nodes that carry an NVIDIA label but advertise no
GPUs; a node that should have GPUs and has none is usually the answer, so ask
for it when `kubectl nv list` comes back shorter than expected.

## Reading a HealthReport

`kubectl nv health -o json` prints a `HealthReport`. Its shape:

```json
{
  "apiVersion": "kubectl-nv.nvidia.com/v1alpha1",
  "kind": "HealthReport",
  "metadata": {"name": "cluster"},
  "findings": [
    {
      "code": "NV-NODE-002",
      "severity": "error",
      "node": "worker-1",
      "component": "node",
      "message": "worker-1 advertises no allocatable nvidia.com/gpu",
      "evidence": {"allocatable": "0", "capacity": "8"},
      "docs": "docs/findings/NV-NODE-002.md",
      "next": "kubectl nv info worker-1"
    }
  ],
  "summary": {"errors": 1, "warnings": 0, "infos": 0, "nodesChecked": 2}
}
```

Read a finding in this order:

1. `code` is the stable identifier, in the form `NV-<AREA>-<NNN>`. It never
   changes meaning, so it is the only field worth branching on or grepping
   for. A message is prose and may be reworded.
1. `severity` is `error`, `warning` or `info`. It decides the exit code
   together with `--fail-on`.
1. `node` is the node the finding is about. It is empty when the finding is
   about the cluster rather than about one node, for instance a ClusterPolicy
   problem.
1. `component` is the part of the stack at fault: the node itself, the device
   plugin, the driver, DCGM, the toolkit, the operator. Two findings on the
   same node with different components are two separate problems.
1. `evidence` is the map of values the rule concluded from, for instance the
   allocatable and capacity counts, a pod phase, a label value. Quote it when
   reporting; it is what makes the answer checkable.
1. `next` is the command to run next, and `docs` names the catalog entry.

`summary` carries the arithmetic over what was checked: `errors`, `warnings`,
`infos`, `nodesChecked`, `gpuCapacity`, `gpuAllocatable` and `gpuRequested`.
When `gpuRequestedKnown` is false the pods could not be read and the requested
count means nothing.

`findings` is always an array, `[]` when there is nothing to report, so it can
be ranged over without a nil check. When part of the cluster could not be
read, `partial` is true and `unreachable` lists what was missed. A report with
`partial: true` and no findings is not a clean cluster: it is a scan that did
not happen.

### Answering a question from a report

A complete answer names three things: the node, the component and the code.
"worker-1 advertises no GPUs" is half an answer. "worker-1 (component `node`)
reports NV-NODE-002: allocatable `nvidia.com/gpu` is 0 against a capacity of
8" is the whole one, and the evidence is there to be checked.

## The explain loop

`kubectl nv health` names a code. `kubectl nv explain` turns the code into the
catalog entry: what it means, what the evidence is, which GPU Operator
versions it applies to, what to run next and how to reproduce it. The catalog
is compiled into the binary, so it needs no cluster and no network.

```console
kubectl nv health -o json
kubectl nv explain NV-NODE-002
kubectl nv info worker-1 -o json
```

Codes are not case sensitive, so `kubectl nv explain nv-node-002` works too.
An unknown code exits 2 and prints the codes the catalog does hold.

The loop is: run `kubectl nv health`, take the highest severity finding,
`kubectl nv explain` its code, run the command the entry's next step names,
and report what that showed. Do not guess at a cause the evidence does not
support; the entry lists the usual ones and the command that tells them apart.

The whole catalog, sixteen codes:

| Code | What it means |
|---|---|
| `NV-NODE-001` | the GPU Operator is not reconciling this node |
| `NV-NODE-002` | a GPU node that advertises no allocatable GPUs |
| `NV-NODE-003` | fewer allocatable GPUs than the node has |
| `NV-NODE-004` | the GPU count label disagrees with the node's capacity |
| `NV-NODE-005` | an NVSentinel check is failing, or the node is cordoned |
| `NV-OP-001` | the ClusterPolicy is not ready |
| `NV-OP-002` | an operand the operator is not running on the node |
| `NV-DP-001` | the device plugin is missing or not ready on a GPU node |
| `NV-DRV-001` | a driver pod on the node is not ready or is restarting |
| `NV-DCGM-001` | dcgm-exporter is missing or not ready where it should run |
| `NV-CTK-001` | the container toolkit validation failed on the node |
| `NV-MIG-001` | the node's MIG configuration has not been applied |
| `NV-UPG-001` | a driver upgrade failed or is stuck on the node |
| `NV-DRA-001` | the node advertises its GPUs twice over |
| `NV-CD-001` | a ComputeDomain covering the node is not serving it |
| `NV-TOPO-001` | the node is missing Topograph tiers its peers carry |

Run `kubectl nv explain` for every code a report carries: the line above is
the headline, and the entry carries the evidence, the versions it applies to
and what to run next.

## Working offline from a bundle

`kubectl nv adm must-gather` collects a directory from a live cluster, and
`kubectl nv debug NODE` collects one narrowed to a single node and prints a
`DebugReport`: that node's `GPUNode` next to what the gather did and did not
manage to collect. `kubectl nv list`, `kubectl nv info` and
`kubectl nv health` read such a directory instead of a cluster with
`--from-bundle`:

```console
kubectl nv adm must-gather --artifacts-dir /tmp/support
kubectl nv debug worker-1 --artifacts-dir /tmp/worker-1 -o json
kubectl nv health --from-bundle /tmp/support -o json
kubectl nv info worker-1 --from-bundle /tmp/support -o json
```

This is how a bundle someone else collected is diagnosed: no kubeconfig, no
cluster, no network. A rule whose inputs the bundle does not carry is reported
in `unreachable` and the run exits 3 rather than calling the cluster clean.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | The run completed and found nothing at or above `--fail-on` |
| 1 | The run found something at or above `--fail-on` |
| 2 | The command line or the configuration was wrong |
| 3 | The run completed over less than it was asked for |
| 4 | The run could not happen |

Exit 1 is an answer, not a failure: the command found what it was asked to
look for, and the report on stdout is the point of the call. Exit 3 is an
answer with a hole in it, listed in `unreachable`. Exit 4 means nothing was
checked and no conclusion can be drawn.

`--fail-on` takes `info`, `warning` or `error` and defaults to `error`, so by
default a warning alone leaves exit 0. Ask for `--fail-on warning` when a
warning should count:

```console
kubectl nv health --fail-on warning -o json
```

Findings win over partial results: a run that found something at or above the
threshold exits 1 even when part of the cluster stayed out of reach.

## Streams

stdout carries the output object and nothing else. Progress, notes and
warnings go to stderr, so reading stdout alone never needs filtering. No
command prompts, and none requires a terminal.

Four commands do not keep to that yet. `kubectl nv version`,
`kubectl nv explain`, `kubectl nv schema` and `kubectl nv mcp --list-tools`
print their output on stderr today. Read both streams when you call those
four, and stdout alone for everything else. `kubectl nv schema` also has no
kinds registered yet, so it answers any argument with exit 2; the output
kinds are real and the contract below is what it will print.

## Bounded output

A whole cluster's report can be large. Two flags cut it down, and they mean
the same thing on every command that lists anything.

```console
kubectl nv list --limit 20 -o json
kubectl nv list --fields status.ready,metadata.labels -o json
```

`--limit N` keeps the first N items after the command's own sort. Truncation
is reported in the output as `metadata.remainingItemCount` as well as on
stderr, so a reader of stdout alone still sees that there was more.

`--fields` keeps only the named dotted JSON paths on each item. `apiVersion`,
`kind` and `metadata.name` survive whatever is asked for. A path a kind does
not have is exit 2 with the valid paths listed, never a silently missing
field. Ask `kubectl nv schema` which paths a kind offers:

```console
kubectl nv schema HealthReport
kubectl nv schema list
```

Prefer `--fields` over `--limit` when the answer needs every node: dropping
fields keeps all the items, dropping items loses the one that mattered.

## What this tool will not do

`kubectl-nv` never creates, updates, patches or deletes a Kubernetes object.
It is run against a cluster that is already unhappy, and a write from it would
change the state being reported. This is enforced by a test that drives every
command against clients that fail on any write verb.

The exception is the commands that collect a bundle: they run
`nvidia-bug-report.sh` inside the driver pods and copy the result out. The
table above says which ones they are, and that is the whole of what this tool
does cluster side. A gather leaves `/tmp/nvidia-bug-report.log.gz` behind
inside each driver pod it collected from. Turn it off with `--no-exec`:

```console
kubectl nv adm must-gather --no-exec --artifacts-dir /tmp/support
```

`--no-exec` collects everything else and records in the bundle what was
skipped. Use it unless a bug report is specifically what was asked for. A
cluster where `pods/exec` is not allowed degrades the same way and exits 3.

Nothing here needs cluster-admin. The plugin runs with the kubeconfig and the
RBAC the caller already has, and reports what it could not read rather than
asking for more.

## Over MCP

`kubectl nv mcp` serves the same read-only commands as a Model Context
Protocol server on stdin and stdout, one tool per command. A tool offers that
command's own flags and `--namespace`, and nothing else: no `--kubeconfig`,
no `--server`, no `--token`. Which cluster the tools read, and as whom, is
fixed by the flags given to `kubectl nv mcp` itself. Commands that can exec
are served with `--no-exec` forced on.

```console
kubectl nv mcp --list-tools
```

Each tool result carries the exit code the command would have left under
`kubectl-nv.nvidia.com/exit-code`. Exit 1 and exit 3 come back as results, not
errors, because a command that found something has answered the question.
