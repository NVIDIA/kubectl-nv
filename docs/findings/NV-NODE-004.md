# NV-NODE-004: the GPU count label disagrees with the node's capacity

Severity: warning. Component: `node`.

## Symptom

`nvidia.com/gpu.count` and the node's `nvidia.com/gpu` capacity describe
the same hardware and do not match. GPU feature discovery wrote the
label from what it found on the node; the capacity is what the device
plugin advertised to the kubelet. Two NVIDIA components counting the
same devices differently means one of them is working from stale or
partial information, and a scheduler is believing the capacity.

The usual causes are a device plugin and a feature discovery pod at
different versions after a partial upgrade, a device plugin that started
before every device was visible, and a node whose hardware changed under
a running operand.

### What the rule accounts for before it fires

Time slicing and MPS make the capacity a multiple of the physical count
on purpose: `nvidia.com/gpu.replicas` says how many times each device is
advertised, so the expected capacity is the count times the replica
factor. A rule that compared the two numbers directly would fire on
every node of every time-sliced cluster.

Nodes running MIG in the mixed strategy are excluded. They advertise one
extended resource per profile and no `nvidia.com/gpu`, so their capacity
is legitimately zero while the count label still reports whole devices.

Nodes in `dra` mode are excluded for the same reason: they publish a
`gpu.nvidia.com` ResourceSlice and no extended resource, so there is no
capacity for the count label to disagree with.

A node with no count label at all is not evaluated: zero there means
"GPU feature discovery has not run", which is NV-NODE-001's finding.

## Evidence

| Key | Meaning |
|---|---|
| `nvidia.com/gpu.count` | what GPU feature discovery found, base 10 |
| `capacity` | the `nvidia.com/gpu` capacity, base 10 |
| `expectedCapacity` | the count times the replica factor, base 10 |
| `nvidia.com/gpu.replicas` | the replica factor, base 10, absent when the node shares nothing |
| `nvidia.com/gpu.sharing-strategy` | the sharing strategy, absent when the node shares nothing |

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

On a Mokka lane, overwrite the count label on one worker with a number
that is not what the device plugin advertised.

```shell
kubectl label node <worker> nvidia.com/gpu.count=7 --overwrite
kubectl nv health --node <worker>
```

GPU feature discovery rewrites the label on its next pass, so the
finding clears on its own. E3 scripts both halves as
`hack/mokka/faults/nv-node-004.sh inject|recover <node>`.
