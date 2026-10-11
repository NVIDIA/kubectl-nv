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
# NV-NODE-004: the nvidia.com/gpu.count label and the node's GPU capacity
# disagree.
#
# Injection: overwrite the count label with one more than it holds. The device
# plugin keeps advertising the real number to the kubelet, so the two NVIDIA
# components that count the same devices now disagree, which is the finding.
#
# One more and not a fixed number: the count is the profile's device count, and
# a script that wrote 7 would be a script that injected nothing on a profile
# with seven devices per node.
#
# The label is written by GPU feature discovery through node feature discovery,
# so whether, and how quickly, it is written back is a property of that
# pipeline rather than of this script. The observation lines below record the
# label at inject time and at recover time; the measured interval is in
# NOTES.md and in the catalog entry.
#
# Usage: hack/mokka/faults/nv-node-004.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-node-004.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-NODE-004" "$@"

LABEL="nvidia.com/gpu.count"

inject() {
    faults::refuse_double_inject

    local before
    before="$(faults::node_label "${FAULTS_NODE}" "${LABEL}")"

    # A node with no count label has nothing for the capacity to disagree with:
    # that absence is NV-NODE-001's finding, and injecting here would report
    # this code for the wrong reason.
    if ! [[ ${before} =~ ^[0-9]+$ ]]; then
        faults::die "${FAULTS_RC_USAGE}" \
            "${FAULTS_NODE} has no numeric ${LABEL} label (got '${before:-<absent>}'); nothing to disagree with"
    fi

    faults::save "label" "${before}"

    local wrong=$((before + 1))
    faults::set_node_label "${FAULTS_NODE}" "${LABEL}" "${wrong}"
    faults::note "NV-NODE-004: ${LABEL} ${before} -> ${wrong} on ${FAULTS_NODE}, capacity $(faults::capacity)"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    local saved now
    saved="$(faults::load label)"
    now="$(faults::node_label "${FAULTS_NODE}" "${LABEL}")"
    faults::note "NV-NODE-004: ${LABEL} is '${now:-<absent>}' at recover time, saved value is '${saved}'"

    faults::set_node_label "${FAULTS_NODE}" "${LABEL}" "${saved}"
    faults::forget
    faults::note "NV-NODE-004: ${LABEL} restored on ${FAULTS_NODE}"
}

# faults::capacity
# The node's whole-GPU capacity, which is the number the label is being made to
# disagree with.
faults::capacity() {
    local value
    value="$(k get node "${FAULTS_NODE}" -o 'jsonpath={.status.capacity.nvidia\.com/gpu}')"
    printf '%s' "${value:-<absent>}"
}

"${FAULTS_ACTION}"
