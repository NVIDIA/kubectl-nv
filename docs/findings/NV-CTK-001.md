# NV-CTK-001: the container toolkit validation failed on the node

Severity: error. Component: `nvidia-container-toolkit-daemonset`.

## Symptom

The operator's validator pod on the node has an init container named
`toolkit-validation` that exited non-zero, either now or on the run it
is being restarted after.

The container toolkit is what makes a GPU reachable from inside a
container. When its validation fails the device plugin never starts, so
the node stops advertising `nvidia.com/gpu` and drops out of the GPU
inventory into the hinted one: a node that was running work a minute ago
now looks, to anything reading capacity alone, like a node that never
had GPUs at all. That is why this rule checks the hinted nodes as well,
and why NV-NODE-002 usually fires on the same node without saying why.

The validator runs one init container per thing it validates and carries
each verdict in that container's exit code rather than in a log line,
which is why `pkg/inventory` keeps the init container results of that
one operand and of no other.

### Where the exit code actually is

Two places carry one, and which is filled depends only on the pod's
restart policy. A container that has terminated has its code in the
current state. A container the kubelet is backing off from restarting is
`Waiting` with reason `CrashLoopBackOff` and carries no current exit
code at all: the code of the run that failed is in
`lastTerminationState`.

The validator runs with a restart policy of `Always`, so the crash loop
is the shape this rule meets most often, and a reader of the current
state alone sees a validator with nothing wrong with it. `pkg/inventory`
carries the last termination beside the current one for exactly this.

A last termination is only read under `CrashLoopBackOff`. Any other
waiting reason, `PodInitializing` for instance, is a container that has
not got as far as running this time, and the previous run's exit code
says nothing about it.

### Hinted nodes are checked too

A node whose toolkit validation fails never gets a working device
plugin, so it advertises no `nvidia.com/gpu` and has moved out of the
GPU inventory into the hinted one by the time there is anything to
report. The Mokka control plane runs no validator pod, so checking the
hinted nodes does not put it back into a report.

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
- The init container is running. It then carries no exit code at all,
  and "has not finished" is not "failed": a validation still running,
  reported as a failure, is a finding that clears itself a second later
  and teaches a reader to ignore the rule.
- The init container is waiting for some reason other than
  `CrashLoopBackOff`.
- It is crash looping and its last run exited zero, which is a container
  being restarted for some other reason.
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
| `exitCode` | the code it exited with, base 10, from the current state or, under a crash loop, from the last termination |
| `reason` | the current state's reason: a termination reason such as `Error`, or `CrashLoopBackOff`. Absent when none was recorded |
| `lastTerminationAt` | when that exit happened, RFC3339 in UTC. Present only when the verdict came from the last termination, which is also what says the container is still failing now |

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
