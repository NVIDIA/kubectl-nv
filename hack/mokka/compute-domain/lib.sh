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
# Pins and helpers for the compute domain lane, on top of hack/mokka/lib.sh.
#
# This file is sourced, not executed. It sources the Mokka lane library first,
# so the kind node image digest, the nvml-mock chart, the GPU Operator version
# and the helpers live in exactly one place and a bump there reaches this lane
# too.
#
# shellcheck disable=SC2034
# As in hack/mokka/lib.sh: every pin below is read by up.sh or down.sh after
# they source this file, which shellcheck cannot see when it lints this file
# on its own.

CD_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/../lib.sh
. "${CD_LIB_DIR}/../lib.sh"

# ---------------------------------------------------------------------------
# Lane
# ---------------------------------------------------------------------------

# The default lane name. The cluster is kubectl-nv-<lane>, and the topology
# document up.sh renders names the workers kind derives from that cluster.
CD_DEFAULT_LANE="compute-domain"

# The profile. Four simulated GPUs per node, and the one profile whose fabric
# block the topology overlay rewrites: see mokka-values.yaml.
CD_PROFILE="gb200"

# The workers the kind config declares. Four, so the lane can hold two cliques
# of two and a job that needs three nodes.
CD_WORKER_COUNT=4

# The ComputeDomain the topology document declares. The UUID is the one the
# Mokka demo uses, and the kind config bakes <uuid>.<clique> into every worker
# as nvidia.com/gpu.clique, so the two cannot drift without the assertion in
# up.sh saying so.
CD_CLUSTER_UUID="00000000-0000-0000-0000-0000000000ab"
CD_DOMAIN_NAME="kubectl-nv-compute-domain"

# ---------------------------------------------------------------------------
# Kueue
# ---------------------------------------------------------------------------
#
# v0.18.11, released 2026-10-01, the newest patch of the newest release line
# that still serves kueue.x-k8s.io/v1beta1 beside v1beta2. Both served
# versions are what the topology command reads; main has already dropped
# v1beta1, so a lane pinned there would stop covering it.
#
# The digest is of the release's manifests.yaml, written out here rather than
# fetched beside it. A checksum served from the same place as the file it
# describes witnesses for nothing; this line is reviewed, and a manifest that
# stops matching it stops the lane.
#
# To refresh: pick the release, check that its topologies and resourceflavors
# CRDs still serve the versions the command reads, then
#   curl -fsSLo manifests.yaml \
#     https://github.com/kubernetes-sigs/kueue/releases/download/<tag>/manifests.yaml
#   shasum -a 256 manifests.yaml
# and record the new tag and the date here.
KUEUE_VERSION="v0.18.11"
# Overridable only so that up_test.sh, which serves its own payload from a
# curl shim, can drive the matching branch as well as the failing one. The
# workflow does not set it, and a test below asserts that an unset
# environment leaves the reviewed digest in place.
KUEUE_SHA256="${KUEUE_SHA256:-299579286157359052a4355c421a829c81aced8b9feca52c37aefb87ea93a623}"
KUEUE_MANIFEST_URL="https://github.com/kubernetes-sigs/kueue/releases/download/${KUEUE_VERSION}/manifests.yaml"
KUEUE_NAMESPACE="kueue-system"
KUEUE_DEPLOYMENT="kueue-controller-manager"

# The namespace the e2e suite puts its suspended jobs in. Created by up.sh so
# that the suite does not have to decide whether it may create a namespace.
CD_JOB_NAMESPACE="kubectl-nv-e2e"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# cd::worker_name <cluster> <index>
# Kind names the first worker <cluster>-worker and the rest <cluster>-workerN
# counting from 2. The topology document and the lane assertions both go
# through here so they cannot disagree about it.
cd::worker_name() {
    if [ "$2" -eq 1 ]; then
        printf '%s-worker' "$1"
    else
        printf '%s-worker%d' "$1" "$2"
    fi
}

# cd::worker_clique <index>
# Workers 1 and 2 are in clique 0, workers 3 and 4 in clique 1. The same split
# the kind config's static labels carry.
cd::worker_clique() {
    if [ "$1" -le 2 ]; then
        printf '0'
    else
        printf '1'
    fi
}

# cd::sha256 <file>
# Prints the hex digest. GNU coreutils and the BSD tools on macOS disagree
# about which binary exists, so both are tried before giving up: a lane that
# silently skipped the checksum would be worse than one that refuses to run.
cd::sha256() {
    if command -v sha256sum > /dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
        return
    fi
    if command -v shasum > /dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
        return
    fi
    return 1
}
