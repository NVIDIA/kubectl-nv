# NOTES

## M0: release tooling, dependency refresh, must-gather fixes (2026-10-10)

Track A moved `go.mod` to `go 1.26.0` and every `k8s.io/*` module from v0.33.3 to v0.37.1, and the
`deployments/devel/Dockerfile` image from `golang:1.24.1` to `golang:1.27.1`. `github.com/openshift/client-go`
had to move from the 2024-02-15 pseudo-version to `v0.0.0-20261006222332-348fc1ca8bb1` (`go.mod:7`), because
apimachinery v0.37.1 expects structured-merge-diff v6 parseable types and the old pin generated v4 ones.
`golang.org/x/net` went to v0.60.0 (`go.mod:69`), which cleared five govulncheck advisories reachable through
`cmd/adm/mustgather/cp.go:216` (GO-2026-6603, -6610, -6611, -6612, -6617); govulncheck now reports none. The
vendor tree changed in 3484 files; the rest of the change is 18 files, 2063 insertions and 434 deletions.
must-gather now writes one bug report per driver or vGPU manager pod, named `nvidia-bug-report_<node>.log.gz` as
upstream `hack/must-gather.sh:355` (f6d32ba) does, with `nvidia-bug-report_<node>_<pod>.log.gz` when two pods share
a node (`cmd/adm/mustgather/mustgather.go:982-984`); before, every pod after the first overwrote one file named
by DaemonSet type. A failing pod no longer stops collection on the other nodes: failures are logged and joined
(`mustgather.go:955`), and pods without a node or not Running are skipped. Logs are requested per container,
init containers first, with `Container` and `Timestamps: true` set (`mustgather.go:766`, `:794`), every line
prefixed `[pod/<pod>/<container>] ` (`mustgather.go:834`); the API server refuses the old container-less request
for any multi-container pod. Output file names changed accordingly (`gpu_operator_logs_<container>.log`,
`<pod>_<container>.log`). The package gained 11 tests (`bugreport_test.go`, `logs_test.go`) and its coverage
went from 11.7% to 25.6%. Release tooling: `.goreleaser.yaml` builds linux and darwin on amd64 and arm64 with
sha256 checksums and one syft SBOM per archive into a draft GitHub release (`.goreleaser.yaml:49`, `:63`);
`.github/workflows/release.yaml` runs on `v*` tags only in NVIDIA/kubectl-nv (`release.yaml:29`, `:46`) with
every action pinned to a commit. `make release-snapshot` produced the four binaries and four archives locally.
`hack/lint-words.sh` (23 test cases) fails `make lint` on an em dash, an en dash or the banned simulator and
device names in tracked files. `bin/kubectl-nv` (85 MB) left git. `ROADMAP.md` is the 2026-10-10 draft with the
krew bullet replaced by release-page install and two unbuilt items marked as from v0.2.0.

## M1: cobra skeleton, output contract, source interface, Mokka lane (2026-10-11)

Track B replaced urfave/cli with cobra and `genericclioptions.ConfigFlags`, so every command takes the kubectl
connection flags; `adm must-gather` kept `--kubeconfig`/`-k`, `KUBECONFIG`, `--artifacts-dir`, `ARTIFACT_DIR` and
`--debug`/`-d`, and moved from `cmd/adm/mustgather` to `pkg/mustgather`. The version now comes from ldflags. Exit
codes are 0 ok, 1 findings, 2 usage (`pkg/output/exitcode.go:34`), 3 partial (`:38`) and 4 could not run (`:42`);
`main` recovers a panic into 4 (`cmd/kubectl-nv/main.go:54`). `pkg/output` holds the envelope with `apiVersion:
kubectl-nv.nvidia.com/v1alpha1`, the printers (`-o name` prints `gpunode/<name>`), `ReportStatus` and a stderr-only
logger that strips control characters. `pkg/source` reads live (paginated at 500 with `FullListIfExpired`, no
`Get`) or from a bundle; a GPU node has `nvidia.com/gpu` capacity above zero or a `gpu.nvidia.com` ResourceSlice,
and the PCI and `gpu.present` labels alone make a hinted node (`pkg/source/source.go:197`). must-gather writes
Kubernetes-native YAML, a `resourceslices.yaml`, artifacts 0700/0600, and says so on stderr when there are no driver
pods (`pkg/mustgather/mustgather.go:911`). A guard runs every leaf command against fakes and fails on any write
(`pkg/cmd/readonly_test.go:86`). `hack/mokka` brings up a kind cluster with the Mokka node image pinned by digest
(`hack/mokka/lib.sh:43`), nvml-mock 0.4.0 and GPU Operator v26.3.3, in its own kubeconfig, and `make e2e-mokka`
(`Makefile:91`) runs the e2e suite against it and deletes it. The milestone is 59 commits on 1be66bd: 78 files and
12124 insertions outside `vendor/`, and 21489 vendored lines removed with urfave/cli and yaml.v2. The acceptance run
on 8d6f3ee passed `make build lint test check-vendor e2e-mokka`, the e2e suite in 8.5 s.

## M2: must-gather parity, node scoping, bundle manifest (2026-10-11)

Track C1 rebuilt `adm must-gather` as a registry of collectors (`pkg/mustgather/collector.go:125`) run in the order of
the gpu-operator script `hack/must-gather.sh` at f6d32bad44ee. `TestPlannedArtifactsCoverTheScript`
(`pkg/mustgather/parity_test.go:103`) computes the planned artifact set without a cluster and fails when a file the
script writes has no counterpart; the one deliberate omission is `version`, now the manifest's `toolVersion`. One
collector failing no longer stops the others: its manifest entry reads `error:<text>` and the run exits 3. Four flags
are new (`pkg/cmd/adm/adm.go:93`, `:97`, `:100`, `:102`): `--node` narrows the node-scoped collectors to one node and
exits 2 on a name the cluster does not have, `--since` bounds container logs but never a previous log, `--no-exec`
skips the bug report and the device usage at exit 0, and `--exec-timeout` (default 2m) bounds each exec and copy; both
durations are refused below one second. An exec the API server refuses now exits 3, not 0. The denial fails the HTTP
upgrade before any Status is decoded, so `forbiddenExec` (`pkg/mustgather/exec.go:49`) also reads client-go's untyped
error, and `TestForbiddenExecThroughTheRealExecPath` (`pkg/mustgather/exec_test.go:54`) needs a real httptest socket:
`transport/spdy.RoundTripperFor` passes `UpgradeTransport: nil`, so `rest.Config.Dial`, and with it a user's custom
dialer, never reaches the exec path (upstream behaviour). The agents' Bash sandbox refuses that bind, so the test runs
only unsandboxed. Every bundle ends with `manifest.json`: `kind: BundleManifest`, `bundleSchemaVersion`
(`pkg/mustgather/manifest.go:42`), the flags, one status per collector, and a size and sha256 per file (`:99`).
Artifacts land under a `.part` name and are renamed into place (`pkg/mustgather/mustgather.go:868`), and an artifacts
directory that already holds something is refused with exit 2 (`pkg/mustgather/bundle.go:62`). Driver pods are found
under four selectors (`pkg/mustgather/driverpods.go:39`), the script's three plus `app=nvidia-driver-daemonset`. The
three gaps M1 left open for Track C are closed, each with a test: both driver collection failures are named
(`pkg/mustgather/driverpods_test.go:159`), log collection continues past a failing container (`:191`), and exec
targets the pod's `kubectl.kubernetes.io/default-container` (`:116`). The read-only guard now also pins the commands
the gather may exec, and that `--no-exec` execs nothing (`pkg/cmd/readonly_test.go:418`); over MCP the command is
served with `--no-exec` forced and a server-pinned empty artifacts directory (`pkg/cmd/root/root.go:148`), and two
tests pin the spelling the server forces and the pinned directory (`pkg/cmd/adm/adm_test.go:350`, `:387`). C1 ran 26
mutants, all killed; four survived a first pass and were closed with new tests. Coverage went from 69.3% to 73.4% in
`pkg/mustgather` and from 71.9% to 94.9% in `pkg/cmd/adm`. `TestMustGatherExecAgainstTheLane`
(`test/e2e/mustgather_exec_test.go:79`) drives the exec path against a driver stand-in DaemonSet; on the t4 lane `make
e2e-mokka`, that test included, passed in 122.9 s. The milestone is 23 commits on 4cd4a109: 34 files, 7276 insertions
and 777 deletions outside `vendor/`. The acceptance run on 06e6adf7 (2026-10-13) passed `make build`, `make test`, `go
test -race ./cmd/... ./pkg/...`, golangci-lint (0 issues), `make mdlint`, `make check-vendor` and `make e2e-mokka` (4
top-level tests, the e2e suite in 133.4 s).

## M3: inventory, list, info, debug (2026-10-12)

Track D builds every read-only view from one pass over the cluster, `inventory.Build` (`pkg/inventory/build.go:64`),
into the GPU node model and finding types D0 pinned in the contracts PR (4cd4a109). GPU-node membership
(`pkg/source/source.go:203`) is now capacity above zero in `nvidia.com/gpu`, in any `nvidia.com/mig-<profile>` (the
mixed strategy advertises no `nvidia.com/gpu`) or, new here, in `nvidia.com/gpu.shared` (time-slicing with
`renameByDefault` advertises only that name), or a `gpu.nvidia.com` ResourceSlice; labels alone still make only a
hinted node. `list` prints the GPU nodes and counts on stderr the hinted nodes it left out
(`pkg/cmd/list/list.go:116`), which `--include-hinted` brings in. Requested GPUs sum every bound pod except Succeeded
and Failed ones (`pkg/inventory/requested.go:44`), following `k8s.io/component-helpers`' PodRequests for init
containers and native sidecars (`:96`), and a DRA driver pod, under any of its four label forms, is an operand in any
namespace (`pkg/inventory/operands.go:153`). Each command costs one paginated list per resource and no per-node read:
on a counting fake with 1,200 nodes and 50 pods, `list` made 3 reads (nodes, pods, resourceslices), `list --no-pods` 2
and `info <node>` 3, pinned by `TestOneReadPerCollection` (`pkg/cmd/list/reads_test.go:107`). The ResourceSlice list
is memoised per `source.Live` (`pkg/source/live.go:196`), so classification and mode share one read, and the client
rate limit is QPS 50, Burst 100 when the kubeconfig sets none (`pkg/cmd/factory.go:151`), against client-go's defaults
of 5 and 10. `info <node>` exits 3, not 2, for a node missing from a partly denied read (`pkg/cmd/info/info.go:108`),
a wrong command line exits 2 whoever runs the command (`pkg/cmd/factory.go:114`), and every key and value of the
described view goes through `output.SanitizeCell` (`pkg/output/sanitize.go:38`), tested against a bundle carrying ESC,
OSC, C1 and CR bytes. With `--from-bundle`, `list` and `info` replay a must-gather bundle; its pod list covers the
operator namespace only, so the replay exits 3 with `requestedKnown: false` (`test/e2e/list_test.go:310`). `debug
<node>` (D2) reads the node first, so an unknown or GPU-less node exits 2 with no bundle written
(`pkg/cmd/debug/debug.go:128`), then runs must-gather narrowed to that node with the flags
`pkg/mustgather/flags.go:48` registers, and prints a `DebugReport` or a summary held to 30 lines by construction
(`pkg/cmd/debug/summary.go:49`), its collector counts read back from the bundle's `manifest.json`; over MCP it is
served with exec forced off and its directory pinned (`pkg/cmd/debug/debug.go:89`). `docs/labels.md` records every
node label and resource kubectl-nv reads, including Topograph v1.0.0's keys and that releases up to 0.5.0 wrote
`network.topology.nvidia.com/*`, which kubectl-nv does not read. Mutation runs killed 35 mutants for D1 and 36 for D2;
two of D2's first survived because the exit code alone could not tell which error came back, and those tests now
assert the error. D1's verbose `make e2e-mokka` on the t4 lane ran ten tests, all passing, among them `list
--from-bundle` against C1's rewritten gather. D2's live run showed `debug <worker>` gathering one bug report, for that
worker only, with `flags.node` in the manifest, the node equal to `info`'s, and `--no-exec` at exit 0; its full-suite
run also found that C1's exec test cleaned up while the driver stand-in's pods were still terminating, so a later
`TestMustGatherAgainstTheLane` found a driver pod; the stand-in's delete now waits for its pods with
`--cascade=foreground` (`test/e2e/mustgather_exec_test.go:317`). The milestone is 49 commits on e92df424, not counting
its NOTES.md records: 54 files, 12404 insertions and 26 deletions outside `vendor/`. The acceptance run on this
milestone before those two fixes (2026-10-13) passed the same gates and `make e2e-mokka` (13 top-level tests, 47.3 s).
The gb300 leg of the e2e-mokka workflow then failed the list and info tests, which expected the t4 profile's device
name on every lane; they now take it from `MOKKA_PROFILE` (`test/e2e/list_test.go:41`), and with that and the stand-in
fix (184bd8f8) golangci-lint (0 issues) and the suite passed on t4 (13 tests, 25.3 s) and on gb300 (13 tests, 38.6 s).

## M4: health, sixteen findings, explain, faults, MIG and scale lanes (2026-10-12)

Track E adds `kubectl nv health` and `kubectl nv explain`. `health` reads the inventory with the hinted nodes included
(`pkg/cmd/health/health.go:174`) and runs sixteen rules (`pkg/health/rules/rules.go:84`) into a `HealthReport`: a
summary of nodes checked and GPU capacity, allocatable and requested, then findings with a stable code, severity,
node, component, message, evidence and a next command. `--node` keeps the cluster-scoped findings
(`pkg/health/engine/engine.go:126`), `--fail-on` (default error, `health.go:98`) decides only whether findings make
the exit code 1, and `--summary` drops the findings from the table but not from `-o json`. A rule runs on a node only
when the GPU Operator targets it (`pkg/health/rules/target.go:190`): not every deploy label false or paused, and a
deploy-gated operand already there or some operand not tainted off; that keeps NV-NODE-002 off the hinted control
plane. E1 wrote the first eight codes (node, operator, device plugin, dcgm-exporter); E2 added NV-DRV-001, NV-CTK-001,
NV-UPG-001 (stuck past `--upgrade-stuck-after`, default 30m, `pkg/health/rules/nv_upg_001.go:108`), NV-MIG-001,
NV-CD-001, NV-DRA-001, NV-TOPO-001 and NV-NODE-005, which recognises an NVSentinel condition by its reason being the
condition type plus `IsNotHealthy`, and moved the driver selectors into `pkg/inventory` so must-gather and the rules
read one list (`pkg/mustgather/driverpods.go:37`). NV-CD-001, NV-DRA-001, NV-TOPO-001 and NV-NODE-005 default to info,
and the DRA rules carry no operator version floor because the operator first ships a DRA operand at v26.7.0. The
operator version is read from its image tag and is unknown for a reference with two colons in its name
(`health.go:336`), so every rule applies rather than being gated on a non-version. `explain <code>` prints the catalog
entry embedded from `docs/findings/` and exits 2 on an unknown code; a gate fails on a rule without an entry or an
entry without a rule (`pkg/health/rules/catalog_test.go:39`, `:49`). E3's faults lane gives every code a
`hack/mokka/faults/nv-*.sh inject|recover <node>` script that refuses any context but a `kind-kubectl-nv-*` one on a
loopback server (`hack/mokka/faults/lib.sh:140`). On the t4 lane every code appeared within 1m32s and cleared within
1m2s, the slowest being NV-CTK-001 appearing (the kubelet's back-off before it calls the init container a crash loop)
and NV-DCGM-001 clearing. The lane overturned three reproductions read from source: GFD left a hand-written
`nvidia.com/gpu.count` in place for three minutes, a ClusterPolicy image patch breaks the first node the rolling
update reaches rather than the one asked for (the scripts now delete the target node's pod), and the operator restores
`nvidia.com/gpu.present` within 5 s while it runs; a hand-written `GPUXidError` condition, though, survives kubelet
status updates, so NV-NODE-005 reproduces live. The healthy gate (`test/e2e/health_test.go:317`) wants nothing at
warning or above and runs after the fault suite; the conductor's re-run on the integration branch (2026-10-12) passed
`make e2e-faults` with 27 top-level tests, 0 failed, go test 393.3 s, every fault injected, observed and recovered.
E4's MIG lane builds from k8s-test-infra 4ce11e5a (`hack/mokka/mig/lib.sh:49`), since chart 0.4.0 cannot partition:
h100, two boards of seven `1g.10gb` per worker, capacity and allocatable 14 on each, GPU Operator 276 s, 349 s to
ready, and zero findings at any severity over 3 nodes and 28 GPUs (`test/e2e/mig_test.go:86`). As a negative control
the device plugin ran `--mig-strategy=mixed` on nodes labelled `single`: NV-MIG-001 fired at warning on both workers
and the gate went red. Switching back left the stale `nvidia.com/mig-1g.10gb` in capacity at 14 with allocatable 0,
which overrode the source reading: NV-MIG-001 decides on allocatable (`pkg/health/rules/rules.go:270`). D3's scale
lane (`test/e2e/scale_test.go:60`) is a nightly CI workflow over 2,000 KWOK nodes
(`.github/workflows/e2e-scale.yaml:45`), KWOK v0.8.0 pinned by sha256 (`hack/mokka/kwok-nodes.sh:76`), with kind's pod
subnet widened to 10.240.0.0/12 (`hack/mokka/kind-scale.yaml:25`) because the default 10.244.0.0/16 runs out at about
253 nodes. Its 20-node local smoke measured `list -o json` at a median 333 ms (runs 929, 150, 333) and `health
--summary -o json` at 231 ms (231, 273, 188), with 40 findings, exactly NV-DP-001 and NV-DCGM-001 on each fake node
and none on a real one. Mutation runs killed 48 mutants for E1, 64 in E2's review round, 58 fault-script mutants for
E3 and 19 in E4's review round. The milestone is 64 commits on 00a7fb9b, not counting its NOTES.md record: 143 files,
30600 insertions and 204 deletions outside `vendor/`. The acceptance run on this milestone (2026-10-13) passed the
gates M2 lists, `make test-scripts` (seven harnesses; M6 adds the eighth and raises the floor to eight), `make
e2e-mokka` (16 tests, 53.0 s), `make e2e-faults` (27 tests, 307.7 s) and `make e2e-mig` (`TestMIGLane`, 3.1 s); on top
of M3's gb300 fix, `make e2e-mokka` passed on t4 (16 tests, 41.4 s) and on gb300 (16 tests, 32.4 s).

## Facts observed

- Toolchain on the build Mac: go 1.27.2, golangci-lint 2.14.0 (0 issues), goreleaser 2.18.3, kind v0.33.0,
  helm v4.3.0, kubectl v1.36.1; syft absent, so `make release-snapshot` skips the SBOM.
- `make fmt` and `make verify` ran gofmt over `vendor/` and rewrote 141 vendored files; both now skip `vendor/`
  and `dist/`, and `verify` no longer writes.
- The standalone gosec binary crashes on Go 1.27 (`package "archive/tar" without types`); through golangci-lint it
  reports 5 findings, all in code older than this milestone.
- `cmd/main.go:41` hard-codes version 0.1.0; a tag does not change what `--version` prints.
- Open for Track C: when both the driver and the vGPU manager DaemonSets fail, the returned error names only the
  driver failure (both still reach the error log); per-container log collection still stops at the first failing
  container; `executeRemoteCommand` sends no container name (it now uses the shared REST config, fixed in M1).
- Mokka lane timings on the build Mac, t4 profile: cluster create 18 to 26 s; nvml-mock 63 to 138 s; GPU Operator
  v26.3.3 228 s warm, 297 s and 427 s cold; DaemonSet and per-worker assertions under 1 s; delete 1 to 2 s;
  ClusterPolicy reached `ready` in every run.
- Operands on the lane: device plugin, GFD, dcgm-exporter and operator validator on the two workers only; NFD master,
  worker and gc; no `nvidia-dcgm` and no container toolkit pods (values file disables them).
- GFD labels on a t4 worker: `nvidia.com/gpu.product=NVIDIA-T4`, `gpu.count=4`, `gpu.memory=16384`,
  `gpu.family=turing`, `gpu.compute.major=7`/`minor=5`, `cuda.driver-version.full=550.163.01`,
  `cuda.runtime-version.full=12.4`, `gpu.replicas=1`, `gpu.sharing-strategy=none`, `mig.capable=true`,
  `vgpu.present=false`, and `gpu.deploy.*=true` for every operand.
- The nvml-mock DaemonSet tolerates every taint, so the control plane gets `nvidia.com/gpu.present=true` and
  `pci-10de.present=true` with no allocatable GPUs. Carlos's decision: keep the chart default, count a node as a GPU
  node only by capacity or ResourceSlice, and treat the labels as hints. Track E's `NV-NODE-002` must not fire on a
  node the GPU Operator never targets.
- `mokka-kind-node` has no release tags; the lane pins `sha256:8bd91085...` (equal to `sha-0960c63`).
- Incident: the first lane run called `kind create cluster` without `--kubeconfig`, which wrote into
  `~/.kube/config` and left its current-context unset after `kind delete`. Fixed with a lane-only kubeconfig and a
  sentinel test; every later run left `~/.kube/config` byte-identical (sha256 `cef9726e...`).
- client-go fixes its default kubeconfig path at package init, so `HOME` alone does not isolate a test; tests point
  `KUBECONFIG` at a temp file (`cmd/kubectl-nv/isolation_test.go`).
- The shared module cache under `/tmp/claude-502/gomodcache` failed `go mod verify` on 2026-10-11 and `go mod
  vendor` from it deleted vendored files; the acceptance ran on a fresh cache.
- Open for later: the e2e workflow has never run on GitHub and its gb300 leg is unexercised; a cancelled run
  uploads no diagnostics; Ctrl-C during `up.sh` leaves the lane up; the workflow comment's timeout arithmetic is off.
