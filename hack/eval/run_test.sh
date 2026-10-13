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
# Tests for hack/eval/run.sh, hack/eval/kubectl-nv-shim.sh and
# hack/eval/render-results.sh.
#
# No cluster is created and no model is called. kubectl is a PATH shim, the
# fault scripts are fakes that record their calls and flip a state file, the
# binary under evaluation is a fake that reports findings from that state, and
# claude is a stub that reads the fake binary through the real shim and prints
# an ANSWER line.
#
# The fake pieces are wired to each other the way the real ones are: the fault
# script is what makes the finding appear, the binary is what the harness
# polls, and the claude stub reaches the binary only through the checked-in
# shim. A harness that scored without injecting, that never waited for the
# fault, or that handed the agent an unguarded binary gets a different answer
# and the assertions notice.
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
SHIM_TEMPLATE="${SCRIPT_DIR}/kubectl-nv-shim.sh"
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
OVERRIDE_DIR="${WORK}/answers"
BIN="${WORK}/kubectl-nv"
SKILL="${WORK}/SKILL.md"
LANE_KUBECONFIG="${WORK}/kubeconfig-faults"

export EVAL_CALL_LOG="${WORK}/calls.log"
export EVAL_STATE_DIR="${STATE_DIR}"

# The five codes the harness runs, in the order it has to run them.
EXPECTED_ORDER="NV-NODE-002 NV-NODE-003 NV-CTK-001 NV-DCGM-001 NV-DRV-001"

WORKER="kubectl-nv-faults-worker"
LANE_CONTEXT="kind-kubectl-nv-faults"

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

assert_eq() {
    local got="$1" want="$2" name="$3"
    if [ "${got}" = "${want}" ]; then
        ok "${name}"
    else
        nope "${name}: got '${got}', want '${want}'"
    fi
}

# assert_jq <filter> <want> <name>
assert_jq() {
    local filter="$1" want="$2" name="$3" got
    if [ -z "${RESULT_FILE}" ] || [ ! -f "${RESULT_FILE}" ]; then
        nope "${name}: no results file was written"
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
# Symlinks to exactly the system tools the subject, the fakes and the agent
# need. The subject's PATH is this plus the shim directory and nothing else,
# so a real kubectl or a real claude on the machine running these tests can
# never be reached, and with them the developer's own clusters and account.
EVAL_SYSTEM_TOOLS="bash sh env date mktemp grep sed awk cat sleep rm mkdir
chmod tr wc head tail cut sort dirname basename shasum stat ls printf jq cp
find touch id kill uniq readlink seq cmp"

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
# Answers the three questions the harness asks kubectl: which context the lane
# kubeconfig is on, what the whole minified config looks like, and which
# workers the lane has.
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
    *"config view"*)
        # One context, one cluster, on a loopback server, unless a fixture
        # says otherwise. This is what the lane guard reads.
        printf 'apiVersion: v1\nkind: Config\ncurrent-context: %s\ncontexts:\n' \
            "${FAKE_CONTEXT-kind-kubectl-nv-faults}"
        printf -- '- name: %s\n' "${FAKE_CONTEXT-kind-kubectl-nv-faults}"
        if [ -n "${FAKE_EXTRA_CONTEXT:-}" ]; then
            printf -- '- name: %s\n' "${FAKE_EXTRA_CONTEXT}"
        fi
        printf 'clusters:\n- cluster:\n    server: %s\n' \
            "${FAKE_SERVER-https://127.0.0.1:52193}"
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
        printf '%s\n' ${nodes}
        exit 0
        ;;
esac
exit 0
SHIM
    chmod +x "${SHIM_DIR}/kubectl"
}

# make_claude_shim
# Stands in for the agent. It is given nothing but the scrubbed environment
# the harness builds, so it has to do what a real agent would: run
# ./kubectl-nv (the harness's shim, never the binary itself) and report what
# it found as an ANSWER line.
#
# A case that needs a wrong answer puts the line it should print in
# ${OVERRIDE_DIR}/<code>; the stub still has to derive the code correctly to
# find it, so an override does not let it skip the work. The override
# directory is baked in rather than passed through the environment, because
# the harness scrubs the environment and that is the point.
make_claude_shim() {
    mkdir -p "${SHIM_DIR}"
    {
        printf '#!/bin/bash\n'
        printf 'CALL_LOG=%s\n' "${EVAL_CALL_LOG}"
        printf 'OVERRIDE_DIR=%s\n' "${OVERRIDE_DIR}"
    } > "${SHIM_DIR}/claude"
    cat >> "${SHIM_DIR}/claude" << 'SHIM'
echo "claude $*" >> "${CALL_LOG}"
for a in "$@"; do
    echo "claude-arg ${a}" >> "${CALL_LOG}"
done
env | sort | while read -r line; do echo "claude-env ${line}" >> "${CALL_LOG}"; done
echo "claude-cwd $(pwd)" >> "${CALL_LOG}"
ls -A | sort | while read -r e; do echo "claude-sees ${e}" >> "${CALL_LOG}"; done
if [ -L ./kubectl-nv ]; then
    echo "claude-sees-symlink kubectl-nv" >> "${CALL_LOG}"
fi
if grep -q '^kind: Config' ./kubeconfig 2> /dev/null; then
    echo "claude-sees-minified-kubeconfig" >> "${CALL_LOG}"
fi
cat > /dev/null

if [ -f "${OVERRIDE_DIR}/.hang" ]; then
    sleep 600
fi

report="$(./kubectl-nv health --fail-on info -o json 2> /dev/null)"

# SKILL.md's rule, and nothing hand tuned: take the finding that names a
# node, highest severity among those, and fall back to a cluster scoped one
# only when no node scoped finding accounts for anything. sort_by is stable,
# so equal severities keep the order the report put them in.
pick="$(printf '%s' "${report}" | jq -c '
    def rank: if . == "error" then 0 elif . == "warning" then 1 else 2 end;
    [.findings[]] as $all
    | ([$all[] | select((.node // "") != "")] | sort_by(.severity | rank)) as $scoped
    | if ($scoped | length) > 0 then $scoped[0] else ($all[0] // empty) end')"
code="$(printf '%s' "${pick}" | jq -r '.code // empty')"
node="$(printf '%s' "${pick}" | jq -r '.node // "-"')"
component="$(printf '%s' "${pick}" | jq -r '.component // empty')"
[ -n "${node}" ] || node="-"

if [ -f "${OVERRIDE_DIR}/.rc" ]; then
    echo "the agent failed" >&2
    exit "$(cat "${OVERRIDE_DIR}/.rc")"
fi

if [ -n "${code}" ] && [ -f "${OVERRIDE_DIR}/${code}" ]; then
    cat "${OVERRIDE_DIR}/${code}"
    exit 0
fi

printf 'I looked at the cluster with ./kubectl-nv health.\n'
printf 'ANSWER node=%s component=%s code=%s\n' "${node}" "${component}" "${code}"
SHIM
    chmod +x "${SHIM_DIR}/claude"
}

# make_fault_scripts
# One fake per code, with the same interface as E3's:
#   MOKKA_KUBECONFIG=<path> <code in lower case>.sh inject|recover <node>
# E3's scripts exit 2 when MOKKA_KUBECONFIG is unset, with no fallback, so
# these do too: a harness that forgot to pass it must not pass these tests.
make_fault_scripts() {
    mkdir -p "${FAULTS_DIR}"
    local code lower
    for code in ${EXPECTED_ORDER}; do
        lower="$(printf '%s' "${code}" | tr '[:upper:]' '[:lower:]')"
        cat > "${FAULTS_DIR}/${lower}.sh" << SHIM
#!/bin/bash
echo "fault ${code} \$*" >> "\${EVAL_CALL_LOG}"
if [ -z "\${MOKKA_KUBECONFIG:-}" ]; then
    echo "${lower}.sh: MOKKA_KUBECONFIG is not set" >&2
    exit 2
fi
echo "fault-kubeconfig ${code} \${MOKKA_KUBECONFIG}" >> "\${EVAL_CALL_LOG}"
mkdir -p "\${EVAL_STATE_DIR}"
case "\${1:-}" in
    inject)
        if [ "\${FAKE_INJECT_FAIL_CODE:-}" = "${code}" ]; then
            echo "inject failed on purpose" >&2
            exit 1
        fi
        printf '%s\n' "\${2:-}" > "\${EVAL_STATE_DIR}/${code}"
        ;;
    recover)
        if [ "\${FAKE_RECOVER_FAIL_CODE:-}" = "${code}" ]; then
            echo "recover failed on purpose" >&2
            exit 1
        fi
        if [ "\${FAKE_RECOVER_FAIL_CLEAN_CODE:-}" = "${code}" ]; then
            # Undoes the fault and still reports failure, which is what a
            # script that cleaned up and then hit an error on its last step
            # looks like. Only the exit code says anything is wrong.
            rm -f "\${EVAL_STATE_DIR}/${code}"
            echo "recover failed on purpose after undoing the fault" >&2
            exit 1
        fi
        if [ "\${FAKE_RECOVER_NOOP_CODE:-}" = "${code}" ]; then
            exit 0
        fi
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
# exactly the faults currently injected, so the harness's wait and its
# recovery wait both have something real to converge on.
#
# The components are the real inventory.Component* values, and the severities
# and co-occurrences are what the faults lane produced on 2026-10-13.
make_binary() {
    # The paths are baked in rather than read from the environment: the
    # harness runs the agent under env -i, so a fake that needed EVAL_* set
    # would answer nothing there and every case would score zero for a reason
    # that has nothing to do with the subject.
    {
        printf '#!/bin/bash\n'
        printf 'EVAL_CALL_LOG=%s\n' "${EVAL_CALL_LOG}"
        printf 'EVAL_STATE_DIR=%s\n' "${EVAL_STATE_DIR}"
    } > "${BIN}"
    cat >> "${BIN}" << 'SHIM'
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
    node_field="\"node\":\"${node}\","
    case "${code}" in
        NV-CTK-001) component="nvidia-container-toolkit-daemonset" ;;
        NV-DCGM-001) component="nvidia-dcgm-exporter" ;;
        NV-DRV-001) component="nvidia-driver-daemonset" ;;
    esac
    # A rule that is about the cluster rather than one node sets no node at
    # all. None of the five injected codes is one today, so a fixture asks for
    # it explicitly and keeps the harness's cluster scoped path covered.
    #
    # Read from the state directory, not the environment: the harness runs the
    # agent under env -i, so a fixture passed through the environment would
    # reach the harness's own polls and not the agent's, and the two would
    # disagree about the same cluster.
    if [ "$(cat "${EVAL_STATE_DIR}/.cluster-scoped" 2> /dev/null)" = "${code}" ]; then
        node_field=""
    fi
    severity="error"
    # Measured on the faults lane on 2026-10-12: NV-DCGM-001 is a warning, and
    # its injection patches the ClusterPolicy, so NV-OP-001 appears with it at
    # error. output.SortFindings orders by node then code, and a cluster
    # scoped finding has no node, so NV-OP-001 comes first in the array.
    if [ "${code}" = NV-DCGM-001 ]; then
        severity="warning"
        rollup="{\"code\":\"NV-OP-001\",\"severity\":\"error\",\"component\":\"gpu-operator\",\"message\":\"the ClusterPolicy is not ready\",\"evidence\":{}}"
        if [ -n "${findings}" ]; then findings="${findings},"; fi
        findings="${findings}${rollup}"
    fi
    entry="{\"code\":\"${code}\",\"severity\":\"${severity}\",${node_field}\"component\":\"${component}\",\"message\":\"fake\",\"evidence\":{}}"
    if [ -n "${findings}" ]; then findings="${findings},"; fi
    findings="${findings}${entry}"
done

printf '{"apiVersion":"kubectl-nv.nvidia.com/v1alpha1","kind":"HealthReport","metadata":{"name":"cluster"},"findings":[%s],"summary":{"errors":0,"warnings":0,"infos":0}}\n' "${findings}"
if [ -n "${findings}" ]; then exit 1; fi
exit 0
SHIM
    chmod +x "${BIN}"
}

# ---------------------------------------------------------------------------
# Running the subject
# ---------------------------------------------------------------------------

RESULT_FILE=""
RC=0
OUT=""
ERR=""

reset_fixtures() {
    unset FAKE_CONTEXT FAKE_WORKERS FAKE_OBSERVE_DELAY FAKE_SERVER FAKE_EXTRA_CONTEXT
    unset FAKE_INJECT_FAIL_CODE FAKE_RECOVER_FAIL_CODE FAKE_RECOVER_NOOP_CODE
    unset FAKE_RECOVER_FAIL_CLEAN_CODE
    rm -rf "${STATE_DIR}" "${RESULTS_DIR}" "${OVERRIDE_DIR}"
    mkdir -p "${STATE_DIR}" "${RESULTS_DIR}" "${OVERRIDE_DIR}"
    : > "${EVAL_CALL_LOG}"
}

# override_answer <code> <line>
override_answer() {
    printf '%s\n' "$2" > "${OVERRIDE_DIR}/$1"
}

# cluster_scope <code>
# Makes the fake report this code the way a cluster scoped rule does, with no
# node at all.
cluster_scope() {
    printf '%s\n' "$1" > "${STATE_DIR}/.cluster-scoped"
}

# subject_env
# The fixtures every invocation shares, one assignment per line.
subject_env() {
    printf '%s\n' \
        "KUBECTL_NV_EVAL_BIN=${BIN}" \
        "KUBECTL_NV_EVAL_SKILL=${SKILL}" \
        "KUBECTL_NV_EVAL_FAULTS_DIR=${FAULTS_DIR}" \
        "KUBECTL_NV_EVAL_KUBECONFIG=${LANE_KUBECONFIG}" \
        "KUBECTL_NV_EVAL_RESULTS_DIR=${RESULTS_DIR}" \
        "KUBECTL_NV_EVAL_POLL_INTERVAL=0" \
        "KUBECTL_NV_EVAL_OBSERVE_TIMEOUT=30" \
        "KUBECTL_NV_EVAL_RECOVER_TIMEOUT=30" \
        "KUBECTL_NV_EVAL_AGENT_TIMEOUT=60"
}

# run_subject [VAR=VALUE]...
# Runs the harness with the default agent, which the claude stub on PATH
# stands in for. Later assignments win, so a case can override a default.
#
# CI is removed rather than left alone, so that this suite passes under CI and
# only the refusal case sets it.
run_subject() {
    local -a env_args=()
    local line
    while IFS= read -r line; do
        env_args+=("${line}")
    done < <(subject_env)
    env_args+=("$@")

    env -u CI -u KUBECTL_NV_EVAL_AGENT -i \
        PATH="${SHIM_DIR}:${TOOL_DIR}" HOME="${FAKE_HOME}" \
        EVAL_CALL_LOG="${EVAL_CALL_LOG}" EVAL_STATE_DIR="${EVAL_STATE_DIR}" \
        TMPDIR="${TMPDIR:-/tmp}" \
        "${env_args[@]}" \
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

mkdir -p "${FAKE_HOME}/.kube"
printf '%s\n' 'SENTINEL do not touch the default kubeconfig' > "${FAKE_HOME}/.kube/config"
SENTINEL_BEFORE="$(shasum -a 256 < "${FAKE_HOME}/.kube/config" | awk '{print $1}')"

printf '%s\n' 'apiVersion: v1' "current-context: ${LANE_CONTEXT}" > "${LANE_KUBECONFIG}"
cp "${REPO_ROOT}/SKILL.md" "${SKILL}"

# ---------------------------------------------------------------------------
echo "# the subjects exist and are executable"
# ---------------------------------------------------------------------------

missing=0
for s in "${RUN}" "${RENDER}" "${SHIM_TEMPLATE}"; do
    if [ -x "${s}" ]; then
        ok "$(basename "${s}") is executable"
    else
        nope "$(basename "${s}") is missing or not executable"
        missing=1
    fi
done
if [ "${missing}" -ne 0 ]; then
    echo
    echo "passed: ${PASS}  failed: ${FAIL}"
    exit 1
fi

# ---------------------------------------------------------------------------
echo "# the checked-in shim refuses what would take the agent off the lane"
# ---------------------------------------------------------------------------
#
# The shim is exercised standalone, with the real binary replaced by a
# recorder, so that each refusal is a test of its own rather than something
# only a whole run could reach.

SHIM_REAL="${WORK}/shim-real"
cat > "${SHIM_REAL}" << 'SHIM'
#!/bin/bash
echo "real $*" >> "${EVAL_CALL_LOG}"
exit 0
SHIM
chmod +x "${SHIM_REAL}"

# run_shim <args...>
run_shim() {
    env PATH="${TOOL_DIR}" \
        EVAL_CALL_LOG="${EVAL_CALL_LOG}" \
        KUBECTL_NV_SHIM_REAL="${SHIM_REAL}" \
        KUBECTL_NV_SHIM_KUBECONFIG="${LANE_KUBECONFIG}" \
        KUBECTL_NV_SHIM_CONTEXT="${LANE_CONTEXT}" \
        bash "${SHIM_TEMPLATE}" "$@" > "${WORK}/out" 2> "${WORK}/err"
    RC=$?
    OUT="$(cat "${WORK}/out")"
    ERR="$(cat "${WORK}/err")"
}

: > "${EVAL_CALL_LOG}"
run_shim health -o json
assert_rc 0 "the shim passes a plain health through"
assert_logged "real health" "the plain command reaches the real binary"
assert_logged "--kubeconfig ${LANE_KUBECONFIG}" "the shim fixes the lane kubeconfig"
assert_logged "--context ${LANE_CONTEXT}" "the shim fixes the lane context"

# Every connection flag ConfigFlags registers, plus the two host directory
# flags. A flag that stopped being refused would let the agent point the
# binary at another cluster or write outside the directory it was given.
for flag in --kubeconfig --context --cluster --server --token --user \
    --as --as-group --as-uid --as-user-extra --certificate-authority \
    --client-certificate --client-key --insecure-skip-tls-verify \
    --tls-server-name --proxy-url --cache-dir --request-timeout \
    --disable-compression --write-dir --artifacts-dir; do
    : > "${EVAL_CALL_LOG}"
    run_shim health "${flag}" value
    if [ "${RC}" -eq 2 ]; then
        ok "the shim refuses ${flag}"
    else
        nope "the shim accepted ${flag}: rc=${RC}" "stderr: ${ERR}"
    fi
    assert_not_logged "real " "the shim does not reach the binary for ${flag}"
done

# The equals spelling is the same flag.
: > "${EVAL_CALL_LOG}"
run_shim health --server=https://evil.example.com
assert_rc 2 "the shim refuses --server=value"
assert_not_logged "real " "the shim does not reach the binary for --server=value"

# The short spellings.
for flag in -k -s; do
    : > "${EVAL_CALL_LOG}"
    run_shim health "${flag}" value
    if [ "${RC}" -eq 2 ]; then
        ok "the shim refuses ${flag}"
    else
        nope "the shim accepted ${flag}: rc=${RC}" "stderr: ${ERR}"
    fi
done

# The commands that can exec into a pod.
for cmd in "adm must-gather" "debug worker-1"; do
    : > "${EVAL_CALL_LOG}"
    # shellcheck disable=SC2086
    run_shim ${cmd}
    if [ "${RC}" -eq 2 ]; then
        ok "the shim refuses '${cmd}'"
    else
        nope "the shim accepted '${cmd}': rc=${RC}" "stderr: ${ERR}"
    fi
    assert_not_logged "real " "the shim does not reach the binary for '${cmd}'"
done

# A refusal names the flag, never its value: echoing a rejected --token back
# would copy the secret into the model's context.
: > "${EVAL_CALL_LOG}"
run_shim health --token s3cr3t-do-not-echo
assert_rc 2 "the shim refuses --token"
if printf '%s%s' "${OUT}" "${ERR}" | grep -qF 's3cr3t-do-not-echo'; then
    nope "the shim echoed the value of a rejected --token back"
else
    ok "the shim names the rejected flag without its value"
fi

# Read-only commands that are not exec capable still pass.
for cmd in "list -o json" "info worker-1 -o json" "explain NV-NODE-002" "topology -o json"; do
    : > "${EVAL_CALL_LOG}"
    # shellcheck disable=SC2086
    run_shim ${cmd}
    if [ "${RC}" -eq 0 ]; then
        ok "the shim passes '${cmd}' through"
    else
        nope "the shim refused '${cmd}': rc=${RC}" "stderr: ${ERR}"
    fi
done

# ---------------------------------------------------------------------------
echo "# every case passes when the agent answers completely"
# ---------------------------------------------------------------------------

reset_fixtures
run_subject

assert_rc 0 "a run where every answer is complete exits 0"
assert_jq '.summary.cases' '5' "the results file records five cases"
assert_jq '.summary.passed' '5' "the results file records five passes"
assert_jq '.summary.failed' '0' "the results file records no failures"
assert_jq '[.cases[].code] | join(" ")' "${EXPECTED_ORDER}" "the cases are recorded in the declared order"
assert_jq '.cases[2].component' 'nvidia-container-toolkit-daemonset' \
    "the component comes from the report, not from a table in the harness"
assert_jq '[.cases[].expectedNode] | unique | join(" ")' "${WORKER}" \
    "every case expects the lane worker, which is where each was injected"
assert_jq '.context' "${LANE_CONTEXT}" "the results file records the context"

assert_eq "$(basename "${RESULT_FILE}")" "$(date -u +%Y-%m-%d).json" "the results file is named for the date"
assert_jq '.schemaVersion' '1' "the results file declares its schema version"
assert_jq '.startedAt | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")' 'true' \
    "startedAt is RFC3339 in UTC"
assert_jq '.finishedAt | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")' 'true' \
    "finishedAt is RFC3339 in UTC"
assert_jq '.cases[0] | keys | join(",")' \
    'agentSeconds,code,component,error,expectedNode,named,node,observedPolls,pass,recoverError' \
    "a case carries exactly the documented keys"
assert_jq '.cases[0].named | keys | join(",")' 'code,component,node' \
    "the named map carries exactly the three parts"

# ---------------------------------------------------------------------------
echo "# the operand's own finding wins over the ClusterPolicy roll-up"
# ---------------------------------------------------------------------------
#
# PE-F3-20, measured on the faults lane on 2026-10-12. nv-dcgm-001.sh patches
# the ClusterPolicy, so four seconds later the policy is notReady and the
# report carries NV-OP-001 at error beside NV-DCGM-001 at warning.
# output.SortFindings orders by node then code and a cluster scoped finding
# has no node, so the roll-up sorts first AND outranks on severity: an agent
# taking the first finding, or the highest severity one, answers NV-OP-001 and
# gets the case wrong. SKILL.md now tells it to prefer the finding that names
# a node, and the stub follows that text.

# First the premise, so the assertions below are not vacuous: the fake really
# does report both, with the roll-up ahead of the operand.
printf '%s\n' "${WORKER}" > "${STATE_DIR}/NV-DCGM-001"
ROLLUP_REPORT="$(env EVAL_CALL_LOG=/dev/null "${BIN}" health --fail-on info -o json 2> /dev/null)"
rm -f "${STATE_DIR}/NV-DCGM-001" "${STATE_DIR}/seen-NV-DCGM-001"
assert_eq "$(printf '%s' "${ROLLUP_REPORT}" | jq -r '[.findings[].code] | join(",")')" \
    "NV-OP-001,NV-DCGM-001" \
    "the report carries the roll-up first and the operand second"
assert_eq "$(printf '%s' "${ROLLUP_REPORT}" | jq -r '[.findings[].severity] | join(",")')" \
    "error,warning" \
    "the roll-up also outranks the operand on severity"

# Then the behaviour.
assert_jq '.cases[3].pass' 'true' \
    "the case passes even though a higher severity cluster scoped finding sorts ahead of it"
assert_jq '.cases[3].component' 'nvidia-dcgm-exporter' \
    "the harness expects the operand's component, not the roll-up's"
assert_jq '.cases[3].named.code' 'true' "the agent named the operand's code, not NV-OP-001"

# ---------------------------------------------------------------------------
echo "# sequencing: one fault at a time, injected then recovered in order"
# ---------------------------------------------------------------------------

INJECTS="$(grep '^fault .* inject ' "${EVAL_CALL_LOG}" | awk '{print $2}' | tr '\n' ' ' | sed 's/ $//')"
assert_eq "${INJECTS}" "${EXPECTED_ORDER}" "the faults are injected in the declared order"

RECOVERS="$(grep '^fault .* recover ' "${EVAL_CALL_LOG}" | awk '{print $2}' | tr '\n' ' ' | sed 's/ $//')"
assert_eq "${RECOVERS}" "${EXPECTED_ORDER}" "every fault is recovered, in the same order"

ORDERED="$(grep -E '^fault .* (inject|recover) ' "${EVAL_CALL_LOG}" | awk '{print $2, $3}' | tr '\n' '|')"
EXPECT_PAIRS=""
for code in ${EXPECTED_ORDER}; do
    EXPECT_PAIRS="${EXPECT_PAIRS}${code} inject|${code} recover|"
done
assert_eq "${ORDERED}" "${EXPECT_PAIRS}" "each fault is recovered before the next is injected"

# E3's scripts exit 2 without MOKKA_KUBECONFIG, so every call has to carry it.
FAULT_CALLS="$(grep -cE '^fault .* (inject|recover) ' "${EVAL_CALL_LOG}")"
KUBECONFIG_CALLS="$(grep -cF "fault-kubeconfig " "${EVAL_CALL_LOG}")"
WRONG_KUBECONFIG="$(grep -F 'fault-kubeconfig ' "${EVAL_CALL_LOG}" | grep -cv " ${LANE_KUBECONFIG}\$" || true)"
if [ "${FAULT_CALLS}" -eq 10 ] && [ "${KUBECONFIG_CALLS}" -eq 10 ] && [ "${WRONG_KUBECONFIG}" -eq 0 ]; then
    ok "all ten fault script calls carried the lane MOKKA_KUBECONFIG"
else
    nope "${KUBECONFIG_CALLS} of ${FAULT_CALLS} fault calls carried MOKKA_KUBECONFIG, ${WRONG_KUBECONFIG} with the wrong path; want 10, 10, 0"
fi

# ---------------------------------------------------------------------------
echo "# what the agent is given, and what it is not"
# ---------------------------------------------------------------------------

SEEN="$(grep '^claude-sees ' "${EVAL_CALL_LOG}" | awk '{print $2}' | sort -u | tr '\n' ' ' | sed 's/ $//')"
assert_eq "${SEEN}" "SKILL.md kubeconfig kubectl-nv" \
    "the agent directory holds exactly the shim, the skill and the kubeconfig"

assert_not_logged "claude-sees-symlink" \
    "the binary in the case directory is a copy, not a link out of it"

# The agent is handed `config view --minify --flatten`, not the lane file:
# the minified form carries this one context and the credentials it needs,
# and nothing else the file happens to hold.
assert_eq "$(grep -c '^claude-sees-minified-kubeconfig$' "${EVAL_CALL_LOG}")" "5" \
    "the kubeconfig handed over is the minified, flattened view"

CWDS="$(grep '^claude-cwd ' "${EVAL_CALL_LOG}" | awk '{print $2}' | sort -u | wc -l | tr -d ' ')"
assert_eq "${CWDS}" "5" "each case gets its own working directory"

# The case directory is its own temporary directory, not a subdirectory of
# the harness scratch, so a cleanup bug in one cannot cover the other.
BAD_CASE_DIR=0
while IFS= read -r d; do
    [ -n "${d}" ] || continue
    case "${d}" in
        */kubectl-nv-eval-case.*) ;;
        *) BAD_CASE_DIR=$((BAD_CASE_DIR + 1)) ;;
    esac
done < <(grep '^claude-cwd ' "${EVAL_CALL_LOG}" | awk '{print $2}' | sort -u)
assert_eq "${BAD_CASE_DIR}" "0" "every case directory is its own temporary directory"

# The environment the agent runs in: an allow-list, and nothing else.
AGENT_ENV_KEYS="$(grep '^claude-env ' "${EVAL_CALL_LOG}" | sed 's/^claude-env //' | cut -d= -f1 |
    grep -vE '^(PWD|SHLVL|_)$' | sort -u | tr '\n' ' ' | sed 's/ $//')"
assert_eq "${AGENT_ENV_KEYS}" "HOME KUBECONFIG LANG PATH" \
    "the agent environment is PATH, LANG, HOME and KUBECONFIG and nothing else"

assert_not_logged "claude-env KUBECTL_NV_EVAL_CODE" \
    "the answer key is not in the agent environment"
assert_not_logged "claude-env KUBECTL_NV_EVAL_BIN" \
    "the harness configuration is not in the agent environment"
assert_not_logged "claude-env EVAL_CALL_LOG" \
    "the test fixtures are not in the agent environment"

AGENT_HOMES="$(grep '^claude-env HOME=' "${EVAL_CALL_LOG}" | sed 's/^claude-env HOME=//' | sort -u)"
if [ -z "${AGENT_HOMES}" ]; then
    nope "the agent recorded no HOME at all"
elif [ "${AGENT_HOMES}" = "${FAKE_HOME}" ]; then
    nope "the agent inherited the caller's HOME, so it can read its settings"
else
    ok "the agent runs with a HOME of its own"
fi

assert_eq "$(grep -c '^claude-env KUBECONFIG=./kubeconfig$' "${EVAL_CALL_LOG}")" "5" \
    "KUBECONFIG points at the copy in the case directory"

SURVIVING=0
while IFS= read -r d; do
    [ -n "${d}" ] || continue
    [ -e "${d}" ] && SURVIVING=$((SURVIVING + 1))
done < <(grep '^claude-cwd ' "${EVAL_CALL_LOG}" | awk '{print $2}' | sort -u)
assert_eq "${SURVIVING}" "0" "every case directory is removed when the run ends"

# ---------------------------------------------------------------------------
echo "# the default agent command is a command line that runs"
# ---------------------------------------------------------------------------

assert_logged "claude-arg -p" "the default agent runs claude in print mode"
assert_logged "claude-arg --restricted" "the default agent runs restricted"
assert_logged "claude-arg Bash,Read" "the default agent is given only Bash and Read"
assert_logged "claude-arg Bash(./kubectl-nv:*)" \
    "the tool allow-list reaches claude as one argument, parentheses and all"
assert_logged "claude-arg dontAsk" "the default agent never stops to ask"
assert_logged "claude-arg --strict-mcp-config" "the default agent ignores configured MCP servers"
assert_logged 'claude-arg {"mcpServers":{}}' "the default agent is handed an empty MCP config"
assert_not_logged "claude-arg --max-turns" "the default agent does not pass a flag claude does not have"

FIRST_ARGV="$(grep -m1 '^claude ' "${EVAL_CALL_LOG}" | sed 's/^claude //')"
assert_eq "${FIRST_ARGV}" \
    '-p --restricted --tools Bash,Read --allowedTools Bash(./kubectl-nv:*) --permission-mode dontAsk --strict-mcp-config --mcp-config {"mcpServers":{}}' \
    "the whole default argv is what it is documented to be"

assert_jq '.agent | test("--restricted")' 'true' "the results file records the agent that ran"

# ---------------------------------------------------------------------------
echo "# the harness waits for the fault to become observable"
# ---------------------------------------------------------------------------

reset_fixtures
run_subject FAKE_OBSERVE_DELAY=2

assert_rc 0 "a fault that takes a few polls to appear still passes"
assert_jq '[.cases[].observedPolls] | map(select(. >= 3)) | length' '5' \
    "each case records that it polled until the finding appeared"

FIRST_AGENT="$(grep -n '^claude ' "${EVAL_CALL_LOG}" | head -1 | cut -d: -f1)"
THIRD_HEALTH="$(grep -n '^kubectl-nv .*health' "${EVAL_CALL_LOG}" | head -3 | tail -1 | cut -d: -f1)"
if [ -n "${FIRST_AGENT}" ] && [ -n "${THIRD_HEALTH}" ] && [ "${FIRST_AGENT}" -gt "${THIRD_HEALTH}" ]; then
    ok "the agent starts only after the finding is observable"
else
    nope "the agent started at line ${FIRST_AGENT:-none}, before the finding appeared at ${THIRD_HEALTH:-none}"
fi

# ---------------------------------------------------------------------------
echo "# scoring reads the ANSWER line and nothing else"
# ---------------------------------------------------------------------------

# A shotgun answer that names everything is not a diagnosis.
reset_fixtures
override_answer NV-NODE-002 \
    "It might be NV-NODE-002 or NV-NODE-003 on kubectl-nv-faults-worker or worker2, component node or gpu-operator."
run_subject
assert_rc 1 "an answer with no ANSWER line fails its case"
assert_jq '.cases[0].pass' 'false' "a shotgun answer that never commits does not pass"
assert_jq '.cases[0].named.code' 'false' "the code is not credited from prose around the answer"
assert_jq '.summary.passed' '4' "the other four cases still pass"

# Thinking out loud in the right shape is not committing to an answer. The
# three fields appear in the prose, correctly, and there is no ANSWER line:
# a scorer that read the whole reply would score this a pass.
reset_fixtures
override_answer NV-NODE-003 \
    "Working through it: node=${WORKER} looks wrong, component=node is the part at fault, code=NV-NODE-003 is the likely code. I have not confirmed any of this."
run_subject
assert_rc 1 "prose in the right shape with no ANSWER line fails its case"
assert_jq '.cases[1].pass' 'false' "an answer that never commits to an ANSWER line does not pass"
assert_jq '.cases[1].named.node' 'false' "the node is not credited from prose"
assert_jq '.cases[1].named.component' 'false' "the component is not credited from prose"
assert_jq '.cases[1].named.code' 'false' "the code is not credited from prose"

# The wrong node.
reset_fixtures
override_answer NV-NODE-002 "ANSWER node=kubectl-nv-faults-worker2 component=node code=NV-NODE-002"
run_subject
assert_rc 1 "a run with one failed case exits 1"
assert_jq '.cases[0].pass' 'false' "an answer naming the wrong node does not pass"
assert_jq '.cases[0].named.node' 'false' "the wrong node is recorded as the part that was missing"
assert_jq '.cases[0].named.code' 'true' "the code it did name is still recorded as named"

# The node match is exact, not case folded: a node name is a DNS label.
reset_fixtures
override_answer NV-NODE-002 "ANSWER node=KUBECTL-NV-FAULTS-WORKER component=node code=NV-NODE-002"
run_subject
assert_jq '.cases[0].named.node' 'false' "a node named in the wrong case is not the node"

# A longer code that starts with the right one is a different code.
reset_fixtures
override_answer NV-CTK-001 \
    "ANSWER node=${WORKER} component=nvidia-container-toolkit-daemonset code=NV-CTK-0010"
run_subject
assert_rc 1 "a run with a wrong code exits 1"
assert_jq '.cases[2].named.code' 'false' "NV-CTK-0010 is not credited as NV-CTK-001"

# A lower case code is the same code.
reset_fixtures
override_answer NV-DRV-001 "ANSWER node=${WORKER} component=nvidia-driver-daemonset code=nv-drv-001"
run_subject
assert_rc 0 "a code written in lower case is still the code"
assert_jq '.cases[4].named.code' 'true' "the code match ignores case"

# The component has to be the field verbatim.
reset_fixtures
override_answer NV-DCGM-001 "ANSWER node=${WORKER} component=dcgm code=NV-DCGM-001"
run_subject
assert_rc 1 "a run with a wrong component exits 1"
assert_jq '.cases[3].named.component' 'false' "an abbreviated component is not the component"
assert_jq '.cases[3].named.node' 'true' "the node it did name is still recorded as named"

# A cluster scoped finding expects a dash, and naming a node there is wrong.
# None of the five injected codes is cluster scoped since NV-CTK-001 replaced
# NV-OP-001, so the fixture makes one of them so: the harness has to read the
# expected node off the report rather than assume the node it injected on.
reset_fixtures
cluster_scope NV-CTK-001
run_subject
assert_rc 0 "a cluster scoped finding is answered with a dash and passes"
assert_jq '.cases[2].expectedNode' '-' "the harness expects no node for a cluster scoped finding"
assert_jq '.cases[2].named.node' 'true' "the agent named the dash"

reset_fixtures
override_answer NV-CTK-001 \
    "ANSWER node=${WORKER} component=nvidia-container-toolkit-daemonset code=NV-CTK-001"
cluster_scope NV-CTK-001
run_subject
assert_rc 1 "naming a node for a cluster scoped finding fails the case"
assert_jq '.cases[2].named.node' 'false' "the cluster scoped case wants a dash, not a node"

# ---------------------------------------------------------------------------
echo "# recovery happens whatever went wrong"
# ---------------------------------------------------------------------------

reset_fixtures
printf '7\n' > "${OVERRIDE_DIR}/.rc"
run_subject
rm -f "${OVERRIDE_DIR}/.rc"

assert_rc 1 "a run where the agent fails exits 1"
RECOVERS="$(grep '^fault .* recover ' "${EVAL_CALL_LOG}" | awk '{print $2}' | tr '\n' ' ' | sed 's/ $//')"
assert_eq "${RECOVERS}" "${EXPECTED_ORDER}" "an agent that fails still leaves every fault recovered"
LEFTOVER="$(find "${STATE_DIR}" -name 'NV-*' 2> /dev/null | wc -l | tr -d ' ')"
assert_eq "${LEFTOVER}" "0" "no fault is left injected after an agent failure"
assert_jq '.cases[0].error' 'the agent command exited 7' "the agent failure is recorded on the case"

# A fault that cannot be injected is a failed case, and the rest still run.
reset_fixtures
run_subject FAKE_INJECT_FAIL_CODE=NV-CTK-001

assert_rc 1 "a run where one injection fails exits 1"
assert_jq '.cases[2].pass' 'false' "the case whose injection failed does not pass"
assert_jq '.summary.passed' '4' "the other four cases still run and pass"
assert_logged "fault NV-CTK-001 recover" "a failed injection is still recovered"
assert_logged "fault NV-DCGM-001 inject" "the run continues to the next fault"

# ---------------------------------------------------------------------------
echo "# a lane that cannot be put back stops the run"
# ---------------------------------------------------------------------------
#
# Injecting the next fault onto a lane still carrying the last one measures
# nothing, and leaves a broken cluster behind for whoever looks next.

reset_fixtures
run_subject FAKE_RECOVER_FAIL_CODE=NV-NODE-003

assert_rc 1 "a failed recover ends the run with exit 1"
assert_jq '.cases[1].pass' 'false' "the case whose recover failed does not pass"
assert_jq '.cases[1].recoverError | length > 0' 'true' "the recovery failure is recorded on the case"
assert_jq '.summary.cases' '2' "the run stops at the fault it could not recover"
assert_not_logged "fault NV-CTK-001 inject" "nothing is injected after a failed recover"
if [ -n "${RESULT_FILE}" ] && [ -f "${RESULT_FILE}" ]; then
    ok "a run stopped by a failed recover still writes its results"
else
    nope "a run stopped by a failed recover wrote no results"
fi

# A recover that undid the fault and still exited non-zero. The finding
# clears, so the wait below it says nothing: the only evidence that the lane
# may not be trustworthy is the script's own exit code.
reset_fixtures
run_subject FAKE_RECOVER_FAIL_CLEAN_CODE=NV-NODE-002

assert_rc 1 "a recover that exits non-zero ends the run even though the finding cleared"
assert_jq '.cases[0].recoverError | length > 0' 'true' \
    "the failed recover is recorded even though nothing was left behind"
assert_jq '.summary.cases' '1' "the run stops at the recover it cannot vouch for"
assert_not_logged "fault NV-NODE-003 inject" "nothing is injected after a recover that reported failure"

# A recover that reports success but leaves the finding in place is the same
# problem wearing a zero exit code.
reset_fixtures
run_subject FAKE_RECOVER_NOOP_CODE=NV-NODE-002 KUBECTL_NV_EVAL_RECOVER_TIMEOUT=1

assert_rc 1 "a finding that outlives the recover timeout ends the run"
assert_jq '.cases[0].recoverError | length > 0' 'true' "the finding that would not clear is recorded"
assert_jq '.summary.cases' '1' "the run stops rather than injecting onto a dirty lane"
assert_not_logged "fault NV-NODE-003 inject" "nothing is injected after a recover that did not take"

# ---------------------------------------------------------------------------
echo "# the agent is bounded by the wall clock"
# ---------------------------------------------------------------------------

reset_fixtures
touch "${OVERRIDE_DIR}/.hang"
run_subject KUBECTL_NV_EVAL_AGENT_TIMEOUT=2
rm -f "${OVERRIDE_DIR}/.hang"

assert_rc 1 "an agent that never returns fails its case"
assert_jq '[.cases[].error] | map(select(test("timed out"))) | length > 0' 'true' \
    "the timeout is recorded on the case"
LEFTOVER="$(find "${STATE_DIR}" -name 'NV-*' 2> /dev/null | wc -l | tr -d ' ')"
assert_eq "${LEFTOVER}" "0" "a timed out agent still leaves the lane recovered"

# ---------------------------------------------------------------------------
echo "# an interrupted run recovers and leaves no kubeconfig behind"
# ---------------------------------------------------------------------------

reset_fixtures
touch "${OVERRIDE_DIR}/.hang"
env -u CI -u KUBECTL_NV_EVAL_AGENT -i \
    PATH="${SHIM_DIR}:${TOOL_DIR}" HOME="${FAKE_HOME}" \
    EVAL_CALL_LOG="${EVAL_CALL_LOG}" EVAL_STATE_DIR="${EVAL_STATE_DIR}" \
    TMPDIR="${TMPDIR:-/tmp}" \
    KUBECTL_NV_EVAL_BIN="${BIN}" \
    KUBECTL_NV_EVAL_SKILL="${SKILL}" \
    KUBECTL_NV_EVAL_FAULTS_DIR="${FAULTS_DIR}" \
    KUBECTL_NV_EVAL_KUBECONFIG="${LANE_KUBECONFIG}" \
    KUBECTL_NV_EVAL_RESULTS_DIR="${RESULTS_DIR}" \
    KUBECTL_NV_EVAL_POLL_INTERVAL=0 \
    KUBECTL_NV_EVAL_OBSERVE_TIMEOUT=30 \
    KUBECTL_NV_EVAL_AGENT_TIMEOUT=120 \
    "${RUN}" > "${WORK}/out" 2> "${WORK}/err" &
SUBJECT_PID=$!
for _ in $(seq 1 200); do
    if grep -q '^claude ' "${EVAL_CALL_LOG}" 2> /dev/null; then break; fi
    sleep 0.1
done
kill -TERM "${SUBJECT_PID}" 2> /dev/null
wait "${SUBJECT_PID}"
RC=$?
OUT="$(cat "${WORK}/out")"
ERR="$(cat "${WORK}/err")"
rm -f "${OVERRIDE_DIR}/.hang"

assert_rc 130 "an interrupted run exits 130"
assert_eq "$(grep -c '^fault .* recover ' "${EVAL_CALL_LOG}")" "1" \
    "an interrupted run recovers the one fault it had injected, once"
LEFTOVER="$(find "${STATE_DIR}" -name 'NV-*' 2> /dev/null | wc -l | tr -d ' ')"
assert_eq "${LEFTOVER}" "0" "an interrupted run leaves no fault injected"

# The directories this run actually used, read out of its own call log. A
# glob over TMPDIR would also count the directories another process is using
# right now, which makes the check fail for reasons that have nothing to do
# with the subject, and would miss a leak at a different depth.
STRAY=0
STRAY_CHECKED=0
while IFS= read -r d; do
    [ -n "${d}" ] || continue
    STRAY_CHECKED=$((STRAY_CHECKED + 1))
    [ -e "${d}" ] && STRAY=$((STRAY + 1))
    [ -e "${d}/kubeconfig" ] && STRAY=$((STRAY + 1))
done < <(grep '^claude-cwd ' "${EVAL_CALL_LOG}" | awk '{print $2}' | sort -u)
if [ "${STRAY_CHECKED}" -lt 1 ]; then
    nope "the interrupted run recorded no case directory, so the leak check proves nothing"
else
    assert_eq "${STRAY}" "0" "an interrupted run leaves no copy of the lane kubeconfig behind"
fi

# ---------------------------------------------------------------------------
echo "# the harness refuses to run where it must not"
# ---------------------------------------------------------------------------

reset_fixtures
run_subject CI=true

assert_rc 2 "CI set is a refusal"
assert_says "CI" "the refusal says what it refused"
assert_not_logged "inject" "nothing is injected when the run is refused for CI"
if [ -z "${RESULT_FILE}" ]; then
    ok "a refused run writes no results file"
else
    nope "a refused run wrote ${RESULT_FILE}"
fi

reset_fixtures
run_subject CI=
assert_rc 0 "an empty CI is not set, so the run proceeds"

reset_fixtures
run_subject FAKE_CONTEXT=docker-desktop
assert_rc 2 "a context that is not a kubectl-nv lane is a refusal"
assert_says "docker-desktop" "the refusal names the context it refused"
assert_not_logged "inject" "nothing is injected against a cluster that is not a lane"

# The context has to be the lane's own, not merely something under the
# prefix: kind-kubectl-nv-faults-evil.example.com is somebody else's cluster.
reset_fixtures
run_subject FAKE_CONTEXT=kind-kubectl-nv-faults-evil.example.com
assert_rc 2 "a context that only starts with the lane name is refused"
assert_not_logged "inject" "nothing is injected against a lookalike context"

reset_fixtures
run_subject FAKE_CONTEXT=prod-kind-kubectl-nv-faults
assert_rc 2 "a context that merely contains the lane prefix is refused"

# A kubeconfig that can reach a cluster that is not a local kind lane is not
# a lane kubeconfig, whatever its context is called.
reset_fixtures
run_subject FAKE_SERVER=https://k8s.prod.example.com
assert_rc 2 "a kubeconfig pointing off the loopback is refused"
assert_says "server" "the refusal says the server is wrong"
assert_not_logged "inject" "nothing is injected through a kubeconfig that is not loopback"

# More than one context means the file can reach more than the lane, and that
# file is handed to an agent.
reset_fixtures
run_subject FAKE_EXTRA_CONTEXT=prod
assert_rc 2 "a kubeconfig carrying a second context is refused"
assert_not_logged "inject" "nothing is injected through a multi context kubeconfig"

# ---------------------------------------------------------------------------
echo "# what cannot be run is exit 3, and nothing is touched"
# ---------------------------------------------------------------------------

reset_fixtures
run_subject KUBECTL_NV_EVAL_BIN="${WORK}/no-such-binary"
assert_rc 3 "a missing binary is exit 3"
assert_not_logged "inject" "nothing is injected when the binary is missing"

reset_fixtures
run_subject KUBECTL_NV_EVAL_SKILL="${WORK}/no-such-skill.md"
assert_rc 3 "a missing skill file is exit 3"
assert_says "skill" "the refusal names the skill"
assert_not_logged "inject" "nothing is injected when the skill is missing"

reset_fixtures
run_subject KUBECTL_NV_EVAL_KUBECONFIG="${WORK}/no-such-kubeconfig"
assert_rc 3 "a missing lane kubeconfig is exit 3"
assert_not_logged "inject" "nothing is injected when the kubeconfig is missing"

reset_fixtures
EMPTY_FAULTS="${WORK}/no-faults"
mkdir -p "${EMPTY_FAULTS}"
run_subject KUBECTL_NV_EVAL_FAULTS_DIR="${EMPTY_FAULTS}"
assert_rc 3 "a fault script that is not there is exit 3"
assert_says "NV-NODE-002" "the refusal names the first script it could not find"
assert_not_logged "inject" "nothing is injected when a fault script is missing"

# ---------------------------------------------------------------------------
echo "# the default kubeconfig is never touched"
# ---------------------------------------------------------------------------

reset_fixtures
run_subject
assert_rc 0 "the full run used for the kubeconfig assertions below passed"

SENTINEL_AFTER="$(shasum -a 256 < "${FAKE_HOME}/.kube/config" | awk '{print $1}')"
assert_eq "${SENTINEL_AFTER}" "${SENTINEL_BEFORE}" "the default kubeconfig is byte for byte what it was"

KUBECTL_CALLS="$(grep -c '^kubectl ' "${EVAL_CALL_LOG}")"
if [ "${KUBECTL_CALLS}" -lt 3 ]; then
    nope "only ${KUBECTL_CALLS} kubectl calls were recorded, so the check below proves nothing"
else
    ok "${KUBECTL_CALLS} kubectl calls were recorded to check"
fi
KUBECTL_WITHOUT_KUBECONFIG="$(grep '^kubectl ' "${EVAL_CALL_LOG}" | grep -cv -- '--kubeconfig' || true)"
assert_eq "${KUBECTL_WITHOUT_KUBECONFIG}" "0" "every kubectl call names the lane kubeconfig"

NV_CALLS="$(grep -c '^kubectl-nv ' "${EVAL_CALL_LOG}")"
if [ "${NV_CALLS}" -lt 5 ]; then
    nope "only ${NV_CALLS} binary calls were recorded, so the check below proves nothing"
else
    ok "${NV_CALLS} binary calls were recorded to check"
fi
NV_WITHOUT_KUBECONFIG="$(grep '^kubectl-nv ' "${EVAL_CALL_LOG}" | grep -cv -- '--kubeconfig' || true)"
assert_eq "${NV_WITHOUT_KUBECONFIG}" "0" "every binary call names the lane kubeconfig"

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

# A run with one failure renders one exact row, naming the code that failed.
reset_fixtures
override_answer NV-DCGM-001 "ANSWER node=${WORKER} component=dcgm code=NV-DCGM-001"
run_subject
assert_rc 1 "the run used for the renderer row had one failure"
RENDER_RESULTS="${WORK}/render-results"
rm -rf "${RENDER_RESULTS}"
mkdir -p "${RENDER_RESULTS}"
# The date is pinned rather than taken from the run, so the expected row below
# is the same on every day of the year. The renderer reads startedAt, not the
# file name, so both are set.
jq '.startedAt = "2026-10-12T09:00:00Z"' < "${RESULT_FILE}" > "${RENDER_RESULTS}/2026-10-12.json"
write_readme_fixture
PATH="${SHIM_DIR}:${TOOL_DIR}" "${RENDER}" --results "${RENDER_RESULTS}" --readme "${README_FIXTURE}" \
    > "${WORK}/out" 2> "${WORK}/err"
RC=$?
assert_rc 0 "rendering a recorded run succeeds"

ROW="$(grep '^| 2026-10-12 ' "${README_FIXTURE}")"
EXPECTED_ROW='| 2026-10-12 | `claude -p --restricted --tools Bash,Read --allowedTools '"'"'Bash(./kubectl-nv:*)'"'"' --permission-mode dontAsk --strict-mcp-config --mcp-config '"'"'{"mcpServers":{}}'"'"'` | 5 | 4 | NV-DCGM-001 |'
assert_eq "${ROW}" "${EXPECTED_ROW}" "the rendered row is exactly the run that was recorded"

if grep -q 'No evaluation run has been recorded' "${README_FIXTURE}"; then
    nope "the empty note survived a recorded run"
else
    ok "the empty note is gone once a run is recorded"
fi

BEFORE_SECOND="$(shasum -a 256 < "${README_FIXTURE}" | awk '{print $1}')"
PATH="${SHIM_DIR}:${TOOL_DIR}" "${RENDER}" --results "${RENDER_RESULTS}" --readme "${README_FIXTURE}" \
    > /dev/null 2>&1
AFTER_SECOND="$(shasum -a 256 < "${README_FIXTURE}" | awk '{print $1}')"
assert_eq "${AFTER_SECOND}" "${BEFORE_SECOND}" "rendering is idempotent"

# A pipe inside a cell has to be escaped or it splits the row in two.
PIPE_RESULTS="${WORK}/pipe-results"
rm -rf "${PIPE_RESULTS}"
mkdir -p "${PIPE_RESULTS}"
jq '.agent = "my-agent --print | tee log" | .startedAt = "2026-10-13T00:00:00Z"' \
    < "${RESULT_FILE}" > "${PIPE_RESULTS}/2026-10-13.json"
write_readme_fixture
PATH="${SHIM_DIR}:${TOOL_DIR}" "${RENDER}" --results "${PIPE_RESULTS}" --readme "${README_FIXTURE}" \
    > /dev/null 2>&1
PIPE_ROW="$(grep '^| 2026-10-13 ' "${README_FIXTURE}")"
if printf '%s' "${PIPE_ROW}" | grep -qF -- '\|'; then
    ok "a pipe in a cell is escaped"
else
    nope "a pipe in a cell was not escaped, so the row has the wrong columns" "row: ${PIPE_ROW}"
fi
PIPE_COLUMNS="$(printf '%s' "${PIPE_ROW}" | sed 's/\\|//g' | tr -cd '|' | wc -c | tr -d ' ')"
assert_eq "${PIPE_COLUMNS}" "6" "the row still has five columns"

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
        if (cd "${dir}" && shellcheck -x "${RUN}" "${RENDER}" "${SHIM_TEMPLATE}" "${SCRIPT_DIR}/run_test.sh"); then
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
