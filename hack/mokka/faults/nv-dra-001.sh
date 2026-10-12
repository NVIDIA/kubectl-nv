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
# NV-DRA-001: the node advertises its GPUs twice over.
#
# Injection: publish a gpu.nvidia.com ResourceSlice for a node that already
# advertises nvidia.com/gpu through the device plugin. The node is then in the
# "both" mode the rule is about, which is what a cluster part way through a
# migration to dynamic resource allocation looks like: the same devices
# counted in two schedulers.
#
# Nothing owns the slice. Its real publisher is a DRA driver's kubelet plugin,
# which this lane does not run, so it stays exactly as applied until the
# recovery deletes it. A ResourceSlice is cluster scoped, which is why the
# fixture carries no namespace.
#
# Usage: hack/mokka/faults/nv-dra-001.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-dra-001.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-DRA-001" "$@"

FIXTURE="resourceslice.yaml"
SLICE="nv-faults-dra-slice"

inject() {
    faults::refuse_double_inject

    if [ -n "$(k get resourceslice "${SLICE}" -o 'jsonpath={.metadata.name}' 2> /dev/null || true)" ]; then
        faults::die "${FAULTS_RC_ALREADY}" \
            "resourceslice ${SLICE} already exists; recover the earlier fault first"
    fi

    faults::save "slice" "${SLICE}"
    faults::note "NV-DRA-001: capacity on ${FAULTS_NODE} is $(faults::capacity)"

    faults::apply_fixture "${FIXTURE}" "__NODE__=${FAULTS_NODE}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    faults::delete_fixture "${FIXTURE}" "__NODE__=${FAULTS_NODE}"
    faults::forget
    faults::note "NV-DRA-001: resourceslice ${SLICE} removed"
}

# faults::capacity
# The node's whole-GPU capacity, which is the number the slice is published
# alongside, for the observation line.
faults::capacity() {
    local value
    value="$(k get node "${FAULTS_NODE}" -o 'jsonpath={.status.capacity.nvidia\.com/gpu}')"
    printf '%s' "${value:-<absent>}"
}

"${FAULTS_ACTION}"
