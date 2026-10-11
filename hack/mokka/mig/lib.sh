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
# Pins and helpers for the MIG lane, on top of hack/mokka/lib.sh.
#
# This file is sourced, not executed. It sources hack/mokka/lib.sh first, so
# the kind node image, the GPU Operator pins and the mokka:: helpers are the
# same objects the whole-GPU lane uses, and only what MIG needs is added here.
#
# WHY THIS LANE HAS ITS OWN SOURCE OF TRUTH
#
# The released nvml-mock chart cannot partition a GPU. MIG layouts landed in
# NVIDIA/k8s-test-infra#829 on 2026-09-29, after the v0.4.0 tag of 2026-09-23,
# and no release since carries them. So this lane, and only this lane, is built
# from an upstream commit: chart, device-plugin manifest and container image all
# come from MIG_UPSTREAM_SHA below. Every other lane stays on chart 0.4.0.
#
# The commit is named by its full SHA and never by a branch or by "latest": a
# branch moves, and this lane builds and runs a container image out of whatever
# it points at.
#
# shellcheck disable=SC2034
# SC2034 is disabled for the whole file for the same reason as in the parent:
# every pin below is read by mig/up.sh or mig/down.sh after they source it,
# which shellcheck cannot see when pointed at this file on its own.

MIG_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/../lib.sh
. "${MIG_LIB_DIR}/../lib.sh"

# ---------------------------------------------------------------------------
# Upstream source
# ---------------------------------------------------------------------------

MIG_UPSTREAM_REPO="NVIDIA/k8s-test-infra"
# Upstream main, 2026-10-09. Chart version 0.5.0-dev.
MIG_UPSTREAM_SHA="4ce11e5ae533416415e223ab43da58435d0df48c"

# The paths the lane reads out of that commit: the chart it installs, the
# device-plugin manifest it copies, and everything the image build's Dockerfile
# COPYs into its build context.
MIG_UPSTREAM_PATHS="go.mod go.sum api pkg cmd internal shims Makefile deployments"

# Where the upstream tree comes from, in order:
#
#   MIG_UPSTREAM_DIR   an already-checked-out tree whose HEAD is the pinned
#                      commit. This is the CI path: actions/checkout of
#                      NVIDIA/k8s-test-infra at the full SHA.
#   MIG_UPSTREAM_CLONE a local clone to read the commit out of with
#                      `git archive`, which touches no branch and no worktree.
#
# Either way the resolved tree is checked against MIG_UPSTREAM_SHA before
# anything is built from it.
MIG_UPSTREAM_DIR="${MIG_UPSTREAM_DIR:-}"
MIG_UPSTREAM_CLONE="${MIG_UPSTREAM_CLONE:-${HOME}/src/github/nvidia/k8s-test-infra}"

# ---------------------------------------------------------------------------
# The container image the chart runs
# ---------------------------------------------------------------------------
#
# Built from the pinned commit and `kind load`ed, because ghcr.io publishes no
# image for an arbitrary commit. The tag is a local name and never pushed; it
# carries the short SHA so that an image left over from an earlier pin is not
# silently reused. mig-nvml-mock-values.yaml names the same two strings, and
# up_test.sh fails if the two ever disagree.
MIG_NVML_MOCK_IMAGE="nvml-mock"
# Derived from the pin rather than written out beside it. Eight characters is
# enough to read in `docker images` and not enough to be unique, so the tag is
# a label, not an identity: a hand-built image, or one left from a tree pinned
# to a different commit whose SHA starts the same way, wears it just as well.
# Deriving it means a bumped pin changes the tag in the same edit.
MIG_NVML_MOCK_IMAGE_TAG="mig-${MIG_UPSTREAM_SHA:0:8}"

# The identity the tag cannot carry. The image is built with this label set to
# the FULL commit, and the skip-rebuild path reads it back: a tag that is
# present but whose label names another commit is rebuilt rather than reused.
# The key is the OCI annotation for exactly this, so `docker inspect` and any
# registry tooling read it without being told to.
MIG_IMAGE_REVISION_LABEL="org.opencontainers.image.revision"

# ---------------------------------------------------------------------------
# The layout
# ---------------------------------------------------------------------------
#
# h100 with each board filled by its smallest slice, which is the only shape
# the device plugin's --mig-strategy=single accepts: single publishes every
# partition as nvidia.com/gpu and rejects a node whose partitions do not all
# carry one profile. The numbers are from docs/guides/mig/README.md at the
# pinned commit.
MIG_PROFILE="h100"
MIG_BOARDS="2"
MIG_SLICE_PROFILE="1g.10gb"
MIG_SLICES_PER_BOARD="7"
MIG_STRATEGY="single"
# What a worker must advertise: two boards of seven slices, each published as
# one nvidia.com/gpu.
MIG_EXPECTED_GPUS="14"

# ---------------------------------------------------------------------------
# Mokka's own device plugin
# ---------------------------------------------------------------------------
#
# The GPU Operator's device plugin cannot serve MIG here. It resolves MIG
# capabilities through /proc/driver/nvidia-caps/mig-minors, at a hardcoded
# absolute path, while it builds its device map, so a plugin that cannot read
# that file advertises nothing at all. A volume mount cannot deliver it: runc
# refuses any mount whose target is inside /proc. What works is a privileged
# container bind-mounting Mokka's staged copy over its own /proc/driver, which
# is what the manifest beside this file does.
MIG_DEVICE_PLUGIN_MANIFEST="device-plugin-mock-mig.yaml"
MIG_DEVICE_PLUGIN_DAEMONSET="nvidia-device-plugin-mock"
MIG_DEVICE_PLUGIN_NAMESPACE="kube-system"

# The node label device-plugin-mock-mig.yaml selects on. kind-mig.yaml puts it
# on the workers and not on the control plane, which is what keeps the plugin
# off a node that carries no operands.
MIG_NODE_LABEL_KEY="mokka.nvidia.com/type"
MIG_NODE_LABEL_VALUE="sgpu"

# The DaemonSets whose rollout decides whether the lane is usable, each written
# as <namespace>/<name> because they do not share a namespace.
#
# gpu-feature-discovery is the operator's and puts the nvidia.com/* labels on;
# the mock device plugin is this lane's and puts nvidia.com/gpu into
# allocatable. The operator's own nvidia-device-plugin-daemonset is NOT here:
# mig-gpu-operator-values.yaml disables it, so waiting for it would wait for
# something that is never created.
MIG_OPERAND_DAEMONSETS="${GPU_OPERATOR_NAMESPACE}/gpu-feature-discovery ${MIG_DEVICE_PLUGIN_NAMESPACE}/${MIG_DEVICE_PLUGIN_DAEMONSET}"


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# mig::require_pinned_sha <ref>
# Accepts only the pinned commit, spelled in full. A branch name, a tag,
# "latest" and an abbreviated SHA are all refused: this lane builds and runs a
# container image out of whatever the reference names, so a reference that can
# move under it is not a pin. Exits 6 rather than returning, because every
# caller's only answer to a wrong commit is to stop.
mig::require_pinned_sha() {
    local ref="${1:-}"
    if [[ ! ${ref} =~ ^[0-9a-f]{40}$ ]]; then
        mokka::die 6 "${MOKKA_PROG}: '${ref:-<empty>}' is not a full 40-character commit SHA; this lane pins ${MIG_UPSTREAM_REPO} at ${MIG_UPSTREAM_SHA} and accepts no branch, tag or abbreviation"
    fi
    if [ "${ref}" != "${MIG_UPSTREAM_SHA}" ]; then
        mokka::die 6 "${MOKKA_PROG}: upstream source is at ${ref}, but this lane is pinned to ${MIG_UPSTREAM_SHA}"
    fi
}

# mig::require_lane_cluster <cluster>
# Refuses any cluster name outside the kubectl-nv- namespace. The name is
# matched against the WHOLE argument, so an embedded newline whose second line
# is somebody else's cluster is refused rather than half-accepted.
#
# This guards `kind create cluster` and `kind delete cluster`, the two places
# where a name reaches a command that acts on a whole cluster.
mig::require_lane_cluster() {
    local cluster="${1:-}"
    if [[ ! ${cluster} =~ ^kubectl-nv-[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$ ]]; then
        mokka::die 2 "${MOKKA_PROG}: refusing to act on '${cluster:-<empty>}': this lane only ever touches a cluster named kubectl-nv-<lane>"
    fi
}

# mig::stage_upstream <dest>
# Resolves the pinned commit's tree and sets MIG_UPSTREAM_TREE to the directory
# holding it. Two sources, in the order described above MIG_UPSTREAM_DIR.
#
# The answer comes back in a global rather than on stdout on purpose: this
# function refuses a wrong commit by calling mokka::die, and a die inside a
# command substitution exits only the subshell, leaving the caller to carry on
# with an empty path.
MIG_UPSTREAM_TREE=""
mig::stage_upstream() {
    local dest="$1"

    if [ -n "${MIG_UPSTREAM_DIR}" ]; then
        if [ ! -d "${MIG_UPSTREAM_DIR}" ]; then
            mokka::die 2 "${MOKKA_PROG}: MIG_UPSTREAM_DIR=${MIG_UPSTREAM_DIR} is not a directory"
        fi
        local head
        if ! head="$(git -C "${MIG_UPSTREAM_DIR}" rev-parse HEAD 2> /dev/null)"; then
            mokka::die 2 "${MOKKA_PROG}: MIG_UPSTREAM_DIR=${MIG_UPSTREAM_DIR} is not a git checkout, so its commit cannot be checked against the pin"
        fi
        mig::require_pinned_sha "${head}"

        # HEAD naming the pinned commit is not the same as the TREE being that
        # commit. An uncommitted change is what the image would actually be
        # built from, under a tag and a label both naming a commit that no
        # longer describes it, and nothing downstream could tell. On a runner
        # this never happens; on the laptop where the upstream clone is also
        # the one being experimented in, it is the normal state.
        local dirty
        dirty="$(git -C "${MIG_UPSTREAM_DIR}" status --porcelain 2> /dev/null)"
        if [ -n "${dirty}" ]; then
            printf '%s\n' "${dirty}" >&2
            mokka::die 6 "${MOKKA_PROG}: MIG_UPSTREAM_DIR=${MIG_UPSTREAM_DIR} is at ${MIG_UPSTREAM_SHA} but has uncommitted changes (listed above); this lane builds an image from that tree and tags it with that commit"
        fi

        # Used in place, not copied: in CI it is an actions/checkout of several
        # hundred megabytes and the only reader is a docker build.
        MIG_UPSTREAM_TREE="${MIG_UPSTREAM_DIR}"
        return 0
    fi

    if [ ! -d "${MIG_UPSTREAM_CLONE}/.git" ] && [ ! -f "${MIG_UPSTREAM_CLONE}/HEAD" ]; then
        mokka::die 2 "${MOKKA_PROG}: no clone of ${MIG_UPSTREAM_REPO} at ${MIG_UPSTREAM_CLONE}; set MIG_UPSTREAM_CLONE, or set MIG_UPSTREAM_DIR to a checkout of ${MIG_UPSTREAM_SHA}"
    fi

    # `cat-file -t` is the question actually being asked: does this object exist
    # in this repository and is it a commit. `rev-parse` answers for a ref of
    # the same name too, and a clone fetched shallow has the ref without the
    # object.
    local kind_of
    if ! kind_of="$(git -C "${MIG_UPSTREAM_CLONE}" cat-file -t "${MIG_UPSTREAM_SHA}" 2> /dev/null)" ||
        [ "${kind_of}" != "commit" ]; then
        mokka::die 2 "${MOKKA_PROG}: ${MIG_UPSTREAM_CLONE} does not hold commit ${MIG_UPSTREAM_SHA}; fetch it there first"
    fi

    mkdir -p "${dest}"
    # `git archive <sha>` reads the commit without checking it out, so the
    # clone's branches, index and worktree are untouched. A developer's clone
    # is not this lane's to move.
    #
    # shellcheck disable=SC2086
    # MIG_UPSTREAM_PATHS is a list of pathspecs and is meant to split.
    git -C "${MIG_UPSTREAM_CLONE}" archive "${MIG_UPSTREAM_SHA}" -- ${MIG_UPSTREAM_PATHS} |
        tar -x -C "${dest}"

    MIG_UPSTREAM_TREE="${dest}"
}
