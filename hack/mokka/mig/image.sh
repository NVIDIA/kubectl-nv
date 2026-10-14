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

# mig::image_revision
# The commit the tagged image in the local daemon says it was built from, or
# nothing when the tag is absent or carries no such label. An image built by
# anything other than this lane has no label, and neither has one built before
# the label existed; both answer empty, which is treated as "cannot be vouched
# for".
mig::image_revision() {
    docker image inspect \
        --format "{{index .Config.Labels \"${MIG_IMAGE_REVISION_LABEL}\"}}" \
        "$(mig::image_ref)" 2> /dev/null
}

# MIG_STAGING_OWNED is the temporary tree this file extracted and is therefore
# this file's to remove. A caller-supplied MIG_STAGING_DIR, and a tree the
# caller staged itself, are left alone. The callers install the EXIT trap that
# reads it: a RETURN trap does not run when `set -e` takes the shell down
# mid-function, which is exactly the path a failed docker build takes, and
# several hundred megabytes were being left in TMPDIR each time.
MIG_STAGING_OWNED=""

# mig::build_image <force>
# Leaves mig::image_ref in the local Docker daemon, built from, and labelled
# with, MIG_UPSTREAM_SHA.
#
# With force false an image already carrying that tag is kept, but only if its
# revision label agrees with the pin. The tag alone cannot decide this: it
# holds eight characters of the commit, which is a label rather than an
# identity, and an image built by hand or left over from a differently-pinned
# tree wears it just as well. Rebuilding costs several minutes; running the
# whole lane against the wrong image costs an afternoon of reading results that
# describe another commit.
mig::build_image() {
    local force="${1:-false}"
    local ref revision
    ref="$(mig::image_ref)"

    if [ "${force}" != true ] && docker image inspect "${ref}" > /dev/null 2>&1; then
        revision="$(mig::image_revision)"
        if [ "${revision}" = "${MIG_UPSTREAM_SHA}" ]; then
            printf '[mig] %s is already in the local Docker daemon, built from %s; not rebuilding (--force to rebuild)\n' \
                "${ref}" "${MIG_UPSTREAM_SHA}"
            return 0
        fi
        printf '[mig] %s is in the local Docker daemon but its %s label is %s, not %s; rebuilding\n' \
            "${ref}" "${MIG_IMAGE_REVISION_LABEL}" \
            "${revision:-<absent>}" "${MIG_UPSTREAM_SHA}"
    fi

    # A caller that has already resolved the tree (up.sh stages it for the
    # chart whether or not the image needs building) hands it over rather than
    # having it extracted a second time.
    if [ -z "${MIG_UPSTREAM_TREE}" ]; then
        mokka::phase "stage ${MIG_UPSTREAM_REPO} at ${MIG_UPSTREAM_SHA}"
        local staging="${MIG_STAGING_DIR:-}"
        if [ -z "${staging}" ]; then
            staging="$(mktemp -d "${TMPDIR:-/tmp}/kubectl-nv-mig-upstream.XXXXXX")"
            MIG_STAGING_OWNED="${staging}"
        fi
        mig::stage_upstream "${staging}"
    fi

    mokka::phase "build $(mig::image_ref)"
    docker build \
        --tag "${ref}" \
        --label "${MIG_IMAGE_REVISION_LABEL}=${MIG_UPSTREAM_SHA}" \
        --file "${MIG_UPSTREAM_TREE}/deployments/nvml-mock/Dockerfile" \
        "${MIG_UPSTREAM_TREE}"

    # `docker build` reporting success and leaving no such tag has happened
    # often enough (a buildx driver that exports nothing to the local daemon is
    # the usual way) that the exit code is not taken as evidence on its own.
    if ! docker image inspect "${ref}" > /dev/null 2>&1; then
        mokka::die 1 "${MOKKA_PROG}: docker build reported success but ${ref} is not in the local daemon; a buildx driver that does not load into it is the usual cause"
    fi
}
