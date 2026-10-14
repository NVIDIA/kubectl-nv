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
# NV-CTK-001: the container toolkit validation failed on the node.
#
# Injection: a validator-shaped pod on the node whose toolkit-validation init
# container exits 1, under a restart policy of Always, which is the shape the
# rule is written for: a failing init container does not sit terminated, it is
# Waiting with reason CrashLoopBackOff and the exit code of the run that failed
# is in its last termination.
#
# A fixture and not the real validator. The lane disables the container toolkit
# in its values, so there is nothing to break through the ClusterPolicy, and
# the validator the operator runs has no toolkit verdict to spoil.
#
# The image is borrowed off the lane so that nothing is pulled, and it is the
# DEVICE PLUGIN's rather than the validator's. The validator runs the operator
# image, which carries no shell: a container whose command cannot be executed
# fails with RunContainerError and a last termination whose reason is
# StartError, and the rule reads a last termination only under
# CrashLoopBackOff, so that injection produced no finding at all. Measured on a
# t4 lane on 2026-10-12. The device plugin image has /bin/sh and exits with the
# code it is given.
#
# The rule reads the FIRST validator pod on the node, pkg/inventory sorting a
# node's operands by name, and the fixture is named to sort before the real
# one. The test pins the pod name in the evidence, so the day that stops being
# true is a failure rather than a pass against the wrong pod.
#
# Usage: hack/mokka/faults/nv-ctk-001.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-ctk-001.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-CTK-001" "$@"

FIXTURE="ctk-validator.yaml"
POD="nv-faults-ctk-validator"

inject() {
    faults::refuse_double_inject

    if [ -n "$(k --namespace "${FAULTS_OPERATOR_NAMESPACE}" get pod "${POD}" \
        -o 'jsonpath={.metadata.name}' 2> /dev/null || true)" ]; then
        faults::die "${FAULTS_RC_ALREADY}" \
            "${POD} is already in ${FAULTS_OPERATOR_NAMESPACE}; recover the earlier fault first"
    fi

    local image
    image="$(faults::operand_image "nvidia-device-plugin-daemonset" "${FAULTS_NODE}")"
    if [ -z "${image}" ]; then
        faults::die "${FAULTS_RC_USAGE}" \
            "no nvidia-device-plugin-daemonset pod on ${FAULTS_NODE} to borrow an image from"
    fi

    faults::save "image" "${image}"
    faults::note "NV-CTK-001: standing in for the validator, running ${image}"

    faults::apply_fixture "${FIXTURE}" \
        "__NODE__=${FAULTS_NODE}" \
        "__NAMESPACE__=${FAULTS_OPERATOR_NAMESPACE}" \
        "__IMAGE__=${image}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    local image
    image="$(faults::load image)"
    faults::delete_fixture "${FIXTURE}" \
        "__NODE__=${FAULTS_NODE}" \
        "__NAMESPACE__=${FAULTS_OPERATOR_NAMESPACE}" \
        "__IMAGE__=${image}"

    faults::forget
    faults::note "NV-CTK-001: ${POD} removed from ${FAULTS_OPERATOR_NAMESPACE}"
}

"${FAULTS_ACTION}"
