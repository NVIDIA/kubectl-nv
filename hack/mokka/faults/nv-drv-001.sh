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
# NV-DRV-001: a driver pod on the node is not ready.
#
# Injection: a driver-shaped pod on the node pointed at an image that cannot be
# pulled, so it stays Pending and never ready. That is the first of the rule's
# two halves; the other, a ready pod that restarted inside ten minutes, needs a
# pod that comes up and dies again on a schedule, which is a fixture that tests
# its own timing rather than the rule.
#
# The labels are the ones pkg/inventory classifies a driver pod by, the same
# pair C1's working stand-in carries. They cannot be guessed from the pod's own
# "app" value alone, because every DaemonSet an NVIDIADriver CR produces puts
# its generated name there.
#
# The deploy label is part of the injection. The lane runs with
# driver.enabled=false, so the operator may be telling every node not to run a
# driver, and the rule reads that as consent: a node told not to run one is not
# a node with a broken one. The prior value is saved and put back.
#
# Usage: hack/mokka/faults/nv-drv-001.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-drv-001.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-DRV-001" "$@"

FIXTURE="driver-standin-failing.yaml"
POD="nv-faults-driver-standin"
DEPLOY_LABEL="nvidia.com/gpu.deploy.driver"

inject() {
    faults::refuse_double_inject

    if [ -n "$(k --namespace "${FAULTS_OPERATOR_NAMESPACE}" get pod "${POD}" \
        -o 'jsonpath={.metadata.name}' 2> /dev/null || true)" ]; then
        faults::die "${FAULTS_RC_ALREADY}" \
            "${POD} is already in ${FAULTS_OPERATOR_NAMESPACE}; recover the earlier fault first"
    fi

    local deploy
    deploy="$(faults::node_label "${FAULTS_NODE}" "${DEPLOY_LABEL}")"
    faults::save "deploy" "${deploy}"
    faults::note "NV-DRV-001: ${DEPLOY_LABEL} was '${deploy:-<absent>}' on ${FAULTS_NODE}"

    # Only when it says the operand is off. Writing it unconditionally would
    # make the recovery write a value the lane never had.
    if faults::deploy_is_off "${deploy}"; then
        faults::set_node_label "${FAULTS_NODE}" "${DEPLOY_LABEL}" "true"
        faults::note "NV-DRV-001: ${DEPLOY_LABEL}=true so the operand counts as wanted"
    fi

    faults::apply_fixture "${FIXTURE}" \
        "__NODE__=${FAULTS_NODE}" \
        "__NAMESPACE__=${FAULTS_OPERATOR_NAMESPACE}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    faults::delete_fixture "${FIXTURE}" \
        "__NODE__=${FAULTS_NODE}" \
        "__NAMESPACE__=${FAULTS_OPERATOR_NAMESPACE}"

    local deploy
    deploy="$(faults::load deploy)"
    faults::set_node_label "${FAULTS_NODE}" "${DEPLOY_LABEL}" "${deploy}"

    faults::forget
    faults::note "NV-DRV-001: ${POD} removed and ${DEPLOY_LABEL} restored"
}

"${FAULTS_ACTION}"
