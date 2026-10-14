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
# NV-CD-001: a ComputeDomain covering the node is not serving it.
#
# Injection, in four parts, because the lane runs no DRA driver and so has
# neither the API nor anything to put in it:
#
#   1. The ComputeDomain CRD, vendored at a pinned release in
#      test/e2e/testdata/faults with its provenance.
#   2. One ComputeDomain.
#   3. Its status, patched through the status subresource, naming the node with
#      a NotReady entry. An apply never writes a subresource, so this is a
#      patch and not part of the manifest.
#   4. An IMEX daemon pod on the node that never becomes ready.
#
# The node also carries no clique, so the rule reports its second finding too,
# the one about a node no domain recorded a clique for. Both are this code, at
# info, and both clear together.
#
# Usage: hack/mokka/faults/nv-cd-001.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-cd-001.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-CD-001" "$@"

CRD_FIXTURE="computedomains-crd.yaml"
DOMAIN_FIXTURE="compute-domain.yaml"
IMEX_FIXTURE="imex-daemon.yaml"

CRD="computedomains.resource.nvidia.com"
DOMAIN="nv-faults-compute-domain"

# How long to wait for the API server to start serving the new kind. A CRD is
# not usable the moment it is applied: the apiextensions controller has to
# establish it first, and a create against an unestablished kind is refused.
FAULTS_CRD_TIMEOUT="${FAULTS_CRD_TIMEOUT:-120}"
FAULTS_CRD_POLL="${FAULTS_CRD_POLL:-2}"

inject() {
    faults::refuse_double_inject

    if [ -n "$(k get crd "${CRD}" -o 'jsonpath={.metadata.name}' 2> /dev/null || true)" ]; then
        faults::die "${FAULTS_RC_ALREADY}" \
            "${CRD} already exists; this lane runs no DRA driver, so recover the earlier fault first"
    fi

    faults::save "domain" "${DOMAIN}"

    faults::apply_fixture "${CRD_FIXTURE}"
    faults::wait_for_crd

    faults::apply_fixture "${DOMAIN_FIXTURE}" "__NAMESPACE__=${FAULTS_OPERATOR_NAMESPACE}"

    # The node entry carries every field the CRD requires. cliqueID is required
    # and empty on purpose: a domain that recorded no clique for the node is
    # the rule's second finding, and the lane has no NVLink partition to name.
    k --namespace "${FAULTS_OPERATOR_NAMESPACE}" patch computedomain "${DOMAIN}" \
        --subresource=status --type=merge \
        -p "{\"status\":{\"status\":\"NotReady\",\"nodes\":[{\"name\":\"${FAULTS_NODE}\",\"cliqueID\":\"\",\"ipAddress\":\"192.0.2.1\",\"status\":\"NotReady\"}]}}" > /dev/null
    faults::note "NV-CD-001: ${DOMAIN} status names ${FAULTS_NODE} as NotReady"

    faults::apply_fixture "${IMEX_FIXTURE}" \
        "__NODE__=${FAULTS_NODE}" \
        "__NAMESPACE__=${FAULTS_OPERATOR_NAMESPACE}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    faults::delete_fixture "${IMEX_FIXTURE}" \
        "__NODE__=${FAULTS_NODE}" \
        "__NAMESPACE__=${FAULTS_OPERATOR_NAMESPACE}"

    # The CRD takes its objects with it, so the domain is deleted first only so
    # that a lane left half recovered has no object of a kind nothing serves.
    faults::delete_fixture "${DOMAIN_FIXTURE}" "__NAMESPACE__=${FAULTS_OPERATOR_NAMESPACE}"
    faults::delete_fixture "${CRD_FIXTURE}"

    faults::forget
    faults::note "NV-CD-001: the ComputeDomain, its CRD and the IMEX pod are gone"
}

# faults::wait_for_crd
# Waits until the API server reports the kind established.
faults::wait_for_crd() {
    local deadline established
    deadline=$(($(date +%s) + FAULTS_CRD_TIMEOUT))
    while :; do
        established="$(k get crd "${CRD}" \
            -o 'jsonpath={.status.conditions[?(@.type=="Established")].status}' 2> /dev/null || true)"
        if [ "${established}" = "True" ]; then
            faults::note "NV-CD-001: ${CRD} is established"

            return 0
        fi
        if [ "$(date +%s)" -ge "${deadline}" ]; then
            faults::die 1 "${CRD} was not established within ${FAULTS_CRD_TIMEOUT}s"
        fi
        sleep "${FAULTS_CRD_POLL}"
    done
}

"${FAULTS_ACTION}"
