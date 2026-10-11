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
# Shared by every hack/mokka/faults/<code>.sh script.
#
# This file is sourced, not executed.
#
# A fault script writes to a live cluster, which is the one thing kubectl-nv
# itself never does. Three guards stand between it and somebody's real cluster,
# and all three are here rather than copied into each script:
#
#   1. The lane kubeconfig is named explicitly, through MOKKA_KUBECONFIG. There
#      is no fallback to $KUBECONFIG and none to ~/.kube/config: a script that
#      guessed would eventually guess a production cluster.
#   2. The current context of that kubeconfig has to match kind-kubectl-nv-*,
#      the same guard `make e2e` applies before it runs the suite. A kubeconfig
#      can name many clusters, so naming the file is not on its own a statement
#      about which cluster is about to be written to.
#   3. Every kubectl call passes --kubeconfig and --context. The environment is
#      not trusted to carry either.
#
# Recovery is built on saved state rather than on a hard-coded "healthy" value.
# `inject` writes what it found into the state directory before it changes
# anything, and `recover` writes exactly that back. A script that reset a label
# to the value its author believed was right would be a script that silently
# changed the lane on the first profile whose value was different.
#
# shellcheck disable=SC2034
# SC2034: the constants below are read by the scripts that source this file,
# which shellcheck cannot see when pointed at this file on its own.

# ---------------------------------------------------------------------------
# Exit codes
# ---------------------------------------------------------------------------
#
#   0  the action succeeded, or there was nothing to recover
#   1  a kubectl call failed; its own output says why
#   2  bad usage: arguments, kubeconfig, context, or an unknown node
#   3  a required tool is missing
#   5  inject was called on a fault that is already injected

readonly FAULTS_RC_USAGE=2
readonly FAULTS_RC_TOOL=3
readonly FAULTS_RC_ALREADY=5

# The namespaces and selectors the lane puts things in. One definition each:
# a script that spelled "gpu-operator" itself would keep working against a lane
# that had moved, right up until it silently patched nothing.
FAULTS_OPERATOR_NAMESPACE="gpu-operator"
FAULTS_MOKKA_NAMESPACE="mokka"
FAULTS_MOKKA_SELECTOR="app.kubernetes.io/name=nvml-mock"
FAULTS_OPERATOR_SELECTOR="app=gpu-operator"
FAULTS_CLUSTER_POLICY="cluster-policy"

# The image tag every "make this operand unpullable" injection points at. It is
# a tag that cannot exist rather than a registry that cannot be reached: an
# unreachable registry fails differently behind a proxy, and the point is a pod
# that stays unready, deterministically, on any runner.
FAULTS_ABSENT_VERSION="v0.0.0-absent"

# The sentinel a saved-state file holds when the field it saved was absent.
# An empty file cannot carry that meaning: "the label was empty" and "there was
# no label" recover differently, and reading an empty file as "absent" would
# make the two indistinguishable.
FAULTS_ABSENT_SENTINEL="<absent>"

# ---------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------

# faults::die <exit-code> <message>...
faults::die() {
    local code="$1"
    shift
    printf '%s: %s\n' "${FAULTS_PROG:-faults}" "$*" >&2
    exit "${code}"
}

# faults::note <message>...
# One observation line, stamped. The timings in NOTES.md come from these, so
# they carry a clock rather than only an ordering.
faults::note() {
    printf '[faults %s] %s\n' "$(date -u '+%H:%M:%SZ')" "$*"
}

# ---------------------------------------------------------------------------
# Argument parsing and the guards
# ---------------------------------------------------------------------------

# faults::usage
faults::usage() {
    printf 'usage: %s inject|recover <node>\n' "${FAULTS_PROG:-faults}" >&2
    printf '  MOKKA_KUBECONFIG must name the lane kubeconfig.\n' >&2
}

# faults::valid_node <name>
# A node name reaches kubectl as an argument, so it is matched against the
# WHOLE string: a line-oriented check accepts an embedded newline and sees only
# its first line, which would let a second line name another object.
faults::valid_node() {
    [[ $1 =~ ^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$ ]]
}

# faults::require_tools <tool>...
faults::require_tools() {
    local tool
    for tool in "$@"; do
        if ! command -v "${tool}" > /dev/null 2>&1; then
            faults::die "${FAULTS_RC_TOOL}" "required tool not found: ${tool}"
        fi
    done
}

# faults::init <code> <arg>...
#
# Parses the command line, applies the three guards and sets:
#
#   FAULTS_CODE       the finding code, upper case
#   FAULTS_ACTION     inject or recover
#   FAULTS_NODE       the node to act on
#   FAULTS_KUBECONFIG the lane kubeconfig
#   FAULTS_CONTEXT    the lane context
#   FAULTS_STATE      the directory this code's saved values live in
#
# It does not dispatch. The caller runs its own inject or recover, so that a
# script reads as "here is what injecting this fault does" rather than as a
# callback registered with a framework.
faults::init() {
    FAULTS_CODE="$1"
    shift

    if [ "$#" -ne 2 ]; then
        faults::usage
        exit "${FAULTS_RC_USAGE}"
    fi

    FAULTS_ACTION="$1"
    FAULTS_NODE="$2"

    case "${FAULTS_ACTION}" in
        inject | recover) ;;
        *)
            faults::usage
            faults::die "${FAULTS_RC_USAGE}" "unknown action '${FAULTS_ACTION}'"
            ;;
    esac

    if ! faults::valid_node "${FAULTS_NODE}"; then
        faults::die "${FAULTS_RC_USAGE}" "node must be a DNS name (lowercase letters, digits, hyphens, dots)"
    fi

    faults::require_tools kubectl

    # Guard 1: the kubeconfig is named, never guessed.
    FAULTS_KUBECONFIG="${MOKKA_KUBECONFIG:-}"
    if [ -z "${FAULTS_KUBECONFIG}" ]; then
        faults::die "${FAULTS_RC_USAGE}" "MOKKA_KUBECONFIG is not set; it must name the lane kubeconfig"
    fi
    if [ ! -f "${FAULTS_KUBECONFIG}" ]; then
        faults::die "${FAULTS_RC_USAGE}" "MOKKA_KUBECONFIG=${FAULTS_KUBECONFIG} is not a file"
    fi

    # Guard 2: and that kubeconfig's current context is a lane of ours. The
    # context is read rather than derived from the file name: the file says
    # which clusters are reachable, the context says which one is about to be
    # written to.
    FAULTS_CONTEXT="$(kubectl --kubeconfig "${FAULTS_KUBECONFIG}" config current-context 2> /dev/null || true)"
    case "${FAULTS_CONTEXT}" in
        kind-kubectl-nv-*) ;;
        *)
            faults::die "${FAULTS_RC_USAGE}" \
                "current context is ${FAULTS_CONTEXT:-<none>}, which is not a kubectl-nv lane; refusing to write to it"
            ;;
    esac

    # The state is per lane as well as per code and node: two lanes up at once
    # must not recover each other's saved values.
    #
    # It defaults under hack/mokka/bin, which is where the lane kubeconfigs
    # already go and which hack/mokka/.gitignore already excludes. A default
    # inside hack/mokka/faults would leave an untracked directory in the source
    # tree after every run.
    local lane="${FAULTS_CONTEXT#kind-}"
    local state_root="${MOKKA_STATE_DIR:-${MOKKA_BIN_DIR:-${FAULTS_SCRIPT_DIR}/../bin}/faults-state}"
    FAULTS_STATE="${state_root}/${lane}/${FAULTS_CODE}/${FAULTS_NODE}"

    if ! faults::node_exists "${FAULTS_NODE}"; then
        faults::die "${FAULTS_RC_USAGE}" "no node named ${FAULTS_NODE} in ${FAULTS_CONTEXT}"
    fi
}

# faults::node_exists <node>
faults::node_exists() {
    k get node "$1" -o name > /dev/null 2>&1
}

# k <arg>...
# Guard 3: kubectl, always against the lane kubeconfig and the lane context.
k() {
    kubectl --kubeconfig "${FAULTS_KUBECONFIG}" --context "${FAULTS_CONTEXT}" "$@"
}

# ---------------------------------------------------------------------------
# Saved state
# ---------------------------------------------------------------------------

# faults::injected
# True once inject has saved anything for this code and node.
faults::injected() {
    [ -d "${FAULTS_STATE}" ]
}

# faults::refuse_double_inject
# inject is not idempotent and must not pretend to be: a second inject would
# save the already-broken value as the one to restore, and recover would then
# leave the lane broken for good.
faults::refuse_double_inject() {
    if faults::injected; then
        faults::die "${FAULTS_RC_ALREADY}" \
            "${FAULTS_CODE} is already injected on ${FAULTS_NODE} (state in ${FAULTS_STATE}); run recover first"
    fi
}

# faults::save <key> <value>
# An empty value is written as the absent sentinel, so that recover can tell
# "there was no such field" from "the field was there and empty".
faults::save() {
    local key="$1" value="$2"
    mkdir -p "${FAULTS_STATE}"
    chmod 700 "${FAULTS_STATE}"
    if [ -z "${value}" ]; then
        value="${FAULTS_ABSENT_SENTINEL}"
    fi
    printf '%s' "${value}" > "${FAULTS_STATE}/${key}"
}

# faults::load <key>
# Prints the saved value, or the absent sentinel when nothing was saved under
# that key. A caller compares against FAULTS_ABSENT_SENTINEL.
faults::load() {
    local file="${FAULTS_STATE}/$1"
    if [ ! -f "${file}" ]; then
        printf '%s' "${FAULTS_ABSENT_SENTINEL}"
        return
    fi
    cat "${file}"
}

# faults::forget
# Drops this code's saved state for this node, which is what makes a second
# recover a no-op rather than a second restore.
faults::forget() {
    rm -rf "${FAULTS_STATE}"
}

# faults::nothing_to_recover
# The idempotent half of recover: called first by every script, it reports
# whether there is no saved state, in which case the script says so and exits 0.
faults::nothing_to_recover() {
    if faults::injected; then
        return 1
    fi
    faults::note "${FAULTS_CODE}: nothing to recover on ${FAULTS_NODE}"

    return 0
}

# ---------------------------------------------------------------------------
# Labels
# ---------------------------------------------------------------------------

# faults::node_label <node> <label>
# Prints the label's value, empty when it is absent. The label name is escaped
# for a jsonpath, where a dot and a slash are both path syntax.
faults::node_label() {
    local node="$1" label="$2" escaped
    escaped="${label//./\\.}"
    escaped="${escaped//\//\\/}"
    k get node "${node}" -o "jsonpath={.metadata.labels.${escaped}}"
}

# faults::set_node_label <node> <label> <value>
# A value of the absent sentinel removes the label, which is how a label that
# was not there before inject is put back the way it was found.
faults::set_node_label() {
    local node="$1" label="$2" value="$3"
    if [ "${value}" = "${FAULTS_ABSENT_SENTINEL}" ]; then
        k label node "${node}" "${label}-" --overwrite > /dev/null

        return
    fi
    k label node "${node}" "${label}=${value}" --overwrite > /dev/null
}

# ---------------------------------------------------------------------------
# ClusterPolicy
# ---------------------------------------------------------------------------

# faults::policy_field <path>
# Reads one dotted spec path out of the lane's ClusterPolicy, empty when it is
# absent. ClusterPolicy is cluster scoped, so there is no namespace here.
faults::policy_field() {
    k get clusterpolicies.nvidia.com "${FAULTS_CLUSTER_POLICY}" \
        -o "jsonpath={$1}" 2> /dev/null
}

# faults::set_operand_version <operand> <version>
# Points one ClusterPolicy operand at an image version. The absent sentinel
# removes spec.<operand>.version instead, through a JSON patch: a merge patch
# cannot express "drop this field" without a null, and the operator's CRD
# rejects a null there.
faults::set_operand_version() {
    local operand="$1" version="$2"

    if [ "${version}" = "${FAULTS_ABSENT_SENTINEL}" ]; then
        k patch clusterpolicies.nvidia.com "${FAULTS_CLUSTER_POLICY}" --type=json \
            -p "[{\"op\":\"remove\",\"path\":\"/spec/${operand}/version\"}]" > /dev/null

        return
    fi

    k patch clusterpolicies.nvidia.com "${FAULTS_CLUSTER_POLICY}" --type=merge \
        -p "{\"spec\":{\"${operand}\":{\"version\":\"${version}\"}}}" > /dev/null
}

# ---------------------------------------------------------------------------
# Pods
# ---------------------------------------------------------------------------

# faults::pod_on_node <namespace> <selector> <node>
# The name of the first pod matching the selector on that node, empty when
# there is none. The node is a field selector rather than a grep over names:
# a lane has one such pod per node and the names differ only by a suffix.
faults::pod_on_node() {
    k --namespace "$1" get pod -l "$2" \
        --field-selector "spec.nodeName=$3" \
        -o 'jsonpath={.items[0].metadata.name}' 2> /dev/null
}
