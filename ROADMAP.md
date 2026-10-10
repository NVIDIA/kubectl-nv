# kubectl-nv roadmap

Last updated: 2026-10-10. This file is the plan of record for the next six months. Milestones map one to one to
GitHub milestones; each line item has an issue. Dates are targets, not commitments.

## What kubectl-nv is

`kubectl nv` is a kubectl plugin for the people who operate NVIDIA GPU clusters: cloud providers, on-prem SREs,
platform teams on OpenShift or managed Kubernetes. It runs on the operator's machine with the kubeconfig and RBAC they
already have, installs nothing in the cluster, and works on any distribution where the NVIDIA GPU Operator (or the
individual components it manages) is deployed.

Three properties are fixed for the whole roadmap:

1. **Read-only by construction.** kubectl-nv reports; it never cordons, drains, resets, labels or edits. The only
   cluster-side action is running `nvidia-bug-report.sh` inside a driver pod during `must-gather`, and `--no-exec`
   (from v0.2.0) turns that off. Remediation belongs to operators and to systems such as NVSentinel.
2. **One binary for humans and agents.** Every command prints a table for a terminal and JSON with a versioned schema
   for everything else, with documented exit codes and no interactive prompts. An SRE can hand the same tool to an
   automation agent without adding approval gates, because there is nothing to approve.
3. **Tested without GPU hardware.** From v0.2.0, CI runs end to end on Mokka, the simulated GPU nodes from
   [NVIDIA/k8s-test-infra](https://github.com/NVIDIA/k8s-test-infra), including injected faults. Every finding in the
   health catalog will have a reproduction in CI.

## Non-goals

No mutations. No in-cluster components, controllers or agents. No metrics storage or dashboards (DCGM exporter and
Prometheus do that). No reimplementation of `nvidia-bug-report.sh` or `dcgmi diag`; kubectl-nv wraps them where it
needs them. No cluster lifecycle: creating clusters, node pools or kubeconfigs is the job of the platform's own CLI.

## Command surface at the end of the roadmap

| Command | What it answers | Milestone |
|---|---|---|
| `kubectl nv adm must-gather` | Collect everything support needs, cluster-wide, into one bundle with an index | v0.1 (exists), parity in v0.2 |
| `kubectl nv list` | Which nodes have GPUs, what model, how many allocatable versus capacity versus requested, which driver and CUDA, device plugin or DRA, MIG and upgrade state, cordoned or not | v0.2 |
| `kubectl nv info <node>` | Everything about one node: labels, allocatable, operand pods and their images, validator results, conditions, taints, DRA ResourceSlices | v0.2 |
| `kubectl nv debug <node>` | A must-gather scoped to one node plus a one-screen summary | v0.2 |
| `kubectl nv health` | Findings against a published catalog, with severity, evidence and the next command to run; `--summary` for the fleet, `--fail-on` for pipelines | v0.3 |
| `kubectl nv explain <code>` | The catalog entry behind a finding code | v0.4 |
| `kubectl nv schema <command>` | The JSON Schema of a command's output | v0.4 |
| `kubectl nv topology` | NVLink cliques, fabric tiers and accelerator domains per node; `--for job/<name>` lists the placements with enough free GPUs | v0.4 |
| `kubectl nv mcp` | The same commands as an MCP server over stdio | v0.4 |

## Milestones

### v0.1.0, October 2026: installable

- Release binaries for linux and darwin, amd64 and arm64, with checksums and an SBOM.
- Install from the GitHub release page; a krew manifest follows in a later release.
- Dependency refresh (Go, client-go, GitHub Actions).
- Fix: `must-gather` wrote one `nvidia-bug-report` file per driver DaemonSet type instead of per node, so multi-node
  clusters kept only the last node's report.

### v0.2.0, December 2026: parity and inventory

- `must-gather` reaches parity with the GPU Operator's `hack/must-gather.sh` as of October 2026 (NVIDIADriver
  resources, KubeVirt resources, operand image list, timestamped multi-container logs, driver pods found by component
  label, the GPU device-usage section) and a CI test keeps the artefact lists equal from then on.
- `must-gather --node`, `--since`, `--no-exec` (degraded mode when `pods/exec` is not allowed), and a `manifest.json`
  index in every bundle.
- `list`, `info`, `debug <node>`, built on `k8s.io/cli-runtime` so `--kubeconfig`, `--context`, `-n` and `-o
  json|yaml|wide|name` behave exactly as in kubectl.
- `list` is one paginated List per resource type and no per-node GETs: under five seconds on 2,000 nodes.
- End-to-end tests on Mokka in GitHub Actions.

### v0.3.0, January 2027: health

- `kubectl nv health [--node N] [--fail-on error|warning] [--summary]`, evaluating API objects only (labels, status,
  allocatable, operand pods, operator CRs, ResourceSlices, ComputeDomain objects). An opt-in `--exec` level that runs
  `nvidia-smi` and `dcgmi` inside the operand pods comes later.
- Twelve findings at launch, each reproduced on Mokka in CI, covering: GPU present but not advertised; allocatable
  zero or below capacity; GFD or device plugin missing on a node; ClusterPolicy not ready; operands paused or disabled
  per node; driver pod not ready or restarting; driver upgrade failed or stuck; container toolkit validation failed;
  MIG configuration not applied; DCGM exporter down; ComputeDomain not ready where one is requested. Informational
  findings for NVSentinel conditions, double advertising (device plugin and DRA on one node) and partial topology
  labels.
- A finding is `{code, severity, node, component, message, evidence, docs, next}`; codes are stable and namespaced
  (`NV-NODE`, `NV-OP`, `NV-DRV`, `NV-CTK`, `NV-DP`, `NV-DRA`, `NV-MIG`, `NV-DCGM`, `NV-CD`, `NV-UPG`, `NV-TOPO`).
- Zero false positives on a healthy Mokka cluster is a release gate.

### v0.4.0, March 2027: the agent contract and topology

- Documented and tested contract for automation: `-o json` on every command with `apiVersion` and `kind`, a JSON
  Schema per command, exit codes (0 ok, 1 findings at or above `--fail-on`, 2 usage or configuration error, 3 partial
  results), no prompts, progress on stderr only, `--no-color` plus TTY detection, bounded output (`--limit`,
  `--fields`, `--summary`), deterministic ordering, `explain <code>`.
- Agent surfaces, cheapest first: a `SKILL.md` following the open Agent Skills format, a HolmesGPT toolset file, a
  kubectl-ai tools example, and `kubectl nv mcp` generated from the command tree.
- A published agent evaluation: five faults injected on Mokka, an agent that has only kubectl-nv and its `SKILL.md`,
  pass means it names the node, the component and the finding code. The pass rate is reported per release.
- `topology`, reading the labels that Node Feature Discovery, Topograph and the NVIDIA DRA driver publish
  (`nvidia.com/gpu.clique`, `fabric.topograph.run/tier-N`, `accelerator.topograph.run/domain`).
- `must-gather --redact` for bundles that leave the premises.
- Catalog at 25 findings.

### v0.5.0, April 2027: adoption

- Central krew index submission.
- `kubectl nv adm must-gather` listed beside the script in the GPU Operator troubleshooting documentation.
- First external pilot feedback folded into the catalog; findings contributed by others.

## How findings are added

One issue per finding with: the symptom an operator sees, the API evidence that proves it, the GPU Operator versions
it applies to, the doc link, the next command, and the Mokka reproduction. A finding without a reproduction does not
ship.

## Feedback

Open an issue with the command you ran, the `-o json` output if you can share it, and what you expected. Feature
requests are welcome; mutating commands will be declined with a pointer to the right tool.
