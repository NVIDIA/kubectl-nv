# NVIDIA kubectl-nv

`kubectl-nv` is a kubectl plugin for the people who operate NVIDIA GPU
clusters. It runs on your machine with the kubeconfig and the RBAC you already
have, installs nothing in the cluster, and works on any distribution where the
NVIDIA GPU Operator is deployed.

It answers three kinds of question: what GPU nodes exist and what they carry,
what is wrong with them, and what to do about it. Findings come back as stable
`NV-<AREA>-<NNN>` codes with the evidence behind each one, in a table for a
person and as JSON or YAML for a program.

It never creates, updates, patches or deletes a Kubernetes object. The
exception is the commands that collect a diagnostic bundle: they run
`nvidia-bug-report.sh` inside the driver pods, and that can be turned off with
`--no-exec`. Nothing else in the tool does anything cluster side.

## Install

Download a binary from the
[GitHub release page](https://github.com/NVIDIA/kubectl-nv/releases), verify it
against the checksums file, and put `kubectl-nv` on your `PATH` so that
`kubectl nv` resolves. Full instructions, including building from source, are
in [docs/installation.md](docs/installation.md).

## Commands

### kubectl nv list

Lists the GPU nodes of the cluster: what they advertise, which driver and CUDA
version they run, whether the device plugin or DRA is serving them, and how
many GPUs are already requested.

```bash
kubectl nv list --include-hinted -o json
```

### kubectl nv info

Shows everything known about one node: its GPU labels, what it advertises, its
conditions and taints, the GPU Operator pods on it with their images and
validator results, and the ResourceSlices published for it.

```bash
kubectl nv info worker-1 -o json
```

### kubectl nv health

Runs every GPU health check over the cluster and reports what is wrong, as
findings carrying a code, a severity, a node, a component, the evidence and
the command to run next.

```bash
kubectl nv health --fail-on warning -o json
```

### kubectl nv explain

Prints the catalog entry for a finding code: what it means, what the evidence
is, which GPU Operator versions it applies to, what to run next and how to
reproduce it. The catalog is built into the binary, so it needs no cluster and
no network.

```bash
kubectl nv explain NV-NODE-002
```

### kubectl nv topology

Groups the GPU nodes by NVLink clique, fabric tier and accelerator domain, and
answers where the pods of one job would fit.

```bash
kubectl nv topology --for job/training -n ml
```

### kubectl nv adm must-gather

Collects the diagnostic bundle support asks for, so you do not have to run the
troubleshooting commands and the must-gather script by hand. See
[docs/must-gather.md](docs/must-gather.md).

```bash
kubectl nv adm must-gather --artifacts-dir /tmp/support
```

### kubectl nv schema

Prints the JSON Schema of an output kind, named either by the kind or by the
command that prints it. The schemas are also checked in under
[docs/schema/](docs/schema/README.md).

```bash
kubectl nv schema HealthReport
```

### kubectl nv mcp

Serves the read-only commands as a Model Context Protocol server on stdin and
stdout, one tool per command.

```bash
kubectl nv mcp --list-tools
```

### kubectl nv version

Prints the version of the plugin.

```bash
kubectl nv version
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | The run completed and found nothing at or above `--fail-on` |
| 1 | The run found something at or above `--fail-on` |
| 2 | The command line or the configuration was wrong |
| 3 | The run completed over less than it was asked for |
| 4 | The run could not happen |

Exit 1 is an answer rather than a failure: the command found what it was asked
to look for. Exit 3 is an answer with a hole in it, and what was missed is
listed in `unreachable`, at the top level of the report. The whole contract,
including the output kinds, the streams and the bounded output flags, is in
[docs/agents.md](docs/agents.md).

Data goes to stdout and progress to stderr, with four commands still to catch
up: `kubectl nv version`, `kubectl nv explain`, `kubectl nv schema` and
`kubectl nv mcp --list-tools` print on stderr today. `kubectl nv schema` also
has no kinds registered yet and answers any argument with exit 2.

## Agent surfaces

Four ways to put this tool in front of an agent, all of them read-only:

| Surface | What it is |
|---|---|
| [SKILL.md](SKILL.md) | An Agent Skills skill: which command answers which question, how to read a finding, the exit codes and the guarantees |
| [agents/holmesgpt-toolset.yaml](agents/holmesgpt-toolset.yaml) | A HolmesGPT custom toolset |
| [agents/kubectl-ai-tools.yaml](agents/kubectl-ai-tools.yaml) | kubectl-ai custom tools |
| `kubectl nv mcp` | A Model Context Protocol server over stdio |

A tool offers its command's own flags and `--namespace`, and nothing else:
which cluster is read, and as whom, is not something a model gets to decide.
Commands that can exec into a pod are served with `--no-exec` forced on.

## Evaluation results

How well an agent holding only the binary and `SKILL.md` diagnoses a broken
GPU node, measured on the faults lane. The schema and how to run it are in
[eval/README.md](eval/README.md); the table below is rendered by
`hack/eval/render-results.sh`.

<!-- eval-results:start -->

No evaluation run has been recorded yet. The table appears here once
a run is recorded under eval/results; see [eval/README.md](eval/README.md).

<!-- eval-results:end -->

## Roadmap

What is planned for the next six months, milestone by milestone, is in
[ROADMAP.md](ROADMAP.md).

## Documentation

- [Documentation index](docs/README.md)
- [Agent contract](docs/agents.md): output kinds, exit codes, streams,
  bounded output, the read-only guarantee and MCP
- [Finding catalog](docs/findings/): one entry per `NV-<AREA>-<NNN>` code
- [Output schemas](docs/schema/README.md): the JSON Schema of each kind
- [Must-gather](docs/must-gather.md): what the bundle holds and how it is
  collected
- [Labels](docs/labels.md): the node labels the GPU stack writes and what
  each one means
- [Usage](docs/usage.md)
- [Installation](docs/installation.md)
- [Design](docs/design.md)
- [Testing](docs/testing.md)
- [Contributing](docs/contributing.md)

## Testing

Unit tests run with `make test`. End-to-end tests run against Mokka, which
runs the GPU Operator on simulated GPUs; see
[docs/testing.md](docs/testing.md).

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
