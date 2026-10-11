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
# argv and answers from fixture variables, so what is tested here is what the
# scripts ASK the cluster to do. That is the part a live run cannot check
# without breaking a lane to find out, and these scripts write to live
# clusters, so it is the part that has to be nailed down hardest.
#
# The shim does two jobs. It answers, and it REFUSES: a call whose argv does
# not begin with the lane's own --kubeconfig and --context is logged as
# UNSCOPED and fails, and a final assertion requires that no call in the whole
# run was. A script that reached for the ambient kubeconfig would otherwise
# look identical to one that did not, because a shim that answers everything
# answers an unscoped call too.
#
# Every recover assertion names the EXACT call, against a saved value that no
# author would have guessed: the operand version is v9.9.9-saved, the operator
# runs 3 replicas, and the deploy labels hold paused-for-driver-upgrade. A
# recover that wrote back what its author believed was healthy passes against a
# fixture holding the healthy value and fails against these.
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
# the tables below rather than being quietly exempt from them.
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

# The distinctive fixture values. None of them is a value a healthy lane would
# hold, which is the whole point: a recover that invents the healthy value
# instead of reading the saved one fails every assertion below.
SAVED_VERSION="v9.9.9-saved"
SAVED_REPLICAS="3"
# A deploy label's realistic values are "true", "false" and the "paused-for-"
# family, and the last two are states inject refuses to write over. So the
# deploy labels cannot carry a value no author would guess, and the
# discriminating case for them is the ABSENT one instead: a label that was not
# there before inject has to be REMOVED again, where a recover that wrote its
# author's idea of healthy writes "true".
SAVED_LABEL="true"
ABSENT_VERSION="v0.0.0-absent"

NODE="kubectl-nv-faults-worker"
OTHER_NODE="kubectl-nv-faults-worker2"

# ---------------------------------------------------------------------------
# The kubectl shim
# ---------------------------------------------------------------------------

SHIM_DIR="${WORK}/bin"
mkdir -p "${SHIM_DIR}"
cat > "${SHIM_DIR}/kubectl" <<'SHIM'
#!/bin/bash
printf 'kubectl %s\n' "$*" >> "${FAKE_CALL_LOG}"
# A second log that no case truncates, so that "nothing was ever unscoped" is a
# statement about the whole run.
printf 'kubectl %s\n' "$*" >> "${FAKE_ALL_CALLS}"

# -----------------------------------------------------------------------
# The scoping guard
# -----------------------------------------------------------------------
#
# Every call has to name the lane's kubeconfig and the lane's context, in that
# order, before anything else. `config current-context` is the one exception:
# it is the call that FINDS the context, so it cannot already carry it.
#
# A call that fails this is recorded as UNSCOPED and fails rather than being
# answered. Answering it would make a script that dropped --context
# indistinguishable from one that kept it, which is how a fault script ends up
# writing to whichever cluster the developer was last looking at.
scoped=false
if [ "${1:-}" = "--kubeconfig" ] && [ "${2:-}" = "${LANE_KUBECONFIG}" ] &&
    [ "${3:-}" = "--context" ] && [ "${4:-}" = "${FAKE_CONTEXT:-}" ]; then
    scoped=true
fi
case "$*" in
    *"config current-context"*) scoped=true ;;
esac
if [ "${scoped}" != true ]; then
    printf 'UNSCOPED %s\n' "$*" >> "${FAKE_ALL_CALLS}"
    printf 'UNSCOPED %s\n' "$*" >> "${FAKE_CALL_LOG}"
    echo "kubectl: refusing an unscoped call: $*" >&2
    exit 1
fi

# The argv with the two scoping flags and any --namespace stripped, logged
# separately so that a test can assert a WHOLE call rather than a substring.
rest=()
skip=0
for a in "$@"; do
    if [ "${skip}" -gt 0 ]; then
        skip=$((skip - 1))
        continue
    fi
    case "${a}" in
        --kubeconfig | --context | --namespace)
            skip=1
            continue
            ;;
    esac
    rest+=("${a}")
done
printf '%s\n' "${rest[*]}" >> "${FAKE_SCOPED_LOG}"

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

# -----------------------------------------------------------------------
# Answers
# -----------------------------------------------------------------------

case "$*" in
    *"config current-context"*)
        printf '%s' "${FAKE_CONTEXT:-}"
        exit 0
        ;;
    *"config view"*)
        printf '%s' "${FAKE_SERVER:-https://127.0.0.1:60123}"
        exit 0
        ;;
    *"get namespace kube-system"*)
        printf '%s' "${FAKE_NS_UID:-uid-lane-a}"
        exit 0
        ;;
esac

verb="${rest[0]:-}"
kind="${rest[1]:-}"

if [ "${verb}" = "get" ] && [ "${kind}" = "node" ]; then
    case "$*" in
        *" -o name"*)
            [ "${FAKE_NODE_EXISTS:-1}" = "1" ] || exit 1
            printf 'node/%s\n' "${rest[2]:-}"
            exit 0
            ;;
        *capacity*)
            printf '%s' "${FAKE_CAPACITY:-4}"
            exit 0
            ;;
        *allocatable*)
            printf '%s' "${FAKE_ALLOCATABLE:-4}"
            exit 0
            ;;
    esac
    printf '%s' "${FAKE_LABEL_VALUE:-}"
    exit 0
fi

if [ "${verb}" = "get" ] && [ "${kind}" = "deploy" ]; then
    case "$*" in
        *readyReplicas*)
            printf '%s' "${FAKE_DEPLOY_READY:-1}"
            exit 0
            ;;
        *containers*)
            printf '%s' "${FAKE_DEPLOY_IMAGE:-nvcr.io/nvidia/gpu-operator:v26.3.3}"
            exit 0
            ;;
    esac
    printf '%s' "${FAKE_DEPLOY_REPLICAS:-3}"
    exit 0
fi

case "${kind}" in
    clusterpolicies.nvidia.com)
        if [ "${verb}" = "get" ]; then
            case "$*" in
                *status.state*)
                    printf '%s' "${FAKE_POLICY_STATE:-ready}"
                    exit 0
                    ;;
            esac
            # The version the last patch set, so inject and recover see a
            # policy that follows what they wrote to it. "removed" is the
            # JSON-patch remove, which reads back as an absent field.
            if [ -s "${FAKE_OPERAND_VERSION_FILE}" ]; then
                v="$(cat "${FAKE_OPERAND_VERSION_FILE}")"
                [ "${v}" = "removed" ] || printf '%s' "${v}"
                exit 0
            fi
            printf '%s' "${FAKE_POLICY_VERSION-v9.9.9-saved}"
            exit 0
        fi
        ;;
esac

if [ "${verb}" = "get" ] && [ "${kind}" = "daemonset" ]; then
    # The operator copies the ClusterPolicy's version into the DaemonSet it
    # owns, so the shim does too: the version of the last patch it saw. A test
    # that wants the template to disagree with the policy, which is what the
    # wait bounds are about, sets FAKE_TEMPLATE_IMAGE and wins over this.
    if [ -n "${FAKE_TEMPLATE_IMAGE:-}" ]; then
        printf '%s' "${FAKE_TEMPLATE_IMAGE}"
        exit 0
    fi
    printf 'registry.example/operand:%s' \
        "$(cat "${FAKE_OPERAND_VERSION_FILE}" 2> /dev/null || echo "v9.9.9-saved")"
    exit 0
fi

if [ "${verb}" = "patch" ]; then
    # Remember what the policy was patched to, so the DaemonSet answer above
    # can follow it.
    case "$*" in
        *'"op":"remove"'*) printf 'removed' > "${FAKE_OPERAND_VERSION_FILE}" ;;
        *'"version":"'*)
            v="$*"
            v="${v##*\"version\":\"}"
            v="${v%%\"*}"
            printf '%s' "${v}" > "${FAKE_OPERAND_VERSION_FILE}"
            ;;
    esac
    exit 0
fi

if [ "${verb}" = "get" ] && [ "${kind}" = "pod" ]; then
    case "$*" in
        *'-o jsonpath={.metadata.uid}'*)
            printf '%s' "${FAKE_POD_UID:-pod-uid-$(cat "${FAKE_POD_GEN_FILE}" 2> /dev/null || echo 1)}"
            exit 0
            ;;
        *'type=="Ready"'*)
            printf '%s' "${FAKE_POD_READY:-True}"
            exit 0
            ;;
    esac
    case "${rest[2]:-}" in
        nv-faults-operator-standin)
            printf '%s' "${FAKE_STANDIN:-}"
            exit 0
            ;;
    esac
    # The operator drain reads every pod of the selector with no field
    # selector, which is a different question from "the pod on this node".
    case "$*" in
        *'items[*].metadata.name'*)
            printf '%s' "${FAKE_OPERATOR_PODS:-}"
            exit 0
            ;;
    esac
    # Everything else is faults::pod_on_node, which MUST carry the node. A
    # lookup without it, or with the wrong node, finds nothing: that is what
    # makes a script that dropped the field selector fail here rather than
    # quietly act on the other worker's pod.
    case "$*" in
        *"--field-selector spec.nodeName=${FAKE_POD_NODE:-kubectl-nv-faults-worker}"*)
            printf '%s\n' "${FAKE_PODS:-pod-on-node pod-uid-$(cat "${FAKE_POD_GEN_FILE}" 2> /dev/null || echo 1) }"
            exit 0
            ;;
    esac
    exit 0
fi

if [ "${verb}" = "delete" ] && [ "${kind}" = "pod" ]; then
    # The DaemonSet controller replaces a deleted pod, and the replacement is a
    # different object. Counting the deletions is how the fixture gives it a
    # different UID.
    printf '%s' "$(( $(cat "${FAKE_POD_GEN_FILE}" 2> /dev/null || echo 1) + 1 ))" \
        > "${FAKE_POD_GEN_FILE}"
    exit 0
fi

# apply reads a manifest on stdin; draining it keeps the writer from a SIGPIPE.
if [ "${verb}" = "apply" ]; then
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

# assert_call <whole-call> <name>
# The call, in full, with the scoping flags and the namespace stripped. A whole
# line and not a substring: a patch has to be the call that was made rather
# than a prefix of one.
assert_call() {
    local want="$1" name="$2"
    if grep -qxF -- "${want}" "${FAKE_SCOPED_LOG}"; then
        ok "${name}"
    else
        nope "${name}: no call was exactly: ${want}" "calls:" "$(cat "${FAKE_SCOPED_LOG}")"
    fi
}

assert_no_call() {
    local want="$1" name="$2"
    if grep -qxF -- "${want}" "${FAKE_SCOPED_LOG}"; then
        nope "${name}: a call was exactly: ${want}" "calls:" "$(cat "${FAKE_SCOPED_LOG}")"
    else
        ok "${name}"
    fi
}

# assert_no_writes <name>
# No call that changes anything. The verbs are the ones these scripts use, and
# the list is here rather than inline so that a new write verb is added once.
assert_no_writes() {
    local name="$1" verb
    for verb in "label node" "patch " "scale " "delete " " exec " "apply "; do
        if grep -qF -- "${verb}" "${FAKE_CALL_LOG}"; then
            nope "${name}: a recorded call writes (${verb})" "calls:" "$(cat "${FAKE_CALL_LOG}")"

            return
        fi
    done
    ok "${name}"
}

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

export FAKE_CALL_LOG="${WORK}/calls.log"
export FAKE_SCOPED_LOG="${WORK}/scoped.log"
export FAKE_ALL_CALLS="${WORK}/all-calls.log"
: > "${FAKE_ALL_CALLS}"
export MOKKA_STATE_DIR="${WORK}/state"
export FAKE_OPERAND_VERSION_FILE="${WORK}/operand-version"
export FAKE_POD_GEN_FILE="${WORK}/pod-generation"

export LANE_KUBECONFIG="${WORK}/kubeconfig-faults"
printf 'apiVersion: v1\ncurrent-context: kind-kubectl-nv-faults\n' > "${LANE_KUBECONFIG}"

# reset
# Empties the per-case logs and the saved state, and puts every fixture back to
# a lane that is healthy and holds the distinctive saved values.
reset() {
    : > "${FAKE_CALL_LOG}"
    : > "${FAKE_SCOPED_LOG}"
    rm -rf "${MOKKA_STATE_DIR}"
    rm -f "${FAKE_OPERAND_VERSION_FILE}" "${FAKE_POD_GEN_FILE}"
    unset FAKE_NODE_EXISTS FAKE_CAPACITY FAKE_ALLOCATABLE FAKE_STANDIN \
        FAKE_OPERATOR_PODS FAKE_POD_READY FAKE_DEPLOY_READY FAKE_PODS \
        FAKE_TEMPLATE_IMAGE FAKE_POD_NODE FAKE_POD_UID FAKE_SERVER \
        FAKE_APPLIED FAKE_POLICY_VERSION FAKE_POLICY_STATE
    export FAULTS_READY_TIMEOUT=10
    export FAULTS_READY_POLL=1
    export FAULTS_TEMPLATE_TIMEOUT=10
    export FAULTS_TEMPLATE_POLL=1
    export FAULTS_DRAIN_TIMEOUT=10
    export FAULTS_DRAIN_POLL=1
    export FAKE_CONTEXT="kind-kubectl-nv-faults"
    export FAKE_NS_UID="uid-lane-a"
    export FAKE_LABEL_VALUE="${SAVED_LABEL}"
    export FAKE_DEPLOY_REPLICAS="${SAVED_REPLICAS}"
    export MOKKA_KUBECONFIG="${LANE_KUBECONFIG}"
    unset KUBECONFIG
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

# inject_fixture <script>
# The lane state each script's inject needs to read before it will act.
inject_fixture() {
    case "$1" in
        nv-node-004.sh) export FAKE_LABEL_VALUE="4" ;;
        nv-dp-001.sh | nv-dcgm-001.sh | nv-op-001.sh) export FAKE_POLICY_VERSION="${SAVED_VERSION}" ;;
        nv-node-001.sh) export FAKE_LABEL_VALUE="true" ;;
        *) export FAKE_LABEL_VALUE="${SAVED_LABEL}" ;;
    esac
}

# recover_fixture <script>
# What the lane looks like once the operator has acted on a recover. The shim
# follows the ClusterPolicy patches on its own, so there is nothing left to set
# here; the hook stays so that a script which needs one later has a place.
recover_fixture() {
    :
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
    assert_no_writes "${script} makes no change for an unknown action"

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

    # KUBECONFIG is set and MOKKA_KUBECONFIG is not. There is no fallback: a
    # script that took KUBECONFIG would write to whichever cluster the
    # developer's shell was pointed at, which is exactly the accident the
    # explicit variable exists to make impossible.
    reset
    unset MOKKA_KUBECONFIG
    export KUBECONFIG="${LANE_KUBECONFIG}"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 2 "${script} does not fall back to KUBECONFIG"
    assert_not_logged "kubectl" "${script} runs no kubectl when only KUBECONFIG is set"
    unset KUBECONFIG

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
    assert_no_writes "${script} writes nothing to a refused context"

    # A context that merely starts with the same letters is not a lane of
    # ours: kind-kubectl-nvidia-prod shares fifteen characters with the prefix
    # and the sixteenth is what separates a lane from somebody's cluster.
    reset
    export FAKE_CONTEXT="kind-kubectl-nvidia-prod"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 2 "${script} refuses kind-kubectl-nvidia-prod"
    assert_no_writes "${script} writes nothing to kind-kubectl-nvidia-prod"

    # The lane suffix is a DNS label, the same pattern mokka::valid_lane
    # enforces when the lane is created. A context nobody could have created
    # with up.sh is a context nobody should be injecting into.
    reset
    export FAKE_CONTEXT="kind-kubectl-nv-Prod"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 2 "${script} refuses a lane suffix that is not a DNS label"
    assert_no_writes "${script} writes nothing to a lane with a bad suffix"

    # The cluster has to be on the loopback address. A kind lane always is, and
    # a context named like one that answers somewhere else is either a mistake
    # or someone else's cluster wearing the name.
    reset
    export FAKE_SERVER="https://prod.example.internal:6443"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 2 "${script} refuses a cluster that is not on the loopback address"
    assert_says "prod.example.internal" "${script} names the server it refused"
    assert_no_writes "${script} writes nothing to a remote server"
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
assert_no_writes "nothing is written for a node that is not there"

# ---------------------------------------------------------------------------
# Every mechanism: inject writes the fault, recover writes back what was saved
# ---------------------------------------------------------------------------
#
# One block per script, each asserting the EXACT recover call against the
# distinctive saved value. These are the assertions the whole file is for.

# -- nv-op-002.sh: a deploy label ------------------------------------------
reset
invoke "nv-op-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-op-002.sh injects"
assert_call "label node ${NODE} nvidia.com/gpu.deploy.dcgm-exporter=false --overwrite" \
    "nv-op-002.sh switches the exporter off"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
export FAKE_LABEL_VALUE="false"
invoke "nv-op-002.sh" "recover" "${NODE}"
assert_rc 0 "nv-op-002.sh recovers"
assert_call "label node ${NODE} nvidia.com/gpu.deploy.dcgm-exporter=${SAVED_LABEL} --overwrite" \
    "nv-op-002.sh writes back the value it saved"

# The absent case is the one that tells a saved value from a guessed one.
reset
export FAKE_LABEL_VALUE=""
invoke "nv-op-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-op-002.sh injects over an absent deploy label"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
export FAKE_LABEL_VALUE="false"
invoke "nv-op-002.sh" "recover" "${NODE}"
assert_rc 0 "nv-op-002.sh recovers an absent deploy label"
assert_call "label node ${NODE} nvidia.com/gpu.deploy.dcgm-exporter- --overwrite" \
    "an absent deploy label is removed again rather than written as true"
assert_no_call "label node ${NODE} nvidia.com/gpu.deploy.dcgm-exporter=true --overwrite" \
    "nv-op-002.sh does not invent a healthy value"

# -- nv-node-002.sh: the device plugin deploy label ------------------------
reset
invoke "nv-node-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-node-002.sh injects"
assert_call "label node ${NODE} nvidia.com/gpu.deploy.device-plugin=false --overwrite" \
    "nv-node-002.sh switches the device plugin off"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
export FAKE_LABEL_VALUE="false"
invoke "nv-node-002.sh" "recover" "${NODE}"
assert_rc 0 "nv-node-002.sh recovers"
assert_call "label node ${NODE} nvidia.com/gpu.deploy.device-plugin=${SAVED_LABEL} --overwrite" \
    "nv-node-002.sh writes back the value it saved"

reset
export FAKE_LABEL_VALUE=""
invoke "nv-node-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-node-002.sh injects over an absent deploy label"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
export FAKE_LABEL_VALUE="false"
invoke "nv-node-002.sh" "recover" "${NODE}"
assert_rc 0 "nv-node-002.sh recovers an absent deploy label"
assert_call "label node ${NODE} nvidia.com/gpu.deploy.device-plugin- --overwrite" \
    "an absent deploy label is removed again rather than written as true"
assert_no_call "label node ${NODE} nvidia.com/gpu.deploy.device-plugin=true --overwrite" \
    "nv-node-002.sh does not invent a healthy value"

# An operand that is already switched off is not one this injection can switch
# off, and saving "false" as the value to restore would leave it off for good.
for script in nv-op-002.sh nv-node-002.sh; do
    for value in "false" "paused-for-mig-change"; do
        reset
        export FAKE_LABEL_VALUE="${value}"
        invoke "${script}" "inject" "${NODE}"
        assert_rc 5 "${script} refuses to inject over a deploy label of ${value}"
        assert_no_writes "${script} writes nothing when the operand is already off"
    done
done

# -- nv-node-004.sh: the count label ---------------------------------------
reset
export FAKE_LABEL_VALUE="4"
invoke "nv-node-004.sh" "inject" "${NODE}"
assert_rc 0 "nv-node-004.sh injects"
assert_call "label node ${NODE} nvidia.com/gpu.count=5 --overwrite" \
    "nv-node-004.sh writes one more than the count it read"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
# The lane now reads 5, which is the value a recover that wrote back "what is
# there now" would restore. It has to write 4.
export FAKE_LABEL_VALUE="5"
invoke "nv-node-004.sh" "recover" "${NODE}"
assert_rc 0 "nv-node-004.sh recovers"
assert_call "label node ${NODE} nvidia.com/gpu.count=4 --overwrite" \
    "nv-node-004.sh writes back the count it saved, not the one it just read"
assert_no_call "label node ${NODE} nvidia.com/gpu.count=5 --overwrite" \
    "nv-node-004.sh does not write the injected count back"

reset
export FAKE_LABEL_VALUE=""
invoke "nv-node-004.sh" "inject" "${NODE}"
assert_rc 2 "nv-node-004.sh refuses a node with no count label"
assert_no_writes "nv-node-004.sh writes nothing when there is no count"

# -- the three ClusterPolicy version scripts -------------------------------
for pair in "nv-dp-001.sh:devicePlugin" "nv-dcgm-001.sh:dcgmExporter" "nv-op-001.sh:devicePlugin"; do
    script="${pair%%:*}"
    operand="${pair##*:}"

    reset
    export FAKE_POLICY_VERSION="${SAVED_VERSION}"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 0 "${script} injects"
    assert_call "patch clusterpolicies.nvidia.com cluster-policy --type=merge -p {\"spec\":{\"${operand}\":{\"version\":\"${ABSENT_VERSION}\"}}}" \
        "${script} points ${operand} at the absent version"

    : > "${FAKE_CALL_LOG}"
    : > "${FAKE_SCOPED_LOG}"
    recover_fixture "${script}"
    invoke "${script}" "recover" "${NODE}"
    assert_rc 0 "${script} recovers"
    assert_call "patch clusterpolicies.nvidia.com cluster-policy --type=merge -p {\"spec\":{\"${operand}\":{\"version\":\"${SAVED_VERSION}\"}}}" \
        "${script} writes back the version it saved"

    # Already absent: another script has this field broken, and saving the
    # broken value as the one to restore would make recover a no-op and leave
    # the lane unpullable for whatever runs next.
    reset
    export FAKE_POLICY_VERSION="${ABSENT_VERSION}"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 5 "${script} refuses when spec.${operand}.version is already absent"
    assert_says "already ${ABSENT_VERSION}" "${script} says why it refused"
    assert_no_writes "${script} patches nothing when the operand is already broken"
done

# An absent field recovers to absent, through a JSON remove: a merge patch
# cannot express "drop this field" without a null, and the CRD rejects a null
# there. Writing an empty string instead would leave the operand pointed at an
# image tag of "".
reset
export FAKE_POLICY_VERSION=""
invoke "nv-dp-001.sh" "inject" "${NODE}"
assert_rc 0 "nv-dp-001.sh injects over an absent version"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
export FAKE_TEMPLATE_IMAGE="nvcr.io/nvidia/k8s-device-plugin:v0.19.3"
invoke "nv-dp-001.sh" "recover" "${NODE}"
assert_rc 0 "nv-dp-001.sh recovers an absent version"
assert_call 'patch clusterpolicies.nvidia.com cluster-policy --type=json -p [{"op":"remove","path":"/spec/devicePlugin/version"}]' \
    "an absent version is removed again rather than written as an empty string"

# op-001 waits for the operator to have written the restored image back.
reset
export FAKE_POLICY_VERSION="${SAVED_VERSION}"
invoke "nv-op-001.sh" "inject" "${NODE}"
assert_rc 0 "nv-op-001.sh injects before the template check"
: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
recover_fixture "nv-op-001.sh"
invoke "nv-op-001.sh" "recover" "${NODE}"
assert_rc 0 "nv-op-001.sh recovers"
assert_logged "get daemonset nvidia-device-plugin-daemonset" \
    "nv-op-001.sh waits for the template to carry the restored image"

# -- nv-node-001.sh: the operator, the stand-in and the label --------------
reset
export FAKE_LABEL_VALUE="true"
export FAKE_APPLIED="${WORK}/standin.yaml"
invoke "nv-node-001.sh" "inject" "${NODE}"
assert_rc 0 "nv-node-001.sh injects"
assert_call "scale deploy/gpu-operator --replicas=0" "nv-node-001.sh scales the operator down"
assert_call "label node ${NODE} nvidia.com/gpu.present- --overwrite" \
    "nv-node-001.sh clears the label the operator writes"
if grep -q 'app: gpu-operator' "${WORK}/standin.yaml" 2> /dev/null; then
    ok "nv-node-001.sh plants a stand-in carrying the operator's own label"
else
    nope "the stand-in manifest does not carry app: gpu-operator" \
        "manifest:" "$(cat "${WORK}/standin.yaml" 2> /dev/null)"
fi
if grep -q "name: nv-faults-operator-standin" "${WORK}/standin.yaml" 2> /dev/null; then
    ok "the stand-in is the pod nv-node-001.sh later deletes"
else
    nope "the stand-in manifest does not name nv-faults-operator-standin"
fi

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
unset FAKE_APPLIED
export FAKE_LABEL_VALUE=""
invoke "nv-node-001.sh" "recover" "${NODE}"
assert_rc 0 "nv-node-001.sh recovers"
assert_call "label node ${NODE} nvidia.com/gpu.present=true --overwrite" \
    "nv-node-001.sh writes back the label it saved"
assert_call "delete pod nv-faults-operator-standin --ignore-not-found --wait=false" \
    "nv-node-001.sh takes the stand-in away again"
assert_call "scale deploy/gpu-operator --replicas=${SAVED_REPLICAS}" \
    "nv-node-001.sh scales the operator back to the replica count it saved"
assert_no_call "scale deploy/gpu-operator --replicas=1" \
    "nv-node-001.sh does not assume one replica"

# And the label it restores is the one it found, not the one the operator
# usually writes. A node whose gpu.present was already absent gets it removed
# again rather than written as true.
reset
export FAKE_LABEL_VALUE=""
invoke "nv-node-001.sh" "inject" "${NODE}"
assert_rc 0 "nv-node-001.sh injects over an absent gpu.present label"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
invoke "nv-node-001.sh" "recover" "${NODE}"
assert_rc 0 "nv-node-001.sh recovers an absent gpu.present label"
assert_call "label node ${NODE} nvidia.com/gpu.present- --overwrite" \
    "an absent gpu.present is removed again rather than written as true"
assert_no_call "label node ${NODE} nvidia.com/gpu.present=true --overwrite" \
    "nv-node-001.sh does not invent a healthy label value"

# Already broken, both shapes.
reset
export FAKE_DEPLOY_REPLICAS="0"
invoke "nv-node-001.sh" "inject" "${NODE}"
assert_rc 5 "nv-node-001.sh refuses when the operator is already at zero replicas"
assert_no_writes "nv-node-001.sh writes nothing when the operator is already down"

reset
export FAKE_STANDIN="nv-faults-operator-standin"
invoke "nv-node-001.sh" "inject" "${NODE}"
assert_rc 5 "nv-node-001.sh refuses when a stand-in is already there"
assert_no_writes "nv-node-001.sh writes nothing when a stand-in is already there"

# -- nv-node-003.sh: nvml-mock-ctl -----------------------------------------
reset
invoke "nv-node-003.sh" "inject" "${NODE}"
assert_rc 0 "nv-node-003.sh injects"
assert_call "exec pod-on-node -- nvml-mock-ctl fail --gpu 0 --mode ecc_uncorrectable --xid 79" \
    "nv-node-003.sh fails one device with the documented mode and Xid"
assert_call "delete pod pod-on-node --wait=false" \
    "nv-node-003.sh restarts the plugin pod so the failure is read"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
invoke "nv-node-003.sh" "recover" "${NODE}"
assert_rc 0 "nv-node-003.sh recovers"
assert_call "exec pod-on-node -- nvml-mock-ctl fail --gpu 0 --mode healthy" \
    "nv-node-003.sh clears the failure rather than repeating it"
assert_no_call "exec pod-on-node -- nvml-mock-ctl fail --gpu 0 --mode ecc_uncorrectable --xid 79" \
    "nv-node-003.sh does not inject again on recover"

# ---------------------------------------------------------------------------
# Node targeting
# ---------------------------------------------------------------------------

reset
invoke "nv-node-003.sh" "inject" "${NODE}"
assert_rc 0 "nv-node-003.sh injects before the targeting check"
assert_logged "--field-selector spec.nodeName=${NODE}" \
    "the pod lookup carries the node it was given"

# The shim answers only for the node under test, so a script that looked up the
# pod on another node, or dropped the field selector, finds nothing.
reset
export FAKE_POD_NODE="${OTHER_NODE}"
invoke "nv-node-003.sh" "inject" "${NODE}"
assert_rc 2 "nv-node-003.sh fails when there is no nvml-mock pod on the node"
assert_no_call "delete pod pod-on-node --wait=false" "no pod is deleted when the node has none"
assert_no_call "exec pod-on-node -- nvml-mock-ctl fail --gpu 0 --mode ecc_uncorrectable --xid 79" \
    "no device is failed on a node with no nvml-mock pod"

reset
export FAKE_POD_NODE="${OTHER_NODE}"
export FAKE_POLICY_VERSION="${SAVED_VERSION}"
invoke "nv-dp-001.sh" "inject" "${NODE}"
assert_rc 0 "nv-dp-001.sh injects even when the node has no plugin pod"
assert_no_call "delete pod pod-on-node --wait=false" \
    "nv-dp-001.sh deletes no pod on a node that has none"
assert_says "no nvidia-device-plugin-daemonset pod on ${NODE}" \
    "nv-dp-001.sh says the node had no pod to restart"

# A terminating pod is not the pod on the node: it still matches the selector
# and still answers Ready, and waiting for it is waiting for the wrong pod.
reset
export FAKE_PODS="dying-pod pod-uid-9 2026-10-11T19:00:00Z"
invoke "nv-node-003.sh" "inject" "${NODE}"
assert_rc 2 "a terminating pod does not count as the pod on the node"
assert_no_call "delete pod dying-pod --wait=false" "a terminating pod is not deleted again"

# ---------------------------------------------------------------------------
# recover is idempotent, for every script
# ---------------------------------------------------------------------------

for script in "${SCRIPTS[@]}"; do
    reset
    inject_fixture "${script}"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 0 "${script} injects before the idempotence check"

    recover_fixture "${script}"
    invoke "${script}" "recover" "${NODE}"
    assert_rc 0 "${script} recovers"

    : > "${FAKE_CALL_LOG}"
    : > "${FAKE_SCOPED_LOG}"
    invoke "${script}" "recover" "${NODE}"
    assert_rc 0 "${script} recovers a second time"
    assert_says "nothing to recover" "${script} says a second recover had nothing to do"
    assert_no_writes "${script} writes nothing on a second recover"

    # And with no inject at all.
    reset
    invoke "${script}" "recover" "${NODE}"
    assert_rc 0 "${script} recovers without a prior inject"
    assert_says "nothing to recover" "${script} says so without a prior inject"
    assert_no_writes "${script} writes nothing without a prior inject"
done

# ---------------------------------------------------------------------------
# inject refuses to run twice
# ---------------------------------------------------------------------------

for script in "${SCRIPTS[@]}"; do
    reset
    inject_fixture "${script}"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 0 "${script} injects before the double-inject check"

    : > "${FAKE_CALL_LOG}"
    : > "${FAKE_SCOPED_LOG}"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 5 "${script} refuses a second inject"
    assert_says "already injected" "${script} says why"
    assert_no_writes "${script} writes nothing on a second inject"
done

# ---------------------------------------------------------------------------
# The saved state is per node, per code and per cluster
# ---------------------------------------------------------------------------

reset
invoke "nv-op-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-op-002.sh injects on one node"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
invoke "nv-op-002.sh" "recover" "${OTHER_NODE}"
assert_says "nothing to recover" "recovering another node finds no state of its own"
assert_no_writes "recovering another node writes nothing"

# One code's state is not another's. nv-op-002 and nv-node-002 write different
# labels, so a shared state directory would have one recovering the other's.
reset
invoke "nv-op-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-op-002.sh injects before the cross-code check"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
invoke "nv-node-002.sh" "recover" "${NODE}"
assert_says "nothing to recover" "another code's recover finds no state of its own"
assert_no_writes "another code's recover writes nothing"

# One lane's state is not another's.
reset
invoke "nv-op-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-op-002.sh injects before the cross-lane check"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
export FAKE_CONTEXT="kind-kubectl-nv-other"
invoke "nv-op-002.sh" "recover" "${NODE}"
assert_says "nothing to recover" "another lane's recover finds no state of its own"
assert_no_writes "another lane's recover writes nothing"

# The same lane NAME, a different cluster. The state directory is the same, and
# the values in it belong to a cluster that is gone: a replica count or an
# image version from the old one is not a value anybody chose for this one.
reset
invoke "nv-op-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-op-002.sh injects before the recreated-lane check"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
export FAKE_NS_UID="uid-lane-b"
invoke "nv-op-002.sh" "recover" "${NODE}"
assert_rc 2 "a lane recreated under the same name refuses the old state"
assert_says "belongs to cluster uid-lane-a" "the refusal names the cluster the state came from"
assert_no_writes "no value from the old cluster is written to the new one"

# ---------------------------------------------------------------------------
# Incomplete saved state is a failure, not a guess
# ---------------------------------------------------------------------------

reset
invoke "nv-op-002.sh" "inject" "${NODE}"
assert_rc 0 "nv-op-002.sh injects before the truncated-state check"
rm -f "${MOKKA_STATE_DIR}/kubectl-nv-faults/NV-OP-002/${NODE}/label"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
invoke "nv-op-002.sh" "recover" "${NODE}"
assert_rc 1 "a saved state missing its key fails rather than guessing"
assert_says "has no 'label' key" "the failure names the key that is missing"
assert_no_writes "nothing is written from incomplete state"

# ---------------------------------------------------------------------------
# recover waits for the operand it disturbed
# ---------------------------------------------------------------------------
#
# Putting a label back is not the same thing as the lane being well again. The
# live run caught exactly that: nv-op-002.sh restored the deploy label and
# returned while the operator was still recreating the exporter pod, and the
# healthy gate that runs after the fault suite then reported NV-DCGM-001 on a
# lane whose faults had all been "recovered".

for pair in "nv-op-002.sh:nvidia-dcgm-exporter" "nv-node-002.sh:nvidia-device-plugin-daemonset" \
    "nv-dcgm-001.sh:nvidia-dcgm-exporter" "nv-dp-001.sh:nvidia-device-plugin-daemonset" \
    "nv-node-003.sh:nvidia-device-plugin-daemonset"; do
    script="${pair%%:*}"
    operand="${pair##*:}"

    reset
    inject_fixture "${script}"
    invoke "${script}" "inject" "${NODE}"
    assert_rc 0 "${script} injects before its readiness check"

    : > "${FAKE_CALL_LOG}"
    : > "${FAKE_SCOPED_LOG}"
    recover_fixture "${script}"
    export FAKE_POD_READY="False"
    export FAULTS_READY_TIMEOUT=0
    export FAULTS_READY_POLL=0
    invoke "${script}" "recover" "${NODE}"
    assert_rc 1 "${script} recover fails while ${operand} is not Ready"
    assert_says "no Ready ${operand} pod" "${script} names the operand it waited for"

    # The state survives a recover that could not finish, so the lane can be
    # recovered again once the operand comes back.
    : > "${FAKE_CALL_LOG}"
    : > "${FAKE_SCOPED_LOG}"
    unset FAKE_POD_READY
    export FAULTS_READY_TIMEOUT=10
    export FAULTS_READY_POLL=1
    invoke "${script}" "recover" "${NODE}"
    assert_rc 0 "${script} recovers once ${operand} is Ready"
    assert_says "is Ready again" "${script} says the operand is back"
done

# nv-node-001.sh waits for the Deployment rather than for a pod.
reset
export FAKE_LABEL_VALUE="true"
invoke "nv-node-001.sh" "inject" "${NODE}"
assert_rc 0 "nv-node-001.sh injects before its readiness check"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
export FAKE_DEPLOY_READY="0"
export FAULTS_READY_TIMEOUT=0
export FAULTS_READY_POLL=0
invoke "nv-node-001.sh" "recover" "${NODE}"
assert_rc 1 "nv-node-001.sh recover fails while the operator has no ready replica"
assert_says "deploy/gpu-operator had no ready replica" "nv-node-001.sh names the Deployment it waited for"

# ---------------------------------------------------------------------------
# The waits are bounded, and the absent wait does not return early
# ---------------------------------------------------------------------------

# The drain: with an operator pod still up, the label it writes comes straight
# back and the injection does nothing. The drain is given no time at all, so a
# script that cleared the label first would reach the label call before failing.
reset
export FAKE_LABEL_VALUE="true"
export FAKE_OPERATOR_PODS="gpu-operator-abcde"
export FAULTS_DRAIN_TIMEOUT=0
export FAULTS_DRAIN_POLL=0
invoke "nv-node-001.sh" "inject" "${NODE}"
assert_rc 1 "nv-node-001.sh fails when the operator will not drain"
assert_says "still has pods after" "the drain timeout says what it was waiting for"
assert_call "scale deploy/gpu-operator --replicas=0" "nv-node-001.sh scales the operator down first"
assert_no_call "label node ${NODE} nvidia.com/gpu.present- --overwrite" \
    "nv-node-001.sh clears no label while an operator pod is up"

# The template wait, present: the operator never writes the absent version into
# the DaemonSet, so inject gives up rather than deleting a pod that would come
# back from the old template.
reset
export FAKE_POLICY_VERSION="${SAVED_VERSION}"
export FAKE_TEMPLATE_IMAGE="nvcr.io/nvidia/k8s-device-plugin:${SAVED_VERSION}"
export FAULTS_TEMPLATE_TIMEOUT=0
export FAULTS_TEMPLATE_POLL=0
invoke "nv-dp-001.sh" "inject" "${NODE}"
assert_rc 1 "nv-dp-001.sh fails when the operator never writes the absent image"
assert_says "did not have ${ABSENT_VERSION} present within" "the template timeout says what it wanted"
assert_no_call "delete pod pod-on-node --wait=false" \
    "no pod is restarted while the template still holds the old image"

# The template wait, absent: the needle is STILL in the template, so the wait
# has to keep waiting and then time out. A wait that returned as soon as it was
# asked would let recover delete the pod and get the broken image straight back.
reset
export FAKE_POLICY_VERSION="${SAVED_VERSION}"
invoke "nv-dp-001.sh" "inject" "${NODE}"
assert_rc 0 "nv-dp-001.sh injects before the absent-wait check"

: > "${FAKE_CALL_LOG}"
: > "${FAKE_SCOPED_LOG}"
export FAKE_TEMPLATE_IMAGE="nvcr.io/nvidia/k8s-device-plugin:${ABSENT_VERSION}"
export FAULTS_TEMPLATE_TIMEOUT=0
export FAULTS_TEMPLATE_POLL=0
invoke "nv-dp-001.sh" "recover" "${NODE}"
assert_rc 1 "nv-dp-001.sh recover fails while the template still holds the absent image"
assert_says "did not have ${ABSENT_VERSION} absent within" "the absent wait says what it wanted gone"
assert_no_call "delete pod pod-on-node --wait=false" \
    "no pod is restarted while the template still holds the broken image"

# ---------------------------------------------------------------------------
# Nothing ever reaches the caller's own kubeconfig, and nothing is unscoped
# ---------------------------------------------------------------------------

if grep -qF -- "/.kube/config" "${FAKE_ALL_CALLS}"; then
    nope "a call made during this run names a .kube/config path" \
        "calls:" "$(grep -F -- '/.kube/config' "${FAKE_ALL_CALLS}")"
else
    ok "no call made during this run names a .kube/config path"
fi

if grep -q '^UNSCOPED ' "${FAKE_ALL_CALLS}"; then
    nope "a call made during this run was not scoped to the lane" \
        "calls:" "$(grep '^UNSCOPED ' "${FAKE_ALL_CALLS}" | head -20)"
else
    ok "every kubectl call in this run named the lane kubeconfig and context"
fi

# The guard itself has to be able to fail, or the assertion above is a
# statement about nothing. This drives one unscoped call deliberately.
if (PATH="${SHIM_DIR}:${PATH}" FAKE_CALL_LOG="${WORK}/guard.log" \
    FAKE_ALL_CALLS="${WORK}/guard.log" kubectl get nodes > /dev/null 2>&1); then
    nope "the shim answered a call that named no kubeconfig"
else
    if grep -q '^UNSCOPED get nodes' "${WORK}/guard.log"; then
        ok "the scoping guard records and refuses an unscoped call"
    else
        nope "the scoping guard did not record an unscoped call" \
            "log:" "$(cat "${WORK}/guard.log" 2> /dev/null)"
    fi
fi
rm -f "${WORK}/guard.log"

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
