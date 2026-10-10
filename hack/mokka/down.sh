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
# Tears one Mokka lane down.
#
# Usage: hack/mokka/down.sh <lane>
#
# Exit codes:
#   0  the lane cluster was deleted
#   2  bad usage, or no cluster named kubectl-nv-<lane>
#   3  a required tool is missing
#
# Only kubectl-nv-<lane> is ever deleted, and the name is matched whole, so a
# neighbouring cluster whose name contains the lane name is left alone. There
# is no "delete everything" mode on purpose: these clusters share a Docker
# daemon with whatever else the developer is running.

set -euo pipefail

MOKKA_PROG="down.sh"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/mokka/lib.sh
. "${SCRIPT_DIR}/lib.sh"

if [ "$#" -ne 1 ]; then
    printf 'usage: down.sh <lane>\n' >&2
    exit 2
fi

LANE="$1"

if ! mokka::valid_lane "${LANE}"; then
    mokka::die 2 "down.sh: lane must be a DNS label (lowercase letters, digits, hyphens): '${LANE}'"
fi

mokka::require_tools kind

CLUSTER="$(mokka::cluster_name "${LANE}")"
BIN_DIR="${MOKKA_BIN_DIR:-${SCRIPT_DIR}/bin}"

if ! mokka::cluster_exists "${CLUSTER}"; then
    mokka::die 2 "down.sh: no such cluster: ${CLUSTER}"
fi

mokka::phase "delete cluster ${CLUSTER}"
kind delete cluster --name "${CLUSTER}"
mokka::phase

rm -f "${BIN_DIR}/kubeconfig-${LANE}"

printf 'lane %s is down\n' "${LANE}"
