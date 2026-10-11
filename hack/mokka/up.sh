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
#   1  an underlying tool failed; its own output says why
#   2  bad usage, or an unknown profile
#   3  a required tool is missing
#   4  the lane cluster already exists, so nothing was touched
#   5  the lane came up but an assertion about it did not come true in time
#
# Readiness is decided by waiting for the nvidia-device-plugin-daemonset and
# gpu-feature-discovery DaemonSets by name, not by waiting for "every pod in
# the gpu-operator namespace to be Ready". Those two are what put
# nvidia.com/gpu into node allocatable and the nvidia.com/* labels onto the
# node, and they are operands: the operator creates them some time after its
# own chart has finished installing. A check over whatever pods happen to be
# in the namespace is therefore satisfiable before any operand exists, and
# passes against the operator and NFD pods alone.
#
# This script never reads or writes the caller's default kubeconfig. The lane
# kubeconfig is written to hack/mokka/bin/kubeconfig-<lane> and every kind,
# kubectl and helm call is pointed at it explicitly. `kind create cluster`
# without --kubeconfig writes $KUBECONFIG or $HOME/.kube/config and switches
# the current context there, which is how an earlier version of this script
# clobbered a developer's kubeconfig.

set -euo pipefail
# Kubeconfigs carry cluster credentials: created mode 0600, directories 0700.
umask 077

MOKKA_PROG="up.sh"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
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
    mokka::die 2 "up.sh: lane must be a DNS label of at most 32 characters (lowercase letters, digits, hyphens)"
fi

if ! GPU_COUNT="$(mokka::profile_gpu_count "${PROFILE}")"; then
    mokka::die 2 "up.sh: unknown profile '${PROFILE}'; known profiles: ${MOKKA_PROFILES}"
fi

mokka::require_tools kind helm kubectl docker

CLUSTER="$(mokka::cluster_name "${LANE}")"
CONTEXT="$(mokka::context "${LANE}")"
BIN_DIR="${MOKKA_BIN_DIR:-${SCRIPT_DIR}/bin}"
KUBECONFIG_OUT="${BIN_DIR}/kubeconfig-${LANE}"

# The kind topology. The scale lane needs a wider pod subnet than kind's
# default, which is a property of the cluster at creation and so a different
# config file rather than a flag; everything else keeps kind-mokka.yaml.
#
# A value with no slash in it is taken as a file next to this script, so a
# caller can say MOKKA_KIND_CONFIG=kind-scale.yaml from any working directory.
# Anything else is a path and is used as given.
#
# A misspelled override is a usage error and not a fall back to the default:
# the lane would come up with the wrong topology and nothing after this point
# would notice.
KIND_CONFIG="${MOKKA_KIND_CONFIG:-kind-mokka.yaml}"
case "${KIND_CONFIG}" in
    */*) ;;
    *) KIND_CONFIG="${SCRIPT_DIR}/${KIND_CONFIG}" ;;
esac
if [ ! -f "${KIND_CONFIG}" ]; then
    mokka::die 2 "up.sh: kind config not found: ${KIND_CONFIG}"
fi

# Belt and braces alongside the explicit --kubeconfig on every call below: with
# this set, a call that somehow lost its flag still cannot reach ~/.kube/config.
export KUBECONFIG="${KUBECONFIG_OUT}"

# A run that fails after the cluster exists leaves containers behind. Say so,
# rather than leaving the reader to discover it from `docker ps`.
CLUSTER_CREATED=false
on_exit() {
    local rc=$?
    if [ "${rc}" -ne 0 ] && [ "${CLUSTER_CREATED}" = true ]; then
        printf 'partial install left in %s; run hack/mokka/down.sh %s\n' \
            "${CLUSTER}" "${LANE}" >&2
    fi
}
trap on_exit EXIT

# kubectl and helm, always against the lane kubeconfig and the lane context.
k() {
    kubectl --kubeconfig "${KUBECONFIG_OUT}" --context "${CONTEXT}" "$@"
}

h() {
    helm --kubeconfig "${KUBECONFIG_OUT}" --kube-context "${CONTEXT}" "$@"
}

# ---------------------------------------------------------------------------
# Refuse rather than adopt a cluster somebody else is using
# ---------------------------------------------------------------------------

if mokka::cluster_exists "${CLUSTER}"; then
    release="$(h list --namespace "${MOKKA_NAMESPACE}" -q 2> /dev/null || true)"
    if printf '%s\n' "${release}" | grep -qxF -- "${MOKKA_RELEASE}"; then
        mokka::die 4 "up.sh: cluster ${CLUSTER} already exists and holds Mokka release ${MOKKA_RELEASE}; run hack/mokka/down.sh ${LANE} first"
    fi
    mokka::die 4 "up.sh: cluster ${CLUSTER} already exists; run hack/mokka/down.sh ${LANE} first"
fi

# ---------------------------------------------------------------------------
# Cluster
# ---------------------------------------------------------------------------

mkdir -p "${BIN_DIR}"
# mkdir -p leaves an existing directory's mode alone, and this one outlives
# any single run, so the mode is set rather than left to whoever created it.
chmod 700 "${BIN_DIR}"

mokka::phase "create cluster ${CLUSTER}"
kind create cluster \
    --name "${CLUSTER}" \
    --image "${MOKKA_KIND_NODE_IMAGE}" \
    --config "${KIND_CONFIG}" \
    --kubeconfig "${KUBECONFIG_OUT}"
CLUSTER_CREATED=true

# ---------------------------------------------------------------------------
# nvml-mock
# ---------------------------------------------------------------------------

mokka::phase "install ${MOKKA_RELEASE} ${MOKKA_CHART_VERSION} (profile ${PROFILE})"
h upgrade --install "${MOKKA_RELEASE}" "${MOKKA_CHART}" \
    --namespace "${MOKKA_NAMESPACE}" \
    --create-namespace \
    --version "${MOKKA_CHART_VERSION}" \
    --set "gpu.profile=${PROFILE}" \
    --wait --timeout "${MOKKA_HELM_TIMEOUT}"

# ---------------------------------------------------------------------------
# GPU Operator
# ---------------------------------------------------------------------------

mokka::phase "install ${GPU_OPERATOR_RELEASE} ${GPU_OPERATOR_VERSION}"
h repo add "${GPU_OPERATOR_REPO_NAME}" "${GPU_OPERATOR_REPO_URL}" --force-update
h repo update "${GPU_OPERATOR_REPO_NAME}"
# Values file only. The operator has enough interacting switches that a --set
# spread over a command line stops being reviewable; the file carries its own
# provenance and its own reasons.
h upgrade --install "${GPU_OPERATOR_RELEASE}" "${GPU_OPERATOR_CHART}" \
    --namespace "${GPU_OPERATOR_NAMESPACE}" \
    --create-namespace \
    --version "${GPU_OPERATOR_VERSION}" \
    --values "${SCRIPT_DIR}/gpu-operator-values.yaml" \
    --wait --timeout "${MOKKA_HELM_TIMEOUT}"

# ---------------------------------------------------------------------------
# Workers
# ---------------------------------------------------------------------------
#
# The operands do not tolerate the control-plane taint, so the control plane
# never carries the GPU resource or the product label, even though nvml-mock
# does run there and does label it nvidia.com/gpu.present=true. Reading
# .items[0] would therefore pass or fail on node ordering rather than on the
# thing being checked: select the workers and check all of them.

workers="$(k get nodes \
    -l '!node-role.kubernetes.io/control-plane' \
    -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}')"

if [ -z "${workers}" ]; then
    mokka::die 5 "up.sh: no worker nodes found in ${CLUSTER}"
fi

worker_count=0
while IFS= read -r node; do
    [ -n "${node}" ] || continue
    worker_count=$((worker_count + 1))
done <<< "${workers}"

# One deadline covers both waits below: what matters to the caller is how long
# the lane takes to become usable, not how the time splits between the two.
DEADLINE=$(( $(date +%s) + MOKKA_READY_TIMEOUT ))

# ---------------------------------------------------------------------------
# Wait for the operand DaemonSets
# ---------------------------------------------------------------------------
#
# helm --wait covers the operator's own workloads, not the operands it then
# creates. Waiting on "every pod currently in the namespace" is satisfiable
# before the operands exist at all, so wait on the DaemonSets by name: a
# DaemonSet that is absent means not yet, never an error.

mokka::phase "wait for operand DaemonSets"
ds_ready=false
ds_report=""
while :; do
    ds_report=""
    all_ok=true
    for ds in ${MOKKA_GATE_DAEMONSETS}; do
        status="$(k --namespace "${GPU_OPERATOR_NAMESPACE}" get daemonset "${ds}" \
            -o 'jsonpath={.status.desiredNumberScheduled} {.status.numberReady} {.status.updatedNumberScheduled}' \
            2> /dev/null || true)"
        if [ -z "${status}" ]; then
            ds_report="${ds_report}${ds}: not found"$'\n'
            all_ok=false
            continue
        fi
        read -r desired ready updated <<< "${status}"
        desired="${desired:-0}"
        ready="${ready:-0}"
        updated="${updated:-0}"
        if [ "${desired}" -ne "${worker_count}" ] ||
            [ "${ready}" -ne "${desired}" ] ||
            [ "${updated}" -ne "${desired}" ]; then
            ds_report="${ds_report}${ds}: desired=${desired} ready=${ready} updated=${updated}, want desired=ready=updated=${worker_count}"$'\n'
            all_ok=false
        fi
    done

    if [ "${all_ok}" = true ]; then
        ds_ready=true
        break
    fi
    if [ "$(date +%s)" -ge "${DEADLINE}" ]; then
        break
    fi
    sleep "${MOKKA_POLL_INTERVAL}"
done

if [ "${ds_ready}" != true ]; then
    printf '%s\n' "up.sh: operand DaemonSets did not roll out within ${MOKKA_READY_TIMEOUT}s:" >&2
    printf '%s' "${ds_report}" >&2
    exit 5
fi

# ---------------------------------------------------------------------------
# Assert the lane looks like a GPU cluster, on EVERY worker
# ---------------------------------------------------------------------------
#
# The assertions are the wait. A rolled-out device plugin has not necessarily
# had its allocatable update observed by the API server yet, so checking once
# and failing would be a race; checking in a loop until the deadline is the
# same assertion with the race removed.

mokka::phase "assert simulated GPUs on every worker"
assert_ok=false
failures=""
summary=""
while :; do
    failures=""
    summary=""
    while IFS= read -r node; do
        [ -n "${node}" ] || continue

        allocatable="$(k get node "${node}" \
            -o 'jsonpath={.status.allocatable.nvidia\.com/gpu}')"
        if [ "${allocatable}" != "${GPU_COUNT}" ]; then
            failures="${failures}${node}: allocatable nvidia.com/gpu is '${allocatable:-<none>}', expected ${GPU_COUNT} for profile ${PROFILE}"$'\n'
        fi

        product="$(k get node "${node}" \
            -o 'jsonpath={.metadata.labels.nvidia\.com/gpu\.product}')"
        if [ -z "${product}" ]; then
            failures="${failures}${node}: label nvidia.com/gpu.product is absent"$'\n'
        fi

        pci="$(k get node "${node}" \
            -o 'jsonpath={.metadata.labels.feature\.node\.kubernetes\.io/pci-10de\.present}')"
        if [ "${pci}" != "true" ]; then
            failures="${failures}${node}: label feature.node.kubernetes.io/pci-10de.present is '${pci:-<none>}', expected true"$'\n'
        fi

        summary="${summary}[mokka] ${node}: nvidia.com/gpu=${allocatable:-<none>} product=${product:-<none>} pci-10de.present=${pci:-<none>}"$'\n'
    done <<< "${workers}"

    if [ -z "${failures}" ]; then
        assert_ok=true
        break
    fi
    if [ "$(date +%s)" -ge "${DEADLINE}" ]; then
        break
    fi
    sleep "${MOKKA_POLL_INTERVAL}"
done

printf '%s' "${summary}"

if [ "${assert_ok}" != true ]; then
    printf '%s\n' "up.sh: ${CLUSTER} did not become a ${PROFILE} lane within ${MOKKA_READY_TIMEOUT}s:" >&2
    printf '%s' "${failures}" >&2
    exit 5
fi

# ---------------------------------------------------------------------------
# Wait for the operator's own verdict
# ---------------------------------------------------------------------------
#
# The ClusterPolicy state is a gate now, not only a record. It was a record
# while the states this deployment reaches had not been characterised; they
# have been, on every lane this repository has brought up, and the answer is
# that it reaches "ready" and stays there.
#
# What makes it worth gating on is the reader. `kubectl nv health` reports
# NV-OP-001 at error for a policy that is not ready, so a lane handed to the
# suite before the operator has finished agreeing with itself fails the healthy
# gate for a reason that has nothing to do with the change under test.
#
# It shares the deadline above rather than taking one of its own: what the
# caller cares about is how long the lane takes to become usable, not how the
# time divides between the operands and the policy. A policy that is
# unreadable, because the CRD is not there at all, is not a failure: a lane
# without the operator is a lane this gate has nothing to say about.

mokka::phase "wait for the ClusterPolicy"
cp_state=""
cp_ready=false
cp_seen=false
while :; do
    if ! cp_state="$(k get clusterpolicies.nvidia.com cluster-policy \
        -o 'jsonpath={.status.state}' 2> /dev/null)"; then
        cp_state=""
        break
    fi
    cp_seen=true
    if [ "${cp_state}" = "ready" ]; then
        cp_ready=true
        break
    fi
    if [ "$(date +%s)" -ge "${DEADLINE}" ]; then
        break
    fi
    sleep "${MOKKA_POLL_INTERVAL}"
done

mokka::phase

if [ "${cp_seen}" = true ] && [ "${cp_ready}" != true ]; then
    printf '%s\n' "up.sh: the ClusterPolicy did not reach ready within ${MOKKA_READY_TIMEOUT}s; its state is '${cp_state:-<empty>}'" >&2
    exit 5
fi

printf '[mokka] ClusterPolicy state: %s\n' "${cp_state:-<unavailable>}"
printf '\n'
printf 'lane %s is up: %d workers, %s simulated GPUs each (profile %s)\n' \
    "${LANE}" "${worker_count}" "${GPU_COUNT}" "${PROFILE}"
printf 'kubeconfig: %s\n' "${KUBECONFIG_OUT}"
printf 'context:    %s\n' "${CONTEXT}"
printf '\n'
printf 'the lane is not in your default kubeconfig; reach it with\n'
printf '  kubectl --kubeconfig %s --context %s get nodes\n' "${KUBECONFIG_OUT}" "${CONTEXT}"
