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

    # The prefix on its own is not enough. What follows it has to be a lane
    # somebody could have created: hack/mokka/lib.sh's mokka::valid_lane holds
    # a lane to a DNS label of at most 32 characters, so a context carrying
    # anything else was not written by up.sh, and the match is anchored against
    # the WHOLE suffix because a line-oriented check accepts an embedded
    # newline and reads only its first line.
    local lane_suffix="${FAULTS_CONTEXT#kind-kubectl-nv-}"
    if ! [[ ${lane_suffix} =~ ^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$ ]]; then
        faults::die "${FAULTS_RC_USAGE}" \
            "lane '${lane_suffix}' in context ${FAULTS_CONTEXT} is not a DNS label; refusing to write to it"
    fi

    # And the cluster answers on the loopback address. A kind lane always
    # does, because kind publishes its API server on 127.0.0.1; a context
    # named like one of ours that answers somewhere else is either a mistake
    # or somebody else's cluster wearing the name, and the name is the only
    # thing the checks above can see.
    local server
    server="$(k config view --minify -o 'jsonpath={.clusters[0].cluster.server}' 2> /dev/null || true)"
    case "${server}" in
        https://127.0.0.1:*) ;;
        *)
            faults::die "${FAULTS_RC_USAGE}" \
                "context ${FAULTS_CONTEXT} points at ${server:-<unreadable>}, which is not a local kind lane"
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

    # Which cluster this is, as against which cluster it is called. A lane torn
    # down and brought up again has the same name, the same context and the
    # same state directory, and none of the values saved against the old one
    # mean anything against the new one. The kube-system namespace is created
    # once per cluster and never recreated, so its UID is the cluster's
    # identity; recover compares it and refuses rather than writing a stale
    # image version or replica count into a cluster that never had it.
    FAULTS_LANE_UID="$(k get namespace kube-system -o 'jsonpath={.metadata.uid}' 2> /dev/null || true)"
    if [ -z "${FAULTS_LANE_UID}" ]; then
        faults::die "${FAULTS_RC_USAGE}" "cannot read the kube-system UID of ${FAULTS_CONTEXT}"
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

# faults::lane_stamp_file
# Where the cluster identity of the saved state lives.
faults::lane_stamp_file() {
    printf '%s/.lane-uid' "${FAULTS_STATE}"
}

# faults::save <key> <value>
# An empty value is written as the absent sentinel, so that recover can tell
# "there was no such field" from "the field was there and empty".
#
# The write is atomic: a temporary file in the same directory, then a rename.
# A half-written key is worse than no key at all, because recover would read
# it and put the truncated value back; a rename either happened or did not.
faults::save() {
    local key="$1" value="$2" tmp
    mkdir -p "${FAULTS_STATE}"
    chmod 700 "${FAULTS_STATE}"

    # The cluster this state belongs to, written once, with the first key.
    if [ ! -f "$(faults::lane_stamp_file)" ]; then
        printf '%s' "${FAULTS_LANE_UID}" > "$(faults::lane_stamp_file).tmp"
        command mv -f "$(faults::lane_stamp_file).tmp" "$(faults::lane_stamp_file)"
    fi

    if [ -z "${value}" ]; then
        value="${FAULTS_ABSENT_SENTINEL}"
    fi
    tmp="${FAULTS_STATE}/.${key}.tmp"
    printf '%s' "${value}" > "${tmp}"
    command mv -f "${tmp}" "${FAULTS_STATE}/${key}"
}

# faults::load <key>
# Prints the saved value. A key that was never saved is a failure rather than
# an absent sentinel: "the field was absent when we looked" is a value inject
# writes down explicitly, so a missing FILE means the state is incomplete, and
# recovering from incomplete state is how a lane gets a value nobody chose.
faults::load() {
    local file="${FAULTS_STATE}/$1"
    if [ ! -f "${file}" ]; then
        faults::die 1 "saved state for ${FAULTS_CODE} on ${FAULTS_NODE} has no '$1' key (looked in ${FAULTS_STATE})"
    fi
    cat "${file}"
}

# faults::check_lane_stamp
# Refuses when the saved state belongs to another cluster. Called by
# nothing_to_recover, so every recover goes through it.
#
# A stamp this code cannot read is treated as a mismatch. The alternative is to
# accept it, and the whole point of the stamp is that state of unknown
# provenance is not applied to a live cluster.
faults::check_lane_stamp() {
    local saved
    saved="$(cat "$(faults::lane_stamp_file)" 2> /dev/null || true)"
    if [ "${saved}" = "${FAULTS_LANE_UID}" ]; then
        return 0
    fi
    faults::die "${FAULTS_RC_USAGE}" \
        "saved state in ${FAULTS_STATE} belongs to cluster ${saved:-<unreadable>}, and ${FAULTS_CONTEXT} is ${FAULTS_LANE_UID}; delete that directory if the old lane is gone"
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
        faults::check_lane_stamp

        return 1
    fi
    faults::note "${FAULTS_CODE}: nothing to recover on ${FAULTS_NODE}"

    return 0
}

# ---------------------------------------------------------------------------
# Labels
# ---------------------------------------------------------------------------

# faults::node_label <node> <label>
# Prints the label's value, empty when it is absent.
#
# Only the dots are escaped. A dot separates path components in a jsonpath and
# a slash does not, so hack/mokka/up.sh reads these same labels as
# nvidia\.com/gpu\.product with the slash left alone; escaping it as well
# produces a key no label matches and an empty answer for every label, which
# recover would then write back as "the label was absent".
faults::node_label() {
    local node="$1" label="$2" escaped
    escaped="${label//./\\.}"
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
# The name of a live pod matching the selector on that node, empty when there
# is none.
#
# The node is a field selector rather than a grep over names: a lane has one
# such pod per node and the names differ only by a suffix, so a name match
# would pick the other worker's pod as readily as this one's.
#
# Two pods are skipped, and both of them have been waited on by mistake here.
# A pod carrying a deletionTimestamp is terminating: it still matches the
# selector and still answers Ready, so a recover that waited for it would
# return while the replacement had not started. And the pod restart_operand
# just deleted, by UID, which is the same trap one moment earlier, before the
# API server has put the timestamp on it.
faults::pod_on_node() {
    local namespace="$1" selector="$2" node="$3" pods name uid deleted
    pods="$(k --namespace "${namespace}" get pod -l "${selector}" \
        --field-selector "spec.nodeName=${node}" \
        -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{.metadata.uid}{" "}{.metadata.deletionTimestamp}{"\n"}{end}' \
        2> /dev/null || true)"

    while IFS=' ' read -r name uid deleted; do
        [ -n "${name}" ] || continue
        [ -z "${deleted}" ] || continue
        [ "${uid}" != "${FAULTS_DELETED_POD_UID:-}" ] || continue
        printf '%s' "${name}"

        return 0
    done <<< "${pods}"
}

# ---------------------------------------------------------------------------
# Operand pods
# ---------------------------------------------------------------------------
#
# Two observations from the first live run against a t4 lane, both of which
# turn a reproduction that "should work" into one that does:
#
#   - A DaemonSet rolling update takes one node at a time and stops at the
#     first replacement that never becomes ready. Patching the ClusterPolicy at
#     an image that cannot be pulled therefore breaks exactly one node, and
#     which one is whichever the controller reached first. The target node has
#     to have its pod deleted, because the DaemonSet controller makes a pod for
#     a node that has none from the CURRENT template, whatever the rollout is
#     waiting on.
#   - The device plugin reads nvml-mock's Xid events when it opens its event
#     set, which it does once, at startup. A failure injected under a running
#     plugin changes nothing; the same failure with the plugin restarted after
#     it takes allocatable from 4 to 3 within ten seconds.
#
# Both come down to the same call, so it is written once.

# How long to wait for a DaemonSet's pod template to carry a change, and how
# often to look.
FAULTS_TEMPLATE_TIMEOUT="${FAULTS_TEMPLATE_TIMEOUT:-180}"
FAULTS_TEMPLATE_POLL="${FAULTS_TEMPLATE_POLL:-2}"

# faults::restart_operand <app> <node>
# Deletes the operand pod of that app label on the node, if there is one. The
# DaemonSet controller replaces it from the template as it stands now.
#
# An absent pod is not an error: the DaemonSet may have taken it away already,
# and the point of the call is the state afterwards rather than the deletion.
faults::restart_operand() {
    local app="$1" node="$2" pod
    pod="$(faults::pod_on_node "${FAULTS_OPERATOR_NAMESPACE}" "app=${app}" "${node}")"
    if [ -z "${pod}" ]; then
        faults::note "${FAULTS_CODE}: no ${app} pod on ${node} to restart"

        return 0
    fi

    # Remembered before the delete, so that every later lookup in this run can
    # skip it even in the window before the API server has given it a
    # deletionTimestamp.
    FAULTS_DELETED_POD_UID="$(k --namespace "${FAULTS_OPERATOR_NAMESPACE}" get pod "${pod}" \
        -o 'jsonpath={.metadata.uid}' 2> /dev/null || true)"

    k --namespace "${FAULTS_OPERATOR_NAMESPACE}" delete pod "${pod}" --wait=false > /dev/null
    faults::note "${FAULTS_CODE}: deleted ${app} pod ${pod} on ${node}"
}

# faults::wait_for_template <daemonset> present|absent <needle>
# Waits until the DaemonSet's pod template images do, or do not, contain the
# needle. The operator writes the template, so this is the wait for the
# operator to have acted on the ClusterPolicy patch; deleting the pod before it
# has would replace the pod from the old template and inject nothing.
faults::wait_for_template() {
    local name="$1" want="$2" needle="$3" deadline images
    deadline=$(($(date +%s) + FAULTS_TEMPLATE_TIMEOUT))
    while :; do
        images="$(k --namespace "${FAULTS_OPERATOR_NAMESPACE}" get daemonset "${name}" \
            -o 'jsonpath={.spec.template.spec.containers[*].image}{" "}{.spec.template.spec.initContainers[*].image}' \
            2> /dev/null || true)"
        case "${want}:${images}" in
            present:*"${needle}"*) return 0 ;;
            absent:*"${needle}"*) ;;
            absent:*) return 0 ;;
        esac
        if [ "$(date +%s)" -ge "${deadline}" ]; then
            faults::die 1 "daemonset ${name} template did not have ${needle} ${want} within ${FAULTS_TEMPLATE_TIMEOUT}s (images: ${images:-<none>})"
        fi
        sleep "${FAULTS_TEMPLATE_POLL}"
    done
}

# How long to wait for an operand to be Ready again after a recover.
FAULTS_READY_TIMEOUT="${FAULTS_READY_TIMEOUT:-300}"
FAULTS_READY_POLL="${FAULTS_READY_POLL:-3}"

# faults::wait_for_operand_ready <app> <node>
# Waits until the operand pod of that app label on the node is Ready.
#
# Every recover that disturbed an operand ends here, and the reason is a
# failure the final live run produced: nv-op-002.sh put the deploy label back
# and returned while the exporter pod was still being recreated, so the healthy
# gate that runs after the fault suite saw NV-DCGM-001 on a lane whose faults
# had all been "recovered". Putting the label back is not the same thing as the
# lane being well again, and only the second one is what recover promises.
faults::wait_for_operand_ready() {
    local app="$1" node="$2" deadline ready pod
    deadline=$(($(date +%s) + FAULTS_READY_TIMEOUT))
    while :; do
        pod="$(faults::pod_on_node "${FAULTS_OPERATOR_NAMESPACE}" "app=${app}" "${node}")"
        if [ -n "${pod}" ]; then
            ready="$(k --namespace "${FAULTS_OPERATOR_NAMESPACE}" get pod "${pod}" \
                -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2> /dev/null || true)"
            if [ "${ready}" = "True" ]; then
                faults::note "${FAULTS_CODE}: ${app} pod ${pod} is Ready again on ${node}"

                return 0
            fi
        fi
        if [ "$(date +%s)" -ge "${deadline}" ]; then
            faults::die 1 "no Ready ${app} pod on ${node} within ${FAULTS_READY_TIMEOUT}s (pod: ${pod:-<none>})"
        fi
        sleep "${FAULTS_READY_POLL}"
    done
}

# faults::wait_for_deployment_available <name>
# The same promise for a Deployment: nv-node-001.sh scales the operator down,
# so its recover is not done until the operator is back and reconciling.
faults::wait_for_deployment_available() {
    local name="$1" deadline ready
    deadline=$(($(date +%s) + FAULTS_READY_TIMEOUT))
    while :; do
        ready="$(k --namespace "${FAULTS_OPERATOR_NAMESPACE}" get deploy "${name}" \
            -o 'jsonpath={.status.readyReplicas}' 2> /dev/null || true)"
        if [ -n "${ready}" ] && [ "${ready}" -ge 1 ] 2> /dev/null; then
            faults::note "${FAULTS_CODE}: deploy/${name} has ${ready} ready replica(s)"

            return 0
        fi
        if [ "$(date +%s)" -ge "${deadline}" ]; then
            faults::die 1 "deploy/${name} had no ready replica within ${FAULTS_READY_TIMEOUT}s"
        fi
        sleep "${FAULTS_READY_POLL}"
    done
}

# faults::deploy_is_off <value>
# Whether a nvidia.com/gpu.deploy.<operand> label value already says the
# operand is not running. The same two shapes pkg/health/rules reads: "false",
# and the "paused-for-..." family whose suffix changes between operator
# versions.
faults::deploy_is_off() {
    case "$1" in
        false | paused-*) return 0 ;;
        *) return 1 ;;
    esac
}
