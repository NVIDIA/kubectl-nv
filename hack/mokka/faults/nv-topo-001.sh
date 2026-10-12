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
# NV-TOPO-001: the node is missing Topograph tiers its peers carry.
#
# Injection: put a fabric tier on every OTHER worker and leave the named node
# without one. The finding is about the node that is missing the tier, so the
# node this script is given is the node that ends up reported, and the labels
# go somewhere else. That is the opposite of every other script here and it is
# what the rule means: a tier is "in use" because the rest of the cluster
# carries it.
#
# tier-0 and not a higher one. The tiers are a hierarchy with 0 nearest the
# node, so a cluster that uses any of them uses that one, and a reproduction
# that picked tier-3 would be reproducing a fabric nobody runs.
#
# Only the fabric tiers count. The rule ignores accelerator.topograph.run
# domains on purpose, so labelling one would inject nothing.
#
# Usage: hack/mokka/faults/nv-topo-001.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-topo-001.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-TOPO-001" "$@"

TIER_LABEL="fabric.topograph.run/tier-0"
TIER_VALUE="nv-faults-block"

inject() {
    faults::refuse_double_inject

    local peers peer before saved_any=false
    peers="$(faults::other_workers "${FAULTS_NODE}")"
    if [ -z "${peers}" ]; then
        faults::die "${FAULTS_RC_USAGE}" \
            "${FAULTS_NODE} is the only worker; there are no peers to carry a tier it is missing"
    fi

    # The node under test must not already carry the tier, or it is not the
    # one missing anything.
    before="$(faults::node_label "${FAULTS_NODE}" "${TIER_LABEL}")"
    if [ -n "${before}" ]; then
        faults::die "${FAULTS_RC_ALREADY}" \
            "${FAULTS_NODE} already carries ${TIER_LABEL}='${before}'; it would be missing nothing"
    fi

    while IFS= read -r peer; do
        [ -n "${peer}" ] || continue
        before="$(faults::node_label "${peer}" "${TIER_LABEL}")"
        faults::save "peer-${peer}" "${before}"
        saved_any=true
        faults::set_node_label "${peer}" "${TIER_LABEL}" "${TIER_VALUE}"
        faults::note "NV-TOPO-001: ${TIER_LABEL}=${TIER_VALUE} on peer ${peer}"
    done <<< "${peers}"

    if [ "${saved_any}" != true ]; then
        faults::die 1 "no peer was labelled; ${FAULTS_NODE} would be missing nothing"
    fi
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    # The peers are read back out of the saved state rather than from the lane:
    # a node added since the injection never carried the label, and putting an
    # empty value on it would be this script inventing a change.
    local file peer saved
    for file in "${FAULTS_STATE}"/peer-*; do
        [ -f "${file}" ] || continue
        peer="$(basename "${file}")"
        peer="${peer#peer-}"
        saved="$(faults::load "peer-${peer}")"
        faults::set_node_label "${peer}" "${TIER_LABEL}" "${saved}"
        faults::note "NV-TOPO-001: ${TIER_LABEL} restored on peer ${peer}"
    done

    faults::forget
}

"${FAULTS_ACTION}"
