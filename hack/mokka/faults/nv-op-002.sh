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
# NV-OP-002: the operator is not running an operand on the node.
#
# Injection: set the dcgm-exporter deploy label to false on the node. The
# finding is reported at info, so a run at the default --fail-on error exits 0
# with it in the report; `kubectl nv health --fail-on info` is what turns it
# into a non-zero exit.
#
# The exporter and not the device plugin, because switching the device plugin
# off takes the node's allocatable GPUs with it and produces NV-NODE-002 as
# well. The exporter is the operand whose absence is only this finding.
#
# Switching it off also suppresses NV-DCGM-001 by design: that rule asks
# whether an operand that should be running is, and this label is the operator
# being told it should not.
#
# Usage: hack/mokka/faults/nv-op-002.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-op-002.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-OP-002" "$@"

LABEL="nvidia.com/gpu.deploy.dcgm-exporter"

inject() {
    faults::refuse_double_inject

    local before
    before="$(faults::node_label "${FAULTS_NODE}" "${LABEL}")"

    # Already off, so there is nothing for this injection to turn off and the
    # saved value would be the broken one. See nv-node-002.sh for the shape.
    if faults::deploy_is_off "${before}"; then
        faults::die "${FAULTS_RC_ALREADY}" \
            "${LABEL} on ${FAULTS_NODE} is already '${before}'; the operand is not running to be stopped"
    fi

    faults::save "label" "${before}"

    faults::note "NV-OP-002: ${LABEL} was '${before:-<absent>}' on ${FAULTS_NODE}"

    faults::set_node_label "${FAULTS_NODE}" "${LABEL}" "false"
    faults::note "NV-OP-002: ${LABEL}=false on ${FAULTS_NODE}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    local saved
    saved="$(faults::load label)"
    faults::set_node_label "${FAULTS_NODE}" "${LABEL}" "${saved}"
    faults::wait_for_operand_ready "nvidia-dcgm-exporter" "${FAULTS_NODE}"
    faults::forget
    faults::note "NV-OP-002: ${LABEL} restored on ${FAULTS_NODE}"
}

"${FAULTS_ACTION}"
