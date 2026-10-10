#!/bin/bash
# Copyright 2026 NVIDIA CORPORATION
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Tests for hack/mokka/up.sh and hack/mokka/down.sh.
#
# No cluster is created. Every external tool is replaced by a PATH shim that
# records its argv into a log and answers from fixture variables, so the tests
# exercise argument validation, the refusal paths, the pins that reach helm,
# and the per-worker assertions.
#
# The subject is resolved relative to THIS file, never through $HOME: a test
# that green-lights a deployed copy of the script says nothing about the tree
# it ships from.
#
# Usage: hack/mokka/up_test.sh
# Exit: 0 all tests pass, 1 at least one failed.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UP="${SCRIPT_DIR}/up.sh"
DOWN="${SCRIPT_DIR}/down.sh"

PASS=0
FAIL=0

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mokka-up-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

# ---------------------------------------------------------------------------
# Shims
# ---------------------------------------------------------------------------

# make_shims <dir> <tool>...
# Writes one recording shim per named tool. A tool left out of the list is
# absent from the shim directory, which is how the missing-tool path is driven.
make_shims() {
    local dir="$1"
    shift
    mkdir -p "${dir}"

    local tool
    for tool in "$@"; do
        case "${tool}" in
            kind)
                cat > "${dir}/kind" <<'SHIM'
#!/bin/bash
echo "kind $*" >> "${MOKKA_CALL_LOG}"
if [ "${1:-}" = "get" ] && [ "${2:-}" = "clusters" ]; then
    if [ -n "${FAKE_KIND_CLUSTERS:-}" ]; then
        echo "${FAKE_KIND_CLUSTERS}"
    fi
    exit 0
fi
exit "${FAKE_KIND_RC:-0}"
SHIM
                ;;
            helm)
                cat > "${dir}/helm" <<'SHIM'
#!/bin/bash
echo "helm $*" >> "${MOKKA_CALL_LOG}"
if [ "${1:-}" = "list" ]; then
    if [ -n "${FAKE_HELM_RELEASES:-}" ]; then
        echo "${FAKE_HELM_RELEASES}"
    fi
    exit 0
fi
exit "${FAKE_HELM_RC:-0}"
SHIM
                ;;
            kubectl)
                cat > "${dir}/kubectl" <<'SHIM'
#!/bin/bash
echo "kubectl $*" >> "${MOKKA_CALL_LOG}"
args="$*"
case "${args}" in
    *"get nodes"*)
        # The selector and the range are honoured, so a subject that forgets to
        # exclude the control plane, or that reads only the first item, gets a
        # different answer and the assertions below notice.
        nodes="${FAKE_WORKERS-kubectl-nv-t1-worker kubectl-nv-t1-worker2}"
        case "${args}" in
            *'!node-role.kubernetes.io/control-plane'*) ;;
            *) nodes="kubectl-nv-t1-control-plane ${nodes}" ;;
        esac
        case "${args}" in
            *'items[*]'*) ;;
            *) nodes="${nodes%% *}" ;;
        esac
        printf '%s\n' ${nodes}
        exit 0
        ;;
    *control-plane*)
        # The control plane runs none of the operands, so it carries no GPU
        # resource and none of the labels.
        exit 0
        ;;
    *allocatable*)
        echo "${FAKE_ALLOCATABLE-4}"
        exit 0
        ;;
    *gpu*product*)
        echo "${FAKE_GPU_PRODUCT-NVIDIA-T4}"
        exit 0
        ;;
    *pci-10de*)
        echo "${FAKE_PCI_LABEL-true}"
        exit 0
        ;;
    *"get pods"*)
        if [ -n "${FAKE_POD_LINES-x}" ]; then
            printf '%s\n' "${FAKE_POD_LINES-gpu-operator-abc Running True}"
        fi
        exit 0
        ;;
esac
exit 0
SHIM
                ;;
            docker)
                cat > "${dir}/docker" <<'SHIM'
#!/bin/bash
echo "docker $*" >> "${MOKKA_CALL_LOG}"
exit 0
SHIM
                ;;
            *)
                echo "make_shims: unknown tool ${tool}" >&2
                return 1
                ;;
        esac
        chmod +x "${dir}/${tool}"
    done
}

# run_subject <script> <args...>
# Runs the subject with a PATH holding only the shim directory plus the system
# directories, so the real kind/helm/kubectl/docker under /opt/homebrew/bin and
# /usr/local/bin cannot be reached. Sets RC, OUT and ERR.
run_subject() {
    local script="$1"
    shift
    : > "${MOKKA_CALL_LOG}"
    PATH="${SHIM_DIR}:/usr/bin:/bin:/usr/sbin:/sbin" \
        "${script}" "$@" > "${WORK}/out" 2> "${WORK}/err"
    RC=$?
    OUT="$(cat "${WORK}/out")"
    ERR="$(cat "${WORK}/err")"
}

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

ok() {
    PASS=$((PASS + 1))
    echo "ok   - $1"
}

nope() {
    FAIL=$((FAIL + 1))
    echo "FAIL - $1"
    shift
    local line
    for line in "$@"; do
        echo "       ${line}"
    done
}

assert_rc() {
    local want="$1" name="$2"
    if [ "${RC}" -eq "${want}" ]; then
        ok "${name} (rc=${want})"
    else
        nope "${name}: want rc=${want}, got rc=${RC}" "stdout: ${OUT}" "stderr: ${ERR}"
    fi
}

assert_says() {
    local want="$1" name="$2"
    if printf '%s\n%s\n' "${OUT}" "${ERR}" | grep -qF -- "${want}"; then
        ok "${name}"
    else
        nope "${name}: output does not contain: ${want}" "stdout: ${OUT}" "stderr: ${ERR}"
    fi
}

assert_logged() {
    local want="$1" name="$2"
    if grep -qF -- "${want}" "${MOKKA_CALL_LOG}"; then
        ok "${name}"
    else
        nope "${name}: no recorded call contains: ${want}" \
            "calls:" "$(cat "${MOKKA_CALL_LOG}")"
    fi
}

# ---------------------------------------------------------------------------
# Fixtures shared by every case
# ---------------------------------------------------------------------------

SHIM_DIR="${WORK}/bin"
export MOKKA_CALL_LOG="${WORK}/calls.log"
export MOKKA_BIN_DIR="${WORK}/kubeconfigs"
# Keep the readiness poll from sleeping and from waiting out the production
# timeout; the shims answer immediately, so a poll that is not satisfied on
# the first pass is never going to be.
export MOKKA_POLL_INTERVAL=0
export MOKKA_READY_TIMEOUT=2

PINNED_DIGEST="sha256:8bd910853e49bd46953c95b646d35e4765fa7097b18765040b3d95dbc376f524"

reset_fixtures() {
    unset FAKE_KIND_CLUSTERS FAKE_KIND_RC FAKE_HELM_RELEASES FAKE_HELM_RC
    unset FAKE_WORKERS FAKE_ALLOCATABLE FAKE_GPU_PRODUCT FAKE_PCI_LABEL
    unset FAKE_POD_LINES
    make_shims "${SHIM_DIR}" kind helm kubectl docker
}


# ---------------------------------------------------------------------------
# Argument validation
# ---------------------------------------------------------------------------

rm -rf "${SHIM_DIR}"
reset_fixtures

run_subject "${UP}"
assert_rc 2 "up.sh with no arguments"
assert_says "usage: up.sh <lane> <profile>" "up.sh with no arguments prints usage"

run_subject "${UP}" mokka
assert_rc 2 "up.sh with one argument"

run_subject "${UP}" mokka a100 extra
assert_rc 2 "up.sh with three arguments"

run_subject "${UP}" mokka v100
assert_rc 2 "up.sh with an unknown profile"
assert_says "up.sh: unknown profile 'v100'; known profiles: a100 b200 gb200 gb300 h100 l40s t4" \
    "up.sh unknown-profile message is exact"

run_subject "${UP}" "bad lane" t4
assert_rc 2 "up.sh with a lane that is not a DNS label"

run_subject "${DOWN}"
assert_rc 2 "down.sh with no arguments"

# ---------------------------------------------------------------------------
# Missing tool
# ---------------------------------------------------------------------------

for missing in kind helm kubectl docker; do
    rm -rf "${SHIM_DIR}"
    present=""
    for t in kind helm kubectl docker; do
        if [ "${t}" != "${missing}" ]; then
            present="${present} ${t}"
        fi
    done
    # shellcheck disable=SC2086
    make_shims "${SHIM_DIR}" ${present}
    run_subject "${UP}" mokka t4
    assert_rc 3 "up.sh with ${missing} missing"
    assert_says "up.sh: required tool not found: ${missing}" \
        "up.sh names the missing tool ${missing}"
done

rm -rf "${SHIM_DIR}"
reset_fixtures

# ---------------------------------------------------------------------------
# Refusal paths
# ---------------------------------------------------------------------------

FAKE_KIND_CLUSTERS="$(printf 'bench-mokka\nkubectl-nv-mokka\n')"
export FAKE_KIND_CLUSTERS
run_subject "${UP}" mokka t4
assert_rc 4 "up.sh refuses when the cluster already exists"
assert_says "up.sh: cluster kubectl-nv-mokka already exists" \
    "up.sh says which cluster is in the way"
assert_says "hack/mokka/down.sh mokka" "up.sh points at down.sh"

# A cluster whose name merely contains the lane name is not the lane cluster.
FAKE_KIND_CLUSTERS="$(printf 'bench-mokka\nkubectl-nv-mokka-old\n')"
export FAKE_KIND_CLUSTERS
run_subject "${UP}" mokka t4
assert_rc 0 "up.sh does not mistake kubectl-nv-mokka-old for kubectl-nv-mokka"

FAKE_KIND_CLUSTERS="$(printf 'kubectl-nv-mokka\n')"
export FAKE_KIND_CLUSTERS
export FAKE_HELM_RELEASES="nvml-mock"
run_subject "${UP}" mokka t4
assert_rc 4 "up.sh refuses when a Mokka release is installed in the cluster"
assert_says "holds Mokka release nvml-mock" "up.sh names the release in the way"

unset FAKE_KIND_CLUSTERS FAKE_HELM_RELEASES

# ---------------------------------------------------------------------------
# Happy path: the pins and the explicit context reach the tools
# ---------------------------------------------------------------------------

run_subject "${UP}" mokka t4
assert_rc 0 "up.sh succeeds when every check and assertion passes"

assert_logged "${PINNED_DIGEST}" "the pinned kind node image digest reaches kind"
assert_logged "oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock" "the nvml-mock chart reference reaches helm"
assert_logged "--version 0.4.0" "the nvml-mock chart version 0.4.0 reaches helm"
assert_logged "--version v26.3.3" "the GPU Operator version v26.3.3 reaches helm"
assert_logged "--set gpu.profile=t4" "the profile reaches helm"
assert_logged "--wait --timeout 15m" "the install waits with a 15m timeout"
assert_logged "gpu-operator-values.yaml" "the operator is installed from the values file"
assert_says "kubeconfig-mokka" "up.sh prints the kubeconfig path"

# The operator must never be configured through --set.
if grep -E '^helm (install|upgrade).*gpu-operator' "${MOKKA_CALL_LOG}" | grep -q -- '--set'; then
    nope "the GPU Operator install carries no --set" "$(grep 'gpu-operator' "${MOKKA_CALL_LOG}")"
else
    ok "the GPU Operator install carries no --set"
fi

# Every kubectl call names the context; every helm call names the kube-context.
bare=""
while IFS= read -r call; do
    case "${call}" in
        kubectl\ *)
            case "${call}" in
                *"--context kind-kubectl-nv-mokka"*) ;;
                *) bare="${bare}${call}"$'\n' ;;
            esac
            ;;
        helm\ *)
            case "${call}" in
                *"--kube-context kind-kubectl-nv-mokka"*) ;;
                *) bare="${bare}${call}"$'\n' ;;
            esac
            ;;
    esac
done < "${MOKKA_CALL_LOG}"

if [ -z "${bare}" ]; then
    ok "every recorded kubectl and helm call names the context explicitly"
else
    nope "some kubectl or helm calls do not name the context" "${bare}"
fi

# ---------------------------------------------------------------------------
# Per-worker assertions must discriminate
# ---------------------------------------------------------------------------

export FAKE_ALLOCATABLE=2
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when allocatable nvidia.com/gpu is not the profile count"
assert_says "expected 4" "up.sh reports the expected GPU count"
unset FAKE_ALLOCATABLE

export FAKE_GPU_PRODUCT=""
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when the nvidia.com/gpu.product label is absent"
unset FAKE_GPU_PRODUCT

export FAKE_PCI_LABEL="false"
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when feature.node.kubernetes.io/pci-10de.present is not true"
unset FAKE_PCI_LABEL

# Every worker is checked, not just the first one: the second worker is the
# one with the wrong count here.
export FAKE_WORKERS="w1 w2"
cat > "${SHIM_DIR}/kubectl" <<'SHIM'
#!/bin/bash
echo "kubectl $*" >> "${MOKKA_CALL_LOG}"
args="$*"
case "${args}" in
    *"get nodes"*)
        case "${args}" in
            *'items[*]'*) printf 'w1\nw2\n' ;;
            *) printf 'w1\n' ;;
        esac
        exit 0
        ;;
    *allocatable*)
        case "${args}" in
            *" w2 "*|*" w2"*) echo 1 ;;
            *) echo 4 ;;
        esac
        exit 0
        ;;
    *gpu*product*) echo NVIDIA-T4; exit 0 ;;
    *pci-10de*) echo true; exit 0 ;;
    *"get pods"*) echo "gpu-operator-abc Running True"; exit 0 ;;
esac
exit 0
SHIM
chmod +x "${SHIM_DIR}/kubectl"
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh checks every worker, not only the first"
assert_says "w2" "up.sh names the worker that failed"

rm -rf "${SHIM_DIR}"
reset_fixtures

# An empty pod list is not readiness.
export FAKE_POD_LINES=""
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh does not treat an empty gpu-operator pod list as ready"
unset FAKE_POD_LINES

# A pod that is neither Ready nor Completed blocks.
export FAKE_POD_LINES="gpu-operator-abc Running False"
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh does not pass a gpu-operator pod that is not Ready"
unset FAKE_POD_LINES

# Completed pods count as done.
FAKE_POD_LINES="$(printf 'gpu-operator-abc Succeeded \ngpu-operator-def Running True\n')"
export FAKE_POD_LINES
run_subject "${UP}" mokka t4
assert_rc 0 "up.sh accepts a Succeeded gpu-operator pod"
unset FAKE_POD_LINES

# ---------------------------------------------------------------------------
# down.sh
# ---------------------------------------------------------------------------

unset FAKE_KIND_CLUSTERS
run_subject "${DOWN}" mokka
assert_rc 2 "down.sh exits 2 when the cluster does not exist"
assert_says "down.sh: no such cluster: kubectl-nv-mokka" "down.sh names the missing cluster"

FAKE_KIND_CLUSTERS="$(printf 'bench-mokka\nkubectl-nv-mokka\n')"
export FAKE_KIND_CLUSTERS
run_subject "${DOWN}" mokka
assert_rc 0 "down.sh deletes the lane cluster"
assert_logged "kind delete cluster --name kubectl-nv-mokka" "down.sh deletes only the lane cluster"

if grep -F 'bench-mokka' "${MOKKA_CALL_LOG}" | grep -q 'delete'; then
    nope "down.sh must not touch any other cluster" "$(cat "${MOKKA_CALL_LOG}")"
else
    ok "down.sh touches no other cluster"
fi
unset FAKE_KIND_CLUSTERS

# ---------------------------------------------------------------------------
# Static analysis of the scripts under test
# ---------------------------------------------------------------------------
#
# -x so that the source directive is followed and the pins in lib.sh count as
# used. Installing shellcheck is out of scope for this script: if it is not on
# PATH, say so loudly rather than reporting a pass nobody earned.

if command -v shellcheck > /dev/null 2>&1; then
    if shellcheck -x "${SCRIPT_DIR}/up.sh" "${SCRIPT_DIR}/down.sh" "${SCRIPT_DIR}/up_test.sh"; then
        ok "shellcheck -x is clean"
    else
        nope "shellcheck -x reported findings"
    fi
else
    echo "SKIP - shellcheck is not installed; the scripts were not linted"
fi

# ---------------------------------------------------------------------------

echo
echo "passed: ${PASS}  failed: ${FAIL}"
[ "${FAIL}" -eq 0 ]
