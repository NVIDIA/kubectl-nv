# NV-NODE-003: fewer allocatable GPUs than the node has

Severity: warning. Component: `node`.

## Symptom

The node's allocatable `nvidia.com/gpu` is above zero and below its
capacity. Some devices are present and not placeable: the kubelet has
taken them out of the allocatable pool, usually because the device
plugin reported them unhealthy, or because a system reserved or kube
reserved setting holds some back.

Allocatable zero is NV-NODE-002 rather than this rule, so the two
partition the space and a node with no placeable GPUs produces one
finding instead of two.

This rule reads the whole-GPU numbers only. A node running MIG in the
mixed strategy advertises one extended resource per profile and no
`nvidia.com/gpu`, so its per-profile shortfall is not covered here.

## Evidence

| Key | Meaning |
|---|---|
| `capacity` | the `nvidia.com/gpu` capacity, base 10 |
| `allocatable` | the `nvidia.com/gpu` allocatable, base 10 |
| `missing` | capacity minus allocatable, base 10 |

## Versions

Every GPU Operator version this plugin supports.

## Documentation

- <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html>
- <https://github.com/NVIDIA/k8s-device-plugin>

## Next command

```shell
kubectl nv info <node>
```

## Reproduction

On a Mokka lane, fail one simulated device on a worker and then restart
the device plugin on that node.

```shell
POD=$(kubectl -n mokka get pod -l app.kubernetes.io/name=nvml-mock \
  --field-selector spec.nodeName=<node> \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n mokka exec "$POD" -- nvml-mock-ctl fail --gpu 0 \
  --mode ecc_uncorrectable --xid 79
kubectl -n gpu-operator delete pod -l app=nvidia-device-plugin-daemonset \
  --field-selector spec.nodeName=<node>
```

Recover with `--mode healthy` on the same GPU and another restart. The
flags are the ones `docs/nvml-mock-ctl.md` documents in k8s-test-infra
v0.4.0, the chart version this lane pins.

The restart is not optional, and it is what the first live run against
this lane found out. The device plugin opens its NVML event set once, at
startup, so a failure injected under a running plugin is never read:
capacity and allocatable stayed at 4 and 4 for the six minutes that run
polled them. Restarted after the injection, the same plugin logs
`XidCriticalError: Xid=79 on Device=GPU-00000074-...; marking device as
unhealthy` and the node goes to capacity 4, allocatable 3 within ten
seconds. The kubelet's device manager keeps unhealthy devices in
capacity and takes them out of allocatable, which is exactly the gap
this rule is about.

`hack/mokka/faults/nv-node-003.sh inject|recover <node>` scripts both
halves, the restart included. Measured on a t4 lane on 2026-10-11: the
finding appears within 11 seconds of the injection and clears within 11
seconds of the recovery.

Reserving a GPU through the kubelet's `--system-reserved` produces the
same split on real hardware.
