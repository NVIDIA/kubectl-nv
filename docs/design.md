# Design

## Overview

`kubectl-nv` is structured as a kubectl plugin using urfave/cli for
command parsing. It provides admin subcommands, including must-gather
for diagnostics.

## Main Components

- **cmd/**: CLI entrypoints and command wiring
- **internal/logger/**: Logging utilities
- **mustgather/**: Artifact and log collection logic

## Testability

- All exported functions are covered by table-driven unit tests
- Kubernetes API calls are tested using fake clients
- No live cluster is required for tests
