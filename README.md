# NVIDIA kubectl-nv

`kubectl nv` is a kubectl plugin for the people who operate NVIDIA GPU
clusters. It runs on your machine with the kubeconfig and the RBAC you already
have, installs nothing in the cluster, and works on any distribution where the
NVIDIA GPU Operator or the components it manages are deployed. It reports and
never mutates: today it collects the diagnostic bundle that support asks for,
so you do not have to run the troubleshooting commands and the must-gather
script by hand.

## Install

Download a binary from the
[GitHub release page](https://github.com/NVIDIA/kubectl-nv/releases), verify it
against the checksums file, and put `kubectl-nv` on your `PATH` so that
`kubectl nv` resolves. Full instructions, including building from source, are
in [docs/installation.md](docs/installation.md).

## Commands

### kubectl nv adm must-gather

Collects the information from your cluster that is most likely needed for
debugging issues.

```bash
kubectl nv adm must-gather --artifacts-dir /tmp/support
```

```text
OPTIONS:
   --kubeconfig value, -k value  path to kubeconfig file (default: "-") [$KUBECONFIG]
   --artifacts-dir value         path to the directory where the artifacts will be stored. Defaults to /tmp/nvidia-gpu-operator_<timestamp> [$ARTIFACT_DIR]
   --help, -h                    show help
```

## Roadmap

What is planned for the next six months, milestone by milestone, is in
[ROADMAP.md](ROADMAP.md).

## Documentation

- [Documentation index](docs/README.md)
- [Usage](docs/usage.md)
- [Installation](docs/installation.md)
- [Design](docs/design.md)
- [Testing](docs/testing.md)
- [Contributing](docs/contributing.md)

## Testing

CI exercises the plugin against Mokka, which runs the GPU Operator on simulated
GPUs, so changes are tested without GPU hardware.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
