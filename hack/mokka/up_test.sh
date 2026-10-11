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
        # The namespace has to be explicit. Without it the query would hit the
        # default namespace, where these DaemonSets do not exist, so answering
        # anyway would hide a missing --namespace.
        case "${args}" in
            *"--namespace gpu-operator"*) ;;
            *)
                echo 'Error from server (NotFound): daemonsets.apps not found' >&2
                exit 1
                ;;
        esac
        # Each DaemonSet answers for itself, so a subject that waits on only
        # one of them cannot be told apart from one that waits on both.
        case "${args}" in
            *nvidia-device-plugin-daemonset*) key=DEVICE_PLUGIN ;;
            *gpu-feature-discovery*) key=GFD ;;
            *)
                echo 'Error from server (NotFound): daemonsets.apps not found' >&2
                exit 1
                ;;
        esac
        n="$(bump "ds_${key}")"
        if [ "${n}" -le "${FAKE_DS_NOTFOUND_CALLS-0}" ]; then
            echo 'Error from server (NotFound): daemonsets.apps not found' >&2
            exit 1
        fi
        if [ "${n}" -le "${FAKE_DS_NOTREADY_CALLS-0}" ]; then
            printf '%s' "${FAKE_DS_NOTREADY_STATUS-2 0 2}"
            exit 0
        fi
        status=""
        case "${key}" in
            DEVICE_PLUGIN) status="${FAKE_DS_STATUS_DEVICE_PLUGIN-}" ;;
            GFD) status="${FAKE_DS_STATUS_GFD-}" ;;
        esac
        if [ -z "${status}" ]; then
            status="${FAKE_DS_STATUS-2 2 2}"
        fi
        printf '%s' "${status}"
        exit 0
        ;;
    *control-plane*)
        # The control plane runs none of the operands, so it carries no GPU
        # resource and none of the labels.
        exit 0
        ;;
    *'allocatable.nvidia\.com/gpu'*)
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

# make_tool_dir
# A directory of symlinks to exactly the system tools the scripts and the shims
# need. The subject's PATH is this plus the shim directory and nothing else, so
# a CI image or a developer laptop that happens to have a real docker, kubectl,
# kind or helm on PATH cannot leak one into a test. Putting /usr/bin on the
# subject's PATH would do exactly that wherever those tools are installed
# there, which is the usual case on Linux.
MOKKA_SYSTEM_TOOLS="bash sh env date mktemp grep sed awk cat sleep rm mkdir
chmod tr wc head tail cut sort dirname basename shasum stat ls printf jq"

make_tool_dir() {
    mkdir -p "${TOOL_DIR}"
    local t resolved
    for t in ${MOKKA_SYSTEM_TOOLS}; do
        resolved="$(command -v "${t}" 2> /dev/null || true)"
        # Shell builtins resolve to a bare name; only real files can be linked.
        case "${resolved}" in
            /*) ln -sf "${resolved}" "${TOOL_DIR}/${t}" ;;
        esac
    done
}

# file_mode <path> -- octal permission bits, BSD and GNU stat.
file_mode() {
    stat -f '%Lp' "$1" 2> /dev/null || stat -c '%a' "$1"
}

# run_subject <script> <args...>
# Runs the subject with an isolated PATH, with HOME pointed at a throwaway
# directory holding a sentinel kubeconfig, and with the lane bin directory
# removed so that every run creates it afresh and its mode is the mode this
# run chose. Sets RC, OUT and ERR.
run_subject() {
    local script="$1"
    shift
    : > "${MOKKA_CALL_LOG}"
    rm -rf "${MOKKA_STATE_DIR}"
    if [ "${KEEP_BIN_DIR:-0}" != "1" ]; then
        rm -rf "${MOKKA_BIN_DIR}"
    fi
    mkdir -p "${MOKKA_STATE_DIR}"
    PATH="${SHIM_DIR}:${TOOL_DIR}" HOME="${FAKE_HOME}" \
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
TOOL_DIR="${WORK}/systools"
OUTER_STUBS="${WORK}/outer-stubs"

# Timeouts. A case that is meant to converge gets a deadline long enough to
# survive a loaded machine: the shims answer on a call count rather than on
# elapsed time, so a converging case needs the wall clock only to not run out,
# and `date +%s` has one-second resolution, which made a two-second deadline a
# coin toss under parallel load. A case that is meant to time out gets a short
# deadline and a one-second poll, so it reaches the deadline in a few polls
# instead of spinning.
expect_converge() {
    export MOKKA_READY_TIMEOUT=60
    export MOKKA_POLL_INTERVAL=0
}

expect_timeout() {
    export MOKKA_READY_TIMEOUT=2
    export MOKKA_POLL_INTERVAL=1
}

expect_converge

PINNED_DIGEST="sha256:8bd910853e49bd46953c95b646d35e4765fa7097b18765040b3d95dbc376f524"

mkdir -p "${FAKE_HOME}/.kube"
printf '%s\n' 'SENTINEL do not touch the default kubeconfig' > "${FAKE_HOME}/.kube/config"
SENTINEL_BEFORE="$(sentinel_hash)"

reset_fixtures() {
    unset FAKE_KIND_CLUSTERS FAKE_KIND_RC FAKE_HELM_RELEASES FAKE_HELM_RC
    unset FAKE_WORKERS FAKE_ALLOCATABLE FAKE_GPU_PRODUCT FAKE_PCI_LABEL
    unset FAKE_DS_STATUS FAKE_DS_NOTFOUND_CALLS FAKE_DS_NOTREADY_CALLS
    unset FAKE_DS_NOTREADY_STATUS FAKE_GPU_NOTREADY_CALLS FAKE_CLUSTERPOLICY_STATE
    unset FAKE_DS_STATUS_DEVICE_PLUGIN FAKE_DS_STATUS_GFD
    expect_converge
    make_shims "${SHIM_DIR}" kind helm kubectl docker
}

make_tool_dir

# Stubs for the two tools most likely to exist on a CI image, placed on the
# HARNESS's PATH but deliberately not on the subject's. If the subject could
# see them, the missing-tool cases below would stop returning 3.
mkdir -p "${OUTER_STUBS}"
for stub in docker kubectl kind helm; do
    printf '#!/bin/bash\necho "OUTER STUB %s SHOULD NEVER RUN" >&2\nexit 0\n' "${stub}" \
        > "${OUTER_STUBS}/${stub}"
    chmod +x "${OUTER_STUBS}/${stub}"
done
PATH="${OUTER_STUBS}:${PATH}"
export PATH

if [ "$(command -v docker)" = "${OUTER_STUBS}/docker" ] &&
    [ "$(command -v kubectl)" = "${OUTER_STUBS}/kubectl" ]; then
    ok "the harness PATH does offer a docker and a kubectl to leak"
else
    nope "the outer stubs are not on the harness PATH, so the isolation test proves nothing" \
        "docker:  $(command -v docker)" "kubectl: $(command -v kubectl)"
fi

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
    # The harness PATH carries a stub for every one of these tools. Still 3
    # means the subject's PATH really is the shims plus the curated system
    # tools, with no leak from the surrounding environment.
    assert_rc 3 "up.sh with ${missing} missing, despite a ${missing} on the harness PATH"
    assert_says "up.sh: required tool not found: ${missing}" \
        "up.sh names the missing tool ${missing}"
    if printf '%s\n%s\n' "${OUT}" "${ERR}" | grep -qF 'OUTER STUB'; then
        nope "the subject reached an outer stub; PATH isolation is broken" "stderr: ${ERR}"
    else
        ok "up.sh did not reach the outer ${missing} stub"
    fi
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

# A kubeconfig holds cluster credentials, so the file it lands in and the
# directory holding it must not be world readable. The fake kind creates the
# file the way the real one does, with the umask the subject set.
lane_kc="${MOKKA_BIN_DIR}/kubeconfig-mokka"
if [ -f "${lane_kc}" ]; then
    if [ "$(file_mode "${lane_kc}")" = "600" ]; then
        ok "the lane kubeconfig is created mode 0600"
    else
        nope "the lane kubeconfig is mode $(file_mode "${lane_kc}"), want 600"
    fi
else
    nope "no lane kubeconfig was created at ${lane_kc}"
fi

if [ "$(file_mode "${MOKKA_BIN_DIR}")" = "700" ]; then
    ok "the lane bin directory is created mode 0700"
else
    nope "the lane bin directory is mode $(file_mode "${MOKKA_BIN_DIR}"), want 700"
fi

# The directory outlives any one run, and `mkdir -p` leaves an existing one
# alone. A bin directory left behind by an older version of this script, or by
# a run under a laxer umask, has to be tightened rather than accepted: that is
# the state the real worktree was actually in.
rm -rf "${MOKKA_BIN_DIR}"
mkdir -p "${MOKKA_BIN_DIR}"
chmod 755 "${MOKKA_BIN_DIR}"
KEEP_BIN_DIR=1 run_subject "${UP}" mokka t4
assert_rc 0 "up.sh succeeds with a pre-existing bin directory"
if [ "$(file_mode "${MOKKA_BIN_DIR}")" = "700" ]; then
    ok "up.sh tightens a pre-existing bin directory to 0700"
else
    nope "a pre-existing bin directory stayed mode $(file_mode "${MOKKA_BIN_DIR}"), want 700"
fi

# ---------------------------------------------------------------------------
# ClusterPolicy is recorded, never gated on
# ---------------------------------------------------------------------------

export FAKE_CLUSTERPOLICY_STATE="notReady"
run_subject "${UP}" mokka t4
assert_rc 0 "a ClusterPolicy that is not ready does not fail the run"
assert_says "ClusterPolicy state: notReady" "the notReady ClusterPolicy state is still printed"
unset FAKE_CLUSTERPOLICY_STATE

# An absent ClusterPolicy CRD makes the query fail; that is not a lane failure.
export FAKE_CLUSTERPOLICY_STATE=""
run_subject "${UP}" mokka t4
assert_rc 0 "an unavailable ClusterPolicy does not fail the run"
assert_says "ClusterPolicy state: <unavailable>" "an unavailable ClusterPolicy is reported as such"
unset FAKE_CLUSTERPOLICY_STATE

# ---------------------------------------------------------------------------
# A failing tool is exit 1, and only a created cluster is reported as partial
# ---------------------------------------------------------------------------

export FAKE_HELM_RC=1
run_subject "${UP}" mokka t4
assert_rc 1 "a failing helm install exits 1"
assert_says "partial install left in kubectl-nv-mokka; run hack/mokka/down.sh mokka" \
    "a failing helm install reports the cluster it left behind"
unset FAKE_HELM_RC

export FAKE_KIND_RC=1
run_subject "${UP}" mokka t4
assert_rc 1 "a failing kind create exits 1"
if printf '%s\n%s\n' "${OUT}" "${ERR}" | grep -qF 'partial install left in'; then
    nope "a cluster that was never created must not be reported as partial" "stderr: ${ERR}"
else
    ok "a failing kind create does not claim a partial install"
fi
unset FAKE_KIND_RC

# ---------------------------------------------------------------------------
# Readiness: the DaemonSet gate
# ---------------------------------------------------------------------------

export FAKE_DS_NOTFOUND_CALLS=3
run_subject "${UP}" mokka t4
assert_rc 0 "up.sh keeps waiting while the operand DaemonSets are not found yet"
unset FAKE_DS_NOTFOUND_CALLS
expect_converge

export FAKE_DS_NOTREADY_CALLS=3
run_subject "${UP}" mokka t4
assert_rc 0 "up.sh keeps waiting while the operand DaemonSets are not ready yet"
unset FAKE_DS_NOTREADY_CALLS
expect_converge

# Never ready: the deadline is reached and the last failing condition printed.
expect_timeout
export FAKE_DS_NOTREADY_CALLS=100000
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when the operand DaemonSets never become ready"
assert_says "nvidia-device-plugin-daemonset" "up.sh names the DaemonSet that is not ready"
assert_says "desired=2 ready=0 updated=2" "up.sh prints the last DaemonSet numbers it saw"
unset FAKE_DS_NOTREADY_CALLS
expect_converge

expect_timeout
export FAKE_DS_NOTFOUND_CALLS=100000
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when the operand DaemonSets never appear"
assert_says "not found" "up.sh reports the DaemonSet as not found at the deadline"
unset FAKE_DS_NOTFOUND_CALLS
expect_converge

# A DaemonSet scheduled onto fewer nodes than there are workers is not ready.
expect_timeout
export FAKE_DS_STATUS="1 1 1"
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when a DaemonSet covers fewer nodes than there are workers"
unset FAKE_DS_STATUS
expect_converge

# Both operand DaemonSets are waited on, not just the first. Each of these
# leaves the OTHER one fully ready, so a subject that checks only one of them
# passes one case and fails the other.
expect_timeout
export FAKE_DS_STATUS_DEVICE_PLUGIN="2 2 2"
export FAKE_DS_STATUS_GFD="2 0 2"
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh waits for gpu-feature-discovery, not only the device plugin"
assert_says "gpu-feature-discovery: desired=2 ready=0 updated=2" \
    "up.sh names gpu-feature-discovery as the one that is not ready"
unset FAKE_DS_STATUS_DEVICE_PLUGIN FAKE_DS_STATUS_GFD
expect_converge

expect_timeout
export FAKE_DS_STATUS_DEVICE_PLUGIN="2 0 2"
export FAKE_DS_STATUS_GFD="2 2 2"
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh waits for the device plugin, not only gpu-feature-discovery"
assert_says "nvidia-device-plugin-daemonset: desired=2 ready=0 updated=2" \
    "up.sh names the device plugin as the one that is not ready"
unset FAKE_DS_STATUS_DEVICE_PLUGIN FAKE_DS_STATUS_GFD
expect_converge

# A rollout still in progress reports every pod ready while fewer are updated.
# Checking only numberReady would call that done and hand back a lane running
# the previous image.
expect_timeout
export FAKE_DS_STATUS_DEVICE_PLUGIN="2 2 1"
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when a DaemonSet has fewer updated pods than desired"
assert_says "nvidia-device-plugin-daemonset: desired=2 ready=2 updated=1" \
    "up.sh prints the updated count it rejected"
unset FAKE_DS_STATUS_DEVICE_PLUGIN
expect_converge

# ---------------------------------------------------------------------------
# Readiness: the per-worker assertions are themselves the loop
# ---------------------------------------------------------------------------

# Two workers, so each poll makes two allocatable calls: six calls is three
# polls that answer "not there yet" before the fourth succeeds.
export FAKE_GPU_NOTREADY_CALLS=6
run_subject "${UP}" mokka t4
assert_rc 0 "up.sh retries the per-worker assertions until they pass"
unset FAKE_GPU_NOTREADY_CALLS
expect_converge

expect_timeout
export FAKE_GPU_NOTREADY_CALLS=100000
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when the per-worker assertions never pass"
assert_says "expected 4" "up.sh prints the last failing condition at the deadline"
unset FAKE_GPU_NOTREADY_CALLS
expect_converge

expect_timeout
export FAKE_ALLOCATABLE=2
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when allocatable nvidia.com/gpu is not the profile count"
assert_says "expected 4" "up.sh reports the expected GPU count"
unset FAKE_ALLOCATABLE
expect_converge

expect_timeout
export FAKE_GPU_PRODUCT=""
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when the nvidia.com/gpu.product label is absent"
unset FAKE_GPU_PRODUCT
expect_converge

expect_timeout
export FAKE_PCI_LABEL="false"
run_subject "${UP}" mokka t4
assert_rc 5 "up.sh fails when feature.node.kubernetes.io/pci-10de.present is not true"
assert_says "partial install left in kubectl-nv-mokka; run hack/mokka/down.sh mokka" \
    "a failed run says a partial install was left behind"
unset FAKE_PCI_LABEL
expect_converge

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
expect_timeout
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
expect_converge
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
# The kind topology
# ---------------------------------------------------------------------------
#
# The scale lane needs a wider pod subnet than kind's default, which is a
# topology change and so a different config file, not a flag. Everything else
# keeps the file it has always used.

reset_fixtures
run_subject "${UP}" t1 t4
assert_logged "--config ${SCRIPT_DIR}/kind-mokka.yaml" "up.sh defaults to kind-mokka.yaml"

reset_fixtures
MOKKA_KIND_CONFIG="kind-scale.yaml" run_subject "${UP}" t1 t4
assert_logged "--config ${SCRIPT_DIR}/kind-scale.yaml" "a bare name resolves next to the script"
assert_not_logged "--config ${SCRIPT_DIR}/kind-mokka.yaml" "the override replaces the default rather than adding to it"

reset_fixtures
printf 'kind: Cluster\n' > "${WORK}/elsewhere.yaml"
MOKKA_KIND_CONFIG="${WORK}/elsewhere.yaml" run_subject "${UP}" t1 t4
assert_logged "--config ${WORK}/elsewhere.yaml" "a path is taken as given"

# A misspelled override must not fall back to the default: the lane that came
# up would be the wrong topology, and nothing downstream would say so.
reset_fixtures
MOKKA_KIND_CONFIG="${WORK}/there-is-no-such-file.yaml" run_subject "${UP}" t1 t4
assert_rc 2 "a kind config that does not exist"
assert_says "kind config not found" "the missing config is named"
assert_not_logged "create cluster" "a missing kind config creates nothing"

# The file the scale lane actually points at has to be there and has to carry
# the wider subnet, or the override is pointing at a topology nobody checked.
if [ -f "${SCRIPT_DIR}/kind-scale.yaml" ]; then
    ok "kind-scale.yaml exists"
    if grep -q 'podSubnet: "10.240.0.0/12"' "${SCRIPT_DIR}/kind-scale.yaml"; then
        ok "kind-scale.yaml widens the pod subnet to 10.240.0.0/12"
    else
        nope "kind-scale.yaml does not set podSubnet to 10.240.0.0/12" \
            "$(grep -n 'podSubnet' "${SCRIPT_DIR}/kind-scale.yaml" || echo '  no podSubnet line at all')"
    fi
    if grep -q 'role: worker' "${SCRIPT_DIR}/kind-scale.yaml"; then
        ok "kind-scale.yaml still asks for workers"
    else
        nope "kind-scale.yaml has no worker nodes"
    fi
else
    nope "kind-scale.yaml is missing, so MOKKA_KIND_CONFIG has nothing to point at"
fi

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
