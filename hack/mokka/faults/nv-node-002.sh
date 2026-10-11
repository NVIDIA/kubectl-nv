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
# NV-NODE-002: the node reports GPUs present and advertises none the scheduler
# can place.
#
# Injection: switch the device plugin off for the node with its own deploy
# label. The operator drains the plugin pod from the node, the kubelet retires
# the extended resource, and allocatable nvidia.com/gpu goes to zero while
# GPU feature discovery leaves nvidia.com/gpu.present=true in place. That pair,
# the hint label with nothing placeable behind it, is the finding.
#
# Only the one deploy label changes, which matters twice over: the node is
# still one the GPU Operator targets, so the rule still evaluates it, and
# NV-DP-001 is suppressed on purpose, so the run reports this code rather than
# two codes about the same pod.
#
# The same change also produces NV-OP-002 at info, which is the rule saying
# the operand was switched off rather than broken. That is the honest reading
# of this lane and not something to hide.
#
# Usage: hack/mokka/faults/nv-node-002.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-node-002.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-NODE-002" "$@"

LABEL="nvidia.com/gpu.deploy.device-plugin"

inject() {
    faults::refuse_double_inject

    local before
    before="$(faults::node_label "${FAULTS_NODE}" "${LABEL}")"
    faults::save "label" "${before}"

    faults::note "NV-NODE-002: ${LABEL} was '${before:-<absent>}' on ${FAULTS_NODE}"
    faults::note "NV-NODE-002: allocatable before: $(faults::allocatable)"

    faults::set_node_label "${FAULTS_NODE}" "${LABEL}" "false"
    faults::note "NV-NODE-002: ${LABEL}=false on ${FAULTS_NODE}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    faults::set_node_label "${FAULTS_NODE}" "${LABEL}" "$(faults::load label)"
    faults::forget
    faults::note "NV-NODE-002: ${LABEL} restored on ${FAULTS_NODE}"
}

# faults::allocatable
# The node's allocatable whole GPUs, for the observation line. Empty means the
# resource is not on the node at all, which is a different state from zero and
# is printed as such.
faults::allocatable() {
    local value
    value="$(k get node "${FAULTS_NODE}" -o 'jsonpath={.status.allocatable.nvidia\.com/gpu}')"
    printf '%s' "${value:-<absent>}"
}

"${FAULTS_ACTION}"
