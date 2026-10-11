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
# NV-OP-001: the ClusterPolicy is not ready.
#
# Injection: point the device plugin at an image version that does not exist.
# The operator rolls the DaemonSet, the new pods never pull, and the operator's
# own verdict on itself leaves "ready". The finding is cluster scoped: it
# carries no node, because the policy is not about one.
#
# The node argument is still required and still checked. Every script in this
# directory takes the same two arguments so that a caller can drive them from a
# table, and the check is what stops a typo in a node name from being noticed
# only by the test that polls for a finding that never arrives.
#
# This is the same injection as nv-dp-001.sh, and deliberately so: one patch
# produces both findings, and the two scripts are separate because the two
# codes recover independently. Running both at once is refused below.
#
# Usage: hack/mokka/faults/nv-op-001.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-op-001.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-OP-001" "$@"

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
    faults::note "NV-OP-001: spec.${OPERAND}.version was '${before:-<absent>}', state was '$(faults::policy_state)'"

    faults::set_operand_version "${OPERAND}" "${FAULTS_ABSENT_VERSION}"
    faults::note "NV-OP-001: spec.${OPERAND}.version=${FAULTS_ABSENT_VERSION}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    faults::set_operand_version "${OPERAND}" "$(faults::load version)"
    faults::forget
    faults::note "NV-OP-001: spec.${OPERAND}.version restored, state is '$(faults::policy_state)'"
}

# faults::policy_state
# The ClusterPolicy's own verdict, for the observation lines.
faults::policy_state() {
    local state
    state="$(faults::policy_field '.status.state')"
    printf '%s' "${state:-<absent>}"
}

"${FAULTS_ACTION}"
