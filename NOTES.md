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
  container; `executeRemoteCommand` ignores `--kubeconfig` and sends no container name (`k8sUtils.go:125-157`).
