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
# but a guarded kubectl-nv shim, SKILL.md and the lane kubeconfig, ask it what
# is wrong, and score the answer. A pass names three things: the node, the
# component and the finding code. Then recover and move on.
#
# This is a manual tool. It talks to a real cluster and it spends money on a
# real model, so it refuses to run under CI and it refuses any kubeconfig that
# is not this lane's. Nothing in this repository calls it.
#
# What the agent can reach is bounded three ways, because the agent is a model
# and the cluster is real:
#   - ./kubectl-nv in the case directory is hack/eval/kubectl-nv-shim.sh, not
#     the binary. It refuses every connection flag, both directory flags and
#     the exec-capable commands, then execs the real binary with the lane
#     fixed. The real binary is outside the case directory.
#   - the agent runs under env -i with PATH, LANG, HOME and KUBECONFIG and
#     nothing else, with a HOME of its own that starts empty, so it cannot
#     read the operator's settings or find the answer in the environment.
#   - a wall clock bound ends an agent that does not return. macOS has no GNU
#     timeout, so it is a background run plus a kill.
#
# Usage: hack/eval/run.sh
#
# Exit codes:
#   0  every case passed
#   1  at least one case failed, or the lane could not be put back
#   2  refused: CI is set, the kubeconfig is not this lane's, or the command
#      line was wrong
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
#   KUBECTL_NV_EVAL_AGENT             the agent command         (see DEFAULT_AGENT)
#   KUBECTL_NV_EVAL_OBSERVE_TIMEOUT   seconds to wait for a fault to appear  (600)
#   KUBECTL_NV_EVAL_RECOVER_TIMEOUT   seconds to wait for it to clear        (600)
#   KUBECTL_NV_EVAL_AGENT_TIMEOUT     seconds one agent run may take         (900)
#   KUBECTL_NV_EVAL_POLL_INTERVAL     seconds between polls                  (10)
#   KUBECTL_NV_EVAL_DEBUG             when set, echo each answer to stderr
#
# The agent command runs with the case directory as its working directory and
# the question on stdin, and its stdout is taken as the final answer. Nothing
# about which case is running reaches it.

set -uo pipefail

PROG="$(basename "${BASH_SOURCE[0]}")"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SHIM_TEMPLATE="${SCRIPT_DIR}/kubectl-nv-shim.sh"

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
AGENT_TIMEOUT="${KUBECTL_NV_EVAL_AGENT_TIMEOUT:-900}"
POLL_INTERVAL="${KUBECTL_NV_EVAL_POLL_INTERVAL:-10}"

# The default agent. Claude Code in print mode, locked down:
#   --restricted        drops the code-running tools and WebFetch unless
#                       --tools names them, ignores user, project and local
#                       settings, and confines the file tools to the working
#                       directory
#   --tools Bash,Read   the two it needs: one to run the shim, one to read
#                       SKILL.md
#   --allowedTools      the only command it may run
#   --permission-mode dontAsk   nothing to answer; the run has no terminal
#   --strict-mcp-config --mcp-config '{"mcpServers":{}}'
#                       no MCP server the operator happens to have configured
#
# Quoted where the shell would otherwise read the parentheses and braces as
# syntax: the agent command is evaluated as a command line.
#
# There is no --max-turns in claude 2.1.296; the wall clock bound below is
# what ends a run that will not finish.
DEFAULT_AGENT="claude -p --restricted --tools Bash,Read --allowedTools 'Bash(./kubectl-nv:*)' --permission-mode dontAsk --strict-mcp-config --mcp-config '{\"mcpServers\":{}}'"
AGENT="${KUBECTL_NV_EVAL_AGENT:-${DEFAULT_AGENT}}"

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

for tool in kubectl jq cmp; do
    command -v "${tool}" > /dev/null 2>&1 || die 3 "required tool not found: ${tool}"
done

[ -x "${BIN}" ] || die 3 "no kubectl-nv binary at ${BIN}; run 'make build' first"
[ -r "${SKILL}" ] || die 3 "no skill file at ${SKILL}"
[ -r "${SHIM_TEMPLATE}" ] || die 3 "no shim template at ${SHIM_TEMPLATE}"
[ -d "${FAULTS_DIR}" ] || die 3 "no fault scripts at ${FAULTS_DIR}"
[ -r "${KUBECONFIG_PATH}" ] || die 3 "no lane kubeconfig at ${KUBECONFIG_PATH}"

for code in ${EVAL_CODES}; do
    script="${FAULTS_DIR}/$(printf '%s' "${code}" | tr '[:upper:]' '[:lower:]').sh"
    [ -x "${script}" ] || die 3 "no fault script for ${code} at ${script}"
done

# The two paths are substituted into the shim, which is a shell script, so a
# path carrying shell syntax would be a hole rather than a path. Nothing in a
# Mokka lane produces one, and refusing is cheaper than quoting correctly.
case "${BIN}${KUBECONFIG_PATH}" in
    *[\"\'\$\`\\]*)
        die 3 "the binary and kubeconfig paths must not contain quotes, dollars, backticks or backslashes"
        ;;
esac

# ---------------------------------------------------------------------------
# The lane guard
# ---------------------------------------------------------------------------
#
# Three statements, not one. The context name alone is a label anybody can
# write: a kubeconfig called kind-kubectl-nv-faults-evil.example.com is not
# this lane, and a file carrying a second context can reach more than the lane
# even when the current one is right. That file is handed to an agent, so what
# it can reach is what the agent can reach.

LANE_CONTEXT="kind-kubectl-nv-${LANE}"

current_context="$(kubectl --kubeconfig "${KUBECONFIG_PATH}" config current-context 2> /dev/null)"
if [ "${current_context}" != "${LANE_CONTEXT}" ]; then
    die 2 "the kubeconfig at ${KUBECONFIG_PATH} is on context '${current_context:-<none>}', not the lane's '${LANE_CONTEXT}'"
fi

lane_config="$(kubectl --kubeconfig "${KUBECONFIG_PATH}" --context "${LANE_CONTEXT}" \
    config view --minify --flatten 2> /dev/null)"
[ -n "${lane_config}" ] || die 2 "could not read the kubeconfig at ${KUBECONFIG_PATH}"

context_count="$(printf '%s\n' "${lane_config}" | grep -c '^- name: ')"
if [ "${context_count}" -ne 1 ]; then
    die 2 "the kubeconfig at ${KUBECONFIG_PATH} carries ${context_count} contexts; a lane kubeconfig carries exactly one"
fi

# A kind lane publishes its API server on the loopback address. Anything else
# is a cluster somewhere, and this harness breaks what it points at.
lane_server="$(printf '%s\n' "${lane_config}" | sed -n 's/^ *server: *//p' | head -1)"
case "${lane_server}" in
    https://127.0.0.1:* | 'https://[::1]:'*) ;;
    *)
        die 2 "the kubeconfig at ${KUBECONFIG_PATH} names server '${lane_server:-<none>}', which is not a local kind lane"
        ;;
esac

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# kc <args...> -- kubectl against the lane and nothing else.
kc() {
    kubectl --kubeconfig "${KUBECONFIG_PATH}" --context "${LANE_CONTEXT}" "$@"
}

# nv <args...> -- the binary under evaluation, against the lane.
nv() {
    "${BIN}" --kubeconfig "${KUBECONFIG_PATH}" --context "${LANE_CONTEXT}" "$@"
}

# fault_script <code>
fault_script() {
    printf '%s/%s.sh' "${FAULTS_DIR}" "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
}

# run_fault <code> <inject|recover>
# E3's scripts require MOKKA_KUBECONFIG and exit 2 without it, with no
# fallback, so it is passed on the call rather than exported once: an exported
# variable is one edit away from reaching something else.
run_fault() {
    MOKKA_KUBECONFIG="${KUBECONFIG_PATH}" "$(fault_script "$1")" "$2" "${NODE}" >&2
}

# finding_component <code> <report-file>
# Prints the component of the finding with this code for the node this run is
# breaking, or nothing when the report does not carry one.
#
# A cluster-scoped rule sets no node at all (NV-OP-001 is the one here), so a
# finding with an empty node counts for whichever node the run is on. Matching
# only on an equal node would wait forever for a code that never carries one.
finding_component() {
    jq -r --arg code "$1" --arg node "${NODE}" \
        'first(.findings[]?
               | select(.code == $code and ((.node // "") == $node or (.node // "") == ""))
               | .component) // empty' \
        < "$2" 2> /dev/null
}

# finding_present <code> <report-file>
finding_present() {
    [ -n "$(finding_component "$1" "$2")" ]
}

# expected_node_for <code> <report-file>
# What a complete answer has to name: the node for a node-scoped finding, and
# a dash for one that is about the cluster.
expected_node_for() {
    local node
    node="$(jq -r --arg code "$1" \
        'first(.findings[]? | select(.code == $code) | .node // "") // ""' \
        < "$2" 2> /dev/null)"
    if [ -z "${node}" ]; then
        printf '%s' '-'
    else
        printf '%s' "${node}"
    fi
}

# answer_field <field> <answer>
# Reads one field out of the last ANSWER line. Scoring reads that line and
# nothing else: an answer that lists every code it can think of has not
# diagnosed anything, and crediting it for containing the right one would
# score the shotgun above the diagnosis.
answer_field() {
    printf '%s\n' "$2" | grep '^ANSWER ' | tail -1 |
        sed -n "s/.*[[:space:]]$1=\\([^[:space:]]*\\).*/\\1/p"
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
[ -n "${NODE}" ] || die 3 "could not find a worker node on ${LANE_CONTEXT}"

# ---------------------------------------------------------------------------
# Cleanup and recovery
# ---------------------------------------------------------------------------

# INJECTED is the code currently injected, if any. SCRATCH_DIRS are the agent
# directories, which hold a copy of the lane kubeconfig and so must not
# outlive the run. The trap deals with both whatever ended the run, including
# an interrupt: a lane left broken, or a credential left in the temporary
# directory, is worse than a run that never happened.
INJECTED=""
SCRATCH_DIRS=()
WORK=""
RECOVER_FAILED=""

recover_injected() {
    [ -n "${INJECTED}" ] || return 0
    local code="${INJECTED}"
    INJECTED=""
    note "recovering ${code} on ${NODE}"
    if ! run_fault "${code}" recover; then
        RECOVER_FAILED="the fault script for ${code} failed to recover"
        note "${RECOVER_FAILED}"
        return 1
    fi

    return 0
}

cleanup_dirs() {
    local d
    for d in ${SCRATCH_DIRS[@]+"${SCRATCH_DIRS[@]}"}; do
        [ -n "${d}" ] && rm -rf "${d}"
    done
    SCRATCH_DIRS=()
    if [ -n "${WORK}" ]; then
        rm -rf "${WORK}"
        WORK=""
    fi
}

on_exit() {
    local rc=$?
    recover_injected
    cleanup_dirs
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

# The prompt. It names the ANSWER line the scorer reads, because an agent
# scored on a format it was never told is being scored on luck.
cat > "${WORK}/prompt.txt" << 'PROMPT'
Something is wrong with the GPUs on this Kubernetes cluster.

This directory holds three things and nothing else: the kubectl-nv binary,
its SKILL.md, and a kubeconfig for the cluster. KUBECONFIG already points at
it. Read SKILL.md first. It writes the commands as "kubectl nv <command>";
run them here as "./kubectl-nv <command>", which is the same program.

Find out what is wrong. Explain what you found, and then end your reply with
one line in exactly this form, and nothing after it:

ANSWER node=<node> component=<component> code=<code>

node is the name of the affected node, or - when the finding is about the
cluster rather than about one node. component is the component field of the
finding, verbatim. code is the kubectl-nv finding code.
PROMPT

started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RESULT_FILE="${RESULTS_DIR}/$(date -u +%Y-%m-%d).json"

CASES_JSON=""
PASSED=0
FAILED=0

note "lane ${LANE}, context ${LANE_CONTEXT}, node ${NODE}"
note "agent: ${AGENT}"

for code in ${EVAL_CODES}; do
    note "=== ${code} ==="

    case_error=""
    recover_error=""
    component=""
    expected_node="-"
    observed_polls=0
    agent_seconds=0
    named_node=false
    named_component=false
    named_code=false
    passed=false
    answer=""

    # Armed before the call, not after it: an injection that failed halfway
    # has still changed the cluster, and recover is idempotent, so the only
    # safe assumption is that something needs undoing.
    INJECTED="${code}"
    if ! run_fault "${code}" inject; then
        case_error="the fault script for ${code} failed to inject"
    fi

    if [ -z "${case_error}" ]; then
        # Wait until kubectl-nv reports the fault. Scoring an agent on a
        # cluster where the fault has not landed yet measures nothing.
        deadline=$(($(date +%s) + OBSERVE_TIMEOUT))
        while :; do
            observed_polls=$((observed_polls + 1))
            nv health --fail-on info -o json > "${REPORT}" 2> /dev/null
            component="$(finding_component "${code}" "${REPORT}")"
            if [ -n "${component}" ]; then
                expected_node="$(expected_node_for "${code}" "${REPORT}")"
                break
            fi
            if [ "$(date +%s)" -ge "${deadline}" ]; then
                case_error="${code} did not appear within ${OBSERVE_TIMEOUT}s"
                break
            fi
            sleep "${POLL_INTERVAL}"
        done
    fi

    if [ -z "${case_error}" ]; then
        note "${code} observed as component ${component} on node '${expected_node}' after ${observed_polls} polls"

        # The agent gets a fresh directory of its own, outside this script's
        # working directory, holding the guarded shim, the skill and the
        # kubeconfig, and nothing else.
        case_dir="$(mktemp -d "${TMPDIR:-/tmp}/kubectl-nv-eval-case.XXXXXX")" ||
            die 3 "could not make a case directory"
        SCRATCH_DIRS+=("${case_dir}")
        agent_home="$(mktemp -d "${TMPDIR:-/tmp}/kubectl-nv-eval-home.XXXXXX")" ||
            die 3 "could not make an agent home"
        SCRATCH_DIRS+=("${agent_home}")

        # The shim, with the lane substituted in. Bash replacement rather than
        # sed: a path is arbitrary text and sed would read a backslash or the
        # delimiter in it as a directive.
        shim_source="$(cat "${SHIM_TEMPLATE}")" || die 3 "could not read ${SHIM_TEMPLATE}"
        shim_source="${shim_source//@KUBECTL_NV_REAL@/${BIN}}"
        shim_source="${shim_source//@KUBECTL_NV_LANE_KUBECONFIG@/${KUBECONFIG_PATH}}"
        shim_source="${shim_source//@KUBECTL_NV_LANE_CONTEXT@/${LANE_CONTEXT}}"
        printf '%s\n' "${shim_source}" > "${case_dir}/kubectl-nv" ||
            die 3 "could not write the shim into ${case_dir}"

        cp "${SKILL}" "${case_dir}/SKILL.md" || die 3 "could not copy ${SKILL} into ${case_dir}"
        cp "${KUBECONFIG_PATH}" "${case_dir}/kubeconfig" ||
            die 3 "could not copy ${KUBECONFIG_PATH} into ${case_dir}"

        chmod 0700 "${case_dir}" "${agent_home}"
        chmod 0600 "${case_dir}/kubeconfig"
        chmod 0755 "${case_dir}/kubectl-nv"

        # Verify the after-state. cp returns 0 while doing nothing useful
        # often enough that the exit code alone is not evidence, and a
        # substitution that silently matched nothing would leave the agent
        # holding a shim that cannot run.
        if grep -q '@KUBECTL_NV_' "${case_dir}/kubectl-nv"; then
            die 3 "the shim in ${case_dir} still carries an unsubstituted placeholder"
        fi
        for f in kubectl-nv SKILL.md kubeconfig; do
            [ -s "${case_dir}/${f}" ] || die 3 "${case_dir}/${f} is missing or empty"
        done
        cmp -s "${SKILL}" "${case_dir}/SKILL.md" || die 3 "the skill did not copy into ${case_dir}"
        cmp -s "${KUBECONFIG_PATH}" "${case_dir}/kubeconfig" ||
            die 3 "the kubeconfig did not copy into ${case_dir}"

        agent_started="$(date +%s)"
        # env -i with an allow-list: the agent must not be able to read the
        # operator's settings through HOME, reach another cluster through
        # KUBECONFIG, or find which case is running in the environment.
        (
            cd "${case_dir}" || exit 70
            env -i \
                PATH="${PATH}" \
                LANG="${LANG:-C}" \
                HOME="${agent_home}" \
                KUBECONFIG="./kubeconfig" \
                /bin/bash -c "${AGENT}"
        ) < "${WORK}/prompt.txt" > "${WORK}/answer.txt" 2> "${WORK}/agent.err" &
        agent_pid=$!

        # macOS has no GNU timeout, so the bound is a poll on the child plus a
        # kill. TERM first, then KILL, so an agent that cleans up gets to.
        agent_deadline=$(($(date +%s) + AGENT_TIMEOUT))
        agent_timed_out=false
        while kill -0 "${agent_pid}" 2> /dev/null; do
            if [ "$(date +%s)" -ge "${agent_deadline}" ]; then
                agent_timed_out=true
                kill -TERM "${agent_pid}" 2> /dev/null
                sleep 2
                kill -KILL "${agent_pid}" 2> /dev/null
                break
            fi
            sleep 1
        done
        wait "${agent_pid}"
        agent_rc=$?
        agent_seconds=$(($(date +%s) - agent_started))
        answer="$(cat "${WORK}/answer.txt")"
        if [ -n "${KUBECTL_NV_EVAL_DEBUG:-}" ]; then
            note "answer: $(cat "${WORK}/answer.txt")"
            note "agent stderr: $(cat "${WORK}/agent.err")"
        fi

        if [ "${agent_timed_out}" = true ]; then
            case_error="the agent timed out after ${AGENT_TIMEOUT}s"
        elif [ "${agent_rc}" -ne 0 ]; then
            case_error="the agent command exited ${agent_rc}"
            note "agent stderr: $(tail -5 "${WORK}/agent.err")"
        fi
    fi

    if [ -z "${case_error}" ]; then
        # Scoring, from the ANSWER line alone. The node and the component are
        # exact: a node name is a DNS label and the component is a field from
        # the report. The code ignores case, because a report prints it upper
        # case and prose does not always, but it is still the whole code:
        # NV-OP-0010 is not NV-OP-001.
        got_node="$(answer_field node "${answer}")"
        got_component="$(answer_field component "${answer}")"
        got_code="$(answer_field code "${answer}")"

        if [ -n "${got_node}" ] && [ "${got_node}" = "${expected_node}" ]; then
            named_node=true
        fi
        if [ -n "${got_component}" ] && [ "${got_component}" = "${component}" ]; then
            named_component=true
        fi
        if [ -n "${got_code}" ] &&
            [ "$(printf '%s' "${got_code}" | tr '[:lower:]' '[:upper:]')" = "${code}" ]; then
            named_code=true
        fi

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
    RECOVER_FAILED=""
    recover_injected
    recover_error="${RECOVER_FAILED}"

    if [ -z "${recover_error}" ]; then
        # Wait for the finding to clear. A recover that reported success and
        # left the finding in place is the same problem wearing a zero exit
        # code, and the next case would score against the leftovers.
        deadline=$(($(date +%s) + RECOVER_TIMEOUT))
        while :; do
            nv health --fail-on info -o json > "${REPORT}" 2> /dev/null
            finding_present "${code}" "${REPORT}" || break
            if [ "$(date +%s)" -ge "${deadline}" ]; then
                recover_error="${code} was still reported ${RECOVER_TIMEOUT}s after recovery"
                break
            fi
            sleep "${POLL_INTERVAL}"
        done
    fi

    if [ -n "${recover_error}" ] && [ "${passed}" = true ]; then
        # A case whose lane could not be put back is not a pass: what the
        # agent saw is no longer what the harness thinks it set up.
        passed=false
        PASSED=$((PASSED - 1))
        FAILED=$((FAILED + 1))
    fi

    entry="$(jq -n \
        --arg code "${code}" \
        --arg node "${NODE}" \
        --arg expectedNode "${expected_node}" \
        --arg component "${component}" \
        --argjson pass "${passed}" \
        --argjson namedNode "${named_node}" \
        --argjson namedComponent "${named_component}" \
        --argjson namedCode "${named_code}" \
        --argjson observedPolls "${observed_polls}" \
        --argjson agentSeconds "${agent_seconds}" \
        --arg error "${case_error}" \
        --arg recoverError "${recover_error}" \
        '{code: $code, node: $node, expectedNode: $expectedNode, component: $component,
          pass: $pass,
          named: {node: $namedNode, component: $namedComponent, code: $namedCode},
          observedPolls: $observedPolls, agentSeconds: $agentSeconds,
          error: $error, recoverError: $recoverError}')"
    CASES_JSON="${CASES_JSON}${CASES_JSON:+,}${entry}"

    if [ -n "${recover_error}" ]; then
        note "stopping: the lane still carries ${code}, so nothing more may be injected onto it"
        break
    fi
done

finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

jq -n \
    --argjson schemaVersion 1 \
    --arg startedAt "${started_at}" \
    --arg finishedAt "${finished_at}" \
    --arg lane "${LANE}" \
    --arg context "${LANE_CONTEXT}" \
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

[ "${FAILED}" -eq 0 ]
