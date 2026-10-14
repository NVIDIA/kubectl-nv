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
# The ./kubectl-nv the evaluated agent is given.
#
# hack/eval/run.sh copies this file into each case directory under the name
# kubectl-nv, with the two paths below substituted, and allows the agent to
# run Bash(./kubectl-nv:*) and nothing else. That pattern allows any argument,
# so without this file an agent could point the real binary at another cluster
# with --server, read another identity's credentials with --kubeconfig, or
# write anywhere on the machine with --artifacts-dir. It refuses all of that
# and then execs the real binary with the lane fixed.
#
# The real binary lives outside the case directory, so the agent cannot reach
# it except through here.
#
# Refused:
#   every kubectl connection flag (the genericclioptions ConfigFlags set),
#   long, short and in the --flag=value spelling;
#   --write-dir and --artifacts-dir, which name a directory to write into;
#   the commands that can exec inside a pod: adm must-gather and debug.
#
# A refusal names the flag and never its value: echoing a rejected --token
# back would copy the secret into the model's context.
#
# Exit codes:
#   2  refused; the real binary was not run
#   *  whatever the real binary exited with
#
# The three settings are substituted by run.sh when it copies this file. The
# environment variables are for testing this file on its own; under the
# scrubbed environment the agent runs in they are never set, so the
# substituted values are what applies.

set -uo pipefail

PROG="kubectl-nv"

REAL="${KUBECTL_NV_SHIM_REAL:-@KUBECTL_NV_REAL@}"
LANE_KUBECONFIG="${KUBECTL_NV_SHIM_KUBECONFIG:-@KUBECTL_NV_LANE_KUBECONFIG@}"
LANE_CONTEXT="${KUBECTL_NV_SHIM_CONTEXT:-@KUBECTL_NV_LANE_CONTEXT@}"

refuse() {
    printf '%s: %s is not available in this evaluation\n' "${PROG}" "$1" >&2
    printf '%s: the cluster, the identity and the output directory are fixed\n' "${PROG}" >&2
    exit 2
}

# The whole ConfigFlags set, plus the two flags that name a directory to write
# into. Spelled out rather than pattern matched: a prefix rule would refuse a
# future read-only flag that happened to start the same way, and a loose rule
# that let one of these through is exactly the hole this file exists to close.
is_refused_flag() {
    case "$1" in
        --kubeconfig | -k | \
            --context | --cluster | --user | --namespace-from-context | \
            --server | -s | --token | \
            --as | --as-group | --as-uid | --as-user-extra | \
            --certificate-authority | --client-certificate | --client-key | \
            --insecure-skip-tls-verify | --tls-server-name | --proxy-url | \
            --cache-dir | --request-timeout | --disable-compression | \
            --password | --username | \
            --write-dir | --artifacts-dir)
            return 0
            ;;
    esac

    return 1
}

for arg in "$@"; do
    # --flag=value carries the flag in its first field, so it is checked as
    # the flag it is rather than slipping past as one unknown word.
    name="${arg%%=*}"
    if is_refused_flag "${name}"; then
        refuse "${name}"
    fi
done

# The command path, which is the leading words that are not flags and not a
# flag's value. Only the first two matter: every exec-capable command is at
# most two words deep.
first=""
second=""
for arg in "$@"; do
    case "${arg}" in
        -*) continue ;;
    esac
    if [ -z "${first}" ]; then
        first="${arg}"
        continue
    fi
    if [ -z "${second}" ]; then
        second="${arg}"
        break
    fi
done

# adm must-gather and debug run nvidia-bug-report.sh inside a driver pod.
# Nothing a model decides should end up executing inside a workload, so they
# are refused here as well as being served with --no-exec over MCP.
case "${first}" in
    debug)
        refuse "the debug command"
        ;;
    adm)
        case "${second}" in
            must-gather) refuse "the adm must-gather command" ;;
        esac
        ;;
esac

# The lane is appended last, so it wins over anything earlier on the command
# line even if a spelling of these flags were ever to get past the loop above.
exec "${REAL}" "$@" --kubeconfig "${LANE_KUBECONFIG}" --context "${LANE_CONTEXT}"
