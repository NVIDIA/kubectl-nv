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
# Adds fake GPU nodes to a Mokka lane with KWOK, so that kubectl-nv can be
# measured against a cluster of a size no laptop and no CI runner could hold
# for real. A KWOK node is an API object with a controller answering for it:
# there is no kubelet, no container runtime and no pod, which is exactly what
# makes two thousand of them affordable and exactly why they are no substitute
# for the real workers the lane already has.
#
# Usage: hack/mokka/kwok-nodes.sh <count> [<lane>]
#        hack/mokka/kwok-nodes.sh --delete [<lane>]
#
#   count  how many fake nodes to create, named kwok-gpu-1 .. kwok-gpu-<count>
#   lane   the Mokka lane to add them to; defaults to "scale"
#
# Exit codes:
#   0  the nodes are there and Ready, or they were deleted
#   1  an underlying tool failed; its own output says why
#   2  bad usage
#   3  a required tool is missing
#   5  the nodes did not become Ready and advertise their GPUs in time
#   6  a downloaded KWOK manifest did not match its pinned sha256
#   7  the target context is not a kubectl-nv lane, so nothing was touched
#
# This script never reads or writes the caller's default kubeconfig: it uses
# the lane kubeconfig up.sh wrote, under hack/mokka/bin, and names it on every
# call.

set -euo pipefail
umask 077

MOKKA_PROG="kwok-nodes.sh"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${SCRIPT_DIR}/lib.sh"

# ---------------------------------------------------------------------------
# The pinned KWOK release
# ---------------------------------------------------------------------------
#
# v0.8.0, published 2026-06-23, is the newest release that is neither a draft
# nor a pre-release. It was picked with two read-only queries against the
# kubernetes-sigs/kwok releases endpoint: one listing the tags with draft and
# prerelease false, one listing the assets of the tag that came out on top.
#
# The newest is the one taken because the previous release, v0.7.0, predates
# the Kubernetes line the Mokka node image is built on (kindest/node v1.35.0),
# and a node lifecycle controller that has not seen the API version it is
# answering for is a poor thing to debug at two thousand nodes.
#
# Two manifests are needed, not one. kwok.yaml is the CRDs, the RBAC and the
# controller; stage-fast.yaml is the Stage objects that tell the controller
# what to do with a node, and without them a fake node is created and then sits
# there forever with no conditions at all.
#
# The digests below were taken from the release assets on 2026-10-11 and are
# written out here rather than fetched alongside the manifests: a checksum file
# served by whoever served the file it describes witnesses for nothing. These
# lines are reviewed, and a manifest that stops matching one stops the script.
# To refresh: bump KWOK_VERSION, download both assets and
#   shasum -a 256 kwok.yaml stage-fast.yaml
KWOK_VERSION="v0.8.0"
KWOK_RELEASE_URL="https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}"
KWOK_MANIFESTS="
kwok.yaml:a4c16e6431e382dcb5c1903139344b7a68652f16a6460337fe17a678a426f405
stage-fast.yaml:2f28d95564ec43056c0873f7a25ac7d2a5bba4c8496c72f8b3ee73fd4f54ee24
"

# The annotation KWOK's shipped configuration selects nodes on
# (manageNodesWithAnnotationSelector in kwok.yaml). A node without it is
# ignored by the controller and never goes Ready.
KWOK_NODE_ANNOTATION="kwok.x-k8s.io/node"

# Every node this script creates carries this label, and --delete selects on
# it. Deleting by name prefix would depend on a convention; deleting by a label
# only this script writes cannot reach a node it did not create.
KWOK_OWNER_LABEL="kubectl-nv.nvidia.com/kwok-node"

# ---------------------------------------------------------------------------
# What a fake node advertises
# ---------------------------------------------------------------------------
#
# A T4 class node as the t4 lane's own workers present it, so that the lane is
# one population and not two. Every value below was read off
# kubectl-nv-scale-worker on 2026-10-11, on a lane brought up by
# "hack/mokka/up.sh scale t4", rather than written from memory.
#
# The one deliberate difference is the GPU count. The nvml-mock t4 profile
# gives a real worker 4 (mokka::profile_gpu_count in lib.sh); the brief fixes
# the fake nodes at 8. It is one number here, and the gpu.count label and both
# resource lists are built from it, so whatever it is set to the node never
# contradicts itself. Override it with KWOK_GPU_COUNT.
#
# What is NOT written here is as deliberate. nvidia.com/mig.strategy does not
# appear on this lane's workers, so it does not appear on a fake node either.
# nvidia.com/gpu.deploy.* is written by the GPU Operator's own state labeller,
# which adopts any node carrying the PCI label within seconds of it appearing;
# setting those here would be a second writer for a label that already has one.
KWOK_GPU_COUNT="${KWOK_GPU_COUNT:-8}"
KWOK_GPU_PRODUCT="${KWOK_GPU_PRODUCT:-NVIDIA-T4}"
KWOK_GPU_FAMILY="${KWOK_GPU_FAMILY:-turing}"
KWOK_GPU_MEMORY="${KWOK_GPU_MEMORY:-16384}"
KWOK_COMPUTE_MAJOR="${KWOK_COMPUTE_MAJOR:-7}"
KWOK_COMPUTE_MINOR="${KWOK_COMPUTE_MINOR:-5}"
KWOK_DRIVER_VERSION="${KWOK_DRIVER_VERSION:-550.163.01}"
KWOK_CUDA_VERSION="${KWOK_CUDA_VERSION:-12.4}"

# The inventory builder reads two generations of the CUDA labels: the current
# cuda.driver-version.* and cuda.runtime-version.* names, and the older
# cuda.driver.* and cuda.runtime.* ones. The lane carries both, so a fake node
# carries both, and both are split from the one version above rather than
# written out again: two sources for one number is how they come to disagree.
if ! [[ ${KWOK_DRIVER_VERSION} =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    mokka::die 2 "kwok-nodes.sh: KWOK_DRIVER_VERSION must be <major>.<minor>.<revision>, got '${KWOK_DRIVER_VERSION}'"
fi
KWOK_DRIVER_MAJOR="${BASH_REMATCH[1]}"
KWOK_DRIVER_MINOR="${BASH_REMATCH[2]}"
KWOK_DRIVER_REV="${BASH_REMATCH[3]}"

if ! [[ ${KWOK_CUDA_VERSION} =~ ^([0-9]+)\.([0-9]+)$ ]]; then
    mokka::die 2 "kwok-nodes.sh: KWOK_CUDA_VERSION must be <major>.<minor>, got '${KWOK_CUDA_VERSION}'"
fi
KWOK_CUDA_MAJOR="${BASH_REMATCH[1]}"
KWOK_CUDA_MINOR="${BASH_REMATCH[2]}"

# How many objects go into one kubectl call. Two thousand nodes one call at a
# time is two thousand process spawns; one call is a request body the API
# server will not take.
KWOK_BATCH_SIZE="${KWOK_BATCH_SIZE:-100}"

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------

usage() {
    printf 'usage: kwok-nodes.sh <count> [<lane>]\n' >&2
    printf '       kwok-nodes.sh --delete [<lane>]\n' >&2
}

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    usage
    exit 2
fi

MODE="create"
COUNT=0
if [ "$1" = "--delete" ]; then
    MODE="delete"
else
    COUNT="$1"
    # Anchored against the whole argument: a line-oriented match would accept
    # an embedded newline and read only its first line.
    if ! [[ ${COUNT} =~ ^[1-9][0-9]*$ ]]; then
        usage
        mokka::die 2 "kwok-nodes.sh: count must be a whole number above zero, got '${COUNT}'"
    fi
fi

LANE="${2:-scale}"
if ! mokka::valid_lane "${LANE}"; then
    usage
    mokka::die 2 "kwok-nodes.sh: lane must be a DNS label of at most 32 characters (lowercase letters, digits, hyphens)"
fi

if ! [[ ${KWOK_GPU_COUNT} =~ ^[1-9][0-9]*$ ]]; then
    mokka::die 2 "kwok-nodes.sh: KWOK_GPU_COUNT must be a whole number above zero, got '${KWOK_GPU_COUNT}'"
fi

if ! [[ ${KWOK_BATCH_SIZE} =~ ^[1-9][0-9]*$ ]]; then
    mokka::die 2 "kwok-nodes.sh: KWOK_BATCH_SIZE must be a whole number above zero, got '${KWOK_BATCH_SIZE}'"
fi

BIN_DIR="${MOKKA_BIN_DIR:-${SCRIPT_DIR}/bin}"
KUBECONFIG_IN="${MOKKA_KUBECONFIG:-${BIN_DIR}/kubeconfig-${LANE}}"
CONTEXT="${MOKKA_CONTEXT:-$(mokka::context "${LANE}")}"

# ---------------------------------------------------------------------------
# The context guard
# ---------------------------------------------------------------------------
#
# This script creates and deletes Node objects. Against a real cluster that is
# a disaster, so the name of the context it would act on is checked before any
# tool is reached for, and the check is anchored at both ends: a context that
# merely ends with a lane name, or that carries a second word after one, is
# not a lane of ours.

if ! [[ ${CONTEXT} =~ ^kind-kubectl-nv-[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$ ]]; then
    mokka::die 7 "kwok-nodes.sh: refusing to touch context '${CONTEXT}': only a kind-kubectl-nv-<lane> context is a Mokka lane"
fi

mokka::require_tools kubectl curl shasum

# Belt and braces alongside the explicit --kubeconfig below.
export KUBECONFIG="${KUBECONFIG_IN}"

k() {
    kubectl --kubeconfig "${KUBECONFIG_IN}" --context "${CONTEXT}" "$@"
}

# ---------------------------------------------------------------------------
# --delete
# ---------------------------------------------------------------------------

if [ "${MODE}" = "delete" ]; then
    mokka::phase "delete the fake nodes of lane ${LANE}"
    # By the ownership label and never --all: the lane's real workers are
    # nodes too, and this script did not make them.
    k delete nodes --selector "${KWOK_OWNER_LABEL}=true" --ignore-not-found --wait=false
    mokka::phase
    printf 'lane %s: the nodes labelled %s=true are gone; KWOK itself was left installed\n' \
        "${LANE}" "${KWOK_OWNER_LABEL}"
    exit 0
fi

# ---------------------------------------------------------------------------
# KWOK
# ---------------------------------------------------------------------------

install_kwok() {
    local tmp entry name sha
    tmp="$(mktemp -d "${TMPDIR:-/tmp}/kwok-manifests.XXXXXX")"

    # Every manifest is fetched and checked before any of them is applied. A
    # loop that applied as it went would leave the CRDs and the controller
    # behind when the second file turned out to be wrong.
    for entry in ${KWOK_MANIFESTS}; do
        name="${entry%%:*}"
        sha="${entry##*:}"
        if ! curl -fsSLo "${tmp}/${name}" "${KWOK_RELEASE_URL}/${name}"; then
            rm -rf "${tmp}"
            mokka::die 1 "kwok-nodes.sh: could not download ${KWOK_RELEASE_URL}/${name}"
        fi
        if ! printf '%s  %s\n' "${sha}" "${tmp}/${name}" | shasum -a 256 -c - > /dev/null 2>&1; then
            rm -rf "${tmp}"
            mokka::die 6 "kwok-nodes.sh: ${name} does not match its pinned sha256 for KWOK ${KWOK_VERSION}; nothing was applied"
        fi
    done

    # Server-side apply: kwok.yaml is large enough that the client-side
    # last-applied annotation runs into the annotation size limit.
    k apply --server-side --force-conflicts -f "${tmp}/kwok.yaml"
    # The Stage objects are instances of a CRD the line above created, so the
    # CRD has to be established before they can be applied.
    k wait --for=condition=Established --timeout=120s crd/stages.kwok.x-k8s.io
    k --namespace kube-system rollout status deployment/kwok-controller --timeout=300s
    k apply --server-side --force-conflicts -f "${tmp}/stage-fast.yaml"

    rm -rf "${tmp}"
}

mokka::phase "ensure KWOK ${KWOK_VERSION} is installed in lane ${LANE}"
if k --namespace kube-system get deployment kwok-controller -o name > /dev/null 2>&1; then
    printf '[mokka] kwok-controller is already there; leaving it alone\n'
else
    install_kwok
fi

# ---------------------------------------------------------------------------
# The nodes
# ---------------------------------------------------------------------------
#
# status is set on the object rather than patched afterwards. The node
# lifecycle Stage in stage-fast.yaml fills allocatable and capacity only when
# the node does not already carry them, so what is written here is what the
# node keeps, and two thousand status patches is a pass this does not need.
#
# Nothing below is a comment: every line of this heredoc is sent to the API
# server, two thousand times over, so the reasons live out here.
#
# The taint is NoSchedule and not NoExecute. It is what keeps the GPU
# Operator's operands off these nodes, which matters: the operator's state
# labeller does adopt them, writing nvidia.com/gpu.deploy.* within seconds of
# a node appearing, and without the taint every operand DaemonSet would
# follow it.
#
# It has no answer to a DaemonSet that tolerates everything with a bare
# "operator: Exists". A 20 node smoke on 2026-10-11 found exactly three such
# pods per fake node and no others: kube-system/kindnet,
# kube-system/kube-proxy and mokka/nvml-mock, 60 across 20 nodes, all Running.
# Those are fakes too, answered for by KWOK's pod Stages, so each costs an API
# object and no compute. Budget three pods per fake node when sizing a lane.

node_yaml() {
    local n="$1"
    cat <<YAML
---
apiVersion: v1
kind: Node
metadata:
  name: kwok-gpu-${n}
  annotations:
    ${KWOK_NODE_ANNOTATION}: fake
    node.alpha.kubernetes.io/ttl: "0"
  labels:
    ${KWOK_OWNER_LABEL}: "true"
    kubernetes.io/arch: amd64
    kubernetes.io/hostname: kwok-gpu-${n}
    kubernetes.io/os: linux
    type: kwok
    feature.node.kubernetes.io/pci-10de.present: "true"
    nvidia.com/gpu.present: "true"
    nvidia.com/gpu.product: ${KWOK_GPU_PRODUCT}
    nvidia.com/gpu.machine: ${KWOK_GPU_PRODUCT}
    nvidia.com/gpu.family: ${KWOK_GPU_FAMILY}
    nvidia.com/gpu.count: "${KWOK_GPU_COUNT}"
    nvidia.com/gpu.memory: "${KWOK_GPU_MEMORY}"
    nvidia.com/gpu.mode: compute
    nvidia.com/gpu.compute.major: "${KWOK_COMPUTE_MAJOR}"
    nvidia.com/gpu.compute.minor: "${KWOK_COMPUTE_MINOR}"
    nvidia.com/gpu.replicas: "1"
    nvidia.com/gpu.sharing-strategy: none
    nvidia.com/mig.capable: "true"
    nvidia.com/mps.capable: "false"
    nvidia.com/vgpu.present: "false"
    nvidia.com/cuda.driver-version.full: "${KWOK_DRIVER_VERSION}"
    nvidia.com/cuda.driver-version.major: "${KWOK_DRIVER_MAJOR}"
    nvidia.com/cuda.driver-version.minor: "${KWOK_DRIVER_MINOR}"
    nvidia.com/cuda.driver-version.revision: "${KWOK_DRIVER_REV}"
    nvidia.com/cuda.driver.major: "${KWOK_DRIVER_MAJOR}"
    nvidia.com/cuda.driver.minor: "${KWOK_DRIVER_MINOR}"
    nvidia.com/cuda.driver.rev: "${KWOK_DRIVER_REV}"
    nvidia.com/cuda.runtime-version.full: "${KWOK_CUDA_VERSION}"
    nvidia.com/cuda.runtime-version.major: "${KWOK_CUDA_MAJOR}"
    nvidia.com/cuda.runtime-version.minor: "${KWOK_CUDA_MINOR}"
    nvidia.com/cuda.runtime.major: "${KWOK_CUDA_MAJOR}"
    nvidia.com/cuda.runtime.minor: "${KWOK_CUDA_MINOR}"
spec:
  taints:
    - key: ${KWOK_NODE_ANNOTATION}
      value: fake
      effect: NoSchedule
status:
  allocatable:
    cpu: "32"
    memory: 128Gi
    pods: "110"
    nvidia.com/gpu: "${KWOK_GPU_COUNT}"
  capacity:
    cpu: "32"
    memory: 128Gi
    pods: "110"
    nvidia.com/gpu: "${KWOK_GPU_COUNT}"
YAML
}

mokka::phase "create ${COUNT} fake nodes in batches of ${KWOK_BATCH_SIZE}"
BATCH_FILE="$(mktemp "${TMPDIR:-/tmp}/kwok-batch.XXXXXX")"
trap 'rm -f "${BATCH_FILE}"' EXIT

batch_count=0
i=1
while [ "${i}" -le "${COUNT}" ]; do
    : > "${BATCH_FILE}"
    in_batch=0
    while [ "${i}" -le "${COUNT}" ] && [ "${in_batch}" -lt "${KWOK_BATCH_SIZE}" ]; do
        node_yaml "${i}" >> "${BATCH_FILE}"
        i=$((i + 1))
        in_batch=$((in_batch + 1))
    done
    # Redirected rather than piped: a pipe would hand this loop the exit code
    # of the thing on its left and a failed apply would go unnoticed.
    k apply -f - < "${BATCH_FILE}"
    batch_count=$((batch_count + 1))
done
printf '[mokka] %d nodes applied in %d calls\n' "${COUNT}" "${batch_count}"

# ---------------------------------------------------------------------------
# Wait until they are nodes a reader would believe
# ---------------------------------------------------------------------------
#
# Ready alone is not enough: KWOK marks a node Ready as soon as it adopts it,
# and the thing this lane exists to measure is the GPU resource. Both are read
# in one query, one line per node, and the line is matched whole.

WANT_LINE="${KWOK_GPU_COUNT} True"
DEADLINE=$(( $(date +%s) + MOKKA_READY_TIMEOUT ))

mokka::phase "wait for ${COUNT} fake nodes to be Ready with ${KWOK_GPU_COUNT} GPUs"
ready=0
while :; do
    lines="$(k get nodes --selector "${KWOK_OWNER_LABEL}=true" \
        -o 'jsonpath={range .items[*]}{.status.allocatable.nvidia\.com/gpu}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' \
        2> /dev/null || true)"
    # grep -c exits 1 on a count of zero, which under set -e would end the
    # run with "an underlying tool failed" rather than with the timeout this
    # loop exists to report.
    ready="$(printf '%s\n' "${lines}" | grep -cxF -- "${WANT_LINE}" | tr -d ' ' || true)"
    [ -n "${ready}" ] || ready=0
    if [ "${ready}" -ge "${COUNT}" ]; then
        break
    fi
    if [ "$(date +%s)" -ge "${DEADLINE}" ]; then
        mokka::die 5 "kwok-nodes.sh: only ${ready} of ${COUNT} fake nodes are Ready with ${KWOK_GPU_COUNT} allocatable GPUs after ${MOKKA_READY_TIMEOUT}s"
    fi
    sleep "${MOKKA_POLL_INTERVAL}"
done

mokka::phase

printf '\n'
printf 'lane %s: %d fake nodes (kwok-gpu-1 .. kwok-gpu-%d), %s simulated GPUs each, KWOK %s\n' \
    "${LANE}" "${COUNT}" "${COUNT}" "${KWOK_GPU_COUNT}" "${KWOK_VERSION}"
printf 'they carry the %s=fake taint, which keeps the GPU Operator operands off.\n' "${KWOK_NODE_ANNOTATION}"
printf 'three DaemonSets tolerate everything and land anyway (kindnet, kube-proxy,\n'
printf 'nvml-mock), so expect three fake pods per fake node.\n'
printf 'remove them with\n'
printf '  hack/mokka/kwok-nodes.sh --delete %s\n' "${LANE}"
