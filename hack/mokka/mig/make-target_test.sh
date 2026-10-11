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
# Checks what the e2e-mig target does around the lane, without a lane.
#
# Modelled on test/e2e/mokka-target_test.sh, and for the same reason: the
# target has to get right exactly the things a passing run never shows. A lane
# that failed to come up still gets dumped before it is deleted; a lane that
# was already there is neither dumped nor deleted; a cancelled run tears down
# exactly once; a build that failed never reaches the cluster at all; and
# whatever went wrong is still the exit code afterwards.
#
# One thing this target has that e2e-mokka does not: it selects its tests by
# name, with -run '^TestMIGLane$'. A filter that matches nothing makes go test
# print "ok" and exit 0, so the target would report success having run no
# assertions at all. The precheck against that is a case below.
#
# The Makefile is the subject and is COPIED into the scaffold rather than
# rewritten: a check against an edited copy proves nothing about what ships.
#
# Usage: hack/mokka/mig/make-target_test.sh
#
# Exit codes:
#   0  every case passed
#   1  a case failed, named on stdout
#   2  the check could not run

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

if [ ! -f "${REPO_ROOT}/Makefile" ]; then
    echo "make-target_test: no Makefile under ${REPO_ROOT}" >&2
    exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mig-target.XXXXXX")" || exit 2
trap '[ -n "${MIG_TARGET_KEEP:-}" ] || rm -rf "${WORK}"' EXIT

failures=0

ok() { printf 'PASS %s\n' "$1"; }
nope() { printf 'FAIL %s\n' "$1"; failures=$((failures + 1)); }

# scaffold builds a throwaway tree holding the repository's real Makefile and
# stubs in place of everything the target shells out to.
scaffold() {
    local root="$1"
    rm -rf "${root}"
    mkdir -p "${root}/hack/mokka/mig" "${root}/stubbin" "${root}/test/e2e"
    cp "${REPO_ROOT}/Makefile" "${root}/Makefile"

    cat > "${root}/hack/mokka/mig/build-image.sh" <<'STUB'
#!/bin/bash
printf 'stub-build-image %s\n' "$*" >> "${STUB_LOG}"
exit "${STUB_BUILD_RC:-0}"
STUB

    # up.sh records, and can raise a signal at the recipe shell so that the
    # "cancelled while the lane is coming up" case is driven at the moment the
    # cluster may already exist and the target has not yet decided anything.
    cat > "${root}/hack/mokka/mig/up.sh" <<'STUB'
#!/bin/bash
printf 'stub-up %s\n' "$*" >> "${STUB_LOG}"
if [ -n "${STUB_UP_SIGNAL:-}" ]; then
    kill -"${STUB_UP_SIGNAL}" "${PPID}"
    sleep 30
fi
exit "${STUB_UP_RC:-0}"
STUB

    cat > "${root}/hack/mokka/mig/down.sh" <<'STUB'
#!/bin/bash
printf 'stub-down %s\n' "$*" >> "${STUB_LOG}"
exit "${STUB_DOWN_RC:-0}"
STUB

    # Stands in for the go toolchain. "build" always succeeds. "test" is either
    # the -list precheck or the suite itself, and the two must be told apart:
    # a stub that answered the same for both could not drive the case where
    # the filter matches nothing.
    cat > "${root}/stubbin/go" <<'STUB'
#!/bin/bash
if [ "${1:-}" != "test" ]; then
    printf 'stub-go %s\n' "${1:-}" >> "${STUB_LOG}"
    exit 0
fi
for arg in "$@"; do
    if [ "${arg}" = "-list" ]; then
        printf 'stub-go list\n' >> "${STUB_LOG}"
        if [ -n "${STUB_LIST_STDERR:-}" ]; then
            printf '%s\n' "${STUB_LIST_STDERR}" >&2
        fi
        printf '%s' "${STUB_LIST_OUT-TestMIGLane
ok  	github.com/NVIDIA/kubectl-nv/test/e2e	0.4s}"
        printf '\n'
        exit "${STUB_LIST_RC:-0}"
    fi
done
printf 'stub-go test\n' >> "${STUB_LOG}"
if [ -n "${STUB_TEST_SIGNAL:-}" ]; then
    kill -"${STUB_TEST_SIGNAL}" "${PPID}"
    sleep 30
fi
exit "${STUB_TEST_RC:-0}"
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

    chmod +x "${root}/hack/mokka/mig/build-image.sh" \
        "${root}/hack/mokka/mig/up.sh" "${root}/hack/mokka/mig/down.sh" \
        "${root}/stubbin/go" "${root}/stubbin/kubectl" "${root}/stubbin/kind"

    # The target points the suite at the lane kubeconfig. Nothing reads it
    # here, but the path has to exist for the -list precheck's environment to
    # be the same one the real run uses.
    mkdir -p "${root}/hack/mokka/bin"
    printf 'apiVersion: v1\n' > "${root}/hack/mokka/bin/kubeconfig-mig"
}

# run_target runs e2e-mig in a scaffolded tree and leaves its exit code in
# RUN_RC, the stub call log in RUN_LOG and the tree in RUN_ROOT.
run_target() {
    RUN_ROOT="${WORK}/case-$1"
    shift
    scaffold "${RUN_ROOT}"
    RUN_LOG="${RUN_ROOT}/calls.log"
    : > "${RUN_LOG}"

    env -u STUB_BUILD_RC -u STUB_UP_RC -u STUB_TEST_RC -u STUB_DOWN_RC \
        -u STUB_TEST_SIGNAL -u STUB_UP_SIGNAL -u STUB_LIST_OUT \
        -u STUB_LIST_RC -u STUB_LIST_STDERR \
        STUB_LOG="${RUN_LOG}" "$@" \
        PATH="${RUN_ROOT}/stubbin:${PATH}" \
        make -C "${RUN_ROOT}" --no-print-directory e2e-mig \
        > "${RUN_ROOT}/out.log" 2>&1
    RUN_RC=$?
}

# recipe_rc is the code the recipe itself exited with. make has an exit status
# of its own, 2 for any failed recipe, so asking $? what the target returned
# answers for make instead; the code make prints in "*** [e2e-mig] Error N"
# is the recipe's.
recipe_rc() {
    sed -n 's/.*\[e2e-mig\] Error \([0-9][0-9]*\).*/\1/p' "${RUN_ROOT}/out.log" | tail -1
}

exited() {
    local want="$1" name="$2" got
    if [ "${want}" -eq 0 ]; then
        if [ "${RUN_RC}" -eq 0 ]; then
            ok "${name}: exit 0"
        else
            nope "${name}: exit ${RUN_RC}, want 0"
            sed -n '1,25p' "${RUN_ROOT}/out.log"
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
        sed -n '1,25p' "${RUN_ROOT}/out.log"
    fi
}

calls() {
    grep -c "^$1" "${RUN_LOG}" 2>/dev/null || true
}

downs_ran() {
    local want="$1" name="$2" got
    got="$(calls stub-down)"
    if [ "${got}" -eq "${want}" ]; then
        ok "${name}: teardown ran ${want} time(s)"
    else
        nope "${name}: teardown ran ${got} time(s), want ${want}"
    fi
}

diagnostics_written() {
    [ -s "${RUN_ROOT}/bin/e2e-diagnostics-mig/pods.txt" ] &&
        [ -s "${RUN_ROOT}/bin/e2e-diagnostics-mig/events.txt" ] &&
        [ -s "${RUN_ROOT}/bin/e2e-diagnostics-mig/nodes.yaml" ] &&
        [ -s "${RUN_ROOT}/bin/e2e-diagnostics-mig/device-plugin-mock.log" ] &&
        [ -s "${RUN_ROOT}/bin/e2e-diagnostics-mig/kind-export.txt" ]
}

dumped() {
    local name="$1"
    if diagnostics_written; then
        ok "${name}: diagnostics written"
    else
        nope "${name}: no diagnostics under ${RUN_ROOT}/bin/e2e-diagnostics-mig"
        find "${RUN_ROOT}/bin" -maxdepth 2 2>&1 | sed -n '1,10p'
    fi
}

not_dumped() {
    local name="$1"
    if diagnostics_written; then
        nope "${name}: dumped a lane it should not have"
    else
        ok "${name}: nothing dumped"
    fi
}

# ---------------------------------------------------------------------------
# The image build fails
# ---------------------------------------------------------------------------
#
# Nothing after it can work, and the cluster is the expensive part: a target
# that creates a lane and then discovers it has no image to install wastes
# several minutes and leaves a teardown to explain.

run_target "build-6" STUB_BUILD_RC=6
exited 6 "build-image.sh exit 6"
if [ "$(calls stub-up)" -eq 0 ]; then
    ok "build-image.sh exit 6: the lane was never brought up"
else
    nope "build-image.sh exit 6: up.sh ran after the image build failed"
fi
downs_ran 0 "build-image.sh exit 6"
not_dumped "build-image.sh exit 6"

# ---------------------------------------------------------------------------
# The lane was already there
# ---------------------------------------------------------------------------
#
# up.sh says it touched nothing, and neither may this target: it is somebody
# else's lane. Deleting it would take a cluster out from under another run.

run_target "up-4" STUB_UP_RC=4
exited 4 "up.sh exit 4"
downs_ran 0 "up.sh exit 4"
not_dumped "up.sh exit 4"
if [ "$(calls 'stub-go test')" -eq 0 ]; then
    ok "up.sh exit 4: the suite was not run"
else
    nope "up.sh exit 4: the suite ran against somebody else's lane"
fi

# ---------------------------------------------------------------------------
# The lane failed to come up
# ---------------------------------------------------------------------------
#
# The reason is in the cluster, so it is dumped before the teardown removes it.
# up.sh's own code is still what the caller sees.

for up_rc in 1 5; do
    run_target "up-${up_rc}" STUB_UP_RC="${up_rc}"
    name="up.sh exit ${up_rc}"
    exited "${up_rc}" "${name}"
    dumped "${name}"
    downs_ran 1 "${name}"
    if [ "$(calls 'stub-go test')" -eq 0 ]; then
        ok "${name}: the suite was not run against a lane that is not up"
    else
        nope "${name}: the suite ran against a lane that is not up"
    fi
done

# ---------------------------------------------------------------------------
# The -run filter matches nothing
# ---------------------------------------------------------------------------
#
# go test -run with a filter that matches no test prints "ok" and exits 0. A
# target that trusted that would report a green MIG lane having asserted
# nothing at all, which is the one failure mode this whole lane exists to
# avoid. The precheck has to turn it into a failure, with the lane still
# dumped and torn down.

run_target "list-empty" STUB_LIST_OUT=""
exited 2 "the -run filter matches no test"
dumped "the -run filter matches no test"
downs_ran 1 "the -run filter matches no test"
if [ "$(calls 'stub-go test')" -eq 0 ]; then
    ok "the -run filter matches no test: the suite was not run"
else
    nope "the -run filter matches no test: the suite ran anyway"
fi

# A filter that matches a DIFFERENT test is the same failure wearing another
# hat: a rename leaves the name behind, not the test.
run_target "list-other" STUB_LIST_OUT="TestSomethingElse"
exited 2 "the -run filter matches only another test"

# Two matches is not one. A second top-level TestMIG... would run silently
# under this filter and nobody would know which assertions ran.
run_target "list-two" STUB_LIST_OUT="TestMIGLane
TestMIGLaneExtra"
exited 2 "the -run filter matches two tests"

# When the precheck fails, whatever go test said on stderr is the only clue to
# why. Swallowing it leaves a bare count and nothing to act on.
run_target "list-stderr" STUB_LIST_OUT="" STUB_LIST_RC=1 \
    STUB_LIST_STDERR="test/e2e/mig_test.go:41:2: undefined: migCommandGroup"
exited 2 "a -list that failed to build"
if grep -qF 'undefined: migCommandGroup' "${RUN_ROOT}/out.log"; then
    ok "a -list that failed to build: go test's stderr is reported"
else
    nope "a -list that failed to build: go test's stderr was swallowed"
    sed -n '1,25p' "${RUN_ROOT}/out.log"
fi

# ---------------------------------------------------------------------------
# A failing suite over a lane that came up fine
# ---------------------------------------------------------------------------

run_target "test-3" STUB_TEST_RC=3
exited 3 "suite exit 3"
dumped "suite exit 3"
downs_ran 1 "suite exit 3"

# ---------------------------------------------------------------------------
# The passing run
# ---------------------------------------------------------------------------

run_target "pass"
exited 0 "passing suite"
downs_ran 1 "passing suite"
not_dumped "passing suite"

# ---------------------------------------------------------------------------
# Cancelled
# ---------------------------------------------------------------------------
#
# Exactly one teardown. A trap on both the signal and EXIT runs it twice, and
# the second kind delete reports a cluster that is already gone, which reads
# as a teardown failure in a log nobody wants to decode.
#
# The dump has to happen inside the signal path too: a run cancelled by a CI
# job timeout is exactly when the state of the lane is worth having, and it is
# the case where a dump placed only on the "the suite failed" branch is
# skipped.

for signal in TERM INT; do
    case "${signal}" in
    TERM) want=143 ;;
    INT) want=130 ;;
    esac

    run_target "signal-suite-${signal}" STUB_TEST_SIGNAL="${signal}"
    name="SIG${signal} during the suite"
    downs_ran 1 "${name}"
    dumped "${name}"
    exited "${want}" "${name}"

    # Cancelled while the lane is still coming up. The cluster may already
    # exist by then: up.sh creates it early and spends most of its time
    # waiting. Leaving it behind is what fills a laptop with half-built kind
    # clusters.
    run_target "signal-up-${signal}" STUB_UP_SIGNAL="${signal}"
    name="SIG${signal} during up.sh"
    downs_ran 1 "${name}"
    exited "${want}" "${name}"
done

printf '\n'
if [ "${failures}" -eq 0 ]; then
    printf 'all cases passed\n'
    exit 0
fi
printf '%d case(s) failed\n' "${failures}"
exit 1
