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
# Scores an agent on the kubectl-nv faults lane.
#
# For each of five faults in turn: inject it with the lane's fault script,
# wait until kubectl-nv reports it, hand an agent a directory holding nothing
# but the binary, SKILL.md and the lane kubeconfig, ask it what is wrong, and
# score the answer. A pass names three things: the node, the component and the
# finding code. Then recover and move on.
#
# This is a manual tool. It talks to a real cluster and it spends money on a
# real model, so it refuses to run under CI and it refuses any context that is
# not a kubectl-nv lane. Nothing in this repository calls it.
#
# Usage: hack/eval/run.sh
#
# Exit codes:
#   0  every case passed
#   1  at least one case failed
#   2  refused: CI is set, the context is not a lane, or the command line was wrong
#   3  could not run: a required tool, file or script is missing
#
# Environment, all optional:
#   KUBECTL_NV_EVAL_LANE              lane name                 (faults)
#   KUBECTL_NV_EVAL_BIN               the binary to evaluate    (bin/kubectl-nv)
#   KUBECTL_NV_EVAL_SKILL             the skill to hand over    (SKILL.md)
#   KUBECTL_NV_EVAL_FAULTS_DIR        the fault scripts         (hack/mokka/faults)
#   KUBECTL_NV_EVAL_KUBECONFIG        the lane kubeconfig       (hack/mokka/bin/kubeconfig-<lane>)
#   KUBECTL_NV_EVAL_RESULTS_DIR       where results are written (eval/results)
#   KUBECTL_NV_EVAL_NODE              the node to break         (the lane's first worker)
#   KUBECTL_NV_EVAL_AGENT             the agent command         (claude -p, see below)
#   KUBECTL_NV_EVAL_OBSERVE_TIMEOUT   seconds to wait for a fault to appear  (600)
#   KUBECTL_NV_EVAL_RECOVER_TIMEOUT   seconds to wait for it to clear        (600)
#   KUBECTL_NV_EVAL_POLL_INTERVAL     seconds between polls                  (10)
#
# The agent command is run with the case directory as its working directory,
# with the question on stdin, and its stdout is taken as the final answer.
# KUBECTL_NV_EVAL_CODE is exported to it, which is what lets a stub agent
# answer per case; a real agent has no use for it and must not be told the
# answer any other way.

set -uo pipefail

PROG="$(basename "${BASH_SOURCE[0]}")"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# The five faults, in the order they are run.
EVAL_CODES="NV-NODE-002 NV-NODE-003 NV-OP-001 NV-DCGM-001 NV-DRV-001"

LANE="${KUBECTL_NV_EVAL_LANE:-faults}"
BIN="${KUBECTL_NV_EVAL_BIN:-${REPO_ROOT}/bin/kubectl-nv}"
SKILL="${KUBECTL_NV_EVAL_SKILL:-${REPO_ROOT}/SKILL.md}"
FAULTS_DIR="${KUBECTL_NV_EVAL_FAULTS_DIR:-${REPO_ROOT}/hack/mokka/faults}"
KUBECONFIG_PATH="${KUBECTL_NV_EVAL_KUBECONFIG:-${REPO_ROOT}/hack/mokka/bin/kubeconfig-${LANE}}"
RESULTS_DIR="${KUBECTL_NV_EVAL_RESULTS_DIR:-${REPO_ROOT}/eval/results}"
OBSERVE_TIMEOUT="${KUBECTL_NV_EVAL_OBSERVE_TIMEOUT:-600}"
RECOVER_TIMEOUT="${KUBECTL_NV_EVAL_RECOVER_TIMEOUT:-600}"
POLL_INTERVAL="${KUBECTL_NV_EVAL_POLL_INTERVAL:-10}"

# The default agent. Claude Code in print mode, with its tool allow-list
# limited to the binary under evaluation: the point of the exercise is whether
# kubectl-nv answers the question, so an agent that could reach kubectl would
# be scored on the wrong thing.
DEFAULT_AGENT='claude -p --allowedTools Bash(./kubectl-nv:*)'
AGENT="${KUBECTL_NV_EVAL_AGENT:-${DEFAULT_AGENT}}"

CONTEXT="kind-kubectl-nv-${LANE}"

die() {
    local code="$1"
    shift
    printf '%s: %s\n' "${PROG}" "$*" >&2
    exit "${code}"
}

note() {
    printf '[eval] %s\n' "$*" >&2
}

# ---------------------------------------------------------------------------
# Refusals, before anything is touched
# ---------------------------------------------------------------------------

if [ "$#" -ne 0 ]; then
    die 2 "takes no arguments, got: $*"
fi

# A set but empty CI is what a shell leaves behind after `CI=`, and it is not
# CI. Anything else that is set at all is.
if [ -n "${CI:-}" ]; then
    die 2 "CI is set to '${CI}'. This harness runs a real model against a real cluster and is manual only."
fi

for tool in kubectl jq; do
    command -v "${tool}" > /dev/null 2>&1 || die 3 "required tool not found: ${tool}"
done

[ -x "${BIN}" ] || die 3 "no kubectl-nv binary at ${BIN}; run 'make build' first"
[ -r "${SKILL}" ] || die 3 "no skill file at ${SKILL}"
[ -d "${FAULTS_DIR}" ] || die 3 "no fault scripts at ${FAULTS_DIR}"
[ -r "${KUBECONFIG_PATH}" ] || die 3 "no lane kubeconfig at ${KUBECONFIG_PATH}"

for code in ${EVAL_CODES}; do
    script="${FAULTS_DIR}/$(printf '%s' "${code}" | tr '[:upper:]' '[:lower:]').sh"
    [ -x "${script}" ] || die 3 "no fault script for ${code} at ${script}"
done

# The context has to be a lane of ours, matched on the whole string. A check
# that merely looked for the prefix somewhere would accept
# prod-kind-kubectl-nv-faults, which is somebody else's cluster.
current_context="$(kubectl --kubeconfig "${KUBECONFIG_PATH}" config current-context 2> /dev/null)"
case "${current_context}" in
    kind-kubectl-nv-*) ;;
    *) die 2 "the kubeconfig at ${KUBECONFIG_PATH} is on context '${current_context:-<none>}', which is not a kubectl-nv lane" ;;
esac
CONTEXT="${current_context}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# kc <args...> -- kubectl against the lane and nothing else.
kc() {
    kubectl --kubeconfig "${KUBECONFIG_PATH}" --context "${CONTEXT}" "$@"
}

# nv <args...> -- the binary under evaluation, against the lane.
nv() {
    "${BIN}" --kubeconfig "${KUBECONFIG_PATH}" --context "${CONTEXT}" "$@"
}

# fault_script <code>
fault_script() {
    printf '%s/%s.sh' "${FAULTS_DIR}" "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
}

# json_string <text> -- the text as a JSON string, quoting and escaping done
# by jq rather than by hand.
json_string() {
    printf '%s' "$1" | jq -Rs .
}

# finding_component <code> <node> <report-file>
# Prints the component of the finding with this code on this node, or nothing
# when the report does not carry one.
finding_component() {
    jq -r --arg code "$1" --arg node "$2" \
        'first(.findings[]? | select(.code == $code and .node == $node) | .component) // empty' \
        < "$3" 2> /dev/null
}

# ---------------------------------------------------------------------------
# The node to break
# ---------------------------------------------------------------------------

NODE="${KUBECTL_NV_EVAL_NODE:-}"
if [ -z "${NODE}" ]; then
    workers="$(kc get nodes --selector '!node-role.kubernetes.io/control-plane' \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2> /dev/null)"
    # head -1 and then the first word: a kubectl that answers one name per
    # line and one that answers them space separated both give one name here.
    NODE="$(printf '%s\n' "${workers}" | head -1 | awk '{print $1}')"
fi
[ -n "${NODE}" ] || die 3 "could not find a worker node on ${CONTEXT}"

# ---------------------------------------------------------------------------
# Recovery
# ---------------------------------------------------------------------------

# INJECTED is the code currently injected, if any. The trap recovers it
# whatever ended the run, including an interrupt: a lane left broken is worse
# than a lane never tested.
INJECTED=""

recover_injected() {
    [ -n "${INJECTED}" ] || return 0
    local code="${INJECTED}"
    INJECTED=""
    note "recovering ${code} on ${NODE}"
    "$(fault_script "${code}")" recover "${NODE}" >&2
}

on_exit() {
    local rc=$?
    recover_injected
    exit "${rc}"
}
trap on_exit EXIT
trap 'exit 130' INT TERM

# ---------------------------------------------------------------------------
# The run
# ---------------------------------------------------------------------------

WORK="$(mktemp -d "${TMPDIR:-/tmp}/kubectl-nv-eval.XXXXXX")" || die 3 "could not make a working directory"
REPORT="${WORK}/health.json"

mkdir -p "${RESULTS_DIR}" || die 3 "could not create ${RESULTS_DIR}"

started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RESULT_FILE="${RESULTS_DIR}/$(date -u +%Y-%m-%d).json"

CASES_JSON=""
PASSED=0
FAILED=0

note "lane ${LANE}, context ${CONTEXT}, node ${NODE}"
note "agent: ${AGENT}"

for code in ${EVAL_CODES}; do
    note "=== ${code} ==="

    case_error=""
    component=""
    observed_polls=0
    named_node=false
    named_component=false
    named_code=false
    passed=false
    answer=""

    script="$(fault_script "${code}")"

    # Armed before the call, not after it: an injection that failed halfway
    # has still changed the cluster, and recover is idempotent, so the only
    # safe assumption is that something needs undoing.
    INJECTED="${code}"
    if ! "${script}" inject "${NODE}" >&2; then
        case_error="the fault script for ${code} failed to inject"
    fi

    if [ -z "${case_error}" ]; then
        # Wait until kubectl-nv reports the fault. Scoring an agent on a
        # cluster where the fault has not landed yet measures nothing.
        deadline=$(($(date +%s) + OBSERVE_TIMEOUT))
        while :; do
            observed_polls=$((observed_polls + 1))
            nv health --node "${NODE}" --fail-on info -o json > "${REPORT}" 2> /dev/null
            component="$(finding_component "${code}" "${NODE}" "${REPORT}")"
            [ -z "${component}" ] || break
            if [ "$(date +%s)" -ge "${deadline}" ]; then
                case_error="${code} did not appear on ${NODE} within ${OBSERVE_TIMEOUT}s"
                break
            fi
            sleep "${POLL_INTERVAL}"
        done
    fi

    if [ -z "${case_error}" ]; then
        note "${code} observed on ${NODE} as component ${component} after ${observed_polls} polls"

        # The agent gets a fresh directory holding the binary, the skill and
        # the kubeconfig, and nothing else. Copies, not links: the agent must
        # not be able to reach the rest of the checkout by following one.
        case_dir="${WORK}/${code}"
        mkdir -p "${case_dir}"
        cp "${BIN}" "${case_dir}/kubectl-nv"
        cp "${SKILL}" "${case_dir}/SKILL.md"
        cp "${KUBECONFIG_PATH}" "${case_dir}/kubeconfig"
        chmod 0700 "${case_dir}"
        chmod 0600 "${case_dir}/kubeconfig"
        chmod 0755 "${case_dir}/kubectl-nv"

        {
            printf 'Something is wrong with the GPUs on this Kubernetes cluster.\n\n'
            printf 'This directory holds three things and nothing else: the kubectl-nv\n'
            printf 'binary, its SKILL.md, and a kubeconfig for the cluster. KUBECONFIG\n'
            printf 'already points at it. Read SKILL.md first. It writes the commands as\n'
            printf '"kubectl nv <command>"; run them here as "./kubectl-nv <command>",\n'
            printf 'which is the same program.\n\n'
            printf 'Find out what is wrong and answer in plain text with three things:\n'
            printf 'the name of the affected node, the component at fault, and the\n'
            printf 'kubectl-nv finding code.\n'
        } > "${WORK}/prompt.txt"

        agent_started="$(date +%s)"
        # The agent command is a command line the operator chose, so it is
        # evaluated as one. KUBECONFIG is relative to the case directory,
        # which is where the agent runs.
        (
            cd "${case_dir}" || exit 70
            KUBECONFIG="./kubeconfig" KUBECTL_NV_EVAL_CODE="${code}" \
                eval "${AGENT}" < "${WORK}/prompt.txt"
        ) > "${WORK}/answer.txt" 2> "${WORK}/agent.err"
        agent_rc=$?
        agent_seconds=$(($(date +%s) - agent_started))
        answer="$(cat "${WORK}/answer.txt")"

        if [ "${agent_rc}" -ne 0 ]; then
            case_error="the agent command exited ${agent_rc}"
            note "agent stderr: $(tail -5 "${WORK}/agent.err")"
        fi
    else
        agent_seconds=0
    fi

    if [ -z "${case_error}" ]; then
        # Scoring. Each of the three has to be named.
        # -w, so that naming worker2 is not scored as naming worker. The
        # code match is case insensitive because a report prints it upper case
        # and prose does not always.
        grep -qwF -- "${NODE}" <<< "${answer}" && named_node=true
        grep -qwF -- "${component}" <<< "${answer}" && named_component=true
        grep -qiwF -- "${code}" <<< "${answer}" && named_code=true

        if [ "${named_node}" = true ] && [ "${named_component}" = true ] && [ "${named_code}" = true ]; then
            passed=true
        fi
    fi

    if [ "${passed}" = true ]; then
        PASSED=$((PASSED + 1))
        note "${code}: pass"
    else
        FAILED=$((FAILED + 1))
        note "${code}: FAIL${case_error:+ (${case_error})}"
    fi

    # Recover before the next fault, so only ever one is injected at a time.
    recover_injected

    # Wait for the finding to clear, so the next case does not score against
    # the leftovers of this one.
    deadline=$(($(date +%s) + RECOVER_TIMEOUT))
    while :; do
        nv health --node "${NODE}" --fail-on info -o json > "${REPORT}" 2> /dev/null
        [ -n "$(finding_component "${code}" "${NODE}" "${REPORT}")" ] || break
        if [ "$(date +%s)" -ge "${deadline}" ]; then
            note "${code} is still reported ${RECOVER_TIMEOUT}s after recovery"
            break
        fi
        sleep "${POLL_INTERVAL}"
    done

    entry="$(jq -n \
        --arg code "${code}" \
        --arg node "${NODE}" \
        --arg component "${component}" \
        --argjson pass "${passed}" \
        --argjson namedNode "${named_node}" \
        --argjson namedComponent "${named_component}" \
        --argjson namedCode "${named_code}" \
        --argjson observedPolls "${observed_polls}" \
        --argjson agentSeconds "${agent_seconds}" \
        --arg error "${case_error}" \
        '{code: $code, node: $node, component: $component, pass: $pass,
          named: {node: $namedNode, component: $namedComponent, code: $namedCode},
          observedPolls: $observedPolls, agentSeconds: $agentSeconds, error: $error}')"
    CASES_JSON="${CASES_JSON}${CASES_JSON:+,}${entry}"
done

finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

jq -n \
    --argjson schemaVersion 1 \
    --arg startedAt "${started_at}" \
    --arg finishedAt "${finished_at}" \
    --arg lane "${LANE}" \
    --arg context "${CONTEXT}" \
    --arg node "${NODE}" \
    --arg agent "${AGENT}" \
    --argjson cases "[${CASES_JSON}]" \
    --argjson passed "${PASSED}" \
    --argjson failed "${FAILED}" \
    '{schemaVersion: $schemaVersion, startedAt: $startedAt, finishedAt: $finishedAt,
      lane: $lane, context: $context, node: $node, agent: $agent, cases: $cases,
      summary: {cases: ($cases | length), passed: $passed, failed: $failed}}' \
    > "${RESULT_FILE}" || die 3 "could not write ${RESULT_FILE}"

note "wrote ${RESULT_FILE}: ${PASSED} passed, ${FAILED} failed"

rm -rf "${WORK}"

[ "${FAILED}" -eq 0 ]
