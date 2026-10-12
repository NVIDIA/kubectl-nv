# Design

## Overview

`kubectl-nv` is a kubectl plugin built on cobra and `k8s.io/cli-runtime`.
It is read-only toward the cluster: no command creates, updates, patches or
deletes a Kubernetes object. The one cluster-side action is `adm must-gather`
running `nvidia-bug-report.sh` inside driver pods through `pods/exec`.

## Main Components

- **cmd/kubectl-nv/**: the binary's entrypoint; maps errors to exit codes
  and recovers panics.
- **pkg/cmd/**: the cobra tree (`root`, `adm`), the standard kubeconfig
  flags (`--kubeconfig`, `--context`, `-n` and the rest) and the client
  factory every command reads through.
- **pkg/output/**: the machine-readable contract: an envelope with
  `apiVersion: kubectl-nv.nvidia.com/v1alpha1` and a `kind`,
  `-o json|yaml|wide|name` printers, exit codes, and a stderr-only progress
  logger.
- **pkg/source/**: the `Source` interface with a live implementation
  (paginated lists, no per-object gets) and a bundle implementation that
  reads a must-gather directory offline.
- **pkg/mustgather/**: artifact and log collection behind `adm must-gather`.
- **hack/mokka/**: scripts that bring up a kind cluster with Mokka simulated
  GPUs and the GPU Operator.
- **test/e2e/**: black-box tests that run the built binary against that
  cluster (build tag `e2e`).

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | findings at or above `--fail-on` |
| 2 | usage or configuration error |
| 3 | partial result: something could not be read, and the output says what |
| 4 | the command could not run (API unreachable, internal error) |

Progress and warnings go to stderr; stdout carries only command output.

## Testability

- Unit tests use fake clients; a read-only guard runs every command against
  fakes and fails on any write.
- No live cluster is required for `make test`.
- `make e2e-mokka` brings up a kind cluster with simulated GPUs, runs the e2e
  suite against it, and deletes the cluster.
