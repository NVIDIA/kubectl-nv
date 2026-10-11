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
# Tests for hack/mokka/kwok-nodes.sh.
#
# No cluster is created and nothing is downloaded. kubectl and curl are PATH
# shims: kubectl records its argv and keeps whatever was piped to it, and curl
# writes a body the case chose. That is what lets the node manifests, the
# batching and the checksum refusal be checked for content rather than for an
# exit code.
#
# The subject is resolved relative to THIS file, never through $HOME: a test
# that green-lights a deployed copy of the script says nothing about the tree
# it ships from.
#
# Usage: hack/mokka/kwok-nodes_test.sh
# Exit: 0 all tests pass, 1 at least one failed.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="${SCRIPT_DIR}/kwok-nodes.sh"

PASS=0
FAIL=0

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mokka-kwok-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

SHIM_DIR="${WORK}/bin"
TOOL_DIR="${WORK}/systools"
FAKE_HOME="${WORK}/home"
CACHE_DIR="${WORK}/manifest-cache"
export MOKKA_CALL_LOG="${WORK}/calls.log"
export MOKKA_STATE_DIR="${WORK}/state"
export MOKKA_BIN_DIR="${WORK}/kubeconfigs"

# ---------------------------------------------------------------------------
# Shims
# ---------------------------------------------------------------------------

make_shims() {
    local dir="$1"
    shift
    mkdir -p "${dir}"

    local tool
    for tool in "$@"; do
        case "${tool}" in
            kubectl)
                cat > "${dir}/kubectl" <<'SHIM'
#!/bin/bash
echo "kubectl $*" >> "${MOKKA_CALL_LOG}"
args="$*"

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
    *"get deployment kwok-controller"*|*"get deploy kwok-controller"*)
        if [ "${FAKE_KWOK_INSTALLED:-0}" = "1" ]; then
            echo "deployment.apps/kwok-controller"
            exit 0
        fi
        echo 'Error from server (NotFound): deployments.apps "kwok-controller" not found' >&2
        exit 1
        ;;
    *"apply -f -"*)
        # Keep every object the subject piped in, one file per call, so the
        # batching can be counted and the content can be read.
        n="$(bump apply_stdin)"
        cat > "${MOKKA_STATE_DIR}/applied-${n}.yaml"
        exit "${FAKE_KUBECTL_APPLY_RC:-0}"
        ;;
    *"get nodes"*)
        # The readiness query. Each line is one fake node: its allocatable GPU
        # count and its Ready condition.
        n=0
        while [ "${n}" -lt "${FAKE_READY_COUNT:-0}" ]; do
            printf '%s\n' "${FAKE_NODE_LINE:-8 True}"
            n=$((n + 1))
        done
        exit 0
        ;;
esac
exit "${FAKE_KUBECTL_RC:-0}"
SHIM
                ;;
            curl)
                cat > "${dir}/curl" <<'SHIM'
#!/bin/bash
echo "curl $*" >> "${MOKKA_CALL_LOG}"

# Honour -o <path> and take the last argument as the URL, as the subject
# calls it.
out=""
prev=""
url=""
for a in "$@"; do
    if [ "${prev}" = "-o" ]; then
        out="${a}"
    fi
    case "${a}" in
        -*) ;;
        *) url="${a}" ;;
    esac
    prev="${a}"
done
name="${url##*/}"

if [ "${FAKE_CURL_RC:-0}" != "0" ]; then
    exit "${FAKE_CURL_RC}"
fi

if [ "${FAKE_CORRUPT_MANIFEST:-}" = "${name}" ]; then
    printf 'this is not the manifest that was pinned\n' > "${out}"
    exit 0
fi

if [ -f "${FAKE_MANIFEST_CACHE:-}/${name}" ]; then
    cat "${FAKE_MANIFEST_CACHE}/${name}" > "${out}"
    exit 0
fi

printf 'no fixture for %s\n' "${name}" > "${out}"
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

# The system tools the subject and the shims need, and nothing else, so a real
# kubectl or curl on the harness PATH cannot leak into a case.
MOKKA_SYSTEM_TOOLS="bash sh env date mktemp grep sed awk cat sleep rm mkdir
chmod tr wc head tail cut sort dirname basename shasum stat ls printf seq"

make_tool_dir() {
    mkdir -p "${TOOL_DIR}"
    local t resolved
    for t in ${MOKKA_SYSTEM_TOOLS}; do
        resolved="$(command -v "${t}" 2> /dev/null || true)"
        case "${resolved}" in
            /*) ln -sf "${resolved}" "${TOOL_DIR}/${t}" ;;
        esac
    done
}

run_subject() {
    : > "${MOKKA_CALL_LOG}"
    rm -rf "${MOKKA_STATE_DIR}"
    mkdir -p "${MOKKA_STATE_DIR}"
    PATH="${SHIM_DIR}:${TOOL_DIR}" HOME="${FAKE_HOME}" \
        "${SUBJECT}" "$@" > "${WORK}/out" 2> "${WORK}/err"
    RC=$?
    OUT="$(cat "${WORK}/out")"
    ERR="$(cat "${WORK}/err")"
}

sentinel_hash() {
    shasum -a 256 < "${FAKE_HOME}/.kube/config" | awk '{print $1}'
}

# applied returns every object the subject piped into kubectl, in call order.
applied() {
    local f
    for f in "${MOKKA_STATE_DIR}"/applied-*.yaml; do
        [ -f "${f}" ] || continue
        cat "${f}"
    done
}

apply_calls() {
    local f="${MOKKA_STATE_DIR}/apply_stdin"
    if [ -f "${f}" ]; then
        cat "${f}"
    else
        echo 0
    fi
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
        nope "${name}: no recorded call contains: ${want}" "calls:" "$(cat "${MOKKA_CALL_LOG}")"
    fi
}

assert_not_logged() {
    local want="$1" name="$2"
    if grep -qF -- "${want}" "${MOKKA_CALL_LOG}"; then
        nope "${name}: a recorded call contains: ${want}" "calls:" "$(cat "${MOKKA_CALL_LOG}")"
    else
        ok "${name}"
    fi
}

assert_applied() {
    local want="$1" name="$2"
    if applied | grep -qF -- "${want}"; then
        ok "${name}"
    else
        nope "${name}: nothing applied contains: ${want}"
    fi
}

assert_applied_count() {
    local want="$1" pattern="$2" name="$3"
    local got
    got="$(applied | grep -cF -- "${pattern}" | tr -d ' ')"
    if [ "${got}" = "${want}" ]; then
        ok "${name}"
    else
        nope "${name}: applied objects contain ${pattern} ${got} times, want ${want}"
    fi
}

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

LANE="scale"
CONTEXT="kind-kubectl-nv-${LANE}"
OWNER_LABEL="kubectl-nv.nvidia.com/kwok-node"

mkdir -p "${FAKE_HOME}/.kube"
printf '%s\n' 'SENTINEL do not touch the default kubeconfig' > "${FAKE_HOME}/.kube/config"
SENTINEL_BEFORE="$(sentinel_hash)"

mkdir -p "${MOKKA_BIN_DIR}"
printf 'apiVersion: v1\ncurrent-context: %s\n' "${CONTEXT}" > "${MOKKA_BIN_DIR}/kubeconfig-${LANE}"

reset_fixtures() {
    unset FAKE_KWOK_INSTALLED FAKE_CORRUPT_MANIFEST FAKE_CURL_RC
    unset FAKE_KUBECTL_RC FAKE_KUBECTL_APPLY_RC FAKE_NODE_LINE
    unset MOKKA_CONTEXT
    export FAKE_MANIFEST_CACHE="${CACHE_DIR}"
    export FAKE_READY_COUNT=0
    export MOKKA_READY_TIMEOUT=2
    export MOKKA_POLL_INTERVAL=1
    # KWOK is already there by default, so most cases exercise the node path
    # without needing the real manifests.
    export FAKE_KWOK_INSTALLED=1
    make_shims "${SHIM_DIR}" kubectl curl
}

make_tool_dir
reset_fixtures

# ---------------------------------------------------------------------------
# Argument validation
# ---------------------------------------------------------------------------

run_subject
assert_rc 2 "no arguments"
assert_says "usage: kwok-nodes.sh <count> [<lane>]" "no arguments prints usage"

run_subject many
assert_rc 2 "a count that is not a number"

run_subject 0
assert_rc 2 "a count of zero"

run_subject -5
assert_rc 2 "a negative count"

run_subject 5 Bad_Lane
assert_rc 2 "a lane that is not a DNS label"

run_subject 5 "${LANE}" extra
assert_rc 2 "too many arguments"

# ---------------------------------------------------------------------------
# Context refusal
# ---------------------------------------------------------------------------
#
# The context is the one thing standing between this script and somebody's
# production cluster. It is checked before any tool is reached for, so a
# refusal has to leave the call log empty.

reset_fixtures
MOKKA_CONTEXT="docker-desktop" run_subject 5
assert_rc 7 "a context that is not a kubectl-nv lane"
assert_says "refusing to touch context 'docker-desktop'" "the refusal names the context"
assert_not_logged "apply" "a refused context applies nothing"
assert_not_logged "delete" "a refused context deletes nothing"

export FAKE_READY_COUNT=5
MOKKA_CONTEXT="kind-kubectl-nv-scale-prod" run_subject 5
assert_rc 0 "a lane context with a hyphen in the lane is accepted"
export FAKE_READY_COUNT=0

MOKKA_CONTEXT="kind-kubectl-nv-scale evil" run_subject 5
assert_rc 7 "a context with a space after a lane name"

MOKKA_CONTEXT="kind-kubectl-nv-" run_subject 5
assert_rc 7 "a context with an empty lane"

MOKKA_CONTEXT="prefix-kind-kubectl-nv-scale" run_subject 5
assert_rc 7 "a context that only ends with a lane name"

MOKKA_CONTEXT="docker-desktop" run_subject --delete
assert_rc 7 "--delete against a context that is not a kubectl-nv lane"
assert_not_logged "delete" "a refused --delete deletes nothing"

# ---------------------------------------------------------------------------
# The pinned KWOK release
# ---------------------------------------------------------------------------

reset_fixtures
export FAKE_KWOK_INSTALLED=0
export FAKE_CORRUPT_MANIFEST="kwok.yaml"
run_subject 5
assert_rc 6 "a manifest that does not match its pinned sha256"
assert_says "kwok.yaml does not match its pinned sha256" "the refusal names the manifest"
assert_not_logged "apply -f" "a bad checksum applies nothing"

reset_fixtures
export FAKE_KWOK_INSTALLED=0
export FAKE_CORRUPT_MANIFEST="stage-fast.yaml"
run_subject 5
assert_rc 6 "the second manifest is checked too"
assert_says "stage-fast.yaml does not match its pinned sha256" "the second refusal names the manifest"

# The good install path needs the real manifests, because the pins are in the
# subject and nothing here can forge them. They are fetched once; with no
# network the install assertions are skipped rather than reported as passes.
KWOK_VERSION_UNDER_TEST="$(sed -n 's/^KWOK_VERSION="\(.*\)"$/\1/p' "${SUBJECT}" 2> /dev/null | head -1)"
mkdir -p "${CACHE_DIR}"
have_manifests=true
if [ -n "${KWOK_VERSION_UNDER_TEST}" ]; then
    for m in kwok.yaml stage-fast.yaml; do
        if [ ! -s "${CACHE_DIR}/${m}" ]; then
            command curl -fsSLo "${CACHE_DIR}/${m}" \
                "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION_UNDER_TEST}/${m}" \
                2> /dev/null || have_manifests=false
        fi
    done
else
    have_manifests=false
fi

if [ "${have_manifests}" = true ]; then
    reset_fixtures
    export FAKE_KWOK_INSTALLED=0
    export FAKE_READY_COUNT=5
    run_subject 5
    assert_rc 0 "the pinned manifests are accepted and installed"
    assert_logged "kwok/releases/download/${KWOK_VERSION_UNDER_TEST}/kwok.yaml" "the pinned release is the one fetched"
    assert_logged "apply -f" "the manifests are applied"
    assert_says "${KWOK_VERSION_UNDER_TEST}" "the run names the KWOK release it installed"
else
    echo "SKIP - the pinned KWOK manifests could not be fetched; the install path was not exercised"
fi

# ---------------------------------------------------------------------------
# Node manifest content
# ---------------------------------------------------------------------------

reset_fixtures
export FAKE_READY_COUNT=3
run_subject 3
assert_rc 0 "three nodes against an installed KWOK"

assert_applied_count 3 "kind: Node" "one Node object per requested node"
assert_applied "name: kwok-gpu-1" "the first node is kwok-gpu-1"
assert_applied "name: kwok-gpu-3" "the last node is kwok-gpu-3"
assert_applied_count 0 "name: kwok-gpu-0" "the numbering starts at one"
assert_applied_count 0 "name: kwok-gpu-4" "the numbering stops at the count"

# KWOK only manages a node carrying this annotation; without it the node never
# goes Ready and the lane is a list of unreachable names.
assert_applied "kwok.x-k8s.io/node: fake" "every node carries the KWOK annotation"

# The taint is what keeps the real operands, and any other real pod, off the
# fake nodes.
assert_applied "key: kwok.x-k8s.io/node" "the KWOK taint key is set"
assert_applied "effect: NoSchedule" "the KWOK taint is NoSchedule"

# What makes the node a GPU node to kubectl-nv.
assert_applied "nvidia.com/gpu.present: \"true\"" "the gpu.present label"
assert_applied "feature.node.kubernetes.io/pci-10de.present: \"true\"" "the PCI label"
assert_applied "nvidia.com/gpu.count: \"8\"" "the gpu.count label matches the capacity"
assert_applied "nvidia.com/gpu.product:" "the product label"
assert_applied "nvidia.com/cuda.driver-version.full:" "the driver version label"
assert_applied "nvidia.com/cuda.runtime-version.full:" "the CUDA runtime label"
assert_applied_count 6 "nvidia.com/gpu: \"8\"" "capacity and allocatable on each of the three nodes carry the GPU resource"
assert_applied "${OWNER_LABEL}: \"true\"" "every node is labelled as this script's"

# A fake node that advertised a different GPU count from its gpu.count label
# would make the health rules fire for a reason this lane did not intend.
reset_fixtures
export FAKE_READY_COUNT=1
KWOK_GPU_COUNT=4 run_subject 1
assert_rc 0 "an overridden GPU count"
assert_applied "nvidia.com/gpu.count: \"4\"" "the overridden count reaches the label"
assert_applied_count 2 "nvidia.com/gpu: \"4\"" "the overridden count reaches capacity and allocatable"

# ---------------------------------------------------------------------------
# Batching
# ---------------------------------------------------------------------------
#
# 2,000 nodes one kubectl call at a time is 2,000 process spawns and 2,000
# round trips. The objects go in batches, and the batch boundary is where an
# off-by-one hides: 25 nodes in batches of 10 is three calls, not two.

reset_fixtures
export FAKE_READY_COUNT=25
KWOK_BATCH_SIZE=10 run_subject 25
assert_rc 0 "25 nodes in batches of 10"
assert_applied_count 25 "kind: Node" "all 25 nodes are applied"
if [ "$(apply_calls)" = "3" ]; then
    ok "25 nodes in batches of 10 take three apply calls"
else
    nope "25 nodes in batches of 10 took $(apply_calls) apply calls, want 3"
fi

reset_fixtures
export FAKE_READY_COUNT=20
KWOK_BATCH_SIZE=10 run_subject 20
assert_applied_count 20 "kind: Node" "an exact multiple applies every node"
if [ "$(apply_calls)" = "2" ]; then
    ok "20 nodes in batches of 10 take two apply calls"
else
    nope "20 nodes in batches of 10 took $(apply_calls) apply calls, want 2"
fi

# ---------------------------------------------------------------------------
# Readiness
# ---------------------------------------------------------------------------

reset_fixtures
export FAKE_READY_COUNT=2
run_subject 3
assert_rc 5 "fewer nodes became Ready than were created"
assert_says "2 of 3" "the timeout says how many came up"

reset_fixtures
export FAKE_READY_COUNT=3
export FAKE_NODE_LINE="<none> True"
run_subject 3
assert_rc 5 "nodes that are Ready but advertise no GPUs"

reset_fixtures
export FAKE_READY_COUNT=3
export FAKE_NODE_LINE="8 False"
run_subject 3
assert_rc 5 "nodes that advertise GPUs but are not Ready"

# ---------------------------------------------------------------------------
# --delete
# ---------------------------------------------------------------------------

reset_fixtures
run_subject --delete
assert_rc 0 "--delete against an installed KWOK"
assert_logged "delete nodes --selector ${OWNER_LABEL}=true" "--delete selects exactly the nodes this script labelled"
assert_not_logged "delete nodes --all" "--delete never asks for every node"
assert_not_logged "apply -f -" "--delete creates nothing"

run_subject --delete "${LANE}"
assert_rc 0 "--delete takes the lane as its second argument"

# ---------------------------------------------------------------------------
# Required tools
# ---------------------------------------------------------------------------

reset_fixtures
rm -f "${SHIM_DIR}/curl"
run_subject 1
assert_rc 3 "curl is missing"
assert_says "required tool not found: curl" "the missing tool is named"

reset_fixtures
rm -f "${SHIM_DIR}/kubectl"
run_subject 1
assert_rc 3 "kubectl is missing"

# ---------------------------------------------------------------------------
# The default kubeconfig
# ---------------------------------------------------------------------------

reset_fixtures
export FAKE_READY_COUNT=1
run_subject 1
if [ "$(sentinel_hash)" = "${SENTINEL_BEFORE}" ]; then
    ok "kwok-nodes.sh leaves the default kubeconfig byte-identical"
else
    nope "kwok-nodes.sh modified the default kubeconfig" \
        "before: ${SENTINEL_BEFORE}" "after:  $(sentinel_hash)"
fi
assert_logged "--kubeconfig ${MOKKA_BIN_DIR}/kubeconfig-${LANE}" "every call names the lane kubeconfig"
assert_logged "--context ${CONTEXT}" "every call names the lane context"

# ---------------------------------------------------------------------------
# Static analysis
# ---------------------------------------------------------------------------

if command -v shellcheck > /dev/null 2>&1; then
    for dir in / "${SCRIPT_DIR}"; do
        if (cd "${dir}" && shellcheck -x "${SUBJECT}" "${SCRIPT_DIR}/kwok-nodes_test.sh"); then
            ok "shellcheck -x is clean from ${dir}"
        else
            nope "shellcheck -x reported findings from ${dir}"
        fi
    done
else
    echo "SKIP - shellcheck is not installed; the scripts were not linted"
fi

# ---------------------------------------------------------------------------

echo
echo "passed: ${PASS}  failed: ${FAIL}"
[ "${FAIL}" -eq 0 ]
