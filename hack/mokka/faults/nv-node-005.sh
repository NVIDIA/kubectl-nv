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
# NV-NODE-005: an NVSentinel check is failing, or the node is cordoned.
#
# Injection: both halves of the rule. The node is cordoned, and a node
# condition in the shape NVSentinel writes is patched onto its status.
#
# The condition is a strategic merge patch against the status subresource, and
# both words matter. A merge patch replaces the whole conditions list, which
# would take Ready with it and have the kubelet evict the lane's pods before it
# put them back; a strategic merge patch has conditions keyed by type, so it
# adds one and leaves the rest alone. Observed on a t4 lane on 2026-10-12:
# after the patch the node carried GPUXidError, MemoryPressure, DiskPressure,
# PIDPressure and Ready, the new condition was still there two minutes later
# across six readings, and a delete patch took it off again leaving the other
# four.
#
# Cordoning adds the node.kubernetes.io/unschedulable taint, which every
# operand DaemonSet tolerates by default, so the operands stay where they are
# and no other rule changes its mind. The node keeps its GPUs and its labels;
# what changes is that nothing new is placed on it.
#
# Two findings come out of this, both at info and both this code: one for the
# condition and one for the cordon.
#
# Usage: hack/mokka/faults/nv-node-005.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-node-005.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-NODE-005" "$@"

# The condition NVSentinel would write. The rule recognises one by its reason
# being the condition's own type with IsNotHealthy appended, which is the shape
# NVSentinel uses, so the type here decides both strings.
CONDITION="GPUXidError"
MESSAGE="injected by hack/mokka/faults/nv-node-005.sh"

inject() {
    faults::refuse_double_inject

    local before
    before="$(k get node "${FAULTS_NODE}" -o 'jsonpath={.spec.unschedulable}')"

    # Already cordoned, by somebody who meant it. Saving "true" as the value to
    # restore would uncordon their node on recovery.
    if [ "${before}" = "true" ]; then
        faults::die "${FAULTS_RC_ALREADY}" \
            "${FAULTS_NODE} is already cordoned; nothing here put it there"
    fi

    faults::save "unschedulable" "${before}"
    faults::note "NV-NODE-005: ${FAULTS_NODE} unschedulable was '${before:-<absent>}'"

    k cordon "${FAULTS_NODE}" > /dev/null
    faults::note "NV-NODE-005: ${FAULTS_NODE} cordoned"

    local now
    now="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    k patch node "${FAULTS_NODE}" --subresource=status --type=strategic \
        -p "{\"status\":{\"conditions\":[{\"type\":\"${CONDITION}\",\"status\":\"True\",\"reason\":\"${CONDITION}IsNotHealthy\",\"message\":\"${MESSAGE}\",\"lastHeartbeatTime\":\"${now}\",\"lastTransitionTime\":\"${now}\"}]}}" > /dev/null
    faults::note "NV-NODE-005: ${CONDITION} is True on ${FAULTS_NODE}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    # The saved value is read and checked rather than assumed: a node that was
    # cordoned before the injection is refused above, so the only value that
    # can be here is one that means schedulable, and anything else is state
    # from a run that is not this one.
    local saved
    saved="$(faults::load unschedulable)"
    if [ "${saved}" = "true" ]; then
        faults::die 1 "saved state says ${FAULTS_NODE} was already cordoned, which inject refuses; not uncordoning it"
    fi

    # The condition goes first. A node left carrying it after the cordon came
    # off is still reported, and the code is what the recovery is measured by.
    k patch node "${FAULTS_NODE}" --subresource=status --type=strategic \
        -p "{\"status\":{\"conditions\":[{\"type\":\"${CONDITION}\",\"\$patch\":\"delete\"}]}}" > /dev/null
    faults::note "NV-NODE-005: ${CONDITION} removed from ${FAULTS_NODE}"

    k uncordon "${FAULTS_NODE}" > /dev/null
    faults::forget
    faults::note "NV-NODE-005: ${FAULTS_NODE} uncordoned"
}

"${FAULTS_ACTION}"
