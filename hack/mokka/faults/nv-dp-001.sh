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
# NV-DP-001: the device plugin pod on the node is not ready.
#
# Injection: point the device plugin at an image version that does not exist.
# The DaemonSet rolls, the replacement pod never pulls its image, and the node
# is left with a plugin pod that is not ready while both things the rule reads
# as consent still say the operand belongs there: the ClusterPolicy still
# enables it, and the node's nvidia.com/gpu.deploy.device-plugin label is still
# true.
#
# Switching the operand off with that deploy label instead would suppress this
# rule on purpose and reproduce NV-NODE-002 and NV-OP-002, which is why
# nv-node-002.sh exists and does exactly that.
#
# This is the same injection as nv-op-001.sh, and deliberately so: one patch
# produces both findings, and the two scripts are separate because the two
# codes recover independently. Running both at once is refused below.
#
# Usage: hack/mokka/faults/nv-dp-001.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-dp-001.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-DP-001" "$@"

OPERAND="devicePlugin"

inject() {
    faults::refuse_double_inject

    local before
    before="$(faults::policy_field ".spec.${OPERAND}.version")"

    # Another script has this operand broken already. Saving the broken value
    # as the one to restore would make recover a no-op and leave the lane
    # unpullable for whatever runs next.
    if [ "${before}" = "${FAULTS_ABSENT_VERSION}" ]; then
        faults::die "${FAULTS_RC_ALREADY}" \
            "spec.${OPERAND}.version is already ${FAULTS_ABSENT_VERSION}; recover the other fault first"
    fi

    faults::save "version" "${before}"
    faults::note "NV-DP-001: spec.${OPERAND}.version was '${before:-<absent>}', plugin pod on ${FAULTS_NODE}: $(faults::plugin_pod)"

    faults::set_operand_version "${OPERAND}" "${FAULTS_ABSENT_VERSION}"
    faults::note "NV-DP-001: spec.${OPERAND}.version=${FAULTS_ABSENT_VERSION}"

    # The patch alone breaks whichever node the rolling update reaches first,
    # and stops there: the first replacement never becomes ready, so the
    # update never moves on. On the first live run that was worker2 while the
    # node under test kept its healthy pod for the whole six minutes. Deleting
    # the pod on the node under test is what puts the fault where this script
    # was asked to put it, because a node with no pod gets one from the
    # template as it stands now rather than from the rollout's position.
    faults::wait_for_template "nvidia-device-plugin-daemonset" present "${FAULTS_ABSENT_VERSION}"
    faults::restart_operand "nvidia-device-plugin-daemonset" "${FAULTS_NODE}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    local saved
    saved="$(faults::load version)"
    faults::set_operand_version "${OPERAND}" "${saved}"

    # The same two steps in reverse: wait for the operator to have written the
    # restored image into the template, then take the broken pod away so the
    # node gets one built from it.
    faults::wait_for_template "nvidia-device-plugin-daemonset" absent "${FAULTS_ABSENT_VERSION}"
    faults::restart_operand "nvidia-device-plugin-daemonset" "${FAULTS_NODE}"
    faults::wait_for_operand_ready "nvidia-device-plugin-daemonset" "${FAULTS_NODE}"

    faults::forget
    faults::note "NV-DP-001: spec.${OPERAND}.version restored"
}

# faults::plugin_pod
# The device plugin pod on the node and its phase, for the observation lines.
# Empty when the DaemonSet has taken it away, which is itself an observation.
faults::plugin_pod() {
    local pod phase
    pod="$(faults::pod_on_node "${FAULTS_OPERATOR_NAMESPACE}" \
        "app=nvidia-device-plugin-daemonset" "${FAULTS_NODE}")"
    if [ -z "${pod}" ]; then
        printf '<none>'

        return
    fi
    phase="$(k --namespace "${FAULTS_OPERATOR_NAMESPACE}" get pod "${pod}" -o 'jsonpath={.status.phase}')"
    printf '%s (%s)' "${pod}" "${phase}"
}

"${FAULTS_ACTION}"
