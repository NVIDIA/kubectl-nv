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
# Checks what the e2e-mokka and e2e targets do around the lane, without a lane.
#
# The target has to get four things right that only show up on a bad day: a
# lane that failed to come up still gets dumped, a lane that was already there
# is neither dumped nor deleted, a cancelled run tears down exactly once, and
# whatever went wrong is still the exit code afterwards. None of that is
# observable from a passing run, so the scripts the target calls are replaced
# with stubs that record what they were asked to do and return what the case
# under test needs.
#
# Usage: test/e2e/mokka-target_test.sh
#
# Exit codes:
#   0  every case passed
#   1  a case failed, named on stdout
#   2  the check could not run

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

if [ ! -f "${REPO_ROOT}/Makefile" ]; then
    echo "mokka-target_test: no Makefile under ${REPO_ROOT}" >&2
    exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mokka-target.XXXXXX")" || exit 2
trap '[ -n "${MOKKA_TARGET_KEEP:-}" ] || rm -rf "${WORK}"' EXIT

failures=0

ok() { printf 'PASS %s\n' "$1"; }
nope() { printf 'FAIL %s\n' "$1"; failures=$((failures + 1)); }

# scaffold builds a throwaway tree holding the repository's real Makefile and
# stubs in place of everything it shells out to. The Makefile is the subject,
# so it is copied rather than rewritten: a check against an edited copy proves
# nothing about what ships.
scaffold() {
    local root="$1"
    rm -rf "${root}"
    mkdir -p "${root}/hack/mokka" "${root}/stubbin"
    cp "${REPO_ROOT}/Makefile" "${root}/Makefile"

    cat > "${root}/hack/mokka/up.sh" <<'STUB'
#!/bin/bash
printf 'stub-up %s\n' "$*" >> "${STUB_LOG}"
exit "${STUB_UP_RC:-0}"
STUB

    cat > "${root}/hack/mokka/down.sh" <<'STUB'
#!/bin/bash
printf 'stub-down %s\n' "$*" >> "${STUB_LOG}"
exit "${STUB_DOWN_RC:-0}"
STUB

    # Stands in for the go toolchain. "build" is whatever the build target asks
    # for and always succeeds; "test" is the suite, and returns what the case
    # needs, or kills the recipe shell when the case is about a signal.
    cat > "${root}/stubbin/go" <<'STUB'
#!/bin/bash
printf 'stub-go %s\n' "$1" >> "${STUB_LOG}"
if [ "$1" = "test" ]; then
    if [ -n "${STUB_TEST_SIGNAL:-}" ]; then
        kill -"${STUB_TEST_SIGNAL}" "${PPID}"
        sleep 30
    fi
    exit "${STUB_TEST_RC:-0}"
fi
exit 0
STUB

    # The diagnostics reach for these. They must never decide the exit code.
    cat > "${root}/stubbin/kubectl" <<'STUB'
#!/bin/bash
printf 'stub-kubectl %s\n' "$*" >> "${STUB_LOG}"
echo "stub kubectl output"
STUB

    cat > "${root}/stubbin/kind" <<'STUB'
#!/bin/bash
printf 'stub-kind %s\n' "$*" >> "${STUB_LOG}"
echo "stub kind output"
STUB

    chmod +x "${root}/hack/mokka/up.sh" "${root}/hack/mokka/down.sh" \
        "${root}/stubbin/go" "${root}/stubbin/kubectl" "${root}/stubbin/kind"
}

# run_target runs e2e-mokka in a scaffolded tree and leaves its exit code in
# RUN_RC, the stub call log in RUN_LOG and the tree in RUN_ROOT.
run_target() {
    RUN_ROOT="${WORK}/case-$1"
    shift
    scaffold "${RUN_ROOT}"
    RUN_LOG="${RUN_ROOT}/calls.log"
    : > "${RUN_LOG}"

    env -u STUB_UP_RC -u STUB_TEST_RC -u STUB_DOWN_RC -u STUB_TEST_SIGNAL \
        STUB_LOG="${RUN_LOG}" "$@" \
        PATH="${RUN_ROOT}/stubbin:${PATH}" \
        make -C "${RUN_ROOT}" --no-print-directory e2e-mokka \
        > "${RUN_ROOT}/out.log" 2>&1
    RUN_RC=$?
}

# recipe_rc is the code the recipe itself exited with. make has an exit status
# of its own, 2 for any failed recipe, so asking $? what the target returned
# answers for make instead; the code make prints in "*** [e2e-mokka] Error N"
# is the recipe's.
recipe_rc() {
    sed -n 's/.*\[e2e-mokka\] Error \([0-9][0-9]*\).*/\1/p' "${RUN_ROOT}/out.log" | tail -1
}

# exited asserts the target returned the given code: through make's own status
# when that code is 0, and through the reported recipe code otherwise.
exited() {
    local want="$1" name="$2" got
    if [ "${want}" -eq 0 ]; then
        if [ "${RUN_RC}" -eq 0 ]; then
            ok "${name}: exit 0"
        else
            nope "${name}: exit ${RUN_RC}, want 0"
        fi
        return
    fi
    got="$(recipe_rc)"
    if [ "${RUN_RC}" -eq 0 ]; then
        nope "${name}: make reported success for a recipe that failed"
        return
    fi
    if [ "${got}" = "${want}" ]; then
        ok "${name}: recipe exit code ${got} preserved"
    else
        nope "${name}: recipe exit code ${got:-<none>}, want ${want}"
    fi
}

# calls counts how many times the stub log recorded a call starting with the
# given text.
calls() {
    grep -c "^$1" "${RUN_LOG}" 2>/dev/null || true
}

diagnostics_written() {
    [ -s "${RUN_ROOT}/bin/e2e-diagnostics-mokka/pods.txt" ] &&
        [ -s "${RUN_ROOT}/bin/e2e-diagnostics-mokka/events.txt" ] &&
        [ -s "${RUN_ROOT}/bin/e2e-diagnostics-mokka/kind-export.txt" ]
}

# A lane that failed to come up: the reason is in the cluster, so it is dumped
# before the teardown removes it, and up.sh's code is still what the caller
# sees.
for up_rc in 1 5; do
    run_target "up-${up_rc}" STUB_UP_RC="${up_rc}"
    name="up.sh exit ${up_rc}"
    exited "${up_rc}" "${name}"
    if diagnostics_written; then
        ok "${name}: diagnostics written"
    else
        nope "${name}: no diagnostics under ${RUN_ROOT}/bin"
    fi
    if [ "$(calls stub-down)" -eq 1 ]; then
        ok "${name}: teardown ran once"
    else
        nope "${name}: teardown ran $(calls stub-down) times, want 1"
    fi
    if [ "$(calls 'stub-go test')" -eq 0 ]; then
        ok "${name}: the suite was not run"
    else
        nope "${name}: the suite ran against a lane that is not up"
    fi
done

# A cluster of that name was already there. up.sh says it touched nothing, and
# neither may this target: it is somebody else's lane.
run_target "up-4" STUB_UP_RC=4
exited 4 "up.sh exit 4"
if [ "$(calls stub-down)" -eq 0 ]; then
    ok "up.sh exit 4: no teardown of a lane we did not create"
else
    nope "up.sh exit 4: teardown ran against a pre-existing lane"
fi
if diagnostics_written; then
    nope "up.sh exit 4: dumped a lane we were told was not touched"
else
    ok "up.sh exit 4: nothing dumped"
fi
if [ "$(calls 'stub-go test')" -eq 0 ]; then
    ok "up.sh exit 4: the suite was not run"
else
    nope "up.sh exit 4: the suite ran against somebody else's lane"
fi

# A failing suite over a lane that came up fine.
run_target "test-3" STUB_TEST_RC=3
exited 3 "suite exit 3"
if diagnostics_written; then
    ok "suite exit 3: diagnostics written"
else
    nope "suite exit 3: no diagnostics"
fi
if [ "$(calls stub-down)" -eq 1 ]; then
    ok "suite exit 3: teardown ran once"
else
    nope "suite exit 3: teardown ran $(calls stub-down) times, want 1"
fi

# The passing run: torn down, nothing dumped, exit 0.
run_target "pass"
exited 0 "passing suite"
if [ "$(calls stub-down)" -eq 1 ]; then
    ok "passing suite: teardown ran once"
else
    nope "passing suite: teardown ran $(calls stub-down) times, want 1"
fi
if diagnostics_written; then
    nope "passing suite: dumped diagnostics for a run that passed"
else
    ok "passing suite: nothing dumped"
fi

# A cancelled run. The signal has to leave exactly one teardown behind: a trap
# on both the signal and EXIT runs the teardown twice, and the second kind
# delete reports a cluster that is already gone.
for signal in TERM INT; do
    case "${signal}" in
    TERM) want=143 ;;
    INT) want=130 ;;
    esac
    run_target "signal-${signal}" STUB_TEST_SIGNAL="${signal}"
    name="SIG${signal} during the suite"
    if [ "$(calls stub-down)" -eq 1 ]; then
        ok "${name}: teardown ran exactly once"
    else
        nope "${name}: teardown ran $(calls stub-down) times, want 1"
    fi
    exited "${want}" "${name}"
done

# The e2e target refuses a kubeconfig pointing somewhere that is not a lane of
# ours, because the suite it runs gathers from whatever it is pointed at.
guard_case() {
    local name="$1" context="$2" rc_want="$3"
    shift 3
    local root="${WORK}/guard-${name}"
    scaffold "${root}"
    local kc="${root}/kubeconfig"
    cat > "${kc}" <<KUBECONFIG
apiVersion: v1
kind: Config
current-context: ${context}
clusters: []
contexts: []
users: []
KUBECONFIG

    # A kubectl that answers the context question from the file it is given,
    # so the guard is exercised rather than mocked out.
    cat > "${root}/stubbin/kubectl" <<'STUB'
#!/bin/bash
for arg in "$@"; do
    case "${prev:-}" in
    --kubeconfig) kc="${arg}" ;;
    esac
    prev="${arg}"
done
sed -n 's/^current-context: //p' "${kc}"
STUB
    chmod +x "${root}/stubbin/kubectl"

    env -u E2E_ANY_CLUSTER STUB_LOG="${root}/calls.log" "$@" \
        KUBECONFIG="${kc}" KUBECTL_NV_BIN=/nonexistent E2E_ALLOW_SKIP=1 \
        PATH="${root}/stubbin:${PATH}" \
        make -C "${root}" --no-print-directory e2e \
        > "${root}/out.log" 2>&1
    local rc=$?
    if [ "${rc}" -eq "${rc_want}" ]; then
        ok "make e2e, context ${context}${1:+ with $1}: exit ${rc}"
    else
        nope "make e2e, context ${context}${1:+ with $1}: exit ${rc}, want ${rc_want}"
        cat "${root}/out.log"
    fi
}

guard_case "foreign" "docker-desktop" 2
guard_case "lane" "kind-kubectl-nv-mokka" 0
guard_case "override" "docker-desktop" 0 E2E_ANY_CLUSTER=1

printf '\n'
if [ "${failures}" -eq 0 ]; then
    printf 'all cases passed\n'
    exit 0
fi
printf '%d case(s) failed\n' "${failures}"
exit 1
