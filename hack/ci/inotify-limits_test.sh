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
# Tests for hack/ci/inotify-limits.sh.
#
# Exit codes under test: 0 both limits at or above their targets, 1 a limit is
# still below its target after the write, 2 sysctl could not be run, read or
# written.
#
# The script is run against a fake sysctl first on PATH. The fake keeps each
# key's value in a file under a state directory and appends every write to a
# log, so a case can say both what the kernel would hold afterwards and which
# writes were attempted. Its failure modes are switched by environment
# variables, one per case, so that every guard in the script has a fixture
# that flips it.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="${SCRIPT_DIR}/inotify-limits.sh"

INSTANCES=fs.inotify.max_user_instances
WATCHES=fs.inotify.max_user_watches

WORK="$(mktemp -d "${TMPDIR:-/tmp}/inotify-limits-test.XXXXXX")" || {
    echo "cannot create a temporary directory"
    exit 2
}
trap 'rm -rf "${WORK}"' EXIT

# The fake. sysctl -n <key> prints the value; sysctl -w <key>=<value> stores it.
mkdir -p "${WORK}/bin" "${WORK}/empty"
cat > "${WORK}/bin/sysctl" <<'FAKE'
#!/bin/bash
set -u
state="${FAKE_SYSCTL_STATE:?}"
case "${1:-}" in
    -n)
        key="$2"
        if [ "${FAKE_SYSCTL_FAIL_READ:-}" = "${key}" ]; then
            echo "sysctl: cannot stat /proc/sys/${key//.//}: No such file or directory" >&2
            exit 255
        fi
        cat "${state}/${key}"
        ;;
    -w)
        key="${2%%=*}"
        value="${2#*=}"
        echo "${key}=${value}" >> "${state}/writes.log"
        if [ -n "${FAKE_SYSCTL_FAIL_WRITE:-}" ]; then
            echo "sysctl: permission denied on key \"${key}\"" >&2
            exit 255
        fi
        if [ -z "${FAKE_SYSCTL_IGNORE_WRITES:-}" ]; then
            printf '%s\n' "${value}" > "${state}/${key}"
        fi
        echo "${key} = ${value}"
        ;;
    *)
        echo "fake sysctl: unexpected arguments: $*" >&2
        exit 64
        ;;
esac
FAKE
chmod +x "${WORK}/bin/sysctl"

failures=0
cases=0

# new_state <instances> <watches>: a state directory holding the two values.
new_state() {
    local dir
    dir="$(mktemp -d "${WORK}/state.XXXXXX")" || exit 2
    printf '%s\n' "$1" > "${dir}/${INSTANCES}"
    printf '%s\n' "$2" > "${dir}/${WATCHES}"
    : > "${dir}/writes.log"
    echo "${dir}"
}

fail() {
    echo "FAIL ${CASE}: $*"
    sed 's/^/    /' "${LOG}"
    failures=$((failures + 1))
}

# run_subject <state-dir> <path> [VAR=value ...]: runs the script with the
# given PATH and fake switches, output in ${LOG}, exit code in ${RC}.
run_subject() {
    local state="$1" path="$2"
    shift 2
    LOG="${WORK}/$(echo "${CASE}" | tr ' /' '__').log"
    env -i HOME="${WORK}" PATH="${path}" FAKE_SYSCTL_STATE="${state}" "$@" \
        "${BASH}" "${SUBJECT}" > "${LOG}" 2>&1
    RC=$?
}

expect_rc() {
    if [ "${RC}" -ne "$1" ]; then
        fail "want rc=$1, got rc=${RC}"
        return 1
    fi
}

expect_value() {
    local state="$1" key="$2" want="$3" got
    got="$(cat "${state}/${key}")"
    if [ "${got}" != "${want}" ]; then
        fail "want ${key}=${want} afterwards, got ${got}"
        return 1
    fi
}

expect_writes() {
    local state="$1" want="$2" got
    got="$(cat "${state}/writes.log")"
    if [ "${got}" != "${want}" ]; then
        fail "want writes [${want}], got [${got}]"
        return 1
    fi
}

expect_line() {
    if ! grep -Fxq -- "$1" "${LOG}"; then
        fail "want the line: $1"
        return 1
    fi
}

pass() {
    echo "PASS ${CASE} (rc=${RC})"
}

FAKE_PATH="${WORK}/bin:/usr/bin:/bin"

# 1. The 26.04 runner: both limits at the kernel defaults are raised to the
# targets, and each is reported with its value before and after.
CASE="both limits below target are raised"
cases=$((cases + 1))
s="$(new_state 128 65536)"
run_subject "${s}" "${FAKE_PATH}"
expect_rc 0 &&
    expect_value "${s}" "${INSTANCES}" 1280 &&
    expect_value "${s}" "${WATCHES}" 655360 &&
    expect_writes "${s}" "$(printf '%s\n%s' "${INSTANCES}=1280" "${WATCHES}=655360")" &&
    expect_line "${INSTANCES}: 128 -> 1280" &&
    expect_line "${WATCHES}: 65536 -> 655360" &&
    pass

# 2. A host already above the targets is left alone. The script raises; it never
# lowers a limit somebody set higher on purpose.
CASE="limits above target are not lowered"
cases=$((cases + 1))
s="$(new_state 8192 1048576)"
run_subject "${s}" "${FAKE_PATH}"
expect_rc 0 &&
    expect_value "${s}" "${INSTANCES}" 8192 &&
    expect_value "${s}" "${WATCHES}" 1048576 &&
    expect_writes "${s}" "" &&
    expect_line "${INSTANCES}: 8192, at or above 1280" &&
    expect_line "${WATCHES}: 1048576, at or above 655360" &&
    pass

# 3. Exactly at the target is enough: no write. The 24.04 runner is this case.
CASE="limits at target are not written"
cases=$((cases + 1))
s="$(new_state 1280 655360)"
run_subject "${s}" "${FAKE_PATH}"
expect_rc 0 &&
    expect_writes "${s}" "" &&
    expect_line "${INSTANCES}: 1280, at or above 1280" &&
    pass

# 4. Each limit is judged on its own: one low, one high, one write.
CASE="only the low limit is written"
cases=$((cases + 1))
s="$(new_state 128 1048576)"
run_subject "${s}" "${FAKE_PATH}"
expect_rc 0 &&
    expect_value "${s}" "${INSTANCES}" 1280 &&
    expect_value "${s}" "${WATCHES}" 1048576 &&
    expect_writes "${s}" "${INSTANCES}=1280" &&
    pass

# 5. The failure this script exists to catch: a write that reports success and
# changes nothing, the way the runner image's /etc/sysctl.conf line did on 26.04.
# The value is read back, and a limit still below target fails the job.
CASE="a write that does not take fails"
cases=$((cases + 1))
s="$(new_state 128 65536)"
run_subject "${s}" "${FAKE_PATH}" FAKE_SYSCTL_IGNORE_WRITES=1
expect_rc 1 &&
    expect_line "inotify-limits.sh: ${INSTANCES} is 128 after writing 1280" &&
    pass

# 6. A write sysctl refuses, such as one made without root, is a tooling failure.
CASE="a refused write fails"
cases=$((cases + 1))
s="$(new_state 128 65536)"
run_subject "${s}" "${FAKE_PATH}" FAKE_SYSCTL_FAIL_WRITE=1
expect_rc 2 &&
    expect_line "inotify-limits.sh: sysctl -w ${INSTANCES}=1280 failed" &&
    pass

# 7. A key that cannot be read cannot be judged, and is not judged fine.
CASE="an unreadable key fails"
cases=$((cases + 1))
s="$(new_state 128 65536)"
run_subject "${s}" "${FAKE_PATH}" FAKE_SYSCTL_FAIL_READ="${WATCHES}"
expect_rc 2 &&
    expect_line "inotify-limits.sh: cannot read ${WATCHES}" &&
    pass

# 8. Nor is a value that is not a number. Compared as a string, an empty value
# would sort below the target and an unparseable one could sort either way.
CASE="a value that is not a number fails"
cases=$((cases + 1))
s="$(new_state "" 65536)"
run_subject "${s}" "${FAKE_PATH}"
expect_rc 2 &&
    expect_line "inotify-limits.sh: ${INSTANCES} reads as '', not a number" &&
    pass

# 9. No sysctl at all is a tooling failure, not a host that needs nothing.
CASE="no sysctl on PATH fails"
cases=$((cases + 1))
s="$(new_state 128 65536)"
run_subject "${s}" "${WORK}/empty"
expect_rc 2 &&
    expect_line "inotify-limits.sh: sysctl not found on PATH" &&
    pass

echo
if [ "${failures}" -ne 0 ]; then
    echo "${failures} of ${cases} cases failed"
    exit 1
fi
echo "all ${cases} cases passed"
