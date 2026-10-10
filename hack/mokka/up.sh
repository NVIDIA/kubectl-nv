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
# Brings up one Mokka lane: a kind cluster of simulated GPU nodes with the
# nvml-mock chart and the GPU Operator on top, so kubectl-nv can be exercised
# against a cluster that advertises nvidia.com/gpu without any GPU hardware.
#
# Usage: hack/mokka/up.sh <lane> <profile>
#
#   lane     DNS label; the cluster is named kubectl-nv-<lane> and the
#            kubeconfig context kind-kubectl-nv-<lane>.
#   profile  one of a100 b200 gb200 gb300 h100 l40s t4
#
# Exit codes:
#   0  the lane is up and every assertion passed
#   2  bad usage, or an unknown profile
#   3  a required tool is missing
#   4  the lane cluster already exists, so nothing was touched
#   5  the lane came up but an assertion about it failed
#
# Lanes are independent: this script creates and reads only kubectl-nv-<lane>,
# and every kubectl and helm call names that cluster's context, so a kubeconfig
# whose current context points somewhere else cannot redirect the install.

set -euo pipefail

MOKKA_PROG="up.sh"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/mokka/lib.sh
. "${SCRIPT_DIR}/lib.sh"

usage() {
    printf 'usage: up.sh <lane> <profile>\n' >&2
    printf '  profile: %s\n' "${MOKKA_PROFILES}" >&2
}

if [ "$#" -ne 2 ]; then
    usage
    exit 2
fi

LANE="$1"
PROFILE="$2"

if ! mokka::valid_lane "${LANE}"; then
    usage
    mokka::die 2 "up.sh: lane must be a DNS label (lowercase letters, digits, hyphens): '${LANE}'"
fi

if ! GPU_COUNT="$(mokka::profile_gpu_count "${PROFILE}")"; then
    mokka::die 2 "up.sh: unknown profile '${PROFILE}'; known profiles: ${MOKKA_PROFILES}"
fi

mokka::require_tools kind helm kubectl docker

CLUSTER="$(mokka::cluster_name "${LANE}")"
CONTEXT="$(mokka::context "${LANE}")"
BIN_DIR="${MOKKA_BIN_DIR:-${SCRIPT_DIR}/bin}"
KUBECONFIG_OUT="${BIN_DIR}/kubeconfig-${LANE}"

# ---------------------------------------------------------------------------
# Refuse rather than adopt a cluster somebody else is using
# ---------------------------------------------------------------------------

if mokka::cluster_exists "${CLUSTER}"; then
    release="$(helm list --kube-context "${CONTEXT}" --namespace "${MOKKA_NAMESPACE}" -q 2> /dev/null || true)"
    if printf '%s' "${release}" | grep -qxF -- "${MOKKA_RELEASE}"; then
        mokka::die 4 "up.sh: cluster ${CLUSTER} already exists and holds Mokka release ${MOKKA_RELEASE}; run hack/mokka/down.sh ${LANE} first"
    fi
    mokka::die 4 "up.sh: cluster ${CLUSTER} already exists; run hack/mokka/down.sh ${LANE} first"
fi

# ---------------------------------------------------------------------------
# Cluster
# ---------------------------------------------------------------------------

mokka::phase "create cluster ${CLUSTER}"
kind create cluster \
    --name "${CLUSTER}" \
    --image "${MOKKA_KIND_NODE_IMAGE}" \
    --config "${SCRIPT_DIR}/kind-mokka.yaml"

mkdir -p "${BIN_DIR}"
kind export kubeconfig --name "${CLUSTER}" --kubeconfig "${KUBECONFIG_OUT}"

# ---------------------------------------------------------------------------
# nvml-mock
# ---------------------------------------------------------------------------

mokka::phase "install ${MOKKA_RELEASE} ${MOKKA_CHART_VERSION} (profile ${PROFILE})"
helm upgrade --install "${MOKKA_RELEASE}" "${MOKKA_CHART}" \
    --kube-context "${CONTEXT}" \
    --namespace "${MOKKA_NAMESPACE}" \
    --create-namespace \
    --version "${MOKKA_CHART_VERSION}" \
    --set "gpu.profile=${PROFILE}" \
    --wait --timeout "${MOKKA_HELM_TIMEOUT}"

# ---------------------------------------------------------------------------
# GPU Operator
# ---------------------------------------------------------------------------

mokka::phase "install ${GPU_OPERATOR_RELEASE} ${GPU_OPERATOR_VERSION}"
helm repo add "${GPU_OPERATOR_REPO_NAME}" "${GPU_OPERATOR_REPO_URL}" \
    --kube-context "${CONTEXT}" --force-update
helm repo update "${GPU_OPERATOR_REPO_NAME}" --kube-context "${CONTEXT}"
# Values file only. The operator has enough interacting switches that a --set
# spread over a command line stops being reviewable; the file carries its own
# provenance and its own reasons.
helm upgrade --install "${GPU_OPERATOR_RELEASE}" "${GPU_OPERATOR_CHART}" \
    --kube-context "${CONTEXT}" \
    --namespace "${GPU_OPERATOR_NAMESPACE}" \
    --create-namespace \
    --version "${GPU_OPERATOR_VERSION}" \
    --values "${SCRIPT_DIR}/gpu-operator-values.yaml" \
    --wait --timeout "${MOKKA_HELM_TIMEOUT}"

# ---------------------------------------------------------------------------
# Wait for the operands
# ---------------------------------------------------------------------------
#
# helm --wait covers the chart's own workloads, not the operands the operator
# then creates, so the namespace is polled until every pod is either Ready or
# Completed. An empty pod list is not readiness: it means the operator has not
# created anything yet.

mokka::phase "wait for ${GPU_OPERATOR_NAMESPACE} pods"
deadline=$(( $(date +%s) + MOKKA_READY_TIMEOUT ))
pods_ready=false
last_pending=""
while :; do
    pods="$(kubectl --context "${CONTEXT}" --namespace "${GPU_OPERATOR_NAMESPACE}" get pods \
        -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{.status.phase}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' \
        2> /dev/null || true)"

    pending=""
    seen=0
    while IFS=' ' read -r name phase ready; do
        [ -n "${name}" ] || continue
        seen=$((seen + 1))
        case "${phase}" in
            Succeeded) continue ;;
        esac
        if [ "${ready:-}" != "True" ]; then
            pending="${pending}${name} (${phase} Ready=${ready:-<none>})"$'\n'
        fi
    done <<< "${pods}"

    if [ "${seen}" -gt 0 ] && [ -z "${pending}" ]; then
        pods_ready=true
        break
    fi

    if [ "${seen}" -eq 0 ]; then
        last_pending="no pods in namespace ${GPU_OPERATOR_NAMESPACE}"
    else
        last_pending="${pending}"
    fi

    if [ "$(date +%s)" -ge "${deadline}" ]; then
        break
    fi
    sleep "${MOKKA_POLL_INTERVAL}"
done

if [ "${pods_ready}" != true ]; then
    printf '%s\n' "up.sh: ${GPU_OPERATOR_NAMESPACE} pods did not become Ready or Completed within ${MOKKA_READY_TIMEOUT}s:" >&2
    printf '%s\n' "${last_pending}" >&2
    exit 5
fi

# ---------------------------------------------------------------------------
# Assert the lane looks like a GPU cluster, on EVERY worker
# ---------------------------------------------------------------------------
#
# The operands do not tolerate the control-plane taint, so the control plane
# never carries the GPU labels. Reading .items[0] would therefore pass or fail
# on node ordering rather than on the thing being checked: select the workers
# and check all of them.

mokka::phase "assert simulated GPUs on every worker"
workers="$(kubectl --context "${CONTEXT}" get nodes \
    -l '!node-role.kubernetes.io/control-plane' \
    -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}')"

if [ -z "${workers}" ]; then
    mokka::die 5 "up.sh: no worker nodes found in ${CLUSTER}"
fi

failures=""
worker_count=0
while IFS= read -r node; do
    [ -n "${node}" ] || continue
    worker_count=$((worker_count + 1))

    allocatable="$(kubectl --context "${CONTEXT}" get node "${node}" \
        -o 'jsonpath={.status.allocatable.nvidia\.com/gpu}')"
    if [ "${allocatable}" != "${GPU_COUNT}" ]; then
        failures="${failures}${node}: allocatable nvidia.com/gpu is '${allocatable:-<none>}', expected ${GPU_COUNT} for profile ${PROFILE}"$'\n'
    fi

    product="$(kubectl --context "${CONTEXT}" get node "${node}" \
        -o 'jsonpath={.metadata.labels.nvidia\.com/gpu\.product}')"
    if [ -z "${product}" ]; then
        failures="${failures}${node}: label nvidia.com/gpu.product is absent"$'\n'
    fi

    pci="$(kubectl --context "${CONTEXT}" get node "${node}" \
        -o 'jsonpath={.metadata.labels.feature\.node\.kubernetes\.io/pci-10de\.present}')"
    if [ "${pci}" != "true" ]; then
        failures="${failures}${node}: label feature.node.kubernetes.io/pci-10de.present is '${pci:-<none>}', expected true"$'\n'
    fi

    printf '[mokka] %s: nvidia.com/gpu=%s product=%s pci-10de.present=%s\n' \
        "${node}" "${allocatable:-<none>}" "${product:-<none>}" "${pci:-<none>}"
done <<< "${workers}"

if [ -n "${failures}" ]; then
    printf '%s\n' "up.sh: ${CLUSTER} did not come up as a ${PROFILE} lane:" >&2
    printf '%s' "${failures}" >&2
    exit 5
fi

mokka::phase

printf '\n'
printf 'lane %s is up: %d workers, %s simulated GPUs each (profile %s)\n' \
    "${LANE}" "${worker_count}" "${GPU_COUNT}" "${PROFILE}"
printf 'kubeconfig: %s\n' "${KUBECONFIG_OUT}"
printf 'context:    %s\n' "${CONTEXT}"
