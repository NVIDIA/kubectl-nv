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
# Builds the nvml-mock image the MIG lane runs, from the pinned upstream
# commit, and leaves it in the local Docker daemon.
#
# Usage: hack/mokka/mig/build-image.sh [--force]
#
#   --force  rebuild even when the tag is already in the daemon
#
# Exit codes:
#   0  the image is in the local daemon
#   1  an underlying tool failed; its own output says why
#   2  bad usage, or the upstream source could not be resolved
#   3  a required tool is missing
#   6  the upstream source is not the pinned commit
#
# It exists as a script of its own so the build can run OUTSIDE the lane lock.
# The build is a Go compile, three cgo shim builds and an apt install; it holds
# no cluster and nothing else is waiting on it, so making the next lane wait
# through it is pure queueing. hack/mokka/mig/up.sh calls the same function and
# is a no-op here when the tag is already present.

set -euo pipefail
umask 077

MOKKA_PROG="build-image.sh"
MIG_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
. "${MIG_SCRIPT_DIR}/lib.sh"
# shellcheck source=SCRIPTDIR/image.sh
. "${MIG_SCRIPT_DIR}/image.sh"

FORCE=false
case "${1:-}" in
    "") ;;
    --force) FORCE=true ;;
    *)
        printf 'usage: build-image.sh [--force]\n' >&2
        exit 2
        ;;
esac
if [ "$#" -gt 1 ]; then
    printf 'usage: build-image.sh [--force]\n' >&2
    exit 2
fi

mokka::require_tools docker git tar

# The staged tree is several hundred megabytes. The path that leaks it is not
# the happy one: it is a failed docker build, which is also the path a
# developer takes repeatedly while working out why it failed.
on_exit() {
    if [ -n "${MIG_STAGING_OWNED}" ]; then
        rm -rf "${MIG_STAGING_OWNED}"
    fi
}
trap on_exit EXIT

mig::build_image "${FORCE}"
mokka::phase

printf '%s is in the local Docker daemon\n' "$(mig::image_ref)"
