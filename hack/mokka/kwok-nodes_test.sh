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
        # Every node query the subject makes has to carry the ownership label
        # selector. Without it the answer would be the whole cluster, the real
        # workers included, and a subject that dropped the selector would read
        # a count that happened to be right and act on the wrong nodes.
        case "${args}" in
            *"--selector kubectl-nv.nvidia.com/kwok-node=true"*) ;;
            *)
                echo 'shim: get nodes without the ownership selector' >&2
                exit 1
                ;;
        esac
        n=1
        case "${args}" in
            *podCIDR*)
                # The pod CIDR query: one line per node, name and CIDR. An
                # empty FAKE_POD_CIDR is a node the IPAM controller had no
                # room left for. FAKE_CIDR_PER_POLL is an allocator still
                # working through its queue: each query after the first finds
                # that many more nodes with a CIDR.
                q="$(bump podcidr_query)"
                # FAKE_CIDR_FAIL_FROM..FAKE_CIDR_FAIL_UNTIL are the queries
                # that fail the way a real kubectl does on a transport or
                # server error: exit 1, nothing on stdout.
                if [ -n "${FAKE_CIDR_FAIL_FROM:-}" ] && [ "${q}" -ge "${FAKE_CIDR_FAIL_FROM}" ] \
                    && [ "${q}" -le "${FAKE_CIDR_FAIL_UNTIL:-999999}" ]; then
                    echo 'Error from server (Timeout): the server was unable to return a response in the time allotted' >&2
                    exit 1
                fi
                missing=$(( ${FAKE_NO_CIDR_NODES:-0} - (q - 1) * ${FAKE_CIDR_PER_POLL:-0} ))
                [ "${missing}" -ge 0 ] || missing=0
                # FAKE_CIDR_LINES is a read that lists fewer nodes than exist.
                lines="${FAKE_CIDR_LINES:-${FAKE_READY_COUNT:-0}}"
                while [ "${n}" -le "${lines}" ]; do
                    if [ "${n}" -le "${missing}" ]; then
                        printf 'kwok-gpu-%s \n' "${n}"
                    else
                        printf 'kwok-gpu-%s %s\n' "${n}" "${FAKE_POD_CIDR:-10.240.0.0/24}"
                    fi
                    n=$((n + 1))
                done
                exit 0
                ;;
            *"-o name"*)
                # The census of nodes this script already owns.
                while [ "${n}" -le "${FAKE_EXISTING_COUNT:-0}" ]; do
                    printf 'node/kwok-gpu-%s\n' "${n}"
                    n=$((n + 1))
                done
                exit 0
                ;;
        esac
        # The readiness query. Each line is one fake node: its allocatable GPU
        # count and its Ready condition. FAKE_READY_AFTER is a lane whose
        # nodes turn Ready only on that query.
        n=0
        rq="$(bump ready_query)"
        if [ "${rq}" -lt "${FAKE_READY_AFTER:-1}" ]; then
            exit 0
        fi
        while [ "${n}" -lt "${FAKE_READY_COUNT:-0}" ]; do
            printf '%s\n' "${FAKE_NODE_LINE:-8 True}"
            n=$((n + 1))
        done
        exit 0
        ;;
    *"config view"*)
        printf '%s' "${FAKE_SERVER-https://127.0.0.1:6443}"
        exit 0
        ;;
    *"get stage"*)
        if [ "${FAKE_KWOK_STAGES:-1}" = "1" ]; then
            echo "stage.kwok.x-k8s.io/node-initialize"
            exit 0
        fi
        echo 'Error from server (NotFound): stages.kwok.x-k8s.io "node-initialize" not found' >&2
        exit 1
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
want_out=false
url=""
for a in "$@"; do
    if [ "${want_out}" = true ]; then
        out="${a}"
        want_out=false
        continue
    fi
    case "${a}" in
        # -o, and the bundled short flags the subject actually uses, such as
        # -fsSLo: the output path is the argument after any flag ending in o.
        -*o) want_out=true ;;
        -*) ;;
        *) url="${a}" ;;
    esac
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

# objects_per_call prints the number of Node objects in each apply call, in
# call order, space separated.
objects_per_call() {
    local n=1 f out=""
    while :; do
        f="${MOKKA_STATE_DIR}/applied-${n}.yaml"
        [ -f "${f}" ] || break
        out="${out}$(grep -cF 'kind: Node' "${f}" | tr -d ' ') "
        n=$((n + 1))
    done
    printf '%s' "${out% }"
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
    unset MOKKA_CONTEXT FAKE_SERVER FAKE_POD_CIDR FAKE_NO_CIDR_NODES FAKE_CIDR_PER_POLL
    unset FAKE_CIDR_FAIL_FROM FAKE_CIDR_FAIL_UNTIL FAKE_CIDR_LINES FAKE_READY_AFTER
    unset FAKE_EXISTING_COUNT FAKE_KWOK_STAGES
    unset KWOK_GPU_PRODUCT KWOK_GPU_FAMILY KWOK_GPU_MEMORY KWOK_DRIVER_VERSION
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

reset_fixtures
export FAKE_KWOK_INSTALLED=0
export FAKE_CORRUPT_MANIFEST="kwok.yaml"
run_subject 5
assert_rc 6 "a manifest that does not match its pinned sha256"
assert_says "kwok.yaml does not match its pinned sha256" "the refusal names the manifest"
assert_not_logged "apply -f" "a bad checksum applies nothing"

if [ "${have_manifests}" = true ]; then
    # Only reachable with the real kwok.yaml in the cache: the first manifest
    # has to pass its pin before the second one is ever looked at.
    reset_fixtures
    export FAKE_KWOK_INSTALLED=0
    export FAKE_CORRUPT_MANIFEST="stage-fast.yaml"
    run_subject 5
    assert_rc 6 "the second manifest is checked too"
    assert_says "stage-fast.yaml does not match its pinned sha256" "the second refusal names the manifest"
    assert_not_logged "apply -f" "a bad checksum in the second manifest applies neither"

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
assert_applied_count 6 "nvidia.com/gpu: \"8\"" "capacity and allocatable on each of the three nodes carry the GPU resource"
assert_applied "${OWNER_LABEL}: \"true\"" "every node is labelled as this script's"

# The values GFD writes on the t4 lane's own workers, read off
# kubectl-nv-scale-worker on 2026-10-11. A fake node that claimed a product or
# a driver the real workers do not have would be a difference every reader of
# a 2,000 node list has to chase before deciding it means nothing.
assert_applied "nvidia.com/gpu.product: NVIDIA-T4" "the product label is the lane's own"
assert_applied "nvidia.com/gpu.machine: NVIDIA-T4" "the machine label"
assert_applied "nvidia.com/gpu.family: turing" "the family label"
assert_applied "nvidia.com/gpu.memory: \"16384\"" "the memory label"
assert_applied "nvidia.com/gpu.compute.major: \"7\"" "the compute capability major"
assert_applied "nvidia.com/gpu.compute.minor: \"5\"" "the compute capability minor"
assert_applied "nvidia.com/gpu.mode: compute" "the gpu.mode label"
assert_applied "nvidia.com/mig.capable: \"true\"" "the mig.capable label"

# Both naming families: the inventory builder reads the current
# cuda.driver-version.* names and the older cuda.driver.* ones, and a lane
# that only ever carried one of them cannot tell a builder that dropped the
# other apart from one that did not.
assert_applied "nvidia.com/cuda.driver-version.full: \"550.163.01\"" "the current driver version label"
assert_applied "nvidia.com/cuda.driver-version.major: \"550\"" "the current driver major"
assert_applied "nvidia.com/cuda.driver.major: \"550\"" "the older driver major"
assert_applied "nvidia.com/cuda.driver.rev: \"01\"" "the older driver revision"
assert_applied "nvidia.com/cuda.runtime-version.full: \"12.4\"" "the current CUDA runtime label"
assert_applied "nvidia.com/cuda.runtime.major: \"12\"" "the older CUDA runtime major"

# GFD does not write mig.strategy on this lane, so neither does a fake node.
assert_applied_count 0 "nvidia.com/mig.strategy" "no label the real workers do not carry"

# The GPU Operator's state labeller writes nvidia.com/gpu.deploy.* itself, on
# any node carrying the PCI label, within seconds of the node appearing. The
# script must not write them: two writers for one label is a fight nobody wins.
assert_applied_count 0 "nvidia.com/gpu.deploy." "the operator's own labels are left to the operator"

# A fake node that advertised a different GPU count from its gpu.count label
# would make the health rules fire for a reason this lane did not intend.
reset_fixtures
export FAKE_READY_COUNT=1
export FAKE_NODE_LINE="4 True"
KWOK_GPU_COUNT=4 run_subject 1
assert_rc 0 "an overridden GPU count"
assert_applied "nvidia.com/gpu.count: \"4\"" "the overridden count reaches the label"
assert_applied_count 2 "nvidia.com/gpu: \"4\"" "the overridden count reaches capacity and allocatable"

# The driver version is one string in the label GFD writes and three numbers
# in the older labels. Overriding it has to move all four together, or a lane
# ends up advertising two different drivers for one node.
reset_fixtures
export FAKE_READY_COUNT=1
KWOK_DRIVER_VERSION=570.86.15 run_subject 1
assert_rc 0 "an overridden driver version"
assert_applied "nvidia.com/cuda.driver-version.full: \"570.86.15\"" "the override reaches the full driver label"
assert_applied "nvidia.com/cuda.driver-version.major: \"570\"" "the override reaches the driver major"
assert_applied "nvidia.com/cuda.driver.minor: \"86\"" "the override reaches the older driver minor"
assert_applied "nvidia.com/cuda.driver.rev: \"15\"" "the override reaches the older driver revision"
assert_applied_count 0 "550" "no part of the default driver version survives an override"

reset_fixtures
export FAKE_READY_COUNT=1
KWOK_DRIVER_VERSION=570 run_subject 1
assert_rc 2 "a driver version that is not three dotted parts"

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
# The call count alone cannot tell 10/10/5 from 9/9/7: both are three calls
# and 25 nodes. The shape of the batches is where an off-by-one lives.
if [ "$(objects_per_call)" = "10 10 5" ]; then
    ok "the batches are 10, 10 and 5 objects"
else
    nope "the batches are $(objects_per_call) objects, want 10 10 5"
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
if [ "$(objects_per_call)" = "10 10" ]; then
    ok "an exact multiple makes two full batches and no empty third"
else
    nope "the batches are $(objects_per_call) objects, want 10 10"
fi

# One node is the boundary at the other end: one call holding one object.
reset_fixtures
export FAKE_READY_COUNT=1
KWOK_BATCH_SIZE=10 run_subject 1
if [ "$(objects_per_call)" = "1" ]; then
    ok "a single node is one call of one object"
else
    nope "a single node made batches of $(objects_per_call), want 1"
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
# Pod CIDRs
# ---------------------------------------------------------------------------
#
# The node IPAM controller gives every node a /24 out of the cluster pod
# subnet, a KWOK node included. kind's default subnet is a /16, so it runs out
# somewhere past 253 nodes and the ones after that come up Ready with no
# spec.podCIDR at all. A lane that is quietly missing its last 1,700 CIDRs is
# not a lane anybody should be timing against.
#
# The allocator is also not instant: kube-controller-manager writes one
# podCIDR per node at its client rate limit, so seconds after 2000 nodes turn
# Ready a third of them can still be waiting for theirs. The check waits for
# the allocator and fails only on CIDRs that never arrive.

reset_fixtures
export FAKE_READY_COUNT=5
export FAKE_NO_CIDR_NODES=2
run_subject 5
assert_rc 5 "nodes that came up Ready with no pod CIDR"
assert_says "2 of 5" "the failure says how many nodes have no pod CIDR"
assert_says "after 2s" "the failure says how long it waited for the allocator"
assert_says "podSubnet" "the failure points at the pod subnet"

reset_fixtures
export FAKE_READY_COUNT=5
export FAKE_NO_CIDR_NODES=2
export FAKE_CIDR_PER_POLL=1
export MOKKA_READY_TIMEOUT=10
run_subject 5
assert_rc 0 "pod CIDRs the allocator writes after the first look are waited for"
cidr_queries="$(grep -c 'podCIDR' "${MOKKA_CALL_LOG}" | tr -d ' ')"
if [ "${cidr_queries}" -eq 3 ]; then
    ok "the pod CIDRs were read until the last node had one (3 queries)"
else
    nope "the pod CIDRs were read ${cidr_queries} times, want 3" "calls:" "$(cat "${MOKKA_CALL_LOG}")"
fi

# The check counts the nodes that have a CIDR rather than looking for ones that
# do not, so a read that failed or came back short is "not yet", never a pass.
# kubectl exits 1 with nothing on stdout on a timeout or a refused connection,
# and a lane with 2,000 nodes is read about eighty times in this wait.

reset_fixtures
export FAKE_READY_COUNT=5
export FAKE_NO_CIDR_NODES=5
export FAKE_CIDR_FAIL_FROM=3
run_subject 5
assert_rc 5 "nodes still without a pod CIDR are not passed by a read that failed"
assert_says "5 of 5" "the failure counts the nodes the last good read found without a CIDR"
assert_says "the last read of the nodes failed" "the failure says the last read failed"

reset_fixtures
export FAKE_READY_COUNT=5
export FAKE_CIDR_FAIL_FROM=1
export FAKE_CIDR_FAIL_UNTIL=1
export MOKKA_READY_TIMEOUT=10
run_subject 5
assert_rc 0 "a read that fails once is read again"
cidr_queries="$(grep -c 'podCIDR' "${MOKKA_CALL_LOG}" | tr -d ' ')"
if [ "${cidr_queries}" -eq 2 ]; then
    ok "the pod CIDRs were read again after the failed read (2 queries)"
else
    nope "the pod CIDRs were read ${cidr_queries} times, want 2" "calls:" "$(cat "${MOKKA_CALL_LOG}")"
fi

reset_fixtures
export FAKE_READY_COUNT=5
export FAKE_CIDR_LINES=3
run_subject 5
assert_rc 5 "a read that lists fewer nodes than were created is not a pass"
assert_says "2 of 5" "the nodes a short read left out count as without a CIDR"

# Both waits share one MOKKA_READY_TIMEOUT, which is what the workflow's time
# budget is written against. Ready arrives on the fourth poll here, about three
# seconds into a four second budget, and the CIDRs never do: the run has to end
# when the shared budget does, not a full budget after Ready.

reset_fixtures
export FAKE_READY_COUNT=5
export FAKE_READY_AFTER=4
export FAKE_NO_CIDR_NODES=2
export MOKKA_READY_TIMEOUT=4
started="$(date +%s)"
run_subject 5
elapsed=$(( $(date +%s) - started ))
assert_rc 5 "pod CIDRs that never arrive after a slow Ready still fail"
if [ "${elapsed}" -le 5 ]; then
    ok "the Ready and pod CIDR waits share one budget (${elapsed}s for a 4s budget)"
else
    nope "the Ready and pod CIDR waits took ${elapsed}s for a 4s budget, want at most 5s"
fi

reset_fixtures
export FAKE_READY_COUNT=5
run_subject 5
assert_rc 0 "every node has a pod CIDR"

# ---------------------------------------------------------------------------
# The API server behind the context
# ---------------------------------------------------------------------------
#
# A context is a name somebody chose, and a name can be given to any cluster.
# A kind lane always answers on the loopback address, so that is checked too:
# somebody who names a production context kind-kubectl-nv-scale still cannot
# get this script to create two thousand nodes in it.

reset_fixtures
export FAKE_SERVER="https://prod.example.com:6443"
run_subject 5
assert_rc 7 "a lane context pointed at a server that is not loopback"
assert_says "https://prod.example.com:6443" "the refusal names the server"
assert_not_logged "apply -f" "a refused server applies nothing"

reset_fixtures
export FAKE_SERVER="https://10.0.0.5:6443"
run_subject 5
assert_rc 7 "a lane context pointed at a private address that is not loopback"

reset_fixtures
export FAKE_SERVER="http://127.0.0.1:6443"
run_subject 5
assert_rc 7 "a loopback server reached over plain http"

reset_fixtures
export FAKE_READY_COUNT=5
export FAKE_SERVER="https://127.0.0.1:51234"
run_subject 5
assert_rc 0 "a loopback server on the port kind happened to pick"

# ---------------------------------------------------------------------------
# A rerun over a lane that already has nodes
# ---------------------------------------------------------------------------

reset_fixtures
export FAKE_READY_COUNT=5
export FAKE_EXISTING_COUNT=5
run_subject 5
assert_rc 0 "a rerun at the same count is allowed"

# More nodes than were asked for means an earlier run at a larger count. The
# extras are invisible to the readiness check, which only counts up to the
# number wanted, and the suite would then measure a lane of a size nobody
# chose.
reset_fixtures
export FAKE_READY_COUNT=5
export FAKE_EXISTING_COUNT=9
run_subject 5
assert_rc 2 "a rerun over more nodes than were asked for"
assert_says "9 fake node" "the refusal says how many are already there"
assert_says "--delete" "the refusal says how to clear them"
assert_not_logged "apply -f -" "a refused rerun creates nothing"

# ---------------------------------------------------------------------------
# Label values
# ---------------------------------------------------------------------------
#
# These go into a label, and a label value the API server rejects fails the
# apply of a whole batch with a message about the 100th node. Checking them
# before the first call costs nothing and names the variable.

reset_fixtures
export FAKE_READY_COUNT=1
KWOK_GPU_PRODUCT="NVIDIA T4" run_subject 1
assert_rc 2 "a product label with a space in it"
assert_says "KWOK_GPU_PRODUCT" "the bad product value names its variable"

reset_fixtures
KWOK_GPU_MEMORY="lots" run_subject 1
assert_rc 2 "a memory label that is not a number"

reset_fixtures
KWOK_GPU_FAMILY="Turing!" run_subject 1
assert_rc 2 "a family label with a character a label cannot hold"

reset_fixtures
export FAKE_READY_COUNT=1
KWOK_GPU_PRODUCT="NVIDIA-H100-80GB-HBM3" KWOK_GPU_FAMILY=hopper KWOK_GPU_MEMORY=81559 run_subject 1
assert_rc 0 "a different but legitimate product, family and memory"
assert_applied "nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3" "the overridden product reaches the label"

# ---------------------------------------------------------------------------
# KWOK is only installed once, and only when it is really there
# ---------------------------------------------------------------------------
#
# The controller Deployment existing is not the same as KWOK working: the
# Stage objects are a separate manifest and a run that applied the first and
# died before the second leaves a lane whose nodes never go Ready.

reset_fixtures
export FAKE_READY_COUNT=5
export FAKE_KWOK_INSTALLED=1
export FAKE_KWOK_STAGES=0
run_subject 5
assert_logged "apply --server-side" "a controller without its Stages is reinstalled"

reset_fixtures
export FAKE_READY_COUNT=5
export FAKE_KWOK_INSTALLED=1
export FAKE_KWOK_STAGES=1
run_subject 5
assert_not_logged "apply --server-side" "a complete install is left alone"

# ---------------------------------------------------------------------------
# --delete
# ---------------------------------------------------------------------------

reset_fixtures
run_subject --delete
assert_rc 0 "--delete against an installed KWOK"
assert_logged "delete nodes --selector ${OWNER_LABEL}=true" "--delete selects exactly the nodes this script labelled"
assert_not_logged "delete nodes --all" "--delete never asks for every node"
assert_not_logged "apply -f -" "--delete creates nothing"
# --wait=false means the API server has accepted the deletions, not that the
# objects are gone. Saying "are gone" invites a rerun that then trips the
# leftover-node check for no reason anybody can see.
assert_says "deletion requested" "--delete says what --wait=false actually promises"
assert_applied_count 0 "are gone" "--delete does not claim the nodes are already gone"

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
