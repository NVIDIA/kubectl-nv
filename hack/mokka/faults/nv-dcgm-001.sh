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
# NV-DCGM-001: the dcgm-exporter pod on the node is not ready.
#
# Injection: point the exporter at an image version that does not exist, the
# same shape nv-dp-001.sh uses on the device plugin. It goes through the
# ClusterPolicy rather than through the DaemonSet, because the operator
# reconciles the DaemonSets it owns: a node selector patched onto
# nvidia-dcgm-exporter directly is written back by the next reconcile, and an
# injection the operator undoes is a test that passes or fails on timing.
#
# The rule is stricter about firing than NV-DP-001 is: it needs the
# ClusterPolicy to enable the exporter and the node's own
# nvidia.com/gpu.deploy.dcgm-exporter label to say the operand belongs there.
# Patching the version leaves both saying yes, which is the point.
#
# Usage: hack/mokka/faults/nv-dcgm-001.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-dcgm-001.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-DCGM-001" "$@"

OPERAND="dcgmExporter"

inject() {
    faults::refuse_double_inject

    local before
    before="$(faults::policy_field ".spec.${OPERAND}.version")"

    if [ "${before}" = "${FAULTS_ABSENT_VERSION}" ]; then
        faults::die "${FAULTS_RC_ALREADY}" \
            "spec.${OPERAND}.version is already ${FAULTS_ABSENT_VERSION}; recover the other fault first"
    fi

    faults::save "version" "${before}"
    faults::note "NV-DCGM-001: spec.${OPERAND}.version was '${before:-<absent>}', exporter pod on ${FAULTS_NODE}: $(faults::exporter_pod)"

    faults::set_operand_version "${OPERAND}" "${FAULTS_ABSENT_VERSION}"
    faults::note "NV-DCGM-001: spec.${OPERAND}.version=${FAULTS_ABSENT_VERSION}"

    # The patch alone breaks whichever node the rolling update reaches first,
    # and stops there: the first replacement never becomes ready, so the
    # update never moves on. On the first live run that was worker2 while the
    # node under test kept its healthy pod for the whole six minutes. Deleting
    # the pod on the node under test is what puts the fault where this script
    # was asked to put it, because a node with no pod gets one from the
    # template as it stands now rather than from the rollout's position.
    faults::wait_for_template "nvidia-dcgm-exporter" present "${FAULTS_ABSENT_VERSION}"
    faults::restart_operand "nvidia-dcgm-exporter" "${FAULTS_NODE}"
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
    faults::wait_for_template "nvidia-dcgm-exporter" absent "${FAULTS_ABSENT_VERSION}"
    faults::restart_operand "nvidia-dcgm-exporter" "${FAULTS_NODE}"
    faults::wait_for_operand_ready "nvidia-dcgm-exporter" "${FAULTS_NODE}"

    faults::forget
    faults::note "NV-DCGM-001: spec.${OPERAND}.version restored"
}

# faults::exporter_pod
# The exporter pod on the node and its phase, for the observation lines.
faults::exporter_pod() {
    local pod phase
    pod="$(faults::pod_on_node "${FAULTS_OPERATOR_NAMESPACE}" \
        "app=nvidia-dcgm-exporter" "${FAULTS_NODE}")"
    if [ -z "${pod}" ]; then
        printf '<none>'

        return
    fi
    phase="$(k --namespace "${FAULTS_OPERATOR_NAMESPACE}" get pod "${pod}" -o 'jsonpath={.status.phase}')"
    printf '%s (%s)' "${pod}" "${phase}"
}

"${FAULTS_ACTION}"
