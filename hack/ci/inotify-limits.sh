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
# Raises the two inotify limits a kind cluster needs on the host that runs it,
# then reads each one back.
#
# Usage: sudo hack/ci/inotify-limits.sh
#
# Exit codes: 0 both limits at or above their targets, 1 a limit is still
# below its target after the write, 2 sysctl could not be run, read or
# written.
#
# Every kind node is a container, and the systemd, containerd, kubelet and
# operands inside all of them run as the same root user on the host, so they
# share one per-user pool of inotify instances. The kernel default of 128 runs
# out on a lane of three nodes or more, and the device plugin is the first to
# notice: it exits with "failed to create FS watcher ... too many open files",
# the validator never sees a GPU, and ClusterPolicy stays InProgress until the
# helm wait gives up.
# https://kind.sigs.k8s.io/docs/user/known-issues/#pod-errors-due-to-too-many-open-files
#
# GitHub's runner image raises both limits, by appending them to
# /etc/sysctl.conf. From ubuntu-26.04 on, procps neither ships nor reads that
# file (Debian procps 2:4.0.4-5), so the lines are written and never applied,
# and every lane went red on 2026-10-23, the first night it ran on 26.04.
# Setting the values here makes the lanes independent of how the image is
# built. The script only raises: a host already above a target keeps its own
# value.

set -u

PROG=inotify-limits.sh

# The values actions/runner-images means to set
# (images/ubuntu/scripts/build/configure-environment.sh), which every lane ran
# green on through ubuntu-24.04.
TARGETS=(
    "fs.inotify.max_user_instances=1280"
    "fs.inotify.max_user_watches=655360"
)

die() {
    local rc="$1"
    shift
    echo "${PROG}: $*" >&2
    exit "${rc}"
}

# read_value <key>: prints the key's value, or exits 2 when there is no number
# to print. Called in a command substitution, so the caller passes the exit on.
read_value() {
    local key="$1" value
    value="$(sysctl -n "${key}")" || die 2 "cannot read ${key}"
    [[ "${value}" =~ ^[0-9]+$ ]] || die 2 "${key} reads as '${value}', not a number"
    printf '%s\n' "${value}"
}

command -v sysctl > /dev/null 2>&1 || die 2 "sysctl not found on PATH"

for target in "${TARGETS[@]}"; do
    key="${target%%=*}"
    want="${target#*=}"

    before="$(read_value "${key}")" || exit $?
    if [ "${before}" -ge "${want}" ]; then
        echo "${key}: ${before}, at or above ${want}"
        continue
    fi

    sysctl -w "${key}=${want}" > /dev/null || die 2 "sysctl -w ${key}=${want} failed"

    # A write can report success and change nothing; the runner image's own
    # write is exactly that. Only the value read back counts.
    after="$(read_value "${key}")" || exit $?
    if [ "${after}" -lt "${want}" ]; then
        die 1 "${key} is ${after} after writing ${want}"
    fi
    echo "${key}: ${before} -> ${after}"
done
