# NV-CTK-001: the container toolkit validation failed on the node

Severity: error. Component: `nvidia-container-toolkit-daemonset`.

## Symptom

The operator's validator pod on the node has an init container named
`toolkit-validation` that terminated with a non-zero exit code.

The container toolkit is what makes a GPU reachable from inside a
container. When its validation fails the node keeps advertising its
GPUs and every pod placed on it fails to start, which is the worst shape
a GPU node can be in: the scheduler still believes it.

The validator runs one init container per thing it validates and carries
each verdict in that container's exit code rather than in a log line,
which is why `pkg/inventory` keeps the init container results of that
one operand and of no other.

The finding names the toolkit rather than the validator. The validator
is the messenger, and a reader sent to `nvidia-operator-validator`
spends the first ten minutes finding out that the pod it names is
working perfectly. The validator pod is in the evidence instead.

The usual causes are a container runtime whose configuration the toolkit
could not rewrite, a runtime socket at a path the toolkit was not told
about, and a driver that is not loaded, in which case NV-DRV-001 is the
finding to read first.

### When the rule stays quiet

- Another init container failed. Each one is a different component's
  verdict, and reporting a failed driver validation as a toolkit fault
  sends a reader to the wrong place with the right level of confidence.
- The init container has not terminated. It then carries no exit code at
  all, and "has not finished" is not "failed": a validation still
  running, reported as a failure, is a finding that clears itself a
  second later and teaches a reader to ignore the rule.
- The node runs no validator pod. That the validator is missing is
  NV-OP-002's business.
- No GPU Operator.
- Hinted nodes.

## Evidence

| Key | Meaning |
|---|---|
| `nvidia.com/gpu.deploy.container-toolkit` | whether the operator is currently running the toolkit on this node, absent when the node carries no such label |
| `pod` | the validator pod's name |
| `initContainer` | always `toolkit-validation`, so that the evidence says which verdict was read |
| `exitCode` | the code it terminated with, base 10 |
| `reason` | the termination reason, usually `Error`, absent when none was recorded |

## Versions

Every GPU Operator version this plugin supports.

## Documentation

- <https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/index.html>
- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/troubleshooting.html>

## Next command

```shell
kubectl nv info <node>
```

## Reproduction

E3 scripts both halves as
`hack/mokka/faults/nv-ctk-001.sh inject|recover <node>`: it places a
validator pod on the node whose `toolkit-validation` init container
exits non-zero, and deletes it again on recover.
