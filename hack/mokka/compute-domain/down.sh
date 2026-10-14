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
# Tears the compute domain lane down.
#
# Usage: hack/mokka/compute-domain/down.sh [lane]
#
# Exit codes:
#   0  the lane cluster was deleted
#   1  kind failed to delete it; its own output says why
#   2  bad usage, or no cluster named kubectl-nv-<lane>
#   3  a required tool is missing
#
# Only kubectl-nv-<lane> is ever deleted, and the name is matched whole, so a
# neighbouring cluster whose name contains the lane name is left alone. As in
# hack/mokka/down.sh, kind is pointed at the lane kubeconfig so that the
# rewrite it does on delete reaches that file and never the caller's default.

set -euo pipefail
umask 077

MOKKA_PROG="compute-domain/down.sh"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${SCRIPT_DIR}/lib.sh"

if [ "$#" -gt 1 ]; then
    printf 'usage: compute-domain/down.sh [lane]\n' >&2
    exit 2
fi

LANE="${1:-${CD_DEFAULT_LANE}}"

if ! mokka::valid_lane "${LANE}"; then
    mokka::die 2 "compute-domain/down.sh: lane must be a DNS label of at most 32 characters (lowercase letters, digits, hyphens)"
fi

mokka::require_tools kind

CLUSTER="$(mokka::cluster_name "${LANE}")"
BIN_DIR="${MOKKA_BIN_DIR:-${SCRIPT_DIR}/../bin}"
KUBECONFIG_OUT="${BIN_DIR}/kubeconfig-${LANE}"

export KUBECONFIG="${KUBECONFIG_OUT}"

if ! mokka::cluster_exists "${CLUSTER}"; then
    mokka::die 2 "compute-domain/down.sh: no such cluster: ${CLUSTER}"
fi

mokka::phase "delete cluster ${CLUSTER}"
kind delete cluster --name "${CLUSTER}" --kubeconfig "${KUBECONFIG_OUT}"
mokka::phase

# The lane kubeconfig and the two artifacts up.sh wrote beside it. Each is
# named after the lane, so nothing another lane left behind is in reach.
rm -f "${KUBECONFIG_OUT}" \
    "${BIN_DIR}/topology-values-${LANE}.yaml" \
    "${BIN_DIR}/kueue-${KUEUE_VERSION}-${LANE}.yaml"

printf 'lane %s is down\n' "${LANE}"
