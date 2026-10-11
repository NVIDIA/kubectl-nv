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
# NV-NODE-001: the GPU Operator is not reconciling this node.
#
# The rule fires on a node that carries node feature discovery's
# feature.node.kubernetes.io/pci-10de.present and not the operator's own
# nvidia.com/gpu.present, AND only where the operator is installed. That last
# condition is what makes this the longest script in the directory, because the
# two obvious ways to clear the label fight each other:
#
#   - Remove the label with the operator running, and the ClusterPolicy
#     controller writes it straight back. The window is a race, not a fault.
#   - Scale the operator to zero so the label stays off, and `kubectl nv
#     health` sees no pod matching app=gpu-operator, reads the operator as not
#     installed, and the rule returns nothing. The catalog entry for this code
#     described exactly that sequence before this script was run against a
#     lane; it is corrected there now.
#
# So the injection does both halves and then supplies the third: the operator
# Deployment is scaled to zero, and a stand-in pod carrying the operator's own
# app=gpu-operator label and its own image is planted in the operator
# namespace. The command reads "is the operator installed" as "is there a pod
# with that label", which is the only thing it can read, so the stand-in makes
# the answer yes while nothing is reconciling anything. That is the fault this
# code is about, reproduced rather than simulated: an operator that is present
# and is not claiming the node.
#
# Usage: hack/mokka/faults/nv-node-001.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-node-001.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-NODE-001" "$@"

LABEL="nvidia.com/gpu.present"
DEPLOYMENT="gpu-operator"
STANDIN="nv-faults-operator-standin"

# How long to wait for the operator's own pods to go away after the scale, and
# how often to look. The scale is what stops the label being written back, so
# removing the label before the pods are gone is the race this avoids.
DRAIN_TIMEOUT="${FAULTS_DRAIN_TIMEOUT:-120}"
DRAIN_POLL="${FAULTS_DRAIN_POLL:-2}"

inject() {
    faults::refuse_double_inject

    local replicas image before
    replicas="$(k --namespace "${FAULTS_OPERATOR_NAMESPACE}" get deploy "${DEPLOYMENT}" \
        -o 'jsonpath={.spec.replicas}')"
    image="$(k --namespace "${FAULTS_OPERATOR_NAMESPACE}" get deploy "${DEPLOYMENT}" \
        -o 'jsonpath={.spec.template.spec.containers[0].image}')"
    before="$(faults::node_label "${FAULTS_NODE}" "${LABEL}")"

    if [ -z "${replicas}" ] || [ -z "${image}" ]; then
        faults::die "${FAULTS_RC_USAGE}" \
            "cannot read deploy/${DEPLOYMENT} in ${FAULTS_OPERATOR_NAMESPACE}"
    fi

    faults::save "replicas" "${replicas}"
    faults::save "label" "${before}"
    faults::note "NV-NODE-001: deploy/${DEPLOYMENT} at ${replicas} replicas, ${LABEL}='${before:-<absent>}' on ${FAULTS_NODE}"

    k --namespace "${FAULTS_OPERATOR_NAMESPACE}" scale "deploy/${DEPLOYMENT}" --replicas=0 > /dev/null
    faults::drain_operator

    faults::plant_standin "${image}"

    faults::set_node_label "${FAULTS_NODE}" "${LABEL}" "${FAULTS_ABSENT_SENTINEL}"
    faults::note "NV-NODE-001: ${LABEL} removed from ${FAULTS_NODE}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    # The label goes back first and the operator comes back last. The other
    # order leaves a window in which the operator is running and the label is
    # absent, which is the fault still being injected while the script claims
    # to have cleared it.
    faults::set_node_label "${FAULTS_NODE}" "${LABEL}" "$(faults::load label)"

    k --namespace "${FAULTS_OPERATOR_NAMESPACE}" delete pod "${STANDIN}" \
        --ignore-not-found --wait=false > /dev/null

    k --namespace "${FAULTS_OPERATOR_NAMESPACE}" scale "deploy/${DEPLOYMENT}" \
        --replicas="$(faults::load replicas)" > /dev/null

    faults::forget
    faults::note "NV-NODE-001: ${LABEL} restored and deploy/${DEPLOYMENT} scaled back"
}

# faults::drain_operator
# Waits until no pod matches the operator selector. Bounded, and a timeout is a
# failure rather than a warning: with a real operator pod still running, the
# label is written back and the injection silently does nothing.
faults::drain_operator() {
    local deadline remaining
    deadline=$(($(date +%s) + DRAIN_TIMEOUT))
    while :; do
        remaining="$(k --namespace "${FAULTS_OPERATOR_NAMESPACE}" get pod \
            -l "${FAULTS_OPERATOR_SELECTOR}" -o 'jsonpath={.items[*].metadata.name}')"
        if [ -z "${remaining}" ]; then
            faults::note "NV-NODE-001: deploy/${DEPLOYMENT} drained"

            return 0
        fi
        if [ "$(date +%s)" -ge "${deadline}" ]; then
            faults::die 1 "deploy/${DEPLOYMENT} still has pods after ${DRAIN_TIMEOUT}s: ${remaining}"
        fi
        sleep "${DRAIN_POLL}"
    done
}

# faults::plant_standin <image>
# Creates the pod that keeps the operator "installed" while nothing reconciles.
#
# The operator's own image, because it is already on every node of the lane and
# needs no pull. What the container does is beside the point: the command reads
# the pod's existence, so a stand-in that crash-loops carries the same meaning
# as one that sleeps, and both are the truthful shape of this fault.
faults::plant_standin() {
    local image="$1"

    k --namespace "${FAULTS_OPERATOR_NAMESPACE}" apply -f - > /dev/null <<POD
apiVersion: v1
kind: Pod
metadata:
  name: ${STANDIN}
  namespace: ${FAULTS_OPERATOR_NAMESPACE}
  labels:
    app: ${DEPLOYMENT}
    nv.example/faults-fixture: "nv-node-001"
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: standin
      image: ${image}
      imagePullPolicy: IfNotPresent
      command: ["/bin/sh", "-c", "sleep 86400"]
POD

    faults::note "NV-NODE-001: stand-in pod ${STANDIN} planted from ${image}"
}

"${FAULTS_ACTION}"
