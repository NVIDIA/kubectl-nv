# Usage

## Basic Command

```bash
kubectl nv adm must-gather --kubeconfig <path> --artifacts-dir <dir>
```

## Flags

- `--kubeconfig, -k`: Path to kubeconfig file (default: env KUBECONFIG)
- `--artifacts-dir`: Directory to store gathered artifacts (default: /tmp/...)
- `--debug, -d`: Enable debug-level logging

## Examples

Collect must-gather logs:

```bash
kubectl nv adm must-gather --artifacts-dir=/tmp/support
```

Show help:

```bash
kubectl nv --help
kubectl nv adm must-gather --help
```
