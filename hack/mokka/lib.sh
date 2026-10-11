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
# Pins and helpers shared by hack/mokka/up.sh and hack/mokka/down.sh.
#
# This file is sourced, not executed. Every pin lives here exactly once so that
# a bump is one edit and a reader can see the whole version surface at a glance.
#
# shellcheck disable=SC2034
# SC2034 is disabled for the whole file: every pin below is read by up.sh or
# down.sh after they source this file, which shellcheck cannot see when it is
# pointed at this file on its own. Linting it standalone is worth keeping, so
# the alternative of only ever running `shellcheck -x up.sh` is not taken.

# ---------------------------------------------------------------------------
# Kind node image
# ---------------------------------------------------------------------------
#
# Provenance: ghcr.io/nvidia/mokka-kind-node, pinned by digest on 2026-10-10.
# The digest is the one tag sha-0960c63 resolves to; it is pinned rather than
# named because the repository publishes no release tag, so a tag could be
# moved under us. It is a multi-arch index covering linux/amd64 and
# linux/arm64, which is what lets the same pin serve CI and an Apple silicon
# laptop.
#
# To refresh: pick the new tag, resolve it to an index digest with
#   crane digest ghcr.io/nvidia/mokka-kind-node:<tag>
# or
#   docker buildx imagetools inspect ghcr.io/nvidia/mokka-kind-node:<tag>
# then replace the value below and record the new tag and the date here.
MOKKA_KIND_NODE_IMAGE="ghcr.io/nvidia/mokka-kind-node@sha256:8bd910853e49bd46953c95b646d35e4765fa7097b18765040b3d95dbc376f524"

# ---------------------------------------------------------------------------
# nvml-mock chart
# ---------------------------------------------------------------------------
MOKKA_CHART="oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock"
MOKKA_CHART_VERSION="0.4.0"
MOKKA_RELEASE="nvml-mock"
MOKKA_NAMESPACE="mokka"

# ---------------------------------------------------------------------------
# GPU Operator
# ---------------------------------------------------------------------------
#
# v26.3.3 and not a v26.7.x: v26.7 gates its operands on an `nvidia` line in
# /proc/modules, which the simulated devices cannot serve, so the operands never
# start. See docs/guides/with-gpu-operator/run.sh:55-57 in NVIDIA/k8s-test-infra
# at 4ce11e5.
GPU_OPERATOR_REPO_NAME="nvidia"
GPU_OPERATOR_REPO_URL="https://helm.ngc.nvidia.com/nvidia"
GPU_OPERATOR_CHART="nvidia/gpu-operator"
GPU_OPERATOR_VERSION="v26.3.3"
GPU_OPERATOR_RELEASE="gpu-operator"
GPU_OPERATOR_NAMESPACE="gpu-operator"

# The operand DaemonSets that make a node a GPU node: these two put
# nvidia.com/gpu into node allocatable and the nvidia.com/* labels onto it. The
# validator and the exporter do neither.
#
# Every lane that builds on this file reads this list, the compute domain lane
# included, so it holds only what is true of all of them. What one lane
# additionally waits for belongs in that lane's own list.
MOKKA_OPERAND_DAEMONSETS="nvidia-device-plugin-daemonset gpu-feature-discovery"

# What hack/mokka/up.sh waits for before it calls its lane ready, which is more
# than the list above.
#
# The exporter is here because of what reads the lane next rather than because
# of what it writes. Its readiness probe carries a 45 second
# initialDelaySeconds, so for three quarters of a minute after the other two
# are ready it is a GPU Operator operand that the ClusterPolicy enables and
# that is not Ready; `kubectl nv health` reports that as NV-DCGM-001 at
# warning, and the healthy gate that runs straight after this script then fails
# on a lane that is merely still starting.
#
# A lane whose values file disabled the exporter would wait here for the whole
# ready timeout and then fail, because an absent DaemonSet is read as "not
# yet". That is why this is the Mokka lane's own list and not the shared one:
# hack/mokka/gpu-operator-values.yaml enables the exporter, and
# hack/mokka/compute-domain has its own values and its own answer.
MOKKA_GATE_DAEMONSETS="${MOKKA_OPERAND_DAEMONSETS} nvidia-dcgm-exporter"

# ---------------------------------------------------------------------------
# Timings, all overridable for tests
# ---------------------------------------------------------------------------
# A cold pull of the nvml-mock image runs about eight minutes per node, so the
# helm installs get a 15m wait rather than the 5m default.
MOKKA_HELM_TIMEOUT="${MOKKA_HELM_TIMEOUT:-15m}"
# How long to wait, in seconds, for the operands to roll out and for the node
# assertions to come true, and how long to sleep between polls.
MOKKA_READY_TIMEOUT="${MOKKA_READY_TIMEOUT:-900}"
MOKKA_POLL_INTERVAL="${MOKKA_POLL_INTERVAL:-10}"

# Known profiles, in the order the usage message prints them.
MOKKA_PROFILES="a100 b200 gb200 gb300 h100 l40s t4"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# mokka::die <exit-code> <message>...
mokka::die() {
    local code="$1"
    shift
    printf '%s\n' "$*" >&2
    exit "${code}"
}

# mokka::cluster_name <lane>
mokka::cluster_name() {
    printf 'kubectl-nv-%s' "$1"
}

# mokka::context <lane>
# kind names its kubeconfig context after the cluster with a kind- prefix.
mokka::context() {
    printf 'kind-kubectl-nv-%s' "$1"
}

# mokka::valid_lane <lane>
# A lane becomes part of a cluster name, so it has to be a DNS label, and the
# match is anchored against the WHOLE argument. A line-oriented check such as
# grep accepts an embedded newline and sees only the first line, which would
# let a second line name somebody else's cluster.
mokka::valid_lane() {
    [[ $1 =~ ^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$ ]]
}

# mokka::profile_gpu_count <profile>
# Prints the number of simulated GPUs the profile advertises per node. The
# counts are the `devices:` list lengths in the nvml-mock chart's
# profiles/<profile>.yaml at k8s-test-infra 4ce11e5. Returns 1 for an unknown
# profile.
mokka::profile_gpu_count() {
    case "$1" in
        gb300|gb200|t4) printf '4' ;;
        a100|b200|h100|l40s) printf '8' ;;
        *) return 1 ;;
    esac
}

# mokka::cluster_exists <cluster>
# `kind get clusters` enumerates Docker containers; it is the one kind command
# with no --kubeconfig flag and it never opens a kubeconfig. Its output is
# captured into a variable first: under `set -o pipefail` a `grep -q` that
# stops reading early can hand kind a SIGPIPE and fail the whole command.
# Matched on the whole line, so kubectl-nv-mokka-old is not kubectl-nv-mokka.
mokka::cluster_exists() {
    local clusters
    clusters="$(kind get clusters 2> /dev/null)" || return 1
    printf '%s\n' "${clusters}" | grep -qxF -- "$1"
}

# mokka::require_tools <tool>...
mokka::require_tools() {
    local tool
    for tool in "$@"; do
        if ! command -v "${tool}" > /dev/null 2>&1; then
            mokka::die 3 "${MOKKA_PROG}: required tool not found: ${tool}"
        fi
    done
}

# mokka::phase <name>
# Closes the previous phase, printing its elapsed seconds, and opens a new one.
# Called with no argument it only closes.
MOKKA_PHASE_NAME=""
MOKKA_PHASE_START=0
mokka::phase() {
    local now
    now="$(date +%s)"
    if [ -n "${MOKKA_PHASE_NAME}" ]; then
        printf '[mokka] %s: %ss\n' "${MOKKA_PHASE_NAME}" "$((now - MOKKA_PHASE_START))"
    fi
    MOKKA_PHASE_NAME="${1:-}"
    MOKKA_PHASE_START="${now}"
    if [ -n "${MOKKA_PHASE_NAME}" ]; then
        printf '[mokka] %s ...\n' "${MOKKA_PHASE_NAME}"
    fi
}
