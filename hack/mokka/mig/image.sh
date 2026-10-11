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
# Building the MIG lane's nvml-mock image. Sourced by build-image.sh and by
# up.sh, so the two cannot build different images.
#
# shellcheck disable=SC2034
# As in lib.sh: what this file defines is read by its callers.

# mig::image_ref
# The local tag the chart pulls by. Never pushed anywhere: it names an image
# that exists in the developer's (or the runner's) Docker daemon and, after
# `kind load`, inside the lane's nodes.
mig::image_ref() {
    printf '%s:%s' "${MIG_NVML_MOCK_IMAGE}" "${MIG_NVML_MOCK_IMAGE_TAG}"
}

# mig::build_image <force>
# Leaves mig::image_ref in the local Docker daemon. With force false, an image
# already carrying that tag is kept: the tag names one commit, so rebuilding it
# produces the same thing at the cost of several minutes.
mig::build_image() {
    local force="${1:-false}"
    local ref
    ref="$(mig::image_ref)"

    if [ "${force}" != true ] && docker image inspect "${ref}" > /dev/null 2>&1; then
        printf '[mig] %s is already in the local Docker daemon; not rebuilding (--force to rebuild)\n' "${ref}"
        return 0
    fi

    # A caller that has already resolved the tree (up.sh stages it for the
    # chart whether or not the image needs building) hands it over rather than
    # having it extracted a second time.
    if [ -z "${MIG_UPSTREAM_TREE}" ]; then
        mokka::phase "stage ${MIG_UPSTREAM_REPO} at ${MIG_UPSTREAM_SHA}"
        local staging="${MIG_STAGING_DIR:-}"
        if [ -z "${staging}" ]; then
            staging="$(mktemp -d "${TMPDIR:-/tmp}/kubectl-nv-mig-upstream.XXXXXX")"
            # Only a temporary directory this function created is this
            # function's to delete. A caller-supplied MIG_STAGING_DIR is left
            # alone, and so is a tree the caller already staged.
            # shellcheck disable=SC2064
            trap "rm -rf '${staging}'" RETURN
        fi
        mig::stage_upstream "${staging}"
    fi

    mokka::phase "build $(mig::image_ref)"
    docker build \
        --tag "${ref}" \
        --file "${MIG_UPSTREAM_TREE}/deployments/nvml-mock/Dockerfile" \
        "${MIG_UPSTREAM_TREE}"

    # `docker build` reporting success and leaving no such tag has happened
    # often enough (a buildx driver that exports nothing to the local daemon is
    # the usual way) that the exit code is not taken as evidence on its own.
    if ! docker image inspect "${ref}" > /dev/null 2>&1; then
        mokka::die 1 "${MOKKA_PROG}: docker build reported success but ${ref} is not in the local daemon; a buildx driver that does not load into it is the usual cause"
    fi
}
