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
# Tests for the hack/mokka/faults scripts and the library behind them.
#
# No cluster is touched. kubectl is replaced by a PATH shim that records its
# argv into a log and answers from fixture variables, so what is tested here is
# what the scripts ASK the cluster to do, which is the part a live run cannot
# check without breaking a lane to find out.
#
# Three statements are what this file exists for, and each one is a failure
# that has shipped in a lane script before:
#
#   - The context guard holds for every script in the directory, not only for
#     the one whose author remembered it.
#   - recover writes back the value inject SAVED, not the value its author
#     believed was healthy.
#   - A second recover changes nothing.
#
# The subject is resolved relative to THIS file, never through $HOME: a test
# that green-lights a deployed copy says nothing about the tree it ships from.
#
# Usage: hack/mokka/faults/faults_test.sh
# Exit: 0 all tests pass, 1 at least one failed.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

PASS=0
FAIL=0

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mokka-faults-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

# Every script in the directory, so that a new one added without a guard fails
# the table below rather than being quietly exempt from it.
SCRIPTS=(
    nv-dcgm-001.sh
    nv-dp-001.sh
    nv-node-001.sh
    nv-node-002.sh
    nv-node-003.sh
    nv-node-004.sh
    nv-op-001.sh
    nv-op-002.sh
)

# ---------------------------------------------------------------------------
# The kubectl shim
# ---------------------------------------------------------------------------
#
# It answers four questions and records everything. A question it does not
# know exits 0 with empty output, which is what an absent field looks like to
# the scripts; a shim that failed on an unknown call would make these tests
# about the shim's coverage rather than about the scripts.

SHIM_DIR="${WORK}/bin"
mkdir -p "${SHIM_DIR}"
cat > "${SHIM_DIR}/kubectl" <<'SHIM'
#!/bin/bash
printf 'kubectl %s\n' "$*" >> "${FAKE_CALL_LOG}"
# A second log that no case truncates, so that "nothing ever reached the
# caller's own kubeconfig" is a statement about the whole run.
printf 'kubectl %s\n' "$*" >> "${FAKE_ALL_CALLS}"

# The scripts always pass --kubeconfig and --context before the verb, so the
# verb is found by skipping those two flags and their values.
args=()
skip=false
for a in "$@"; do
    if [ "${skip}" = true ]; then
        skip=false
        continue
    fi
    case "${a}" in
        --kubeconfig | --context | --namespace) skip=true ;;
        -*) args+=("${a}") ;;
        *) args+=("${a}") ;;
    esac
done

case "${args[0]:-} ${args[1]:-}" in
    "config current-context")
        printf '%s' "${FAKE_CONTEXT:-}"
        exit 0
        ;;
esac

# A jsonpath that escapes the slash of a label key matches nothing in real
# kubectl: a dot separates path components and a slash does not, so \/ is a
# literal backslash followed by a slash. Answering such a read with the fixture
# would let a script that escaped it look as though it had read the label.
case "$*" in
    *'\/'*)
        echo "kubectl: no label key contains a backslash: $*" >&2
        exit 1
        ;;
esac

if [ "${args[0]:-}" = "get" ] && [ "${args[1]:-}" = "node" ]; then
    # `get node <name> -o name` is the existence probe.
    for a in "$@"; do
        if [ "${a}" = "name" ]; then
            [ "${FAKE_NODE_EXISTS:-1}" = "1" ] || exit 1
            printf 'node/%s\n' "${args[2]:-}"
            exit 0
        fi
    done
    # Anything else about a node is a jsonpath read, answered from the fixture.
    printf '%s' "${FAKE_LABEL_VALUE:-}"
    exit 0
fi

if [ "${args[0]:-}" = "get" ] && [ "${args[1]:-}" = "deploy" ]; then
    # A Deployment readiness read is answered separately from the spec reads,
    # so that a recover waiting for the operator to come back can be driven
    # into both outcomes.
    case "$*" in
        *readyReplicas*)
            printf '%s' "${FAKE_READY_REPLICAS:-1}"
            exit 0
            ;;
    esac
    printf '%s' "${FAKE_DEPLOY_FIELD:-}"
    exit 0
fi

if [ "${args[0]:-}" = "get" ] && [ "${args[1]:-}" = "pod" ]; then
    # Likewise for a pod's Ready condition.
    case "$*" in
        *'type=="Ready"'*)
            printf '%s' "${FAKE_POD_READY:-True}"
            exit 0
            ;;
    esac
    printf '%s' "${FAKE_POD:-}"
    exit 0
fi

if [ "${args[0]:-}" = "get" ]; then
    printf '%s' "${FAKE_GET:-}"
    exit 0
fi

# apply reads a manifest on stdin; draining it keeps the writer from a SIGPIPE.
if [ "${args[0]:-}" = "apply" ]; then
    cat > "${FAKE_APPLIED:-/dev/null}"
    exit 0
fi

exit 0
SHIM
chmod 755 "${SHIM_DIR}/kubectl"

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
    if grep -qF -- "${want}" "${FAKE_CALL_LOG}"; then
        ok "${name}"
    else
        nope "${name}: no recorded call contains: ${want}" "calls:" "$(cat "${FAKE_CALL_LOG}")"
    fi
}

assert_not_logged() {
    local want="$1" name="$2"
    if grep -qF -- "${want}" "${FAKE_CALL_LOG}"; then
        nope "${name}: a recorded call contains: ${want}" "calls:" "$(cat "${FAKE_CALL_LOG}")"
    else
        ok "${name}"
    fi
}

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

export FAKE_CALL_LOG="${WORK}/calls.log"
export FAKE_ALL_CALLS="${WORK}/all-calls.log"
: > "${FAKE_ALL_CALLS}"
export MOKKA_STATE_DIR="${WORK}/state"

LANE_KUBECONFIG="${WORK}/kubeconfig-faults"
printf 'apiVersion: v1\ncurrent-context: kind-kubectl-nv-faults\n' > "${LANE_KUBECONFIG}"

NODE="kubectl-nv-faults-worker"

# reset
# Empties the call log and the saved state between cases, so that a case
# asserting "no label call was made" cannot pass on a log another case wrote.
reset() {
    : > "${FAKE_CALL_LOG}"
    rm -rf "${MOKKA_STATE_DIR}"
    unset FAKE_LABEL_VALUE FAKE_DEPLOY_FIELD FAKE_GET FAKE_NODE_EXISTS
    unset FAKE_POD_READY FAKE_READY_REPLICAS FAULTS_READY_TIMEOUT FAULTS_READY_POLL
    # A recover waits for the operand it disturbed, so the default lane the
    # cases run against has one, and it is Ready.
    export FAKE_POD="nvidia-dcgm-exporter-fixture"
    export FAKE_CONTEXT="kind-kubectl-nv-faults"
    export MOKKA_KUBECONFIG="${LANE_KUBECONFIG}"
}

# invoke <script> <arg>...
# Runs one fault script with the shim in front of PATH and captures its three
# outputs. HOME is redirected at a directory that holds no kubeconfig, so a
# script that fell back to the caller's default one has nothing to fall back to.
invoke() {
    local script="$1"
    shift
    local out_file="${WORK}/out" err_file="${WORK}/err"
    HOME="${WORK}/home" PATH="${SHIM_DIR}:${PATH}" \
        "${SCRIPT_DIR}/${script}" "$@" > "${out_file}" 2> "${err_file}"
    RC=$?
    OUT="$(cat "${out_file}")"
    ERR="$(cat "${err_file}")"
}

# ---------------------------------------------------------------------------
# Usage, applied to every script in the directory
# ---------------------------------------------------------------------------

for script in "${SCRIPTS[@]}"; do
    reset
    invoke "${script}"
    assert_rc 2 "${script} with no arguments"
    assert_says "usage: ${script} inject|recover <node>" "${script} prints usage"

    reset
    invoke "${script}" "inject"
    assert_rc 2 "${script} with one argument"

    reset
    invoke "${script}" "demolish" "${NODE}"
    assert_rc 2 "${script} rejects an unknown action"
    assert_not_logged "label node" "${script} makes no change for an unknown action"

    reset
    invoke "${script}" "inject" "Worker-1"
    assert_rc 2 "${script} rejects a node name that is not a DNS name"

    reset
    invoke "${script}" "inject" "$(printf 'worker\nkube-system')"
    assert_rc 2 "${script} rejects a node name with an embedded newline"
done

# ---------------------------------------------------------------------------
# The kubeconfig guard
# ---------------------------------------------------------------------------

for script in "${SCRIPTS[@]}"; do
    reset
    unset MOKKA_KUBECONFIG
    invoke "${script}" "inject" "${NODE}"
    assert_rc 2 "${script} refuses to run with no MOKKA_KUBECONFIG"
    assert_says "MOKKA_KUBECONFIG is not set" "${script} says which variable is missing"
    assert_not_logged "kubectl" "${script} runs no kubectl without a kubeconfig"

    reset
    export MOKKA_KUBECONFIG="${WORK}/not-a-file"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 2 "${script} refuses a MOKKA_KUBECONFIG that is not a file"
done

# ---------------------------------------------------------------------------
# The context guard
# ---------------------------------------------------------------------------

for script in "${SCRIPTS[@]}"; do
    reset
    export FAKE_CONTEXT="docker-desktop"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 2 "${script} refuses a context that is not a kubectl-nv lane"
    assert_says "docker-desktop" "${script} names the context it refused"
    assert_not_logged "label node" "${script} writes no label to a refused context"
    assert_not_logged "patch " "${script} patches nothing in a refused context"
    assert_not_logged "scale " "${script} scales nothing in a refused context"
    assert_not_logged " exec " "${script} execs nothing in a refused context"

    # A context that merely starts with the same letters is not a lane of
    # ours: kind-kubectl-nvidia-prod shares fifteen characters with the
    # prefix and the sixteenth is what separates a lane from somebody's
    # cluster.
    reset
    export FAKE_CONTEXT="kind-kubectl-nvidia-prod"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 2 "${script} refuses kind-kubectl-nvidia-prod"
    assert_not_logged "label node" "${script} writes no label to kind-kubectl-nvidia-prod"
done

reset
export FAKE_CONTEXT=""
invoke "nv-op-002.sh" "inject" "${NODE}"
assert_rc 2 "an empty current context is refused"
assert_says "<none>" "an empty current context is named as none"

# ---------------------------------------------------------------------------
# An unknown node
# ---------------------------------------------------------------------------

reset
export FAKE_NODE_EXISTS=0
invoke "nv-op-002.sh" "inject" "${NODE}"
assert_rc 2 "a node the lane does not have is refused"
assert_not_logged "label node" "no label is written for a node that is not there"

# ---------------------------------------------------------------------------
# recover restores the value inject saved
# ---------------------------------------------------------------------------
#
# The saved value is deliberately NOT "true". A script that wrote back what its
# author believed the healthy value was would pass against a lane whose label
# happened to hold it, and would silently rewrite a paused operand here.

reset
export FAKE_LABEL_VALUE="paused-for-driver-upgrade"
invoke "nv-op-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-op-002.sh injects"
assert_logged "label node ${NODE} nvidia.com/gpu.deploy.dcgm-exporter=false" \
    "nv-op-002.sh switches the exporter off"

: > "${FAKE_CALL_LOG}"
export FAKE_LABEL_VALUE="false"
invoke "nv-op-002.sh" "recover" "${NODE}"
assert_rc 0 "nv-op-002.sh recovers"
assert_logged "label node ${NODE} nvidia.com/gpu.deploy.dcgm-exporter=paused-for-driver-upgrade" \
    "nv-op-002.sh writes back the value it saved, not a hard-coded true"
assert_not_logged "=true" "nv-op-002.sh does not invent a healthy value"

# ---------------------------------------------------------------------------
# recover is idempotent
# ---------------------------------------------------------------------------

: > "${FAKE_CALL_LOG}"
invoke "nv-op-002.sh" "recover" "${NODE}"
assert_rc 0 "a second recover succeeds"
assert_says "nothing to recover" "a second recover says there was nothing to do"
assert_not_logged "label node" "a second recover writes no label"

reset
invoke "nv-node-002.sh" "recover" "${NODE}"
assert_rc 0 "recover without a prior inject succeeds"
assert_says "nothing to recover" "recover without a prior inject says so"
assert_not_logged "label node" "recover without a prior inject writes no label"

# ---------------------------------------------------------------------------
# A label that was absent is put back absent
# ---------------------------------------------------------------------------

reset
export FAKE_LABEL_VALUE=""
invoke "nv-op-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-op-002.sh injects over an absent label"

: > "${FAKE_CALL_LOG}"
invoke "nv-op-002.sh" "recover" "${NODE}"
assert_rc 0 "nv-op-002.sh recovers an absent label"
assert_logged "label node ${NODE} nvidia.com/gpu.deploy.dcgm-exporter-" \
    "an absent label is removed again rather than written as an empty string"

# ---------------------------------------------------------------------------
# inject refuses to run twice
# ---------------------------------------------------------------------------
#
# The second inject would save the broken value as the one to restore, and
# recover would then leave the lane broken for good.

reset
export FAKE_LABEL_VALUE="true"
invoke "nv-node-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-node-002.sh injects"

: > "${FAKE_CALL_LOG}"
invoke "nv-node-002.sh" "inject" "${NODE}"
assert_rc 5 "a second inject is refused"
assert_says "already injected" "a second inject says why"
assert_not_logged "label node" "a second inject writes no label"

# ---------------------------------------------------------------------------
# The saved state is per node
# ---------------------------------------------------------------------------

reset
export FAKE_LABEL_VALUE="true"
invoke "nv-op-002.sh" "inject" "${NODE}-a"
assert_rc 0 "nv-op-002.sh injects on one node"

: > "${FAKE_CALL_LOG}"
invoke "nv-op-002.sh" "recover" "${NODE}-b"
assert_says "nothing to recover" "recovering another node finds no state of its own"
assert_not_logged "label node" "recovering another node writes nothing"

# ---------------------------------------------------------------------------
# NV-NODE-004 needs a number to disagree with
# ---------------------------------------------------------------------------

reset
export FAKE_LABEL_VALUE="4"
invoke "nv-node-004.sh" "inject" "${NODE}"
assert_rc 0 "nv-node-004.sh injects"
assert_logged "label node ${NODE} nvidia.com/gpu.count=5" \
    "nv-node-004.sh writes one more than the count it read"

reset
export FAKE_LABEL_VALUE=""
invoke "nv-node-004.sh" "inject" "${NODE}"
assert_rc 2 "nv-node-004.sh refuses a node with no count label"
assert_not_logged "label node" "nv-node-004.sh writes nothing when there is no count"

# ---------------------------------------------------------------------------
# NV-NODE-001 does not clear the label while the operator is still running
# ---------------------------------------------------------------------------
#
# The order matters: with an operator pod still up, the label it writes comes
# straight back and the injection does nothing. The drain is given no time at
# all here, so a script that cleared the label first would reach the label call
# before failing.

reset
export FAKE_DEPLOY_FIELD="1"
export FAKE_POD="gpu-operator-abcde"
export FAULTS_DRAIN_TIMEOUT=0
export FAULTS_DRAIN_POLL=0
invoke "nv-node-001.sh" "inject" "${NODE}"
assert_rc 1 "nv-node-001.sh fails when the operator will not drain"
assert_logged "scale deploy/gpu-operator --replicas=0" "nv-node-001.sh scales the operator down"
assert_not_logged "label node" "nv-node-001.sh clears no label while an operator pod is up"
unset FAULTS_DRAIN_TIMEOUT FAULTS_DRAIN_POLL

# ---------------------------------------------------------------------------
# recover waits for the operand it disturbed
# ---------------------------------------------------------------------------
#
# Putting a label back is not the same thing as the lane being well again. The
# final live run caught exactly that: nv-op-002.sh restored the deploy label and
# returned while the operator was still recreating the exporter pod, and the
# healthy gate that runs after the fault suite then reported NV-DCGM-001 on a
# lane whose faults had all been "recovered".

reset
export FAKE_LABEL_VALUE="true"
invoke "nv-op-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-op-002.sh injects before the readiness case"

: > "${FAKE_CALL_LOG}"
export FAKE_POD_READY="False"
export FAULTS_READY_TIMEOUT=0
export FAULTS_READY_POLL=0
invoke "nv-op-002.sh" "recover" "${NODE}"
assert_rc 1 "recover fails when the operand never becomes Ready"
assert_says "no Ready nvidia-dcgm-exporter pod" "recover says which operand it was waiting for"
assert_logged "label node ${NODE} nvidia.com/gpu.deploy.dcgm-exporter=true" \
    "recover restores the label before it waits"

# The saved state survives a recover that could not finish, so the lane can be
# recovered again once the operand comes back.
: > "${FAKE_CALL_LOG}"
unset FAKE_POD_READY FAULTS_READY_TIMEOUT FAULTS_READY_POLL
invoke "nv-op-002.sh" "recover" "${NODE}"
assert_rc 0 "recover succeeds once the operand is Ready"
assert_says "is Ready again" "recover says the operand is back"

# ---------------------------------------------------------------------------
# Nothing ever reaches the caller's own kubeconfig
# ---------------------------------------------------------------------------

if grep -qF -- "/.kube/config" "${FAKE_ALL_CALLS}"; then
    nope "a call made during this run names a .kube/config path" \
        "calls:" "$(grep -F -- '/.kube/config' "${FAKE_ALL_CALLS}")"
else
    ok "no call made during this run names a .kube/config path"
fi

# ---------------------------------------------------------------------------
# Linting the scripts themselves
# ---------------------------------------------------------------------------
#
# From two working directories, because a source path that resolves from one
# and not the other is a difference only this catches.

if command -v shellcheck > /dev/null 2>&1; then
    for dir in / "${SCRIPT_DIR}"; do
        subjects=()
        for script in "${SCRIPTS[@]}"; do
            subjects+=("${SCRIPT_DIR}/${script}")
        done
        subjects+=("${SCRIPT_DIR}/faults_test.sh")
        if (cd "${dir}" && shellcheck -x "${subjects[@]}"); then
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
