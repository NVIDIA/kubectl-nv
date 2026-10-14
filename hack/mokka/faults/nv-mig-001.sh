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
# NV-MIG-001: the node's MIG configuration has not been applied.
#
# Injection: ask for a MIG profile and say the attempt failed, through the two
# labels the mig-manager owns. The failed state is the one the rule reports at
# ERROR; its own severity, warning, is what a config that is merely pending
# gets.
#
# Nothing rewrites these labels on this lane. hack/mokka/gpu-operator-values
# .yaml disables the mig-manager, which is the component that would otherwise
# notice the config, try to apply it and move the state on; the lane's
# simulated devices are not partitioned and nothing else reads them.
#
# Usage: hack/mokka/faults/nv-mig-001.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-mig-001.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-MIG-001" "$@"

CONFIG_LABEL="nvidia.com/mig.config"
STATE_LABEL="nvidia.com/mig.config.state"

# A profile name, not a profile this lane could apply. The rule compares the
# state against success and failed and never looks at the profile, and a lane
# with no mig-manager has nothing that could act on it either way.
PROFILE="all-1g.10gb"

inject() {
    faults::refuse_double_inject

    local config state
    config="$(faults::node_label "${FAULTS_NODE}" "${CONFIG_LABEL}")"
    state="$(faults::node_label "${FAULTS_NODE}" "${STATE_LABEL}")"

    # A node already asking for a profile is a node somebody else is
    # partitioning, and the saved pair would be theirs.
    if [ -n "${config}" ]; then
        faults::die "${FAULTS_RC_ALREADY}" \
            "${CONFIG_LABEL} on ${FAULTS_NODE} is already '${config}'; a MIG change is already in flight"
    fi

    faults::save "config" "${config}"
    faults::save "state" "${state}"
    faults::note "NV-MIG-001: ${CONFIG_LABEL} was '${config:-<absent>}', ${STATE_LABEL} was '${state:-<absent>}'"

    faults::set_node_label "${FAULTS_NODE}" "${CONFIG_LABEL}" "${PROFILE}"
    faults::set_node_label "${FAULTS_NODE}" "${STATE_LABEL}" "failed"
    faults::note "NV-MIG-001: ${CONFIG_LABEL}=${PROFILE} ${STATE_LABEL}=failed on ${FAULTS_NODE}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    local config state
    config="$(faults::load config)"
    state="$(faults::load state)"

    faults::set_node_label "${FAULTS_NODE}" "${STATE_LABEL}" "${state}"
    faults::set_node_label "${FAULTS_NODE}" "${CONFIG_LABEL}" "${config}"
    faults::forget
    faults::note "NV-MIG-001: both MIG labels restored on ${FAULTS_NODE}"
}

"${FAULTS_ACTION}"
