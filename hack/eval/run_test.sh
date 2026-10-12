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
# Tests for hack/eval/run.sh and hack/eval/render-results.sh.
#
# No cluster is created and no model is called. kubectl is a PATH shim, the
# fault scripts are fakes that record their calls and flip a state file, the
# binary under evaluation is a fake that reports findings from that state, and
# the agent is a stub that prints whatever answer the case fixes.
#
# The fake pieces are wired to each other the way the real ones are: the fault
# script is what makes the finding appear, and the binary is what the harness
# polls, so a subject that scored without injecting, or that never waited for
# the fault, gets a different answer and the assertions notice.
#
# The subject is resolved relative to THIS file, never through $HOME: a test
# that green-lights a deployed copy says nothing about the tree it ships from.
#
# Usage: hack/eval/run_test.sh
# Exit: 0 all tests pass, 1 at least one failed.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN="${SCRIPT_DIR}/run.sh"
RENDER="${SCRIPT_DIR}/render-results.sh"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

PASS=0
FAIL=0

WORK="$(mktemp -d "${TMPDIR:-/tmp}/kubectl-nv-eval-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

SHIM_DIR="${WORK}/shims"
TOOL_DIR="${WORK}/systools"
FAKE_HOME="${WORK}/home"
FAULTS_DIR="${WORK}/faults"
RESULTS_DIR="${WORK}/results"
STATE_DIR="${WORK}/state"
BIN="${WORK}/kubectl-nv"
AGENT_STUB="${WORK}/agent-stub"
SKILL="${WORK}/SKILL.md"
LANE_KUBECONFIG="${WORK}/kubeconfig-faults"

export EVAL_CALL_LOG="${WORK}/calls.log"
export EVAL_STATE_DIR="${STATE_DIR}"

# The five codes the harness runs, in the order it has to run them.
EXPECTED_ORDER="NV-NODE-002 NV-NODE-003 NV-OP-001 NV-DCGM-001 NV-DRV-001"

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

ok() {
    PASS=$((PASS + 1))
    echo "ok   - $1"
}

nope() {
    FAIL=$((FAIL + 1))
    echo "FAIL - $1"
    shift
    local line
    for line in "$@"; do
        echo "       ${line}"
    done
}

assert_rc() {
    local want="$1" name="$2"
    if [ "${RC}" -eq "${want}" ]; then
        ok "${name} (rc=${want})"
    else
        nope "${name}: want rc=${want}, got rc=${RC}" "stdout: ${OUT}" "stderr: ${ERR}"
    fi
}

assert_says() {
    local want="$1" name="$2"
    if printf '%s\n%s\n' "${OUT}" "${ERR}" | grep -qF -- "${want}"; then
        ok "${name}"
    else
        nope "${name}: output does not contain: ${want}" "stdout: ${OUT}" "stderr: ${ERR}"
    fi
}

assert_logged() {
    local want="$1" name="$2"
    if grep -qF -- "${want}" "${EVAL_CALL_LOG}" 2> /dev/null; then
        ok "${name}"
    else
        nope "${name}: no recorded call contains: ${want}" "calls:" "$(cat "${EVAL_CALL_LOG}" 2> /dev/null)"
    fi
}

assert_not_logged() {
    local want="$1" name="$2"
    if grep -qF -- "${want}" "${EVAL_CALL_LOG}" 2> /dev/null; then
        nope "${name}: a recorded call contains: ${want}" "calls:" "$(cat "${EVAL_CALL_LOG}")"
    else
        ok "${name}"
    fi
}

# assert_jq <filter> <want> <name>
# Runs a jq filter over the results file this run wrote and compares the raw
# output against a literal.
assert_jq() {
    local filter="$1" want="$2" name="$3" got
    if [ ! -f "${RESULT_FILE}" ]; then
        nope "${name}: no results file at ${RESULT_FILE}"
        return
    fi
    got="$(jq -r "${filter}" < "${RESULT_FILE}" 2>&1)"
    if [ "${got}" = "${want}" ]; then
        ok "${name}"
    else
        nope "${name}: ${filter} is '${got}', want '${want}'" "file: $(cat "${RESULT_FILE}")"
    fi
}

# ---------------------------------------------------------------------------
# Fakes
# ---------------------------------------------------------------------------

# make_tool_dir
# Symlinks to exactly the system tools the subject and the fakes need. The
# subject's PATH is this plus the shim directory and nothing else, so a real
# kubectl on the machine running these tests can never be reached, and with it
# the developer's own clusters.
EVAL_SYSTEM_TOOLS="bash sh env date mktemp grep sed awk cat sleep rm mkdir
chmod tr wc head tail cut sort dirname basename shasum stat ls printf jq cp
find touch id"

make_tool_dir() {
    mkdir -p "${TOOL_DIR}"
    local t resolved
    for t in ${EVAL_SYSTEM_TOOLS}; do
        resolved="$(command -v "${t}" 2> /dev/null || true)"
        case "${resolved}" in
            /*) ln -sf "${resolved}" "${TOOL_DIR}/${t}" ;;
        esac
    done
}

# make_kubectl_shim
# Answers the two questions the harness asks kubectl: which context the lane
# kubeconfig is on, and which workers the lane has. Both answers are driven by
# fixture variables so a case can make the context wrong.
make_kubectl_shim() {
    mkdir -p "${SHIM_DIR}"
    cat > "${SHIM_DIR}/kubectl" << 'SHIM'
#!/bin/bash
echo "kubectl $*" >> "${EVAL_CALL_LOG}"
args="$*"
case "${args}" in
    *"config current-context"*)
        printf '%s\n' "${FAKE_CONTEXT-kind-kubectl-nv-faults}"
        exit 0
        ;;
    *"get nodes"*)
        # The control plane is only left out when the selector asks for it, so
        # a subject that forgets the selector picks it up and the assertions
        # on the node name notice.
        nodes="${FAKE_WORKERS-kubectl-nv-faults-worker kubectl-nv-faults-worker2}"
        case "${args}" in
            *'!node-role.kubernetes.io/control-plane'*) ;;
            *) nodes="kubectl-nv-faults-control-plane ${nodes}" ;;
        esac
        printf '%s\n' "${nodes}"
        exit 0
        ;;
esac
exit 0
SHIM
    chmod +x "${SHIM_DIR}/kubectl"
}

# make_claude_shim
# Stands in for the default agent, so that the default KUBECTL_NV_EVAL_AGENT
# is exercised rather than taken on trust. It records its whole argv, which is
# what lets the test see that the allow-list reached it as one argument
# rather than being split or eaten by the shell.
make_claude_shim() {
    mkdir -p "${SHIM_DIR}"
    cat > "${SHIM_DIR}/claude" << 'SHIM'
#!/bin/bash
echo "claude $*" >> "${EVAL_CALL_LOG}"
for a in "$@"; do
    echo "claude-arg ${a}" >> "${EVAL_CALL_LOG}"
done
cat > /dev/null
key="FAKE_ANSWER_$(printf '%s' "${KUBECTL_NV_EVAL_CODE}" | tr 'A-Z-' 'a-z_')"
printf '%s\n' "${!key-}"
SHIM
    chmod +x "${SHIM_DIR}/claude"
}

# make_fault_scripts
# One fake per code, with the same interface as the real ones:
#   <code in lower case>.sh inject|recover <node>
# Each records its call and flips a state file that the fake binary reads, so
# the finding only appears after inject and only disappears after recover.
make_fault_scripts() {
    mkdir -p "${FAULTS_DIR}"
    local code lower
    for code in ${EXPECTED_ORDER}; do
        lower="$(printf '%s' "${code}" | tr '[:upper:]' '[:lower:]')"
        cat > "${FAULTS_DIR}/${lower}.sh" << SHIM
#!/bin/bash
echo "fault ${code} \$*" >> "\${EVAL_CALL_LOG}"
mkdir -p "\${EVAL_STATE_DIR}"
case "\${1:-}" in
    inject)
        if [ -n "\${FAKE_INJECT_FAIL_CODE:-}" ] && [ "\${FAKE_INJECT_FAIL_CODE}" = "${code}" ]; then
            echo "inject failed on purpose" >&2
            exit 1
        fi
        printf '%s\n' "\${2:-}" > "\${EVAL_STATE_DIR}/${code}"
        ;;
    recover)
        rm -f "\${EVAL_STATE_DIR}/${code}"
        ;;
    *)
        echo "usage: ${lower}.sh inject|recover <node>" >&2
        exit 2
        ;;
esac
exit 0
SHIM
        chmod +x "${FAULTS_DIR}/${lower}.sh"
    done
}

# make_binary
# A fake kubectl-nv. `health -o json` prints a HealthReport whose findings are
# exactly the faults currently injected, so the harness's wait and its recovery
# wait both have something real to converge on.
#
# FAKE_OBSERVE_DELAY makes the first N health calls for a code report nothing,
# which is how "wait until the fault is observable" is driven.
make_binary() {
    cat > "${BIN}" << 'SHIM'
#!/bin/bash
echo "kubectl-nv $*" >> "${EVAL_CALL_LOG}"
case " $* " in
    *" health "*) ;;
    *" version "*) echo "kubectl-nv version fake"; exit 0 ;;
    *) exit 0 ;;
esac

findings=""
for f in "${EVAL_STATE_DIR}"/NV-*; do
    [ -e "${f}" ] || continue
    code="$(basename "${f}")"
    node="$(cat "${f}")"

    seen_file="${EVAL_STATE_DIR}/seen-${code}"
    n=0
    if [ -f "${seen_file}" ]; then n="$(cat "${seen_file}")"; fi
    n=$((n + 1))
    echo "${n}" > "${seen_file}"
    if [ "${n}" -le "${FAKE_OBSERVE_DELAY-0}" ]; then
        continue
    fi

    component="node"
    case "${code}" in
        NV-OP-001) component="gpu-operator" ;;
        NV-DCGM-001) component="dcgm-exporter" ;;
        NV-DRV-001) component="driver" ;;
    esac
    entry="{\"code\":\"${code}\",\"severity\":\"error\",\"node\":\"${node}\",\"component\":\"${component}\",\"message\":\"fake\",\"evidence\":{}}"
    if [ -n "${findings}" ]; then findings="${findings},"; fi
    findings="${findings}${entry}"
done

printf '{"apiVersion":"kubectl-nv.nvidia.com/v1alpha1","kind":"HealthReport","metadata":{"name":"health"},"findings":[%s],"summary":{"errors":0,"warnings":0,"infos":0}}\n' "${findings}"
if [ -n "${findings}" ]; then exit 1; fi
exit 0
SHIM
    chmod +x "${BIN}"
}

# make_agent_stub
# Prints a canned answer and records the working directory it was run in, so
# the test can assert what the agent was given. FAKE_ANSWER_<code with hyphens
# as underscores> overrides the answer for one case; FAKE_ANSWER is the
# default, and the placeholders are filled from the state file the fault wrote.
make_agent_stub() {
    cat > "${AGENT_STUB}" << 'SHIM'
#!/bin/bash
echo "agent $*" >> "${EVAL_CALL_LOG}"
echo "agent-cwd $(pwd)" >> "${EVAL_CALL_LOG}"
# Record exactly what the agent can see, sorted, one entry per line.
(cd "$(pwd)" && ls -A | sort | while read -r e; do echo "agent-sees ${e}" >> "${EVAL_CALL_LOG}"; done)
# Drain the prompt so the harness never blocks writing it.
prompt="$(cat)"
echo "agent-prompt-bytes ${#prompt}" >> "${EVAL_CALL_LOG}"

if [ "${FAKE_AGENT_RC-0}" != "0" ]; then
    echo "the agent failed" >&2
    exit "${FAKE_AGENT_RC}"
fi

key="FAKE_ANSWER_$(printf '%s' "${KUBECTL_NV_EVAL_CODE}" | tr 'A-Z-' 'a-z_')"
answer="${!key-}"
if [ -z "${answer}" ]; then
    answer="${FAKE_ANSWER-}"
fi
printf '%s\n' "${answer}"
SHIM
    chmod +x "${AGENT_STUB}"
}

# ---------------------------------------------------------------------------
# Running the subject
# ---------------------------------------------------------------------------

RESULT_FILE=""

reset_fixtures() {
    unset FAKE_CONTEXT FAKE_WORKERS FAKE_OBSERVE_DELAY FAKE_AGENT_RC
    unset FAKE_INJECT_FAIL_CODE FAKE_ANSWER
    local v
    for v in $(env | sed -n 's/^\(FAKE_ANSWER_[a-z0-9_]*\)=.*/\1/p'); do
        unset "${v}"
    done
    rm -rf "${STATE_DIR}" "${RESULTS_DIR}"
    mkdir -p "${STATE_DIR}" "${RESULTS_DIR}"
    : > "${EVAL_CALL_LOG}"
}

# run_subject
# The fixtures are exported by the caller; the subject itself takes no
# arguments, and a case that needs to pass one builds its own invocation.
run_subject() {
    PATH="${SHIM_DIR}:${TOOL_DIR}" HOME="${FAKE_HOME}" \
        KUBECTL_NV_EVAL_BIN="${BIN}" \
        KUBECTL_NV_EVAL_SKILL="${SKILL}" \
        KUBECTL_NV_EVAL_FAULTS_DIR="${FAULTS_DIR}" \
        KUBECTL_NV_EVAL_KUBECONFIG="${LANE_KUBECONFIG}" \
        KUBECTL_NV_EVAL_RESULTS_DIR="${RESULTS_DIR}" \
        KUBECTL_NV_EVAL_AGENT="${AGENT_STUB}" \
        KUBECTL_NV_EVAL_POLL_INTERVAL=0 \
        KUBECTL_NV_EVAL_OBSERVE_TIMEOUT=30 \
        KUBECTL_NV_EVAL_RECOVER_TIMEOUT=30 \
        "${RUN}" > "${WORK}/out" 2> "${WORK}/err"
    RC=$?
    OUT="$(cat "${WORK}/out")"
    ERR="$(cat "${WORK}/err")"
    RESULT_FILE="$(find "${RESULTS_DIR}" -name '*.json' -type f 2> /dev/null | sort | tail -1)"
}

# ---------------------------------------------------------------------------
# Fixtures shared by every case
# ---------------------------------------------------------------------------

make_tool_dir
make_kubectl_shim
make_claude_shim
make_fault_scripts
make_binary
make_agent_stub

mkdir -p "${FAKE_HOME}/.kube"
printf '%s\n' 'SENTINEL do not touch the default kubeconfig' > "${FAKE_HOME}/.kube/config"
SENTINEL_BEFORE="$(shasum -a 256 < "${FAKE_HOME}/.kube/config" | awk '{print $1}')"

printf '%s\n' 'apiVersion: v1' 'current-context: kind-kubectl-nv-faults' > "${LANE_KUBECONFIG}"
cp "${REPO_ROOT}/SKILL.md" "${SKILL}"

WORKER="kubectl-nv-faults-worker"

# A complete answer for one code, filled in by the caller.
answer_for() {
    local code="$1" component="$2" node="${3:-${WORKER}}"
    printf 'The GPU problem is on node %s. The failing component is %s and the finding code is %s.' \
        "${node}" "${component}" "${code}"
}

set_passing_answers() {
    export FAKE_ANSWER_nv_node_002 FAKE_ANSWER_nv_node_003 FAKE_ANSWER_nv_op_001
    export FAKE_ANSWER_nv_dcgm_001 FAKE_ANSWER_nv_drv_001
    FAKE_ANSWER_nv_node_002="$(answer_for NV-NODE-002 node)"
    FAKE_ANSWER_nv_node_003="$(answer_for NV-NODE-003 node)"
    FAKE_ANSWER_nv_op_001="$(answer_for NV-OP-001 gpu-operator)"
    FAKE_ANSWER_nv_dcgm_001="$(answer_for NV-DCGM-001 dcgm-exporter)"
    FAKE_ANSWER_nv_drv_001="$(answer_for NV-DRV-001 driver)"
}

# ---------------------------------------------------------------------------
echo "# the subject exists and is executable"
# ---------------------------------------------------------------------------

if [ -x "${RUN}" ]; then
    ok "hack/eval/run.sh is executable"
else
    nope "hack/eval/run.sh is missing or not executable"
    echo
    echo "passed: ${PASS}  failed: ${FAIL}"
    exit 1
fi

if [ -x "${RENDER}" ]; then
    ok "hack/eval/render-results.sh is executable"
else
    nope "hack/eval/render-results.sh is missing or not executable"
fi

# ---------------------------------------------------------------------------
echo "# every case passes when the agent answers completely"
# ---------------------------------------------------------------------------

reset_fixtures
set_passing_answers
run_subject

assert_rc 0 "a run where every answer is complete exits 0"
assert_jq '.summary.cases' '5' "the results file records five cases"
assert_jq '.summary.passed' '5' "the results file records five passes"
assert_jq '.summary.failed' '0' "the results file records no failures"
assert_jq '[.cases[].code] | join(" ")' "${EXPECTED_ORDER}" "the cases are recorded in the declared order"
assert_jq '.cases[2].component' 'gpu-operator' "the component comes from the report, not from a table in the harness"
assert_jq '[.cases[].node] | unique | join(" ")' "${WORKER}" "every case ran against the lane worker"
assert_jq '.context' 'kind-kubectl-nv-faults' "the results file records the context"

# ---------------------------------------------------------------------------
echo "# sequencing: one fault at a time, injected then recovered in order"
# ---------------------------------------------------------------------------

INJECTS="$(grep '^fault .* inject ' "${EVAL_CALL_LOG}" | awk '{print $2}' | tr '\n' ' ' | sed 's/ $//')"
if [ "${INJECTS}" = "${EXPECTED_ORDER}" ]; then
    ok "the faults are injected in the declared order"
else
    nope "injection order is '${INJECTS}', want '${EXPECTED_ORDER}'"
fi

RECOVERS="$(grep '^fault .* recover ' "${EVAL_CALL_LOG}" | awk '{print $2}' | tr '\n' ' ' | sed 's/ $//')"
if [ "${RECOVERS}" = "${EXPECTED_ORDER}" ]; then
    ok "every fault is recovered, in the same order"
else
    nope "recovery order is '${RECOVERS}', want '${EXPECTED_ORDER}'"
fi

# An inject of the next code must never appear before the recover of the one
# before it: the harness runs one fault at a time, not five at once.
ORDERED="$(grep -E '^fault .* (inject|recover) ' "${EVAL_CALL_LOG}" | awk '{print $2, $3}' | tr '\n' '|')"
EXPECT_PAIRS=""
for code in ${EXPECTED_ORDER}; do
    EXPECT_PAIRS="${EXPECT_PAIRS}${code} inject|${code} recover|"
done
if [ "${ORDERED}" = "${EXPECT_PAIRS}" ]; then
    ok "each fault is recovered before the next is injected"
else
    nope "inject and recover interleave as '${ORDERED}'" "want '${EXPECT_PAIRS}'"
fi

# ---------------------------------------------------------------------------
echo "# the agent is given the binary, the skill and the kubeconfig, and nothing else"
# ---------------------------------------------------------------------------

SEEN="$(grep '^agent-sees ' "${EVAL_CALL_LOG}" | awk '{print $2}' | sort -u | tr '\n' ' ' | sed 's/ $//')"
if [ "${SEEN}" = "SKILL.md kubeconfig kubectl-nv" ]; then
    ok "the agent working directory holds exactly the binary, the skill and the kubeconfig"
else
    nope "the agent saw '${SEEN}', want 'SKILL.md kubeconfig kubectl-nv'"
fi

CWDS="$(grep '^agent-cwd ' "${EVAL_CALL_LOG}" | awk '{print $2}' | sort -u | wc -l | tr -d ' ')"
if [ "${CWDS}" = "5" ]; then
    ok "each case gets its own working directory"
else
    nope "the agent ran in ${CWDS} distinct directories, want 5"
fi

# ---------------------------------------------------------------------------
echo "# the harness waits for the fault to become observable"
# ---------------------------------------------------------------------------

reset_fixtures
set_passing_answers
export FAKE_OBSERVE_DELAY=2
run_subject
unset FAKE_OBSERVE_DELAY

assert_rc 0 "a fault that takes a few polls to appear still passes"
assert_jq '[.cases[].observedPolls] | map(select(. >= 3)) | length' '5' \
    "each case records that it polled until the finding appeared"

# The agent must never be started before the finding is observable: a run that
# scored on a cluster that was still healthy proves nothing.
FIRST_AGENT="$(grep -n '^agent ' "${EVAL_CALL_LOG}" | head -1 | cut -d: -f1)"
THIRD_HEALTH="$(grep -n '^kubectl-nv .*health' "${EVAL_CALL_LOG}" | head -3 | tail -1 | cut -d: -f1)"
if [ -n "${FIRST_AGENT}" ] && [ -n "${THIRD_HEALTH}" ] && [ "${FIRST_AGENT}" -gt "${THIRD_HEALTH}" ]; then
    ok "the agent starts only after the finding is observable"
else
    nope "the agent started at line ${FIRST_AGENT:-none}, before the finding appeared at ${THIRD_HEALTH:-none}"
fi

# ---------------------------------------------------------------------------
echo "# scoring"
# ---------------------------------------------------------------------------

# An answer that names the wrong node is not an answer about this cluster.
reset_fixtures
set_passing_answers
export FAKE_ANSWER_nv_node_002
FAKE_ANSWER_nv_node_002="$(answer_for NV-NODE-002 node "kubectl-nv-faults-worker2")"
run_subject
assert_rc 1 "a run with one failed case exits 1"
assert_jq '.cases[0].pass' 'false' "an answer naming the wrong node does not pass"
assert_jq '.cases[0].named.node' 'false' "the wrong node is recorded as the part that was missing"
assert_jq '.cases[0].named.code' 'true' "the code it did name is still recorded as named"
assert_jq '.summary.passed' '4' "the other four cases still pass"

# An answer that names the wrong code points at the wrong problem.
reset_fixtures
set_passing_answers
export FAKE_ANSWER_nv_op_001
FAKE_ANSWER_nv_op_001="$(answer_for NV-NODE-002 gpu-operator)"
run_subject
assert_rc 1 "a run with a wrong code exits 1"
assert_jq '.cases[2].pass' 'false' "an answer naming the wrong code does not pass"
assert_jq '.cases[2].named.code' 'false' "the wrong code is recorded as the part that was missing"

# An answer that leaves the component out is half an answer.
reset_fixtures
set_passing_answers
export FAKE_ANSWER_nv_dcgm_001
FAKE_ANSWER_nv_dcgm_001="The GPU problem is on node ${WORKER} and the finding code is NV-DCGM-001."
run_subject
assert_rc 1 "a run with a missing component exits 1"
assert_jq '.cases[3].pass' 'false' "an answer with no component does not pass"
assert_jq '.cases[3].named.component' 'false' "the missing component is recorded"
assert_jq '.cases[3].named.node' 'true' "the node it did name is still recorded as named"

# An empty answer names nothing at all.
reset_fixtures
set_passing_answers
export FAKE_ANSWER_nv_drv_001=""
run_subject
assert_rc 1 "an empty answer fails its case"
assert_jq '.cases[4].pass' 'false' "an empty answer does not pass"

# ---------------------------------------------------------------------------
echo "# recovery happens whatever went wrong"
# ---------------------------------------------------------------------------

reset_fixtures
set_passing_answers
export FAKE_AGENT_RC=7
run_subject
unset FAKE_AGENT_RC

assert_rc 1 "a run where the agent fails exits 1"
RECOVERS="$(grep '^fault .* recover ' "${EVAL_CALL_LOG}" | awk '{print $2}' | tr '\n' ' ' | sed 's/ $//')"
if [ "${RECOVERS}" = "${EXPECTED_ORDER}" ]; then
    ok "an agent that fails still leaves every fault recovered"
else
    nope "after an agent failure the recovered faults are '${RECOVERS}', want '${EXPECTED_ORDER}'"
fi
LEFTOVER="$(find "${STATE_DIR}" -name 'NV-*' 2> /dev/null | wc -l | tr -d ' ')"
if [ "${LEFTOVER}" = "0" ]; then
    ok "no fault is left injected after an agent failure"
else
    nope "${LEFTOVER} faults are still injected after an agent failure"
fi
assert_jq '.cases[0].error' 'the agent command exited 7' "the agent failure is recorded on the case"

# A fault that cannot be injected is a failed case, and the rest still run.
reset_fixtures
set_passing_answers
export FAKE_INJECT_FAIL_CODE=NV-OP-001
run_subject
unset FAKE_INJECT_FAIL_CODE

assert_rc 1 "a run where one injection fails exits 1"
assert_jq '.cases[2].pass' 'false' "the case whose injection failed does not pass"
assert_jq '.summary.passed' '4' "the other four cases still run and pass"
assert_logged "fault NV-OP-001 recover" "a failed injection is still recovered"
assert_logged "fault NV-DCGM-001 inject" "the run continues to the next fault"

# ---------------------------------------------------------------------------
echo "# the default agent command is a command line that runs"
# ---------------------------------------------------------------------------
#
# The default is a claude -p line carrying a tool allow-list with parentheses
# in it. The harness evaluates the agent command as a shell command line, so an
# allow-list that is not quoted is a syntax error rather than a run, and no
# amount of reading the string says which.

reset_fixtures
set_passing_answers
PATH="${SHIM_DIR}:${TOOL_DIR}" HOME="${FAKE_HOME}" \
    KUBECTL_NV_EVAL_BIN="${BIN}" \
    KUBECTL_NV_EVAL_SKILL="${SKILL}" \
    KUBECTL_NV_EVAL_FAULTS_DIR="${FAULTS_DIR}" \
    KUBECTL_NV_EVAL_KUBECONFIG="${LANE_KUBECONFIG}" \
    KUBECTL_NV_EVAL_RESULTS_DIR="${RESULTS_DIR}" \
    KUBECTL_NV_EVAL_POLL_INTERVAL=0 \
    KUBECTL_NV_EVAL_OBSERVE_TIMEOUT=30 \
    KUBECTL_NV_EVAL_RECOVER_TIMEOUT=30 \
    "${RUN}" > "${WORK}/out" 2> "${WORK}/err"
RC=$?
OUT="$(cat "${WORK}/out")"
ERR="$(cat "${WORK}/err")"
RESULT_FILE="$(find "${RESULTS_DIR}" -name '*.json' -type f 2> /dev/null | sort | tail -1)"

assert_rc 0 "the default agent command runs and its answers score"
assert_logged "claude-arg -p" "the default agent runs claude in print mode"
assert_logged "claude-arg Bash(./kubectl-nv:*)" \
    "the tool allow-list reaches claude as one argument, parentheses and all"
assert_jq '.agent | test("claude")' 'true' "the results file records the agent that ran"

# ---------------------------------------------------------------------------
echo "# the harness refuses to run where it must not"
# ---------------------------------------------------------------------------

reset_fixtures
set_passing_answers
export CI=true
run_subject
unset CI

assert_rc 2 "CI set is a refusal"
assert_says "CI" "the refusal says what it refused"
assert_not_logged "inject" "nothing is injected when the run is refused for CI"
if [ -z "${RESULT_FILE}" ]; then
    ok "a refused run writes no results file"
else
    nope "a refused run wrote ${RESULT_FILE}"
fi

reset_fixtures
set_passing_answers
export CI=""
run_subject
unset CI
assert_rc 0 "an empty CI is not set, so the run proceeds"

reset_fixtures
set_passing_answers
export FAKE_CONTEXT="docker-desktop"
run_subject
unset FAKE_CONTEXT

assert_rc 2 "a context that is not a kubectl-nv lane is a refusal"
assert_says "docker-desktop" "the refusal names the context it refused"
assert_not_logged "inject" "nothing is injected against a cluster that is not a lane"

reset_fixtures
set_passing_answers
export FAKE_CONTEXT="kind-kubectl-nv-faults-evil.example.com"
run_subject
unset FAKE_CONTEXT
assert_rc 0 "a context under the kubectl-nv lane prefix is accepted"

reset_fixtures
set_passing_answers
export FAKE_CONTEXT="prod-kind-kubectl-nv-faults"
run_subject
unset FAKE_CONTEXT
assert_rc 2 "a context that merely contains the lane prefix is refused"

# A missing binary cannot be evaluated, and must not be worked around.
reset_fixtures
set_passing_answers
PATH="${SHIM_DIR}:${TOOL_DIR}" HOME="${FAKE_HOME}" \
    KUBECTL_NV_EVAL_BIN="${WORK}/no-such-binary" \
    KUBECTL_NV_EVAL_SKILL="${SKILL}" \
    KUBECTL_NV_EVAL_FAULTS_DIR="${FAULTS_DIR}" \
    KUBECTL_NV_EVAL_KUBECONFIG="${LANE_KUBECONFIG}" \
    KUBECTL_NV_EVAL_RESULTS_DIR="${RESULTS_DIR}" \
    KUBECTL_NV_EVAL_AGENT="${AGENT_STUB}" \
    "${RUN}" > "${WORK}/out" 2> "${WORK}/err"
RC=$?
OUT="$(cat "${WORK}/out")"
ERR="$(cat "${WORK}/err")"
assert_rc 3 "a missing binary is exit 3"
assert_not_logged "inject" "nothing is injected when the binary is missing"

# ---------------------------------------------------------------------------
echo "# the default kubeconfig is never touched"
# ---------------------------------------------------------------------------

SENTINEL_AFTER="$(shasum -a 256 < "${FAKE_HOME}/.kube/config" | awk '{print $1}')"
if [ "${SENTINEL_BEFORE}" = "${SENTINEL_AFTER}" ]; then
    ok "the default kubeconfig is byte for byte what it was"
else
    nope "the default kubeconfig changed: ${SENTINEL_BEFORE} -> ${SENTINEL_AFTER}"
fi

KUBECTL_WITHOUT_KUBECONFIG="$(grep '^kubectl ' "${EVAL_CALL_LOG}" | grep -cv -- '--kubeconfig' || true)"
if [ "${KUBECTL_WITHOUT_KUBECONFIG}" = "0" ]; then
    ok "every kubectl call names the lane kubeconfig"
else
    nope "${KUBECTL_WITHOUT_KUBECONFIG} kubectl calls were made without --kubeconfig"
fi

# ---------------------------------------------------------------------------
echo "# rendering the README table"
# ---------------------------------------------------------------------------

README_FIXTURE="${WORK}/README.md"
write_readme_fixture() {
    {
        printf '%s\n' '# Title'
        printf '%s\n' ''
        printf '%s\n' '<!-- eval-results:start -->'
        printf '%s\n' 'stale content that must be replaced'
        printf '%s\n' '<!-- eval-results:end -->'
        printf '%s\n' ''
        printf '%s\n' '## After'
    } > "${README_FIXTURE}"
}

EMPTY_RESULTS="${WORK}/empty-results"
mkdir -p "${EMPTY_RESULTS}"
write_readme_fixture
PATH="${SHIM_DIR}:${TOOL_DIR}" "${RENDER}" --results "${EMPTY_RESULTS}" --readme "${README_FIXTURE}" \
    > "${WORK}/out" 2> "${WORK}/err"
RC=$?
OUT="$(cat "${WORK}/out")"
ERR="$(cat "${WORK}/err")"
assert_rc 0 "rendering an empty results directory succeeds"
if grep -q 'No evaluation run has been recorded' "${README_FIXTURE}"; then
    ok "an empty results directory renders a line saying so"
else
    nope "an empty results directory did not render the empty note" "$(cat "${README_FIXTURE}")"
fi
if grep -q 'stale content' "${README_FIXTURE}"; then
    nope "rendering left the stale content in place"
else
    ok "rendering replaced what was between the markers"
fi
if grep -q '^## After' "${README_FIXTURE}"; then
    ok "rendering left the rest of the README alone"
else
    nope "rendering ate the content after the end marker"
fi

reset_fixtures
set_passing_answers
run_subject
write_readme_fixture
PATH="${SHIM_DIR}:${TOOL_DIR}" "${RENDER}" --results "${RESULTS_DIR}" --readme "${README_FIXTURE}" \
    > "${WORK}/out" 2> "${WORK}/err"
RC=$?
OUT="$(cat "${WORK}/out")"
ERR="$(cat "${WORK}/err")"
assert_rc 0 "rendering a recorded run succeeds"
if grep -q '| 5 | 5 |' "${README_FIXTURE}"; then
    ok "the rendered table carries the case and pass counts"
else
    nope "the rendered table has no row for the recorded run" "$(cat "${README_FIXTURE}")"
fi
if grep -q 'No evaluation run has been recorded' "${README_FIXTURE}"; then
    nope "the empty note survived a recorded run"
else
    ok "the empty note is gone once a run is recorded"
fi

# Rendering twice must give the same file: the markers are replaced, not
# appended to.
BEFORE_SECOND="$(shasum -a 256 < "${README_FIXTURE}" | awk '{print $1}')"
PATH="${SHIM_DIR}:${TOOL_DIR}" "${RENDER}" --results "${RESULTS_DIR}" --readme "${README_FIXTURE}" \
    > /dev/null 2>&1
AFTER_SECOND="$(shasum -a 256 < "${README_FIXTURE}" | awk '{print $1}')"
if [ "${BEFORE_SECOND}" = "${AFTER_SECOND}" ]; then
    ok "rendering is idempotent"
else
    nope "rendering twice gave two different files"
fi

# ---------------------------------------------------------------------------
echo "# the shipped README carries the markers the renderer needs"
# ---------------------------------------------------------------------------

if grep -q '<!-- eval-results:start -->' "${REPO_ROOT}/README.md" &&
    grep -q '<!-- eval-results:end -->' "${REPO_ROOT}/README.md"; then
    ok "README.md carries both result markers"
else
    nope "README.md is missing one of the eval-results markers"
fi

# ---------------------------------------------------------------------------
echo "# static analysis"
# ---------------------------------------------------------------------------

if command -v shellcheck > /dev/null 2>&1; then
    for dir in / "${SCRIPT_DIR}"; do
        if (cd "${dir}" && shellcheck -x "${RUN}" "${RENDER}" "${SCRIPT_DIR}/run_test.sh"); then
            ok "shellcheck -x is clean from ${dir}"
        else
            nope "shellcheck -x reported findings from ${dir}"
        fi
    done
else
    echo "SKIP - shellcheck is not installed; the scripts were not linted"
fi

# ---------------------------------------------------------------------------

echo
echo "passed: ${PASS}  failed: ${FAIL}"
[ "${FAIL}" -eq 0 ]
