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
# Brings up the compute domain lane: four simulated GPU workers split into two
# NVLink cliques, the GPU Operator on top, and Kueue with one Topology and one
# ResourceFlavor, so "kubectl nv topology" can be exercised against a cluster
# that has a fabric to report and jobs to fit into it.
#
# Usage: hack/mokka/compute-domain/up.sh [lane]
#
#   lane  DNS label, default compute-domain. The cluster is kubectl-nv-<lane>
#         and the kubeconfig context kind-kubectl-nv-<lane>.
#
# Exit codes:
#   0  the lane is up and every assertion passed
#   1  an underlying tool failed; its own output says why
#   2  bad usage
#   3  a required tool is missing
#   4  the lane cluster already exists, so nothing was touched
#   5  the lane came up but an assertion about it did not come true in time
#   6  a pinned artifact did not match its recorded checksum
#
# This lane is a nightly CI job. It is not meant to be brought up on a
# developer machine by hand: it builds a five node kind cluster and installs
# three charts. See .github/workflows/e2e-compute-domain.yaml.
#
# As in hack/mokka/up.sh, this script never reads or writes the caller's
# default kubeconfig: the lane kubeconfig is written under hack/mokka/bin and
# every kind, kubectl and helm call is pointed at it explicitly.

set -euo pipefail
# Kubeconfigs carry cluster credentials: created mode 0600, directories 0700.
umask 077

MOKKA_PROG="compute-domain/up.sh"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${SCRIPT_DIR}/lib.sh"

usage() {
    printf 'usage: compute-domain/up.sh [lane]\n' >&2
}

if [ "$#" -gt 1 ]; then
    usage
    exit 2
fi

LANE="${1:-${CD_DEFAULT_LANE}}"

if ! mokka::valid_lane "${LANE}"; then
    usage
    mokka::die 2 "compute-domain/up.sh: lane must be a DNS label of at most 32 characters (lowercase letters, digits, hyphens)"
fi

mokka::require_tools kind helm kubectl docker curl

CLUSTER="$(mokka::cluster_name "${LANE}")"
CONTEXT="$(mokka::context "${LANE}")"
BIN_DIR="${MOKKA_BIN_DIR:-${SCRIPT_DIR}/../bin}"
KUBECONFIG_OUT="${BIN_DIR}/kubeconfig-${LANE}"
TOPOLOGY_VALUES="${BIN_DIR}/topology-values-${LANE}.yaml"
KUEUE_MANIFEST="${BIN_DIR}/kueue-${KUEUE_VERSION}-${LANE}.yaml"

# Belt and braces alongside the explicit --kubeconfig on every call below.
export KUBECONFIG="${KUBECONFIG_OUT}"

CLUSTER_CREATED=false
on_exit() {
    local rc=$?
    if [ "${rc}" -ne 0 ] && [ "${CLUSTER_CREATED}" = true ]; then
        printf 'partial install left in %s; run hack/mokka/compute-domain/down.sh %s\n' \
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
        mokka::die 4 "compute-domain/up.sh: cluster ${CLUSTER} already exists and holds Mokka release ${MOKKA_RELEASE}; run hack/mokka/compute-domain/down.sh ${LANE} first"
    fi
    mokka::die 4 "compute-domain/up.sh: cluster ${CLUSTER} already exists; run hack/mokka/compute-domain/down.sh ${LANE} first"
fi

# ---------------------------------------------------------------------------
# Cluster
# ---------------------------------------------------------------------------

mkdir -p "${BIN_DIR}"
chmod 700 "${BIN_DIR}"

mokka::phase "create cluster ${CLUSTER}"
kind create cluster \
    --name "${CLUSTER}" \
    --image "${MOKKA_KIND_NODE_IMAGE}" \
    --config "${SCRIPT_DIR}/kind-compute-domain.yaml" \
    --kubeconfig "${KUBECONFIG_OUT}"
CLUSTER_CREATED=true

# ---------------------------------------------------------------------------
# The topology document
# ---------------------------------------------------------------------------
#
# Rendered rather than checked in, because it has to name the kind workers and
# those are derived from the cluster name. The split is deliberate: the
# switches live in a reviewed values file, and only the node lists, which are a
# function of the lane, are generated. It is written with helm --values like
# the rest; nothing about this lane goes in through --set.

mokka::phase "render the topology document for ${CLUSTER}"
{
    printf '# Rendered by hack/mokka/compute-domain/up.sh for cluster %s.\n' "${CLUSTER}"
    printf '# Do not edit: it is recreated on every run.\n'
    printf 'topology:\n'
    printf '  domains:\n'
    printf '    - name: %s\n' "${CD_DOMAIN_NAME}"
    printf '      uuid: "%s"\n' "${CD_CLUSTER_UUID}"
    printf '      cliques:\n'
    for clique in 0 1; do
        printf '        - id: %s\n' "${clique}"
        printf '          nodes:\n'
        for index in $(seq 1 "${CD_WORKER_COUNT}"); do
            if [ "$(cd::worker_clique "${index}")" = "${clique}" ]; then
                printf '            - %s\n' "$(cd::worker_name "${CLUSTER}" "${index}")"
            fi
        done
    done
} > "${TOPOLOGY_VALUES}"

printf '[mokka] topology document:\n'
sed 's/^/[mokka]   /' "${TOPOLOGY_VALUES}"

# ---------------------------------------------------------------------------
# nvml-mock
# ---------------------------------------------------------------------------

mokka::phase "install ${MOKKA_RELEASE} ${MOKKA_CHART_VERSION} (profile ${CD_PROFILE})"
h upgrade --install "${MOKKA_RELEASE}" "${MOKKA_CHART}" \
    --namespace "${MOKKA_NAMESPACE}" \
    --create-namespace \
    --version "${MOKKA_CHART_VERSION}" \
    --values "${SCRIPT_DIR}/mokka-values.yaml" \
    --values "${TOPOLOGY_VALUES}" \
    --wait --timeout "${MOKKA_HELM_TIMEOUT}"

# ---------------------------------------------------------------------------
# GPU Operator
# ---------------------------------------------------------------------------
#
# The same values overlay the two worker lane uses, read from its directory
# rather than copied: the two lanes must not drift about how the operator is
# configured against simulated devices.

mokka::phase "install ${GPU_OPERATOR_RELEASE} ${GPU_OPERATOR_VERSION}"
h repo add "${GPU_OPERATOR_REPO_NAME}" "${GPU_OPERATOR_REPO_URL}" --force-update
h repo update "${GPU_OPERATOR_REPO_NAME}"
h upgrade --install "${GPU_OPERATOR_RELEASE}" "${GPU_OPERATOR_CHART}" \
    --namespace "${GPU_OPERATOR_NAMESPACE}" \
    --create-namespace \
    --version "${GPU_OPERATOR_VERSION}" \
    --values "${SCRIPT_DIR}/../gpu-operator-values.yaml" \
    --wait --timeout "${MOKKA_HELM_TIMEOUT}"

# ---------------------------------------------------------------------------
# Workers
# ---------------------------------------------------------------------------

workers="$(k get nodes \
    -l '!node-role.kubernetes.io/control-plane' \
    -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}')"

worker_count=0
while IFS= read -r node; do
    [ -n "${node}" ] || continue
    worker_count=$((worker_count + 1))
done <<< "${workers}"

if [ "${worker_count}" -ne "${CD_WORKER_COUNT}" ]; then
    mokka::die 5 "compute-domain/up.sh: ${CLUSTER} has ${worker_count} worker(s), expected ${CD_WORKER_COUNT}"
fi

# ---------------------------------------------------------------------------
# Wait for the operand DaemonSets
# ---------------------------------------------------------------------------
#
# Every wait below arms its own deadline. MOKKA_READY_TIMEOUT is therefore the
# budget of one phase, not of all of them together, which is what the
# workflow's timeout-minutes is reckoned against.

mokka::phase "wait for operand DaemonSets"
DEADLINE="$(cd::deadline)"
ds_ready=false
ds_report=""
while :; do
    ds_report=""
    all_ok=true
    for ds in ${MOKKA_OPERAND_DAEMONSETS}; do
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
    printf '%s\n' "compute-domain/up.sh: operand DaemonSets did not roll out within ${MOKKA_READY_TIMEOUT}s:" >&2
    printf '%s' "${ds_report}" >&2
    exit 5
fi

# ---------------------------------------------------------------------------
# Assert the lane is the fabric the suite expects, on EVERY worker
# ---------------------------------------------------------------------------
#
# The clique assertion is the one that matters here, and it is checked against
# the value this worker's index implies rather than against "some clique
# label". Three things can write it wrong, and the message has to tell them
# apart:
#
#   - absent, because GPU Feature Discovery never resolved the fabric and the
#     kind config's static label is gone too;
#   - the gb200 profile's placeholder, 00000000-0000-0000-0000-000000000001.0,
#     on every worker, which means GFD read the mock without the topology
#     overlay and overwrote the static label: NRI did not reach it;
#   - this lane's UUID but the other clique, which means the topology document
#     and the kind config disagree about which worker is where.

mokka::phase "assert the fabric on every worker"
DEADLINE="$(cd::deadline)"
assert_ok=false
failures=""
summary=""
while :; do
    failures=""
    summary=""
    for index in $(seq 1 "${CD_WORKER_COUNT}"); do
        node="$(cd::worker_name "${CLUSTER}" "${index}")"
        want_clique="${CD_CLUSTER_UUID}.$(cd::worker_clique "${index}")"

        allocatable="$(k get node "${node}" \
            -o 'jsonpath={.status.allocatable.nvidia\.com/gpu}' 2> /dev/null || true)"
        if [ "${allocatable}" != "4" ]; then
            failures="${failures}${node}: allocatable nvidia.com/gpu is '${allocatable:-<none>}', expected 4 for profile ${CD_PROFILE}"$'\n'
        fi

        product="$(k get node "${node}" \
            -o 'jsonpath={.metadata.labels.nvidia\.com/gpu\.product}' 2> /dev/null || true)"
        if [ -z "${product}" ]; then
            failures="${failures}${node}: label nvidia.com/gpu.product is absent"$'\n'
        fi

        clique="$(k get node "${node}" \
            -o 'jsonpath={.metadata.labels.nvidia\.com/gpu\.clique}' 2> /dev/null || true)"
        if [ "${clique}" != "${want_clique}" ]; then
            failures="${failures}${node}: label nvidia.com/gpu.clique is '${clique:-<none>}', expected ${want_clique}"$'\n'
        fi

        for label in \
            'accelerator\.topograph\.run/sub-domain' \
            'accelerator\.topograph\.run/domain' \
            'fabric\.topograph\.run/tier-0' \
            'fabric\.topograph\.run/tier-1'; do
            value="$(k get node "${node}" \
                -o "jsonpath={.metadata.labels.${label}}" 2> /dev/null || true)"
            if [ -z "${value}" ]; then
                plain="${label//\\/}"
                failures="${failures}${node}: label ${plain} is absent"$'\n'
            fi
        done

        summary="${summary}[mokka] ${node}: nvidia.com/gpu=${allocatable:-<none>} product=${product:-<none>} clique=${clique:-<none>}"$'\n'
    done

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
    printf '%s\n' "compute-domain/up.sh: ${CLUSTER} did not become a two clique lane within ${MOKKA_READY_TIMEOUT}s:" >&2
    printf '%s' "${failures}" >&2
    exit 5
fi

# ---------------------------------------------------------------------------
# Kueue
# ---------------------------------------------------------------------------
#
# Fetched, checked, then applied. The checksum is verified before kubectl is
# ever pointed at the file: a manifest that does not match the pin installs a
# cluster scoped controller with webhooks, and finding out afterwards is too
# late.

mokka::phase "install Kueue ${KUEUE_VERSION}"
rm -f "${KUEUE_MANIFEST}"
curl -fsSLo "${KUEUE_MANIFEST}" "${KUEUE_MANIFEST_URL}"

if ! got="$(cd::sha256 "${KUEUE_MANIFEST}")"; then
    mokka::die 3 "compute-domain/up.sh: neither sha256sum nor shasum is available to check ${KUEUE_MANIFEST}"
fi
if [ "${got}" != "${KUEUE_SHA256}" ]; then
    rm -f "${KUEUE_MANIFEST}"
    mokka::die 6 "compute-domain/up.sh: Kueue ${KUEUE_VERSION} manifest digest is ${got}, expected ${KUEUE_SHA256}; nothing was applied"
fi

k apply --server-side -f "${KUEUE_MANIFEST}"
k --namespace "${KUEUE_NAMESPACE}" rollout status "deployment/${KUEUE_DEPLOYMENT}" \
    --timeout "${MOKKA_READY_TIMEOUT}s"

# The Topology and the ResourceFlavor go through Kueue's validating webhook,
# which answers only once the controller has its certificate. A rolled out
# deployment is not yet that, so the apply is retried until the deadline
# rather than failing on the first rejection.
mokka::phase "apply the Kueue topology objects"
DEADLINE="$(cd::deadline)"
applied=false
apply_err=""
while :; do
    if apply_err="$(k apply -f "${SCRIPT_DIR}/kueue-topology.yaml" 2>&1)"; then
        printf '%s\n' "${apply_err}"
        applied=true
        break
    fi
    if [ "$(date +%s)" -ge "${DEADLINE}" ]; then
        break
    fi
    sleep "${MOKKA_POLL_INTERVAL}"
done

if [ "${applied}" != true ]; then
    printf '%s\n' "compute-domain/up.sh: the Kueue topology objects were not accepted within ${MOKKA_READY_TIMEOUT}s:" >&2
    printf '%s\n' "${apply_err}" >&2
    exit 5
fi

# ---------------------------------------------------------------------------
# The namespace the suite puts its jobs in
# ---------------------------------------------------------------------------

mokka::phase "create namespace ${CD_JOB_NAMESPACE}"
# Not "create --dry-run | apply -f -": a pipeline reports the reader's exit
# code, so a kubectl that refused the namespace would be read as a success or,
# when it never read its stdin, as the writer's SIGPIPE. One call, and the one
# failure that is not a failure is named.
if ! ns_out="$(k create namespace "${CD_JOB_NAMESPACE}" 2>&1)"; then
    case "${ns_out}" in
        *"already exists"*)
            printf '[mokka] namespace %s was already there\n' "${CD_JOB_NAMESPACE}"
            ;;
        *)
            printf '%s\n' "${ns_out}" >&2
            mokka::die 1 "compute-domain/up.sh: could not create namespace ${CD_JOB_NAMESPACE}"
            ;;
    esac
else
    printf '%s\n' "${ns_out}"
fi

mokka::phase

printf '\n'
printf 'lane %s is up: %d workers, 4 simulated GPUs each (profile %s)\n' \
    "${LANE}" "${worker_count}" "${CD_PROFILE}"
printf 'cliques:    %s.0 and %s.1\n' "${CD_CLUSTER_UUID}" "${CD_CLUSTER_UUID}"
printf 'kueue:      %s with Topology/ResourceFlavor %s\n' \
    "${KUEUE_VERSION}" "${CD_DOMAIN_NAME}"
printf 'kubeconfig: %s\n' "${KUBECONFIG_OUT}"
printf 'context:    %s\n' "${CONTEXT}"
printf '\n'
printf 'the lane is not in your default kubeconfig; reach it with\n'
printf '  kubectl --kubeconfig %s --context %s get nodes\n' "${KUBECONFIG_OUT}" "${CONTEXT}"
