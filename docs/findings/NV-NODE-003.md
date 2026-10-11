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

On a Mokka lane, fail one simulated device on a worker and let the
device plugin re-advertise what is left.

```shell
POD=$(kubectl -n mokka get pod -l app.kubernetes.io/name=nvml-mock \
  --field-selector spec.nodeName=<node> \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n mokka exec "$POD" -- nvml-mock-ctl fail --gpu 0 \
  --mode ecc_uncorrectable --xid 79
```

Recover with `--mode healthy` on the same GPU. The flags are the ones
`docs/nvml-mock-ctl.md` documents in k8s-test-infra v0.4.0, the chart
version this lane pins.

What the node's allocatable count does in response has not been observed
here, and this entry does not claim it: the device plugin may drop the
device, mark it unhealthy, or leave the count alone. E3 runs the
injection against a live lane and records the answer, in
`hack/mokka/faults/nv-node-003.sh inject|recover <node>`.

Reserving a GPU through the kubelet's `--system-reserved` produces the
same split on real hardware.
