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
# Injection: cordon the node. Of the rule's two halves this is the one a lane
# can produce honestly: the other reads a node condition NVSentinel writes, and
# NVSentinel is not installed here, so reproducing it would mean writing a
# condition in its name and then checking that the rule reads what this script
# wrote. E2's unit tests cover that half against a table.
#
# Cordoning adds the node.kubernetes.io/unschedulable taint, which every
# operand DaemonSet tolerates by default, so the operands stay where they are
# and no other rule changes its mind. The node keeps its GPUs and its labels;
# what changes is that nothing new is placed on it.
#
# Usage: hack/mokka/faults/nv-node-005.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-node-005.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-NODE-005" "$@"

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

    k uncordon "${FAULTS_NODE}" > /dev/null
    faults::forget
    faults::note "NV-NODE-005: ${FAULTS_NODE} uncordoned"
}

"${FAULTS_ACTION}"
