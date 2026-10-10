# kubectl-nv Documentation

## Overview

`kubectl-nv` is a kubectl plugin for managing and troubleshooting
NVIDIA GPU resources in Kubernetes clusters. It provides admin
commands for diagnostics, must-gather, and operator support.

## Quick Start

Install a released binary from the
[GitHub release page](https://github.com/NVIDIA/kubectl-nv/releases), or build
from source:

```bash
git clone https://github.com/NVIDIA/kubectl-nv.git
cd kubectl-nv
make build
./bin/kubectl-nv --help
```

Either way, see [Installation](installation.md) for verifying the download and
putting `kubectl-nv` on your `PATH` so that `kubectl nv` resolves.

## Architecture

```mermaid
graph TD
    CLI[kubectl-nv CLI]
    subgraph Core
      ADM[adm command]
      MG[must-gather]
      LOG[logger]
    end
    K8S[Kubernetes API]
    CRD[Custom Resources]
    CLI --> ADM
    ADM --> MG
    MG -->|collects| K8S
    MG -->|fetches| CRD
    ADM --> LOG
    MG --> LOG
```

## Documentation Index

- [Usage](usage.md)
- [Installation](installation.md)
- [Design](design.md)
- [Contributing](contributing.md)
- [Testing](testing.md)
