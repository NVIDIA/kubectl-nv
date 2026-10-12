# NV-NODE-005: an NVSentinel check is failing, or the node is cordoned

Severity: info. Component: `node`.

Informational until the Phase 2 false-positive baseline. The finding is
real, and nothing yet says how often a healthy fleet produces one; a rule
that fails a pipeline before anybody has measured that is a rule
operators switch off rather than read. The severity is raised once the
baseline says what a clean cluster looks like.

It applies with particular force here: what NVSentinel's checks are
called and how often they fire is a property of each deployment's
configuration, and nothing in this repository has seen a real one.

## Symptom

Either an NVSentinel health check is reporting this node unhealthy, or
the GPU node is cordoned, or both. They are reported as separate
findings, because collapsing them into one loses whichever the reader
needed.

### The NVSentinel condition

NVSentinel writes one node condition per health check it runs, and the
condition's type is the check's own name: `GPUXidError`, `GPURowRemap`,
whatever the deployment configured. There is no fixed list to match
against and no prefix on the type, so the recogniser is the reason,
which the platform connector builds as the check name plus
`IsNotHealthy` when the check is failing and plus `IsHealthy` when it is
not.

A condition is this finding when all three hold:

| | |
|---|---|
| status | `True` |
| reason | the condition's own type plus `IsNotHealthy` |
| type | whatever the check is called |

That pairing is what makes the match discriminate. Matching every
non-standard condition at `True` would fire on every node of every
cluster running a log shipper, a storage operator or anything else that
writes a condition of its own, and the rule would be noise within a
week. Matching the reason alone would claim a condition whose reason
happens to name another check. And the status has to be `True`:
NVSentinel writes the `IsNotHealthy` reason only alongside `True`, so a
`False` one is a check that has recovered, with the message
`No Health Failures`.

The source is NVSentinel `93fa8222` (`main` at 2026-10-11),
`platform-connectors/pkg/connectors/kubernetes/process_node_events.go`:
line 193 builds the condition type from the health event's check name,
and line 950 builds the reason from the same name plus `IsNotHealthy` or
`IsHealthy`. Upstream rather than the local fork, which is at 2026-02-12.

### The cordon

`spec.unschedulable` is set. A cordoned GPU node is invisible to the
scheduler, which is worth saying in a report about why no work is
landing, but it is usually somebody doing maintenance on purpose: an
error there would make `kubectl nv health` fail a pipeline every time an
operator drained a node. A cordon stays `info` whatever the baseline
says about the health checks.

### When the rule stays quiet

- A condition NVSentinel did not write, however unusual it looks. A
  cluster's own operators put all sorts of conditions on a node.
- A standard Kubernetes condition at `True`, such as `MemoryPressure`
  with reason `KubeletHasInsufficientMemory`.
- An NVSentinel condition at `False`, which is a check reporting the
  node healthy.
- A node with no conditions at all. `kubectl nv health` fills them, so
  this is a bundle that did not record them rather than a node with no
  faults.
- Hinted nodes.

## Evidence

The health check finding:

| Key | Meaning |
|---|---|
| `condition` | the condition type, which is the health check's name |
| `status` | always `True`, so that the evidence says what was read |
| `reason` | the condition's reason, verbatim |
| `message` | what the check said, absent when it said nothing |
| `since` | the condition's last transition time, RFC3339 in UTC, absent when it carries none |

The cordon finding:

| Key | Meaning |
|---|---|
| `unschedulable` | always `true` |

## Versions

Every GPU Operator version this plugin supports. NVSentinel is not a GPU
Operator component and a cordon is Kubernetes' own, so no operator
version decides whether either means anything.

## Documentation

- <https://github.com/NVIDIA/NVSentinel>
- <https://kubernetes.io/docs/reference/node/node-status/#condition>

## Next command

```shell
kubectl nv info <node>
```

## Reproduction

E3 scripts both halves as
`hack/mokka/faults/nv-node-005.sh inject|recover <node>`: it cordons the
node and patches its status with a `GPUXidError` condition at `True`
whose reason is `GPUXidErrorIsNotHealthy`, then uncordons it and removes
the condition on recover.

The condition is a STRATEGIC merge patch against the status
subresource, and both words carry weight. A plain merge patch replaces
the whole conditions list, taking `Ready` with it, and the kubelet would
evict the lane's pods before putting it back; conditions are keyed by
type under a strategic merge, so one is added and the rest are left
alone.

Observed on a t4 lane on 2026-10-12: after the patch the node carried
`GPUXidError`, `MemoryPressure`, `DiskPressure`, `PIDPressure` and
`Ready`; the new condition was still there two minutes later across six
readings, so nothing reconciles it away; and a `$patch: delete` took it
off again leaving the other four. The finding appears within 1 second
and clears within 3.

Both halves produce a finding, both at info on the same node: one for
the condition and one for the cordon.
