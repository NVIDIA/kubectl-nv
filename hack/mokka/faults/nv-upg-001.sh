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
# NV-UPG-001: a driver upgrade failed or is stuck on the node.
#
# Injection: set the upgrade state label to upgrade-failed. That value is the
# one the rule reports at ERROR, and it needs nothing dated: the stuck states
# are reported against --upgrade-stuck-after and an event that says when the
# node entered them, and a lane that runs no upgrade controller writes no such
# event. upgrade-failed is the controller saying it gave up, which is a verdict
# rather than a duration.
#
# The label also makes the node mid-upgrade for every other rule, which is the
# point of it: NV-DRV-001 lowers itself to info while an upgrade is in
# progress. Nothing else is injected at the same time, so that is not visible
# here, and it is why this script refuses a node that is already mid-upgrade.
#
# Usage: hack/mokka/faults/nv-upg-001.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-upg-001.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-UPG-001" "$@"

LABEL="nvidia.com/gpu-driver-upgrade-state"
FAILED="upgrade-failed"

inject() {
    faults::refuse_double_inject

    local before
    before="$(faults::node_label "${FAULTS_NODE}" "${LABEL}")"

    # Already mid-upgrade. Saving that state as the one to restore would leave
    # the node looking upgraded-by-somebody after the recovery, and every rule
    # that lowers itself mid-upgrade would stay lowered.
    if [ -n "${before}" ] && [ "${before}" != "upgrade-done" ]; then
        faults::die "${FAULTS_RC_ALREADY}" \
            "${LABEL} on ${FAULTS_NODE} is already '${before}'; the node is mid-upgrade"
    fi

    faults::save "label" "${before}"
    faults::note "NV-UPG-001: ${LABEL} was '${before:-<absent>}' on ${FAULTS_NODE}"

    faults::set_node_label "${FAULTS_NODE}" "${LABEL}" "${FAILED}"
    faults::note "NV-UPG-001: ${LABEL}=${FAILED} on ${FAULTS_NODE}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    local saved
    saved="$(faults::load label)"
    faults::set_node_label "${FAULTS_NODE}" "${LABEL}" "${saved}"
    faults::forget
    faults::note "NV-UPG-001: ${LABEL} restored on ${FAULTS_NODE}"
}

"${FAULTS_ACTION}"
