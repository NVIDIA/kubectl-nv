# Agent evaluation

Can an agent holding nothing but `kubectl-nv` and its `SKILL.md` diagnose a
broken GPU node? This directory holds the answers, one file per run.

The harness is `hack/eval/run.sh`. It breaks the faults lane five times in
turn, hands an agent a directory containing the binary, the skill and the lane
kubeconfig, asks what is wrong, scores the answer, and puts the lane back.

## Running it

Runs are manual. The harness talks to a real cluster and spends money on a
real model, so nothing in CI calls it and it refuses to start when `CI` is
set. It also refuses any kubeconfig whose current context is not a
`kind-kubectl-nv-*` lane.

```console
make build
hack/mokka/up.sh faults t4
hack/eval/run.sh
hack/mokka/down.sh faults
```

The default agent command is `claude -p` with its tool allow-list limited to
the binary under evaluation. Override it to score something else:

```console
KUBECTL_NV_EVAL_AGENT='my-agent --print' hack/eval/run.sh
```

The agent command runs with the case directory as its working directory, the
question on stdin, and `KUBECONFIG` pointing at `./kubeconfig`. Whatever it
writes to stdout is the final answer. `hack/eval/run.sh` lists the rest of the
environment it reads.

Once a run is recorded, put its row in the top level README:

```console
hack/eval/render-results.sh
```

## Scoring

A case passes when the final answer names three things: the node that was
broken, the component the finding blames, and the finding code. Each is
matched on whole words, so an answer naming `worker2` is not credited with
naming `worker`, and the code match ignores case.

The component is read out of the live `HealthReport` rather than from a table
inside the harness, so a rule that changes which component it blames changes
what the harness expects. One consequence is worth knowing: where the
component is `node`, almost any sentence about a node contains the word, so
that part of the score is close to free for the `NV-NODE-*` cases. The node
name and the code carry the weight there.

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

Each entry of `cases`:

| Field | Meaning |
|---|---|
| `code` | The finding code that was injected |
| `node` | The node it was injected on |
| `component` | The component the report blamed, empty if it never appeared |
| `pass` | True when all three parts were named |
| `named.node` | The answer named the node |
| `named.component` | The answer named the component |
| `named.code` | The answer named the code |
| `observedPolls` | How many health polls it took for the fault to show |
| `agentSeconds` | How long the agent took |
| `error` | Why the case could not be scored, empty when it was |

An example:

```json
{
  "schemaVersion": 1,
  "startedAt": "2026-10-12T09:00:00Z",
  "finishedAt": "2026-10-12T09:31:00Z",
  "lane": "faults",
  "context": "kind-kubectl-nv-faults",
  "node": "kubectl-nv-faults-worker",
  "agent": "claude -p --allowedTools Bash(./kubectl-nv:*)",
  "cases": [
    {
      "code": "NV-NODE-002",
      "node": "kubectl-nv-faults-worker",
      "component": "node",
      "pass": true,
      "named": {"node": true, "component": true, "code": true},
      "observedPolls": 4,
      "agentSeconds": 73,
      "error": ""
    }
  ],
  "summary": {"cases": 5, "passed": 5, "failed": 0}
}
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Every case passed |
| 1 | At least one case failed |
| 2 | Refused: `CI` is set, or the context is not a lane |
| 3 | Could not run: a required tool, file or script is missing |

The lane is put back whichever way the run ended, including an interrupt. A
fault that could not be injected is still recovered, because an injection that
failed halfway has still changed the cluster.

## Testing the harness

`hack/eval/run_test.sh` runs the whole harness against fake fault scripts, a
fake binary and a stub agent. No cluster is created and no model is called.
