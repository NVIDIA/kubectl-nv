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
# Tears the MIG lane down.
#
# Usage: hack/mokka/mig/down.sh <lane>
#
# Exit codes:
#   0  the lane cluster was deleted
#   1  kind failed to delete it; its own output says why
#   2  bad usage, or no cluster named kubectl-nv-<lane>
#   3  a required tool is missing
#
# Only kubectl-nv-<lane> is ever deleted, and the name is matched whole, so a
# neighbouring cluster whose name contains the lane name is left alone. There
# is no "delete everything" mode on purpose: these clusters share a Docker
# daemon with whatever else the developer is running.
#
# The nvml-mock image this lane built is deliberately NOT removed. It is tagged
# with the pinned commit's short SHA, rebuilding it costs several minutes, and
# the next run of the lane is the thing that wants it.

set -euo pipefail
umask 077

MOKKA_PROG="mig/down.sh"
MIG_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${MIG_SCRIPT_DIR}/lib.sh"

if [ "$#" -ne 1 ]; then
    printf 'usage: mig/down.sh <lane>\n' >&2
    exit 2
fi

LANE="$1"

if ! mokka::valid_lane "${LANE}"; then
    mokka::die 2 "mig/down.sh: lane must be a DNS label of at most 32 characters (lowercase letters, digits, hyphens)"
fi

mokka::require_tools kind

CLUSTER="$(mokka::cluster_name "${LANE}")"
mig::require_lane_cluster "${CLUSTER}"

BIN_DIR="${MOKKA_BIN_DIR:-${MIG_SCRIPT_DIR}/../bin}"
KUBECONFIG_OUT="${BIN_DIR}/kubeconfig-${LANE}"

export KUBECONFIG="${KUBECONFIG_OUT}"

if ! mokka::cluster_exists "${CLUSTER}"; then
    mokka::die 2 "mig/down.sh: no such cluster: ${CLUSTER}"
fi

mokka::phase "delete cluster ${CLUSTER}"
kind delete cluster --name "${CLUSTER}" --kubeconfig "${KUBECONFIG_OUT}"
mokka::phase

rm -f "${KUBECONFIG_OUT}"

printf 'lane %s is down\n' "${LANE}"
