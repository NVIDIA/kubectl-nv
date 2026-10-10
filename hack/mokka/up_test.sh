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
# records its argv into a log and answers from fixture variables.
#
# The kind shim does not merely record: it writes a kubeconfig to whichever
# path the real kind would have written to, which is --kubeconfig if given,
# else $KUBECONFIG, else $HOME/.kube/config. That is what makes the sentinel
# test below able to fail. A shim that only recorded would leave the sentinel
# untouched no matter what the subject did, and the test would be theater.
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

# Resolve the kubeconfig the real kind would write, in kind's own order of
# precedence: --kubeconfig, then $KUBECONFIG, then $HOME/.kube/config.
kubeconfig_target() {
    local prev="" a target=""
    for a in "$@"; do
        if [ "${prev}" = "--kubeconfig" ]; then
            target="${a}"
        fi
        prev="${a}"
    done
    if [ -z "${target}" ]; then
        target="${KUBECONFIG:-}"
    fi
    if [ -z "${target}" ]; then
        target="${HOME}/.kube/config"
    fi
    printf '%s' "${target}"
}

case "${1:-} ${2:-}" in
    "get clusters")
        if [ -n "${FAKE_KIND_CLUSTERS:-}" ]; then
            echo "${FAKE_KIND_CLUSTERS}"
        fi
        exit 0
        ;;
    "create cluster")
        t="$(kubeconfig_target "$@")"
        mkdir -p "$(dirname "${t}")"
        printf 'apiVersion: v1\ncurrent-context: kind-cluster\n' > "${t}"
        exit "${FAKE_KIND_RC:-0}"
        ;;
    "delete cluster")
        t="$(kubeconfig_target "$@")"
        # The real kind rewrites the file to drop the deleted context.
        if [ -f "${t}" ]; then
            printf 'apiVersion: v1\ncurrent-context: ""\n' > "${t}"
        fi
        exit "${FAKE_KIND_RC:-0}"
        ;;
esac
exit "${FAKE_KIND_RC:-0}"
SHIM
                ;;
            helm)
                cat > "${dir}/helm" <<'SHIM'
#!/bin/bash
echo "helm $*" >> "${MOKKA_CALL_LOG}"

# Real helm takes its global flags either side of the subcommand, so the
# subcommand is the first word that is neither a flag nor a flag's value.
subcommand=""
skip_value=false
for a in "$@"; do
    if [ "${skip_value}" = true ]; then
        skip_value=false
        continue
    fi
    case "${a}" in
        --kubeconfig|--kube-context|--namespace|--version|--values|--set|--timeout)
            skip_value=true
            ;;
        -*) ;;
        *)
            subcommand="${a}"
            break
            ;;
    esac
done

if [ "${subcommand}" = "list" ]; then
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

# Per-query-kind call counter, so a fixture can make the first N calls of one
# kind answer "not ready" and later ones answer "ready".
bump() {
    local f="${MOKKA_STATE_DIR}/$1" n=0
    if [ -f "${f}" ]; then
        n="$(cat "${f}")"
    fi
    n=$((n + 1))
    echo "${n}" > "${f}"
    printf '%s' "${n}"
}

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
    *clusterpolic*)
        if [ -z "${FAKE_CLUSTERPOLICY_STATE-ready}" ]; then
            exit 1
        fi
        printf '%s' "${FAKE_CLUSTERPOLICY_STATE-ready}"
        exit 0
        ;;
    *"get daemonset"*)
        n="$(bump ds)"
        if [ "${n}" -le "${FAKE_DS_NOTFOUND_CALLS-0}" ]; then
            echo 'Error from server (NotFound): daemonsets.apps not found' >&2
            exit 1
        fi
        if [ "${n}" -le "${FAKE_DS_NOTREADY_CALLS-0}" ]; then
            printf '%s' "${FAKE_DS_NOTREADY_STATUS-2 0 2}"
            exit 0
        fi
        printf '%s' "${FAKE_DS_STATUS-2 2 2}"
        exit 0
        ;;
    *control-plane*)
        # The control plane runs none of the operands, so it carries no GPU
        # resource and none of the labels.
        exit 0
        ;;
    *allocatable*)
        n="$(bump gpu)"
        if [ "${n}" -le "${FAKE_GPU_NOTREADY_CALLS-0}" ]; then
            exit 0
        fi
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
# /usr/local/bin cannot be reached, and with HOME pointed at a throwaway
# directory holding a sentinel kubeconfig. Sets RC, OUT and ERR.
run_subject() {
    local script="$1"
    shift
    : > "${MOKKA_CALL_LOG}"
    rm -rf "${MOKKA_STATE_DIR}"
    mkdir -p "${MOKKA_STATE_DIR}"
    PATH="${SHIM_DIR}:/usr/bin:/bin:/usr/sbin:/sbin" HOME="${FAKE_HOME}" \
        "${script}" "$@" > "${WORK}/out" 2> "${WORK}/err"
    RC=$?
    OUT="$(cat "${WORK}/out")"
    ERR="$(cat "${WORK}/err")"
}

sentinel_hash() {
    shasum -a 256 < "${FAKE_HOME}/.kube/config" | awk '{print $1}'
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

assert_not_logged() {
    local want="$1" name="$2"
    if grep -qF -- "${want}" "${MOKKA_CALL_LOG}"; then
        nope "${name}: a recorded call contains: ${want}" \
            "calls:" "$(cat "${MOKKA_CALL_LOG}")"
    else
        ok "${name}"
    fi
}

# ---------------------------------------------------------------------------
# Fixtures shared by every case
# ---------------------------------------------------------------------------

SHIM_DIR="${WORK}/bin"
FAKE_HOME="${WORK}/home"
export MOKKA_CALL_LOG="${WORK}/calls.log"
export MOKKA_STATE_DIR="${WORK}/state"
export MOKKA_BIN_DIR="${WORK}/kubeconfigs"
# Keep the readiness poll from sleeping and from waiting out the production
# timeout; the shims answer on a call count, not on elapsed time.
export MOKKA_POLL_INTERVAL=0
export MOKKA_READY_TIMEOUT=2

PINNED_DIGEST="sha256:8bd910853e49bd46953c95b646d35e4765fa7097b18765040b3d95dbc376f524"

mkdir -p "${FAKE_HOME}/.kube"
printf '%s\n' 'SENTINEL do not touch the default kubeconfig' > "${FAKE_HOME}/.kube/config"
SENTINEL_BEFORE="$(sentinel_hash)"

reset_fixtures() {
    unset FAKE_KIND_CLUSTERS FAKE_KIND_RC FAKE_HELM_RELEASES FAKE_HELM_RC
    unset FAKE_WORKERS FAKE_ALLOCATABLE FAKE_GPU_PRODUCT FAKE_PCI_LABEL
    unset FAKE_DS_STATUS FAKE_DS_NOTFOUND_CALLS FAKE_DS_NOTREADY_CALLS
    unset FAKE_DS_NOTREADY_STATUS FAKE_GPU_NOTREADY_CALLS FAKE_CLUSTERPOLICY_STATE
    make_shims "${SHIM_DIR}" kind helm kubectl docker
}

rm -rf "${SHIM_DIR}"
reset_fixtures

# ---------------------------------------------------------------------------
# Argument validation
# ---------------------------------------------------------------------------

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

run_subject "${UP}" "UPPER" t4
assert_rc 2 "up.sh rejects an uppercase lane"

run_subject "${UP}" "-leading" t4
assert_rc 2 "up.sh rejects a lane starting with a hyphen"

# A lane carrying an embedded newline must be rejected on the WHOLE string.
# A line-oriented check accepts this, and the second line is another cluster's
# name, which is how a teardown reaches the wrong cluster.
run_subject "${UP}" "$(printf 'x\nbench-mokka')" t4
assert_rc 2 "up.sh rejects a lane with an embedded newline"
assert_not_logged "create cluster" "a rejected lane creates no cluster"

run_subject "${DOWN}" "$(printf 'x\nbench-mokka')"
assert_rc 2 "down.sh rejects a lane with an embedded newline"
assert_not_logged "delete cluster" "a rejected lane deletes no cluster"

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
assert_logged "gpu-operator-values.yaml" "the operator is installed from the values file"
assert_says "kubeconfig-mokka" "up.sh prints the kubeconfig path"
assert_says "ClusterPolicy state: ready" "up.sh records the ClusterPolicy state"

# The operator must never be configured through --set.
operator_installs="$(grep -F 'upgrade --install gpu-operator' "${MOKKA_CALL_LOG}")"
if [ -z "${operator_installs}" ]; then
    nope "no GPU Operator install was recorded at all" "$(cat "${MOKKA_CALL_LOG}")"
elif printf '%s\n' "${operator_installs}" | grep -q -- '--set'; then
    nope "the GPU Operator install carries no --set" "$(grep 'gpu-operator' "${MOKKA_CALL_LOG}")"
else
    ok "the GPU Operator install carries no --set"
fi

# Every chart install waits rather than returning the moment the objects exist.
nowait=""
installs=0
while IFS= read -r call; do
    case "${call}" in
        helm\ *upgrade\ --install\ *)
            installs=$((installs + 1))
            case "${call}" in
                *" --wait "*|*" --wait") ;;
                *) nowait="${nowait}${call}"$'\n' ;;
            esac
            ;;
    esac
done < "${MOKKA_CALL_LOG}"
if [ "${installs}" -ne 2 ]; then
    nope "expected 2 helm installs, saw ${installs}" "$(cat "${MOKKA_CALL_LOG}")"
elif [ -z "${nowait}" ]; then
    ok "every helm install carries --wait"
else
    nope "a helm install does not carry --wait" "${nowait}"
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
# Kubeconfig isolation
# ---------------------------------------------------------------------------
#
# `kind get clusters` is the one command here with no --kubeconfig flag; it
# enumerates Docker containers and never opens a kubeconfig. It is the only
# exemption, and the sentinel check below is what makes the exemption evidence
# rather than an assertion.

missing_kc=""
while IFS= read -r call; do
    case "${call}" in
        "kind get clusters"*) continue ;;
        kind\ *|kubectl\ *|helm\ *)
            case "${call}" in
                *"--kubeconfig ${MOKKA_BIN_DIR}/kubeconfig-mokka"*) ;;
                *) missing_kc="${missing_kc}${call}"$'\n' ;;
            esac
            ;;
    esac
done < "${MOKKA_CALL_LOG}"

if [ -z "${missing_kc}" ]; then
    ok "every kind, kubectl and helm call carries the lane --kubeconfig"
else
    nope "some calls do not carry the lane --kubeconfig" "${missing_kc}"
fi

if grep -F 'kind create cluster' "${MOKKA_CALL_LOG}" | grep -qF -- "--kubeconfig ${MOKKA_BIN_DIR}/kubeconfig-mokka"; then
    ok "kind create cluster writes the lane kubeconfig, not the default one"
else
    nope "kind create cluster does not carry --kubeconfig" "$(grep 'kind create' "${MOKKA_CALL_LOG}")"
fi

assert_not_logged "/.kube/config" "no recorded call names a .kube/config path"
assert_not_logged "export kubeconfig" "up.sh does not export into the default kubeconfig"

if [ "$(sentinel_hash)" = "${SENTINEL_BEFORE}" ]; then
    ok "up.sh leaves the default kubeconfig byte-identical"
else
    nope "up.sh modified the default kubeconfig" \
        "before: ${SENTINEL_BEFORE}" "after:  $(sentinel_hash)"
fi

# ---------------------------------------------------------------------------
# Readiness: the DaemonSet gate
# ---------------------------------------------------------------------------

export FAKE_DS_NOTFOUND_CALLS=3
run_subject "${UP}" mokka t4
assert_rc 0 "up.sh keeps waiting while the operand DaemonSets are not found yet"
unset FAKE_DS_NOTFOUND_CALLS

export FAKE_DS_NOTREADY_CALLS=3
run_subject "${UP}" mokka t4
assert_rc 0 "up.sh keeps waiting while the operand DaemonSets are not ready yet"
unset FAKE_DS_NOTREADY_CALLS

# Never ready: the deadline is reached and the last failing condition printed.
export FAKE_DS_NOTREADY_CALLS=100000
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when the operand DaemonSets never become ready"
assert_says "nvidia-device-plugin-daemonset" "up.sh names the DaemonSet that is not ready"
assert_says "desired=2 ready=0 updated=2" "up.sh prints the last DaemonSet numbers it saw"
unset FAKE_DS_NOTREADY_CALLS

export FAKE_DS_NOTFOUND_CALLS=100000
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when the operand DaemonSets never appear"
assert_says "not found" "up.sh reports the DaemonSet as not found at the deadline"
unset FAKE_DS_NOTFOUND_CALLS

# A DaemonSet scheduled onto fewer nodes than there are workers is not ready.
export FAKE_DS_STATUS="1 1 1"
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when a DaemonSet covers fewer nodes than there are workers"
unset FAKE_DS_STATUS

# ---------------------------------------------------------------------------
# Readiness: the per-worker assertions are themselves the loop
# ---------------------------------------------------------------------------

# Two workers, so each poll makes two allocatable calls: six calls is three
# polls that answer "not there yet" before the fourth succeeds.
export FAKE_GPU_NOTREADY_CALLS=6
run_subject "${UP}" mokka t4
assert_rc 0 "up.sh retries the per-worker assertions until they pass"
unset FAKE_GPU_NOTREADY_CALLS

export FAKE_GPU_NOTREADY_CALLS=100000
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when the per-worker assertions never pass"
assert_says "expected 4" "up.sh prints the last failing condition at the deadline"
unset FAKE_GPU_NOTREADY_CALLS

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
assert_says "partial install left in kubectl-nv-mokka; run hack/mokka/down.sh mokka" \
    "a failed run says a partial install was left behind"
unset FAKE_PCI_LABEL

# A run that never creates a cluster must NOT claim one was left behind.
export FAKE_KIND_CLUSTERS="kubectl-nv-mokka"
run_subject "${UP}" mokka t4
if printf '%s\n%s\n' "${OUT}" "${ERR}" | grep -qF 'partial install left in'; then
    nope "a refusal must not claim a partial install" "stderr: ${ERR}"
else
    ok "a refusal does not claim a partial install"
fi
unset FAKE_KIND_CLUSTERS

# Every worker is checked, not just the first one.
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
    *clusterpolic*) printf 'ready'; exit 0 ;;
    *"get daemonset"*) printf '2 2 2'; exit 0 ;;
    *allocatable*)
        case "${args}" in
            *" w2 "*|*" w2"*) echo 1 ;;
            *) echo 4 ;;
        esac
        exit 0
        ;;
    *gpu*product*) echo NVIDIA-T4; exit 0 ;;
    *pci-10de*) echo true; exit 0 ;;
esac
exit 0
SHIM
chmod +x "${SHIM_DIR}/kubectl"
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh checks every worker, not only the first"
assert_says "w2" "up.sh names the worker that failed"

rm -rf "${SHIM_DIR}"
reset_fixtures

# ---------------------------------------------------------------------------
# Profiles map to the counts the chart advertises
# ---------------------------------------------------------------------------
#
# The expected numbers are written out here rather than read from lib.sh:
# they come from counting the `devices:` list in the nvml-mock chart's
# profiles/<profile>.yaml at k8s-test-infra 4ce11e5. A test that asked lib.sh
# what lib.sh should say would pass for any value.

check_profile() {
    local profile="$1" want="$2"
    export FAKE_ALLOCATABLE="${want}"
    run_subject "${UP}" mokka "${profile}"
    if [ "${RC}" -eq 0 ] && printf '%s' "${OUT}" | grep -qF "${want} simulated GPUs each (profile ${profile})"; then
        ok "profile ${profile} means ${want} simulated GPUs per node"
    else
        nope "profile ${profile} should mean ${want} simulated GPUs per node" \
            "rc=${RC}" "stdout: ${OUT}" "stderr: ${ERR}"
    fi
    unset FAKE_ALLOCATABLE
}

check_profile gb300 4
check_profile gb200 4
check_profile t4 4
check_profile a100 8
check_profile b200 8
check_profile h100 8
check_profile l40s 8

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

if grep -F 'kind delete cluster' "${MOKKA_CALL_LOG}" | grep -qF -- "--kubeconfig ${MOKKA_BIN_DIR}/kubeconfig-mokka"; then
    ok "kind delete cluster edits the lane kubeconfig, not the default one"
else
    nope "kind delete cluster does not carry --kubeconfig" "$(grep 'kind delete' "${MOKKA_CALL_LOG}")"
fi

if grep -F 'bench-mokka' "${MOKKA_CALL_LOG}" | grep -q 'delete'; then
    nope "down.sh must not touch any other cluster" "$(cat "${MOKKA_CALL_LOG}")"
else
    ok "down.sh touches no other cluster"
fi

if [ "$(sentinel_hash)" = "${SENTINEL_BEFORE}" ]; then
    ok "down.sh leaves the default kubeconfig byte-identical"
else
    nope "down.sh modified the default kubeconfig" \
        "before: ${SENTINEL_BEFORE}" "after:  $(sentinel_hash)"
fi
unset FAKE_KIND_CLUSTERS

# ---------------------------------------------------------------------------
# Static analysis of the scripts under test
# ---------------------------------------------------------------------------
#
# Run from two different working directories, because a source directive that
# resolves relative to the caller's cwd is clean from one and broken from the
# other. Installing shellcheck is out of scope for this script: if it is not on
# PATH, say so loudly rather than reporting a pass nobody earned.

if command -v shellcheck > /dev/null 2>&1; then
    for dir in / "${SCRIPT_DIR}"; do
        if (cd "${dir}" && shellcheck -x "${SCRIPT_DIR}/up.sh" "${SCRIPT_DIR}/down.sh" "${SCRIPT_DIR}/up_test.sh"); then
            ok "shellcheck -x is clean from ${dir}"
        else
            nope "shellcheck -x reported findings from ${dir}"
        fi
        if (cd "${dir}" && shellcheck "${SCRIPT_DIR}/lib.sh"); then
            ok "shellcheck on lib.sh alone is clean from ${dir}"
        else
            nope "shellcheck on lib.sh alone reported findings from ${dir}"
        fi
    done
else
    echo "SKIP - shellcheck is not installed; the scripts were not linted"
fi

# ---------------------------------------------------------------------------

echo
echo "passed: ${PASS}  failed: ${FAIL}"
[ "${FAIL}" -eq 0 ]
