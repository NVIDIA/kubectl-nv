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
# NV-NODE-003: the node has fewer allocatable GPUs than its capacity.
#
# Injection: fail one simulated device through nvml-mock-ctl in the nvml-mock
# pod on that node. The device plugin's health check sees the device go
# unhealthy and stops advertising it, and the kubelet's device manager keeps
# unhealthy devices in capacity while taking them out of allocatable. That gap
# is the finding, and it is the one shape this lane can produce without
# touching the kubelet's own reservations.
#
# The flags are the ones docs/nvml-mock-ctl.md documents in k8s-test-infra
# v0.4.0, the chart version hack/mokka/lib.sh pins. Recovery is the same
# command with --mode healthy, which is why there is no saved value here: the
# healthy mode is not a value read off the lane, it is the mode's own name. The
# state file still exists, because it is what makes recover idempotent.
#
# Usage: hack/mokka/faults/nv-node-003.sh inject|recover <node>

set -euo pipefail
umask 077

FAULTS_PROG="nv-node-003.sh"
FAULTS_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${FAULTS_SCRIPT_DIR}/lib.sh"

faults::init "NV-NODE-003" "$@"

# The device to fail. Zero rather than "all of them": the finding is about the
# gap between capacity and allocatable, and failing every device would close
# the gap the other way and produce NV-NODE-002 instead.
GPU_INDEX="0"

inject() {
    faults::refuse_double_inject

    local pod
    pod="$(faults::mock_pod)"
    faults::save "pod" "${pod}"

    faults::note "NV-NODE-003: capacity/allocatable before: $(faults::counts), nvml-mock pod ${pod}"

    faults::mock_ctl "${pod}" ecc_uncorrectable
    faults::note "NV-NODE-003: GPU ${GPU_INDEX} failed on ${FAULTS_NODE}"

    # The failure alone changes nothing. The device plugin opens its NVML event
    # set once, at startup, so a failure injected under a running plugin is
    # never read: six minutes of polling on the first live run showed 4/4
    # throughout. Restarted after the injection, the same plugin logs
    # "XidCriticalError: Xid=79 ... marking device as unhealthy" and the node
    # goes to 4/3 within ten seconds.
    faults::restart_operand "nvidia-device-plugin-daemonset" "${FAULTS_NODE}"
}

recover() {
    if faults::nothing_to_recover; then
        return 0
    fi

    # The pod is looked up again rather than taken from the saved state: the
    # nvml-mock DaemonSet may have replaced its pod between inject and recover,
    # and exec'ing into a pod that no longer exists would leave the device
    # failed with the state file deleted.
    local pod
    pod="$(faults::mock_pod)"

    faults::mock_ctl "${pod}" healthy
    # Restarted for the same reason as the injection: the plugin reads the
    # device's state when it opens its event set, so the device is healthy
    # again only from the next plugin's point of view.
    faults::restart_operand "nvidia-device-plugin-daemonset" "${FAULTS_NODE}"
    faults::wait_for_operand_ready "nvidia-device-plugin-daemonset" "${FAULTS_NODE}"
    faults::forget
    faults::note "NV-NODE-003: GPU ${GPU_INDEX} healthy again on ${FAULTS_NODE}, counts $(faults::counts)"
}

# faults::mock_pod
# The nvml-mock pod on this node, or a usage error naming the lane assumption
# that did not hold.
faults::mock_pod() {
    local pod
    pod="$(faults::pod_on_node "${FAULTS_MOKKA_NAMESPACE}" "${FAULTS_MOKKA_SELECTOR}" "${FAULTS_NODE}")"
    if [ -z "${pod}" ]; then
        faults::die "${FAULTS_RC_USAGE}" \
            "no ${FAULTS_MOKKA_SELECTOR} pod on ${FAULTS_NODE} in namespace ${FAULTS_MOKKA_NAMESPACE}"
    fi
    printf '%s' "${pod}"
}

# faults::mock_ctl <pod> <mode>
# One nvml-mock-ctl call against one device. The xid accompanies the failure
# mode and is ignored by the healthy mode, so it is passed only with the
# failure.
faults::mock_ctl() {
    local pod="$1" mode="$2"
    if [ "${mode}" = "healthy" ]; then
        k --namespace "${FAULTS_MOKKA_NAMESPACE}" exec "${pod}" -- \
            nvml-mock-ctl fail --gpu "${GPU_INDEX}" --mode healthy

        return
    fi
    k --namespace "${FAULTS_MOKKA_NAMESPACE}" exec "${pod}" -- \
        nvml-mock-ctl fail --gpu "${GPU_INDEX}" --mode "${mode}" --xid 79
}

# faults::counts
# capacity/allocatable, for the observation lines.
faults::counts() {
    local capacity allocatable
    capacity="$(k get node "${FAULTS_NODE}" -o 'jsonpath={.status.capacity.nvidia\.com/gpu}')"
    allocatable="$(k get node "${FAULTS_NODE}" -o 'jsonpath={.status.allocatable.nvidia\.com/gpu}')"
    printf '%s/%s' "${capacity:-<absent>}" "${allocatable:-<absent>}"
}

"${FAULTS_ACTION}"
