# Agent evaluation

Can an agent holding nothing but `kubectl-nv` and its `SKILL.md` diagnose a
broken GPU node? This directory holds the answers, one file per run.

The harness is `hack/eval/run.sh`. It breaks the faults lane five times in
turn, hands an agent a directory containing a guarded `kubectl-nv`, the skill
and the lane kubeconfig, asks what is wrong, scores the answer, and puts the
lane back.

## Running it

Runs are manual. The harness talks to a real cluster and spends money on a
real model, so nothing in CI calls it and it refuses to start when `CI` is
set.

```console
make build
hack/mokka/up.sh faults t4
hack/eval/run.sh
hack/mokka/down.sh faults
```

The default agent command is Claude Code in print mode, locked down:

```console
claude -p --restricted --tools Bash,Read --allowedTools 'Bash(./kubectl-nv:*)' \
  --permission-mode dontAsk --strict-mcp-config --mcp-config '{"mcpServers":{}}'
```

Override it to score something else:

```console
KUBECTL_NV_EVAL_AGENT='my-agent --print' hack/eval/run.sh
```

The agent command is evaluated as a shell command line, so anything in it that
the shell would read as syntax has to be quoted, as the parentheses and braces
are above. It runs with the case directory as its working directory, the
question on stdin, and its stdout taken as the final answer.
`hack/eval/run.sh` lists the rest of the environment it reads.

Once a run is recorded, put its row in the top level README:

```console
hack/eval/render-results.sh
```

## What the agent can reach

The agent is a model and the lane is a real cluster, so what it can reach is
bounded rather than trusted.

`./kubectl-nv` in the case directory is `hack/eval/kubectl-nv-shim.sh`, not
the binary. The allow-list pattern `Bash(./kubectl-nv:*)` allows any argument,
so without the shim an agent could point the binary at another cluster with
`--server`, read another identity with `--kubeconfig`, or write anywhere on
the machine with `--artifacts-dir`. The shim refuses every kubectl connection
flag, both directory flags and the two commands that can exec inside a pod,
then execs the real binary with the lane fixed. The real binary is outside the
case directory.

The agent runs under `env -i` with `PATH`, `LANG`, `HOME` and `KUBECONFIG` and
nothing else, with a `HOME` of its own that starts empty. It cannot read the
operator's Claude settings, and nothing in its environment says which fault is
running, so it has to find out.

A wall clock bound ends an agent that does not return. There is no
`--max-turns` in claude 2.1.296, so the bound is a background run plus a kill.

The lane kubeconfig is checked before anything is injected: the current
context has to be this lane's exactly, the file has to carry exactly one
context, and the server has to be on the loopback address. A kubeconfig that
can reach anything but a local kind lane is refused, because that file is
handed to the agent.

Each case directory and the agent's `HOME` are removed when the run ends,
whichever way it ended, because the case directory holds a copy of the lane
kubeconfig.

## Scoring

The prompt asks the agent to end its reply with one line:

```text
ANSWER node=<node> component=<component> code=<code>
```

A case passes when that line names all three correctly. Scoring reads that
line and nothing else: an answer that lists every code it can think of has not
diagnosed anything, and crediting it for containing the right one would score
the shotgun above the diagnosis.

- `node` is matched exactly. A node name is a DNS label, so case matters, and
  naming `worker2` is not naming `worker`.
- `component` is matched exactly against the `component` field of the finding,
  which is read out of the live `HealthReport` rather than from a table inside
  the harness. A rule that changes which component it blames changes what the
  harness expects.
- `code` ignores case, because a report prints it upper case and prose does
  not always, but it is still the whole code: `NV-CTK-0010` is not
  `NV-CTK-001`.

A finding about the cluster rather than about one node carries no node at
all. For those the expected node is `-`, and an answer that names a node
instead is wrong. The harness reads that off the report rather than assuming
the node it injected on, so a rule that stops carrying a node changes what is
expected without the harness being edited.

## Which faults it runs

| Code | Why it is in the set |
|---|---|
| `NV-NODE-002` | a GPU node advertising nothing placeable, the commonest fault |
| `NV-NODE-003` | fewer GPUs than the node has, and a warning rather than an error |
| `NV-CTK-001` | a failing operand init container, reported against the toolkit |
| `NV-DCGM-001` | an operand that is not ready, under a ClusterPolicy roll-up |
| `NV-DRV-001` | a driver pod that is not ready |

`NV-OP-001` was in this set and was taken out on 2026-10-13. Its reproduction
and `NV-DCGM-001`'s are the same move, an operand version pointed at an image
that does not exist, so both produce a report of the same shape: the
cluster-scoped `NV-OP-001` beside the node-scoped findings of whichever
operand went down. The two cases wanted different answers from reports an
agent cannot tell apart, so no reading of the report could score both. The
roll-up still appears beside `NV-DCGM-001`, which is what that case now
tests.

## The result file

One file per run, at `eval/results/<date>.json`, overwritten by a second run
on the same day.

| Field | Meaning |
|---|---|
| `schemaVersion` | `1`. A change in meaning gets a new number. |
| `startedAt`, `finishedAt` | RFC3339 UTC timestamps |
| `lane` | The Mokka lane name, for instance `faults` |
| `context` | The kubeconfig context the run used |
| `node` | The node every case was injected on |
| `agent` | The agent command line, verbatim |
| `cases` | One entry per fault, in the order they ran |
| `summary.cases` | How many cases ran |
| `summary.passed` | How many passed |
| `summary.failed` | How many did not |

A run that stopped early because the lane could not be put back has fewer
`cases` than faults, which is the difference between a short run and a clean
one.

Each entry of `cases`:

| Field | Meaning |
|---|---|
| `code` | The finding code that was injected |
| `node` | The node it was injected on |
| `expectedNode` | What the answer had to name: the node, or `-` for a cluster scoped finding |
| `component` | The component the report blamed, empty if it never appeared |
| `pass` | True when all three parts were named and the lane recovered |
| `named.node` | The ANSWER line named the node |
| `named.component` | The ANSWER line named the component |
| `named.code` | The ANSWER line named the code |
| `observedPolls` | How many health polls it took for the fault to show |
| `agentSeconds` | How long the agent took |
| `error` | Why the case could not be scored, empty when it was |
| `recoverError` | Why the lane could not be put back, empty when it was |

An example:

```json
{
  "schemaVersion": 1,
  "startedAt": "2026-10-12T09:00:00Z",
  "finishedAt": "2026-10-12T09:31:00Z",
  "lane": "faults",
  "context": "kind-kubectl-nv-faults",
  "node": "kubectl-nv-faults-worker",
  "agent": "claude -p --restricted ...",
  "cases": [
    {
      "code": "NV-NODE-002",
      "node": "kubectl-nv-faults-worker",
      "expectedNode": "kubectl-nv-faults-worker",
      "component": "node",
      "pass": true,
      "named": {"node": true, "component": true, "code": true},
      "observedPolls": 4,
      "agentSeconds": 73,
      "error": "",
      "recoverError": ""
    }
  ],
  "summary": {"cases": 5, "passed": 5, "failed": 0}
}
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Every case passed |
| 1 | At least one case failed, or the lane could not be put back |
| 2 | Refused: `CI` is set, or the kubeconfig is not this lane's |
| 3 | Could not run: a required tool, file or script is missing |

The lane is put back whichever way the run ended, including an interrupt. A
fault that could not be injected is still recovered, because an injection that
failed halfway has still changed the cluster.

A recover that fails, or a finding that is still reported after the recover
timeout, ends the run: the next fault must not be injected onto a lane that
still carries the last one, and a cluster left broken is worse than a short
run. The results written up to that point are still recorded.

## Testing the harness

`hack/eval/run_test.sh` runs the whole harness against fake fault scripts, a
fake binary, the real shim and a stub agent. No cluster is created and no
model is called, and it passes with `CI` set, so it can join the suite that
CI runs.
