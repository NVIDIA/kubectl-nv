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
# Brings up the MIG lane: a kind cluster whose workers each carry two simulated
# H100 boards carved into seven 1g.10gb partitions, so each worker advertises
# fourteen nvidia.com/gpu.
#
# Usage: hack/mokka/mig/up.sh <lane>
#
#   lane  DNS label; the cluster is named kubectl-nv-<lane> and the kubeconfig
#         context kind-kubectl-nv-<lane>. "make e2e-mig" passes mig.
#
# There is no profile argument. The layout is the lane: h100 is one of the five
# profiles that can partition at all, and filling each board with its smallest
# slice is the only shape the device plugin's --mig-strategy=single accepts.
# Taking a profile here would offer l40s and t4, which the chart refuses, and
# layouts that the plugin then declines to serve.
#
# Exit codes:
#   0  the lane is up and every assertion passed
#   1  an underlying tool failed; its own output says why
#   2  bad usage, or the upstream source could not be resolved
#   3  a required tool is missing
#   4  the lane cluster already exists, so nothing was touched
#   5  the lane came up but an assertion about it did not come true in time
#   6  the upstream source is not the pinned commit
#
# HOW THIS LANE DIFFERS FROM hack/mokka/up.sh
#
#   - the chart is a local directory out of a pinned upstream commit, not the
#     released 0.4.0 from ghcr.io, because no release can partition a board
#   - the container image is built from that commit and loaded into the nodes
#   - the GPU Operator's device plugin is disabled and Mokka's own privileged
#     one takes its place; lib.sh says why
#   - the readiness gate therefore waits on gpu-feature-discovery in the
#     operator's namespace AND on that plugin in kube-system
#
# Everything else, including the kind node image and the GPU Operator version
# and base values file, is shared with that lane.
#
# This script never reads or writes the caller's default kubeconfig. The lane
# kubeconfig is written to hack/mokka/bin/kubeconfig-<lane> and every kind,
# kubectl and helm call is pointed at it explicitly.

set -euo pipefail
# Kubeconfigs carry cluster credentials: created mode 0600, directories 0700.
umask 077

MOKKA_PROG="mig/up.sh"
MIG_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${MIG_SCRIPT_DIR}/lib.sh"
# shellcheck source=SCRIPTDIR/image.sh
. "${MIG_SCRIPT_DIR}/image.sh"

if [ "$#" -ne 1 ]; then
    printf 'usage: mig/up.sh <lane>\n' >&2
    exit 2
fi

LANE="$1"

if ! mokka::valid_lane "${LANE}"; then
    printf 'usage: mig/up.sh <lane>\n' >&2
    mokka::die 2 "mig/up.sh: lane must be a DNS label of at most 32 characters (lowercase letters, digits, hyphens)"
fi

mokka::require_tools kind helm kubectl docker git tar

CLUSTER="$(mokka::cluster_name "${LANE}")"
CONTEXT="$(mokka::context "${LANE}")"
mig::require_lane_cluster "${CLUSTER}"

# The lane kubeconfig lives beside the whole-GPU lane's, under hack/mokka/bin,
# which is gitignored and mode 0700.
BIN_DIR="${MOKKA_BIN_DIR:-${MIG_SCRIPT_DIR}/../bin}"
KUBECONFIG_OUT="${BIN_DIR}/kubeconfig-${LANE}"

# Belt and braces alongside the explicit --kubeconfig on every call below.
export KUBECONFIG="${KUBECONFIG_OUT}"

STAGING=""
CLUSTER_CREATED=false
on_exit() {
    local rc=$?
    if [ -n "${STAGING}" ]; then
        rm -rf "${STAGING}"
    fi
    if [ -n "${MIG_STAGING_OWNED}" ]; then
        rm -rf "${MIG_STAGING_OWNED}"
    fi
    if [ "${rc}" -ne 0 ] && [ "${CLUSTER_CREATED}" = true ]; then
        printf 'partial install left in %s; run hack/mokka/mig/down.sh %s\n' \
            "${CLUSTER}" "${LANE}" >&2
    fi
}
trap on_exit EXIT

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
        mokka::die 4 "mig/up.sh: cluster ${CLUSTER} already exists and holds Mokka release ${MOKKA_RELEASE}; run hack/mokka/mig/down.sh ${LANE} first"
    fi
    mokka::die 4 "mig/up.sh: cluster ${CLUSTER} already exists; run hack/mokka/mig/down.sh ${LANE} first"
fi

LANE_START="$(date +%s)"

# ---------------------------------------------------------------------------
# Upstream source and the image
# ---------------------------------------------------------------------------
#
# Staged even when the image is already built: the chart this lane installs is
# a directory in that same tree.

STAGING="$(mktemp -d "${TMPDIR:-/tmp}/kubectl-nv-mig-upstream.XXXXXX")"
MIG_STAGING_DIR="${STAGING}"
export MIG_STAGING_DIR
mokka::phase "stage ${MIG_UPSTREAM_REPO} at ${MIG_UPSTREAM_SHA}"
mig::stage_upstream "${STAGING}"
MIG_CHART_DIR="${MIG_UPSTREAM_TREE}/deployments/nvml-mock/helm/nvml-mock"
if [ ! -f "${MIG_CHART_DIR}/Chart.yaml" ]; then
    mokka::die 2 "mig/up.sh: no chart at ${MIG_CHART_DIR}; the upstream tree is not the one this lane expects"
fi

# A no-op when the tag is already in the daemon. The long path is meant to be
# taken by hack/mokka/mig/build-image.sh, outside the lane lock.
mig::build_image false

# ---------------------------------------------------------------------------
# Cluster
# ---------------------------------------------------------------------------

mkdir -p "${BIN_DIR}"
chmod 700 "${BIN_DIR}"

mokka::phase "create cluster ${CLUSTER}"
kind create cluster \
    --name "${CLUSTER}" \
    --image "${MOKKA_KIND_NODE_IMAGE}" \
    --config "${MIG_SCRIPT_DIR}/kind-mig.yaml" \
    --kubeconfig "${KUBECONFIG_OUT}"
CLUSTER_CREATED=true

mokka::phase "load $(mig::image_ref) into ${CLUSTER}"
kind load docker-image "$(mig::image_ref)" --name "${CLUSTER}"

# ---------------------------------------------------------------------------
# nvml-mock, partitioned
# ---------------------------------------------------------------------------
#
# No --version: the chart is a directory, and its version is whatever the
# pinned commit says, which is the point. Values file only, never --set: the
# MIG switches interact (a board that is MIG-enabled with no layout publishes
# no GPU resource at all) and a --set spread over a command line is not
# something a reviewer can check.

mokka::phase "install ${MOKKA_RELEASE} from ${MIG_UPSTREAM_SHA} (${MIG_BOARDS} x ${MIG_PROFILE}, ${MIG_SLICES_PER_BOARD} x ${MIG_SLICE_PROFILE} each)"
h upgrade --install "${MOKKA_RELEASE}" "${MIG_CHART_DIR}" \
    --namespace "${MOKKA_NAMESPACE}" \
    --create-namespace \
    --values "${MIG_SCRIPT_DIR}/mig-nvml-mock-values.yaml" \
    --wait --timeout "${MOKKA_HELM_TIMEOUT}"

# ---------------------------------------------------------------------------
# GPU Operator
# ---------------------------------------------------------------------------
#
# Two values files: the whole-GPU lane's, unchanged, then this lane's two
# overrides on top. Later --values wins, so the order is load bearing.

mokka::phase "install ${GPU_OPERATOR_RELEASE} ${GPU_OPERATOR_VERSION} (mig.strategy=${MIG_STRATEGY}, device plugin off)"
h repo add "${GPU_OPERATOR_REPO_NAME}" "${GPU_OPERATOR_REPO_URL}" --force-update
h repo update "${GPU_OPERATOR_REPO_NAME}"
h upgrade --install "${GPU_OPERATOR_RELEASE}" "${GPU_OPERATOR_CHART}" \
    --namespace "${GPU_OPERATOR_NAMESPACE}" \
    --create-namespace \
    --version "${GPU_OPERATOR_VERSION}" \
    --values "${MIG_SCRIPT_DIR}/../gpu-operator-values.yaml" \
    --values "${MIG_SCRIPT_DIR}/mig-gpu-operator-values.yaml" \
    --wait --timeout "${MOKKA_HELM_TIMEOUT}"

# ---------------------------------------------------------------------------
# Mokka's device plugin
# ---------------------------------------------------------------------------

mokka::phase "apply ${MIG_DEVICE_PLUGIN_DAEMONSET}"
k apply -f "${MIG_SCRIPT_DIR}/${MIG_DEVICE_PLUGIN_MANIFEST}"

# ---------------------------------------------------------------------------
# Workers
# ---------------------------------------------------------------------------

workers="$(k get nodes \
    -l '!node-role.kubernetes.io/control-plane' \
    -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}')"

if [ -z "${workers}" ]; then
    mokka::die 5 "mig/up.sh: no worker nodes found in ${CLUSTER}"
fi

worker_count=0
while IFS= read -r node; do
    [ -n "${node}" ] || continue
    worker_count=$((worker_count + 1))
done <<< "${workers}"

DEADLINE=$(( $(date +%s) + MOKKA_READY_TIMEOUT ))

# ---------------------------------------------------------------------------
# Wait for the operand DaemonSets
# ---------------------------------------------------------------------------
#
# Two namespaces: gpu-feature-discovery is the operator's and lives in
# gpu-operator; the device plugin is this lane's and lives in kube-system. A
# DaemonSet that is absent means not yet, never an error: the operator creates
# its operands some time after its own chart has finished installing.

mokka::phase "wait for operand DaemonSets"
ds_ready=false
ds_report=""
while :; do
    ds_report=""
    all_ok=true
    # shellcheck disable=SC2086
    # MIG_OPERAND_DAEMONSETS is a space-separated list and is meant to split.
    for spec in ${MIG_OPERAND_DAEMONSETS}; do
        ns="${spec%%/*}"
        ds="${spec##*/}"
        status="$(k --namespace "${ns}" get daemonset "${ds}" \
            -o 'jsonpath={.status.desiredNumberScheduled} {.status.numberReady} {.status.updatedNumberScheduled}' \
            2> /dev/null || true)"
        if [ -z "${status}" ]; then
            ds_report="${ds_report}${ns}/${ds}: not found"$'\n'
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
            ds_report="${ds_report}${ns}/${ds}: desired=${desired} ready=${ready} updated=${updated}, want desired=ready=updated=${worker_count}"$'\n'
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
    printf '%s\n' "mig/up.sh: operand DaemonSets did not roll out within ${MOKKA_READY_TIMEOUT}s:" >&2
    printf '%s' "${ds_report}" >&2
    exit 5
fi

# ---------------------------------------------------------------------------
# Assert the lane looks like a PARTITIONED GPU cluster, on EVERY worker
# ---------------------------------------------------------------------------
#
# The assertions are the wait, for the same reason as in the whole-GPU lane: a
# rolled-out device plugin has not necessarily had its allocatable update
# observed by the API server yet.
#
# Capacity as well as allocatable: the two coming apart is how a node that
# registered fourteen partitions and then lost the plugin serving them looks,
# and it is the failure the upstream manifest's own comments warn about.

label() {
    k get node "$1" -o "jsonpath={.metadata.labels.$2}"
}

mokka::phase "assert ${MIG_EXPECTED_GPUS} partitions on every worker"
assert_ok=false
failures=""
summary=""
while :; do
    failures=""
    summary=""
    while IFS= read -r node; do
        [ -n "${node}" ] || continue

        allocatable="$(k get node "${node}" -o 'jsonpath={.status.allocatable.nvidia\.com/gpu}')"
        capacity="$(k get node "${node}" -o 'jsonpath={.status.capacity.nvidia\.com/gpu}')"
        strategy="$(label "${node}" 'nvidia\.com/mig\.strategy')"
        capable="$(label "${node}" 'nvidia\.com/mig\.capable')"
        product="$(label "${node}" 'nvidia\.com/gpu\.product')"
        count="$(label "${node}" 'nvidia\.com/gpu\.count')"
        memory="$(label "${node}" 'nvidia\.com/gpu\.memory')"

        if [ "${allocatable}" != "${MIG_EXPECTED_GPUS}" ]; then
            failures="${failures}${node}: allocatable nvidia.com/gpu is '${allocatable:-<none>}', expected ${MIG_EXPECTED_GPUS} (${MIG_BOARDS} boards x ${MIG_SLICES_PER_BOARD} ${MIG_SLICE_PROFILE})"$'\n'
        fi
        if [ "${capacity}" != "${MIG_EXPECTED_GPUS}" ]; then
            failures="${failures}${node}: capacity nvidia.com/gpu is '${capacity:-<none>}', expected ${MIG_EXPECTED_GPUS}"$'\n'
        fi
        if [ "${strategy}" != "${MIG_STRATEGY}" ]; then
            failures="${failures}${node}: label nvidia.com/mig.strategy is '${strategy:-<none>}', expected ${MIG_STRATEGY}"$'\n'
        fi
        if [ "${capable}" != "true" ]; then
            failures="${failures}${node}: label nvidia.com/mig.capable is '${capable:-<none>}', expected true"$'\n'
        fi
        if [ -z "${product}" ]; then
            failures="${failures}${node}: label nvidia.com/gpu.product is absent"$'\n'
        fi
        if [ -z "${count}" ]; then
            failures="${failures}${node}: label nvidia.com/gpu.count is absent"$'\n'
        fi
        if [ -z "${memory}" ]; then
            failures="${failures}${node}: label nvidia.com/gpu.memory is absent"$'\n'
        fi

        summary="${summary}[mig] ${node}: allocatable=${allocatable:-<none>} capacity=${capacity:-<none>} mig.strategy=${strategy:-<none>} mig.capable=${capable:-<none>} gpu.product=${product:-<none>} gpu.count=${count:-<none>} gpu.memory=${memory:-<none>}"$'\n'
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
    printf '%s\n' "mig/up.sh: ${CLUSTER} did not become a MIG lane within ${MOKKA_READY_TIMEOUT}s:" >&2
    printf '%s' "${failures}" >&2
    exit 5
fi

# ---------------------------------------------------------------------------
# Record, do not gate
# ---------------------------------------------------------------------------

cp_state="$(k get clusterpolicies.nvidia.com cluster-policy \
    -o 'jsonpath={.status.state}' 2> /dev/null || true)"

mokka::phase

printf '\n'
printf '[mig] operand pods\n'
k --namespace "${GPU_OPERATOR_NAMESPACE}" get pods \
    -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,NODE:.spec.nodeName,PHASE:.status.phase' \
    2>&1 || true
k --namespace "${MIG_DEVICE_PLUGIN_NAMESPACE}" get pods \
    -l "name=${MIG_DEVICE_PLUGIN_DAEMONSET}" \
    -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,NODE:.spec.nodeName,PHASE:.status.phase' \
    2>&1 || true

printf '\n'
printf '[mig] ClusterPolicy state: %s\n' "${cp_state:-<unavailable>}"
printf '[mig] time to ready: %ss\n' "$(( $(date +%s) - LANE_START ))"
printf '\n'
printf 'lane %s is up: %d workers, %s partitions each (%s boards of %s %s, strategy %s)\n' \
    "${LANE}" "${worker_count}" "${MIG_EXPECTED_GPUS}" \
    "${MIG_BOARDS}" "${MIG_SLICES_PER_BOARD}" "${MIG_SLICE_PROFILE}" "${MIG_STRATEGY}"
printf 'upstream:   %s at %s\n' "${MIG_UPSTREAM_REPO}" "${MIG_UPSTREAM_SHA}"
printf 'kubeconfig: %s\n' "${KUBECONFIG_OUT}"
printf 'context:    %s\n' "${CONTEXT}"
printf '\n'
printf 'the lane is not in your default kubeconfig; reach it with\n'
printf '  kubectl --kubeconfig %s --context %s get nodes\n' "${KUBECONFIG_OUT}" "${CONTEXT}"
