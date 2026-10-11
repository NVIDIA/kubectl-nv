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
# Tests for the MIG lane scripts: up.sh, down.sh, build-image.sh and the two
# libraries they share.
#
# No cluster is created and no image is built. Every external tool is replaced
# by a PATH shim that records its argv into a log and answers from fixture
# variables. The shims are not inert: the kind shim writes a kubeconfig to
# whichever path the real kind would have written to, and the git shim really
# produces a tar of a fixture tree, so the sentinel test and the chart-path
# assertions can fail.
#
# The subject is resolved relative to THIS file, never through $HOME: a test
# that green-lights a deployed copy of the script says nothing about the tree
# it ships from.
#
# Usage: hack/mokka/mig/up_test.sh
# Exit: 0 all tests pass, 1 at least one failed.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UP="${SCRIPT_DIR}/up.sh"
DOWN="${SCRIPT_DIR}/down.sh"
BUILD="${SCRIPT_DIR}/build-image.sh"
LIB="${SCRIPT_DIR}/lib.sh"

PASS=0
FAIL=0

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mokka-mig-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

# The pins this suite holds the subject to. Written out here rather than read
# from lib.sh: a test that reads its expected value out of the file under test
# agrees with that file by construction and catches nothing.
PINNED_SHA="4ce11e5ae533416415e223ab43da58435d0df48c"
PINNED_NODE_DIGEST="sha256:8bd910853e49bd46953c95b646d35e4765fa7097b18765040b3d95dbc376f524"
# The image tag is DERIVED from the pin, here and in lib.sh, rather than
# written out beside it. A hand-written tag is how an image built from one
# commit goes on being used after the pin has moved to another: the tag still
# matches, so nothing rebuilds. Deriving it in both places means a bumped pin
# changes this literal too, and every assertion below that names the image
# follows the pin rather than lagging it.
PINNED_IMAGE="nvml-mock:mig-${PINNED_SHA:0:8}"
# The privileged device plugin, pinned by digest rather than by the v0.18.2
# tag upstream names. Resolved read-only on 2026-10-11 and agreed on by two
# independent clients; hack/mokka/mig/device-plugin-mock-mig.yaml records how.
PINNED_DEVICE_PLUGIN_DIGEST="sha256:b5788e29e7ae5272de8de863ebe386d6611e608421a6ecc5b4e7d5952aba637f"
EXPECTED_GPUS="14"
# How many workers kind-mig.yaml creates. The DaemonSet gate compares desired
# against this, which is the only check that sees a privileged plugin land on
# a node it was never meant to run on.
EXPECTED_WORKERS="2"

# ---------------------------------------------------------------------------
# Shims
# ---------------------------------------------------------------------------

make_shims() {
    local dir="$1"
    shift
    mkdir -p "${dir}"

    local tool
    for tool in "$@"; do
        case "${tool}" in
            kind)
                cat > "${dir}/kind" <<'SHIM'
#!/bin/bash
echo "kind $*" >> "${MOKKA_CALL_LOG}"

kubeconfig_target() {
    local prev="" a target=""
    for a in "$@"; do
        if [ "${prev}" = "--kubeconfig" ]; then
            target="${a}"
        fi
        prev="${a}"
    done
    if [ -z "${target}" ]; then
        target="${KUBECONFIG:-}"
    fi
    if [ -z "${target}" ]; then
        target="${HOME}/.kube/config"
    fi
    printf '%s' "${target}"
}

case "${1:-} ${2:-}" in
    "get clusters")
        if [ -n "${FAKE_KIND_CLUSTERS:-}" ]; then
            echo "${FAKE_KIND_CLUSTERS}"
        fi
        exit 0
        ;;
    "create cluster")
        t="$(kubeconfig_target "$@")"
        mkdir -p "$(dirname "${t}")"
        printf 'apiVersion: v1\ncurrent-context: kind-cluster\n' > "${t}"
        exit "${FAKE_KIND_RC:-0}"
        ;;
    "delete cluster")
        t="$(kubeconfig_target "$@")"
        if [ -f "${t}" ]; then
            printf 'apiVersion: v1\ncurrent-context: ""\n' > "${t}"
        fi
        exit "${FAKE_KIND_RC:-0}"
        ;;
esac
exit "${FAKE_KIND_RC:-0}"
SHIM
                ;;
            helm)
                cat > "${dir}/helm" <<'SHIM'
#!/bin/bash
echo "helm $*" >> "${MOKKA_CALL_LOG}"

subcommand=""
skip_value=false
for a in "$@"; do
    if [ "${skip_value}" = true ]; then
        skip_value=false
        continue
    fi
    case "${a}" in
        --kubeconfig|--kube-context|--namespace|--version|--values|--set|--timeout)
            skip_value=true
            ;;
        -*) ;;
        *)
            subcommand="${a}"
            break
            ;;
    esac
done

if [ "${subcommand}" = "list" ]; then
    if [ -n "${FAKE_HELM_RELEASES:-}" ]; then
        echo "${FAKE_HELM_RELEASES}"
    fi
    exit 0
fi
exit "${FAKE_HELM_RC:-0}"
SHIM
                ;;
            kubectl)
                cat > "${dir}/kubectl" <<'SHIM'
#!/bin/bash
echo "kubectl $*" >> "${MOKKA_CALL_LOG}"
args="$*"

bump() {
    local f="${MOKKA_STATE_DIR}/$1" n=0
    if [ -f "${f}" ]; then
        n="$(cat "${f}")"
    fi
    n=$((n + 1))
    echo "${n}" > "${f}"
    printf '%s' "${n}"
}

case "${args}" in
    *"get nodes"*)
        nodes="${FAKE_WORKERS-kubectl-nv-mig-worker kubectl-nv-mig-worker2}"
        case "${args}" in
            *'!node-role.kubernetes.io/control-plane'*) ;;
            *) nodes="kubectl-nv-mig-control-plane ${nodes}" ;;
        esac
        case "${args}" in
            *'items[*]'*) ;;
            *) nodes="${nodes%% *}" ;;
        esac
        printf '%s\n' ${nodes}
        exit 0
        ;;
    *clusterpolic*)
        printf '%s' "${FAKE_CLUSTERPOLICY_STATE-ready}"
        exit 0
        ;;
    *"get daemonset"*)
        # Each DaemonSet answers for itself and only in its own namespace, so a
        # subject that waits on one of them, or that forgets a --namespace,
        # cannot be told apart from one that does it right.
        case "${args}" in
            *gpu-feature-discovery*)
                case "${args}" in
                    *"--namespace gpu-operator"*) key=GFD ;;
                    *) echo 'Error from server (NotFound): daemonsets.apps not found' >&2; exit 1 ;;
                esac
                ;;
            *nvidia-device-plugin-mock*)
                case "${args}" in
                    *"--namespace kube-system"*) key=PLUGIN ;;
                    *) echo 'Error from server (NotFound): daemonsets.apps not found' >&2; exit 1 ;;
                esac
                ;;
            *)
                echo 'Error from server (NotFound): daemonsets.apps not found' >&2
                exit 1
                ;;
        esac
        n="$(bump "ds_${key}")"
        if [ "${n}" -le "${FAKE_DS_NOTFOUND_CALLS-0}" ]; then
            echo 'Error from server (NotFound): daemonsets.apps not found' >&2
            exit 1
        fi
        status=""
        case "${key}" in
            GFD) status="${FAKE_DS_STATUS_GFD-}" ;;
            PLUGIN) status="${FAKE_DS_STATUS_PLUGIN-}" ;;
        esac
        if [ -z "${status}" ]; then
            status="${FAKE_DS_STATUS-2 2 2}"
        fi
        printf '%s' "${status}"
        exit 0
        ;;
    *control-plane*)
        # The control plane carries no mokka.nvidia.com/type label, so it runs
        # no device plugin and carries no GPU resource and none of the labels.
        exit 0
        ;;
    *'allocatable.nvidia\.com/gpu'*)
        n="$(bump gpu)"
        if [ "${n}" -le "${FAKE_GPU_NOTREADY_CALLS-0}" ]; then
            exit 0
        fi
        printf '%s' "${FAKE_ALLOCATABLE-14}"
        exit 0
        ;;
    *'capacity.nvidia\.com/gpu'*)
        printf '%s' "${FAKE_CAPACITY-14}"
        exit 0
        ;;
    *'mig\.strategy'*)
        printf '%s' "${FAKE_MIG_STRATEGY-single}"
        exit 0
        ;;
    *'mig\.capable'*)
        printf '%s' "${FAKE_MIG_CAPABLE-true}"
        exit 0
        ;;
    *'gpu\.product'*)
        printf '%s' "${FAKE_GPU_PRODUCT-NVIDIA-H100-80GB-HBM3-MIG-1g.10gb}"
        exit 0
        ;;
    *'gpu\.count'*)
        printf '%s' "${FAKE_GPU_COUNT-14}"
        exit 0
        ;;
    *'gpu\.memory'*)
        printf '%s' "${FAKE_GPU_MEMORY-10240}"
        exit 0
        ;;
esac
exit 0
SHIM
                ;;
            docker)
                cat > "${dir}/docker" <<'SHIM'
#!/bin/bash
echo "docker $*" >> "${MOKKA_CALL_LOG}"

bump() {
    local f="${MOKKA_STATE_DIR}/$1" n=0
    if [ -f "${f}" ]; then
        n="$(cat "${f}")"
    fi
    n=$((n + 1))
    echo "${n}" > "${f}"
    printf '%s' "${n}"
}

case "${1:-} ${2:-}" in
    "image inspect")
        n="$(bump image_inspect)"
        # FAKE_IMAGE_ABSENT_CALLS inspections answer "no such image", so the
        # subject has to build; the ones after it answer "present", which is
        # what the post-build re-check reads.
        if [ "${n}" -le "${FAKE_IMAGE_ABSENT_CALLS-0}" ]; then
            echo "Error: No such image: $*" >&2
            exit 1
        fi
        if [ "${FAKE_IMAGE_NEVER_LANDS:-false}" = true ]; then
            echo "Error: No such image: $*" >&2
            exit 1
        fi
        # An inspection that asks for a value gets one. The revision label is
        # what says which commit the tag was built from, and it is a separate
        # fixture from presence so that "the tag is there but it is the wrong
        # commit" can be driven.
        case "$*" in
            *org.opencontainers.image.revision*)
                printf '%s\n' "${FAKE_IMAGE_REVISION-4ce11e5ae533416415e223ab43da58435d0df48c}"
                ;;
        esac
        exit 0
        ;;
esac
exit "${FAKE_DOCKER_RC:-0}"
SHIM
                ;;
            git)
                cat > "${dir}/git" <<'SHIM'
#!/bin/bash
echo "git $*" >> "${MOKKA_CALL_LOG}"

# Strip a leading -C <dir>, which is how both subjects address a repository.
if [ "${1:-}" = "-C" ]; then
    shift 2
fi

case "${1:-}" in
    rev-parse)
        printf '%s\n' "${FAKE_GIT_HEAD-4ce11e5ae533416415e223ab43da58435d0df48c}"
        exit 0
        ;;
    status)
        # --porcelain prints one line per changed path and nothing at all for
        # a clean tree, which is the whole of the question being asked.
        if [ -n "${FAKE_GIT_DIRTY:-}" ]; then
            printf '%s\n' "${FAKE_GIT_DIRTY}"
        fi
        exit 0
        ;;
    cat-file)
        # $2 is -t, $3 the object. Only the commit the fixture holds resolves.
        if [ "${3:-}" = "${FAKE_GIT_HAVE_COMMIT-4ce11e5ae533416415e223ab43da58435d0df48c}" ]; then
            echo commit
            exit 0
        fi
        echo "fatal: Not a valid object name ${3:-}" >&2
        exit 128
        ;;
    archive)
        # A real tar of the fixture tree, so what the subject then points helm
        # at either exists or does not.
        tar -c -C "${FAKE_UPSTREAM_FIXTURE}" .
        exit 0
        ;;
esac
exit 0
SHIM
                ;;
            *)
                echo "make_shims: unknown tool ${tool}" >&2
                return 1
                ;;
        esac
        chmod +x "${dir}/${tool}"
    done
}

# The system tools the scripts and the shims need, symlinked one by one so that
# a real docker, kubectl, kind, helm or git on the machine cannot leak in.
MOKKA_SYSTEM_TOOLS="bash sh env date mktemp grep sed awk cat sleep rm mkdir
chmod tr wc head tail cut sort dirname basename shasum stat ls printf tar diff"

make_tool_dir() {
    mkdir -p "${TOOL_DIR}"
    local t resolved
    for t in ${MOKKA_SYSTEM_TOOLS}; do
        resolved="$(command -v "${t}" 2> /dev/null || true)"
        case "${resolved}" in
            /*) ln -sf "${resolved}" "${TOOL_DIR}/${t}" ;;
        esac
    done
}

run_subject() {
    local script="$1"
    shift
    : > "${MOKKA_CALL_LOG}"
    rm -rf "${MOKKA_STATE_DIR}"
    if [ "${KEEP_BIN_DIR:-0}" != "1" ]; then
        rm -rf "${MOKKA_BIN_DIR}"
    fi
    mkdir -p "${MOKKA_STATE_DIR}"
    PATH="${SHIM_DIR}:${TOOL_DIR}" HOME="${FAKE_HOME}" \
        "${script}" "$@" > "${WORK}/out" 2> "${WORK}/err"
    RC=$?
    OUT="$(cat "${WORK}/out")"
    ERR="$(cat "${WORK}/err")"
}

# run_subject_bounded <seconds> <script> <args...>
# run_subject with a watchdog, and with the wall time left in ELAPSED.
#
# The cases that drive a timeout are the ones that can hang: a subject whose
# deadline check is wrong does not fail, it spins, and a suite that waits for
# it reports nothing at all. The watchdog turns that into RC=124 and a named
# failure. ELAPSED is then asserted against the deadline the case set, because
# "it finished" and "it finished when it said it would" are different claims.
run_subject_bounded() {
    local limit="$1"
    shift
    local script="$1"
    shift
    : > "${MOKKA_CALL_LOG}"
    rm -rf "${MOKKA_STATE_DIR}"
    if [ "${KEEP_BIN_DIR:-0}" != "1" ]; then
        rm -rf "${MOKKA_BIN_DIR}"
    fi
    mkdir -p "${MOKKA_STATE_DIR}"

    local start pid waited=0
    start="$(date +%s)"
    PATH="${SHIM_DIR}:${TOOL_DIR}" HOME="${FAKE_HOME}" \
        "${script}" "$@" > "${WORK}/out" 2> "${WORK}/err" &
    pid=$!
    while kill -0 "${pid}" 2> /dev/null; do
        if [ "${waited}" -ge "${limit}" ]; then
            kill -KILL "${pid}" 2> /dev/null
            wait "${pid}" 2> /dev/null
            RC=124
            ELAPSED=$(( $(date +%s) - start ))
            OUT="$(cat "${WORK}/out")"
            ERR="$(cat "${WORK}/err")"
            return
        fi
        sleep 1
        waited=$((waited + 1))
    done
    wait "${pid}"
    RC=$?
    ELAPSED=$(( $(date +%s) - start ))
    OUT="$(cat "${WORK}/out")"
    ERR="$(cat "${WORK}/err")"
}

sentinel_hash() {
    shasum -a 256 < "${FAKE_HOME}/.kube/config" | awk '{print $1}'
}

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
    if grep -qF -- "${want}" "${MOKKA_CALL_LOG}"; then
        ok "${name}"
    else
        nope "${name}: no recorded call contains: ${want}" \
            "calls:" "$(cat "${MOKKA_CALL_LOG}")"
    fi
}

assert_not_logged() {
    local want="$1" name="$2"
    if grep -qF -- "${want}" "${MOKKA_CALL_LOG}"; then
        nope "${name}: a recorded call contains: ${want}" \
            "calls:" "$(cat "${MOKKA_CALL_LOG}")"
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

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

SHIM_DIR="${WORK}/bin"
FAKE_HOME="${WORK}/home"
export MOKKA_CALL_LOG="${WORK}/calls.log"
export MOKKA_STATE_DIR="${WORK}/state"
export MOKKA_BIN_DIR="${WORK}/kubeconfigs"
TOOL_DIR="${WORK}/systools"
OUTER_STUBS="${WORK}/outer-stubs"

# A fixture tree standing in for the upstream commit. It carries the one file
# up.sh checks for before it points helm at the chart, so a subject that stages
# into one directory and reads the chart out of another fails here.
export FAKE_UPSTREAM_FIXTURE="${WORK}/upstream-fixture"
mkdir -p "${FAKE_UPSTREAM_FIXTURE}/deployments/nvml-mock/helm/nvml-mock"
printf 'name: nvml-mock\nversion: 0.5.0-dev\n' \
    > "${FAKE_UPSTREAM_FIXTURE}/deployments/nvml-mock/helm/nvml-mock/Chart.yaml"
printf 'FROM scratch\n' > "${FAKE_UPSTREAM_FIXTURE}/deployments/nvml-mock/Dockerfile"

# A clone directory the subject's "is this a clone" check accepts.
export MIG_UPSTREAM_CLONE="${WORK}/fake-clone"
mkdir -p "${MIG_UPSTREAM_CLONE}/.git"

expect_converge() {
    export MOKKA_READY_TIMEOUT=60
    export MOKKA_POLL_INTERVAL=0
}

expect_timeout() {
    export MOKKA_READY_TIMEOUT=2
    export MOKKA_POLL_INTERVAL=1
}

expect_converge

mkdir -p "${FAKE_HOME}/.kube"
printf '%s\n' 'SENTINEL do not touch the default kubeconfig' > "${FAKE_HOME}/.kube/config"
SENTINEL_BEFORE="$(sentinel_hash)"

reset_fixtures() {
    unset FAKE_KIND_CLUSTERS FAKE_KIND_RC FAKE_HELM_RELEASES FAKE_HELM_RC
    unset FAKE_WORKERS FAKE_ALLOCATABLE FAKE_CAPACITY FAKE_MIG_STRATEGY
    unset FAKE_MIG_CAPABLE FAKE_GPU_PRODUCT FAKE_GPU_COUNT FAKE_GPU_MEMORY
    unset FAKE_DS_STATUS FAKE_DS_STATUS_GFD FAKE_DS_STATUS_PLUGIN
    unset FAKE_DS_NOTFOUND_CALLS FAKE_GPU_NOTREADY_CALLS FAKE_CLUSTERPOLICY_STATE
    unset FAKE_GIT_HEAD FAKE_GIT_HAVE_COMMIT FAKE_GIT_DIRTY FAKE_DOCKER_RC
    unset FAKE_IMAGE_ABSENT_CALLS FAKE_IMAGE_NEVER_LANDS FAKE_IMAGE_REVISION
    unset MIG_UPSTREAM_DIR
    expect_converge
    make_shims "${SHIM_DIR}" kind helm kubectl docker git
}

make_tool_dir

# Resolved BEFORE the outer stubs go on PATH: the manifest comparison near the
# end of this file needs the real git, and from there on PATH answers with a
# stub that exits 0 saying nothing, which would turn that comparison into a
# silent skip.
REAL_GIT="$(command -v git 2> /dev/null || true)"

mkdir -p "${OUTER_STUBS}"
for stub in docker kubectl kind helm git; do
    printf '#!/bin/bash\necho "OUTER STUB %s SHOULD NEVER RUN" >&2\nexit 0\n' "${stub}" \
        > "${OUTER_STUBS}/${stub}"
    chmod +x "${OUTER_STUBS}/${stub}"
done
PATH="${OUTER_STUBS}:${PATH}"
export PATH

if [ "$(command -v docker)" = "${OUTER_STUBS}/docker" ] &&
    [ "$(command -v git)" = "${OUTER_STUBS}/git" ]; then
    ok "the harness PATH does offer a docker and a git to leak"
else
    nope "the outer stubs are not on the harness PATH, so the isolation test proves nothing" \
        "docker: $(command -v docker)" "git: $(command -v git)"
fi

rm -rf "${SHIM_DIR}"
reset_fixtures

# ---------------------------------------------------------------------------
# The two guards, called directly
# ---------------------------------------------------------------------------
#
# Driven as functions rather than only through up.sh, because the inputs that
# matter cannot be reached through the command line: mokka::valid_lane rejects
# every lane that would produce a non-kubectl-nv cluster name, so a guard
# exercised only end to end is a guard nobody has seen go red.

guard_rc() {
    # Run in a subshell: both guards exit rather than return.
    (
        MOKKA_PROG="guard-test"
        # shellcheck source=SCRIPTDIR/lib.sh
        . "${LIB}"
        "$@"
    ) > /dev/null 2>&1
    printf '%s' "$?"
}

assert_eq "$(guard_rc mig::require_lane_cluster kubectl-nv-mig)" 0 \
    "require_lane_cluster accepts kubectl-nv-mig"
assert_eq "$(guard_rc mig::require_lane_cluster bench-mokka)" 2 \
    "require_lane_cluster refuses bench-mokka"
assert_eq "$(guard_rc mig::require_lane_cluster kind-kubectl-nv-mig)" 2 \
    "require_lane_cluster refuses a name that merely contains kubectl-nv"
assert_eq "$(guard_rc mig::require_lane_cluster "$(printf 'kubectl-nv-mig\nbench-mokka')")" 2 \
    "require_lane_cluster refuses a name with an embedded newline"
assert_eq "$(guard_rc mig::require_lane_cluster "")" 2 \
    "require_lane_cluster refuses an empty name"

assert_eq "$(guard_rc mig::require_pinned_sha "${PINNED_SHA}")" 0 \
    "require_pinned_sha accepts the pinned commit"
assert_eq "$(guard_rc mig::require_pinned_sha main)" 6 \
    "require_pinned_sha refuses a branch name"
assert_eq "$(guard_rc mig::require_pinned_sha latest)" 6 \
    "require_pinned_sha refuses latest"
assert_eq "$(guard_rc mig::require_pinned_sha 4ce11e5a)" 6 \
    "require_pinned_sha refuses an abbreviated SHA"
assert_eq "$(guard_rc mig::require_pinned_sha 0000000000000000000000000000000000000000)" 6 \
    "require_pinned_sha refuses a different full SHA"
assert_eq "$(guard_rc mig::require_pinned_sha "")" 6 \
    "require_pinned_sha refuses an empty reference"

# ---------------------------------------------------------------------------
# Argument validation
# ---------------------------------------------------------------------------

run_subject "${UP}"
assert_rc 2 "up.sh with no arguments"
assert_says "usage: mig/up.sh <lane>" "up.sh with no arguments prints usage"

run_subject "${UP}" mig extra
assert_rc 2 "up.sh with two arguments"

run_subject "${UP}" "bad lane"
assert_rc 2 "up.sh with a lane that is not a DNS label"

run_subject "${UP}" "UPPER"
assert_rc 2 "up.sh rejects an uppercase lane"

run_subject "${UP}" "$(printf 'x\nbench-mokka')"
assert_rc 2 "up.sh rejects a lane with an embedded newline"
assert_not_logged "create cluster" "a rejected lane creates no cluster"

run_subject "${DOWN}" "$(printf 'x\nbench-mokka')"
assert_rc 2 "down.sh rejects a lane with an embedded newline"
assert_not_logged "delete cluster" "a rejected lane deletes no cluster"

run_subject "${DOWN}"
assert_rc 2 "down.sh with no arguments"

run_subject "${BUILD}" --nonsense
assert_rc 2 "build-image.sh with an unknown flag"

# ---------------------------------------------------------------------------
# Missing tool
# ---------------------------------------------------------------------------

for missing in kind helm kubectl docker git; do
    rm -rf "${SHIM_DIR}"
    present=""
    for t in kind helm kubectl docker git; do
        if [ "${t}" != "${missing}" ]; then
            present="${present} ${t}"
        fi
    done
    # shellcheck disable=SC2086
    make_shims "${SHIM_DIR}" ${present}
    run_subject "${UP}" mig
    assert_rc 3 "up.sh with ${missing} missing, despite a ${missing} on the harness PATH"
    assert_says "mig/up.sh: required tool not found: ${missing}" \
        "up.sh names the missing tool ${missing}"
    if printf '%s\n%s\n' "${OUT}" "${ERR}" | grep -qF 'OUTER STUB'; then
        nope "the subject reached an outer stub; PATH isolation is broken" "stderr: ${ERR}"
    else
        ok "up.sh did not reach the outer ${missing} stub"
    fi
done

rm -rf "${SHIM_DIR}"
reset_fixtures

# ---------------------------------------------------------------------------
# The pin is a gate, not a comment
# ---------------------------------------------------------------------------

# The guard_rc helper above reads lib.sh in a subshell, which is why shellcheck
# thinks this assignment might be lost. It is set here, in the harness shell,
# and read by the subject process started below.
# shellcheck disable=SC2031
export MIG_UPSTREAM_DIR="${WORK}/checkout"
# A checkout stands in for actions/checkout of NVIDIA/k8s-test-infra, so it
# holds the same tree the archive path extracts.
rm -rf "${MIG_UPSTREAM_DIR}"
mkdir -p "${MIG_UPSTREAM_DIR}"
cp -R "${FAKE_UPSTREAM_FIXTURE}/." "${MIG_UPSTREAM_DIR}/"
export FAKE_GIT_HEAD="0123456789abcdef0123456789abcdef01234567"
run_subject "${UP}" mig
assert_rc 6 "up.sh refuses an MIG_UPSTREAM_DIR at the wrong commit"
assert_says "${PINNED_SHA}" "the refusal names the pinned commit"
assert_not_logged "create cluster" "a wrong commit creates no cluster"
assert_not_logged "docker build" "a wrong commit builds no image"
unset FAKE_GIT_HEAD

export FAKE_GIT_HEAD="${PINNED_SHA}"
run_subject "${UP}" mig
assert_rc 0 "up.sh accepts an MIG_UPSTREAM_DIR at the pinned commit"
assert_not_logged "archive ${PINNED_SHA}" "a checkout at the pin is used in place, not re-archived"
unset FAKE_GIT_HEAD MIG_UPSTREAM_DIR

export FAKE_GIT_HAVE_COMMIT="0123456789abcdef0123456789abcdef01234567"
run_subject "${UP}" mig
assert_rc 2 "up.sh refuses a clone that does not hold the pinned commit"
assert_not_logged "create cluster" "a clone without the commit creates no cluster"
unset FAKE_GIT_HAVE_COMMIT

# ---------------------------------------------------------------------------
# Refusal paths
# ---------------------------------------------------------------------------

FAKE_KIND_CLUSTERS="$(printf 'bench-mokka\nkubectl-nv-mig\n')"
export FAKE_KIND_CLUSTERS
run_subject "${UP}" mig
assert_rc 4 "up.sh refuses when the cluster already exists"
assert_says "cluster kubectl-nv-mig already exists" "up.sh says which cluster is in the way"
assert_says "hack/mokka/mig/down.sh mig" "up.sh points at mig/down.sh"

FAKE_KIND_CLUSTERS="$(printf 'bench-mokka\nkubectl-nv-mig-old\n')"
export FAKE_KIND_CLUSTERS
run_subject "${UP}" mig
assert_rc 0 "up.sh does not mistake kubectl-nv-mig-old for kubectl-nv-mig"

FAKE_KIND_CLUSTERS="$(printf 'kubectl-nv-mig\n')"
export FAKE_KIND_CLUSTERS
export FAKE_HELM_RELEASES="nvml-mock"
run_subject "${UP}" mig
assert_rc 4 "up.sh refuses when a Mokka release is installed in the cluster"
assert_says "holds Mokka release nvml-mock" "up.sh names the release in the way"

unset FAKE_KIND_CLUSTERS FAKE_HELM_RELEASES

# ---------------------------------------------------------------------------
# Happy path: what reaches the tools
# ---------------------------------------------------------------------------

run_subject "${UP}" mig
assert_rc 0 "up.sh succeeds when every check and assertion passes"

assert_logged "${PINNED_DIGEST:-${PINNED_NODE_DIGEST}}" "the pinned kind node image digest reaches kind"
assert_logged "--config ${SCRIPT_DIR}/kind-mig.yaml" "the MIG kind topology reaches kind"
assert_logged "kind load docker-image ${PINNED_IMAGE}" "the built image is loaded into the lane"
assert_logged "archive ${PINNED_SHA}" "the upstream tree is read at the pinned commit"
assert_logged "--values ${SCRIPT_DIR}/mig-nvml-mock-values.yaml" "the MIG layout values file reaches helm"
assert_logged "--values ${SCRIPT_DIR}/../gpu-operator-values.yaml" "the shared GPU Operator values file reaches helm"
assert_logged "--values ${SCRIPT_DIR}/mig-gpu-operator-values.yaml" "the MIG GPU Operator overlay reaches helm"
assert_logged "--version v26.3.3" "the GPU Operator version v26.3.3 reaches helm"
assert_logged "apply -f ${SCRIPT_DIR}/device-plugin-mock-mig.yaml" "Mokka's device plugin is applied"
assert_logged "--namespace gpu-operator get daemonset gpu-feature-discovery" "GFD is waited for in the operator namespace"
assert_logged "--namespace kube-system get daemonset nvidia-device-plugin-mock" "the mock plugin is waited for in kube-system"

# Never --set. The MIG switches interact, and the whole point of the values
# files is that a reviewer can read the configuration in one place.
assert_not_logged "--set " "no helm call carries a --set"

# The chart is the staged directory, not the released OCI chart. A copy of the
# whole-GPU lane that forgot this would install 0.4.0, which cannot partition.
assert_not_logged "oci://" "no helm call reaches for the released OCI chart"
assert_not_logged "--version 0.4.0" "the released chart version never reaches helm"

# The one place the upstream version is allowed to be named is the chart
# directory, which lives in the staging tree.
if grep -qE "helm .*upgrade --install nvml-mock [^ ]*/deployments/nvml-mock/helm/nvml-mock" "${MOKKA_CALL_LOG}"; then
    ok "nvml-mock is installed from the staged chart directory"
else
    nope "nvml-mock is not installed from a staged chart directory" \
        "calls:" "$(cat "${MOKKA_CALL_LOG}")"
fi

# The two GPU Operator values files have to arrive in that order, and there
# have to be exactly two of them. Later --values wins, so the overlay coming
# second is what turns the operator's device plugin off; a third file nobody
# accounted for could silently turn it back on, and counting is the only way
# to see that.
operator_line="$(grep -F 'upgrade --install gpu-operator' "${MOKKA_CALL_LOG}" | head -1)"
operator_values_count="$(printf '%s\n' "${operator_line}" | tr ' ' '\n' | grep -cxF -- '--values')"
assert_eq "${operator_values_count}" "2" \
    "the GPU Operator install carries exactly two --values files"

base_at="${operator_line%%gpu-operator-values.yaml*}"
overlay_at="${operator_line%%mig-gpu-operator-values.yaml*}"
if [ "${#base_at}" -lt "${#overlay_at}" ]; then
    ok "the shared values file is passed before the MIG overlay"
else
    nope "the MIG overlay is not passed after the shared values file" "call: ${operator_line}"
fi

# Both installs have to wait, and both have to be bounded. A helm install
# without --wait returns before the workloads it created are running, so the
# gates that follow it race it; one without --timeout inherits helm's 5m
# default, which a cold image pull on a simulated node overruns.
for release in nvml-mock \
    gpu-operator; do
    install_line="$(grep -F "upgrade --install ${release}" "${MOKKA_CALL_LOG}" | head -1)"
    case "${install_line}" in
        *"--wait --timeout "*)
            ok "the ${release} install waits, with a timeout"
            ;;
        *)
            nope "the ${release} install is not '--wait --timeout <d>'" "call: ${install_line}"
            ;;
    esac
done

# The lane's own observations, which NOTES.md is written from.
assert_says "allocatable=${EXPECTED_GPUS} capacity=${EXPECTED_GPUS}" "up.sh reports allocatable and capacity per worker"
assert_says "mig.strategy=single" "up.sh reports the MIG strategy per worker"
assert_says "gpu.memory=10240" "up.sh reports the GFD memory label per worker"

# Time to ready is a number that goes into NOTES.md, so it has to be a number.
# A format string that lost its argument still prints the line, which is why
# "the line is there" is not the assertion.
time_to_ready="$(printf '%s\n' "${OUT}" | sed -n 's/^\[mig\] time to ready: \([0-9][0-9]*\)s$/\1/p')"
if [ -n "${time_to_ready}" ] && [ "${time_to_ready}" -ge 0 ] && [ "${time_to_ready}" -lt 600 ]; then
    ok "up.sh reports an elapsed time to ready (${time_to_ready}s)"
else
    nope "up.sh's time to ready is '${time_to_ready:-<absent or not a number>}'" "stdout: ${OUT}"
fi

if [ "$(sentinel_hash)" = "${SENTINEL_BEFORE}" ]; then
    ok "up.sh leaves the default kubeconfig byte-identical"
else
    nope "up.sh modified the default kubeconfig" \
        "before: ${SENTINEL_BEFORE}" "after:  $(sentinel_hash)"
fi

# ---------------------------------------------------------------------------
# The image build
# ---------------------------------------------------------------------------

run_subject "${UP}" mig
assert_not_logged "docker build" "an image already in the daemon is not rebuilt"

export FAKE_IMAGE_ABSENT_CALLS=1
run_subject "${UP}" mig
assert_rc 0 "up.sh builds the image when it is not in the daemon"
assert_logged "docker build --tag ${PINNED_IMAGE}" "the build tags the pinned image name"
assert_logged "org.opencontainers.image.revision=${PINNED_SHA}" \
    "the build labels the image with the full commit it was built from"
unset FAKE_IMAGE_ABSENT_CALLS

# The tag carries eight hex characters of the commit. Eight is enough to read
# and not enough to be unique, and an image built by hand or left over from a
# differently-pinned tree can wear the same one. The label is the full SHA, so
# the skip-rebuild path has something to check, and a tag whose label names a
# different commit is rebuilt rather than reused.
export FAKE_IMAGE_REVISION="0123456789abcdef0123456789abcdef01234567"
run_subject "${UP}" mig
assert_rc 0 "up.sh still succeeds when the cached image is from another commit"
assert_logged "docker build --tag ${PINNED_IMAGE}" \
    "an image whose revision label names another commit is rebuilt"
assert_says "${PINNED_SHA}" "the rebuild says which commit it wanted"
unset FAKE_IMAGE_REVISION

# An image with no revision label at all is one this lane did not build. Same
# answer: it cannot be vouched for, so it is rebuilt.
export FAKE_IMAGE_REVISION=""
run_subject "${UP}" mig
assert_rc 0 "up.sh succeeds when the cached image carries no revision label"
assert_logged "docker build --tag ${PINNED_IMAGE}" \
    "an image with no revision label is rebuilt"
unset FAKE_IMAGE_REVISION

# ---------------------------------------------------------------------------
# A checkout with uncommitted changes is not the pinned commit
# ---------------------------------------------------------------------------
#
# HEAD says 4ce11e5a and the working tree says something else. The image would
# be built from the something else, under a tag naming the commit, and nothing
# downstream could tell. On a runner this does not happen; on the laptop where
# the upstream clone also gets experimented in, it is the normal case.

# shellcheck disable=SC2031
export MIG_UPSTREAM_DIR="${WORK}/checkout"
export FAKE_GIT_DIRTY=" M pkg/gpu/mocknvml/engine/mig.go"
run_subject "${UP}" mig
assert_rc 6 "up.sh refuses a dirty MIG_UPSTREAM_DIR"
assert_says "pkg/gpu/mocknvml/engine/mig.go" "the refusal names what is modified"
assert_not_logged "docker build" "a dirty checkout builds no image"
assert_not_logged "create cluster" "a dirty checkout creates no cluster"
unset FAKE_GIT_DIRTY MIG_UPSTREAM_DIR

# ---------------------------------------------------------------------------
# A failed build leaves no staged tree behind
# ---------------------------------------------------------------------------
#
# build-image.sh extracts several hundred megabytes into a temporary directory.
# On the success path it is cleaned up; the path that matters is the one where
# docker build fails, which is also the one a developer takes repeatedly while
# working out why.

LEAK_TMP="${WORK}/leak-tmp"
rm -rf "${LEAK_TMP}"
mkdir -p "${LEAK_TMP}"
: > "${MOKKA_CALL_LOG}"
rm -rf "${MOKKA_STATE_DIR}"
mkdir -p "${MOKKA_STATE_DIR}"
PATH="${SHIM_DIR}:${TOOL_DIR}" HOME="${FAKE_HOME}" TMPDIR="${LEAK_TMP}" \
    FAKE_IMAGE_ABSENT_CALLS=99 FAKE_DOCKER_RC=1 \
    "${BUILD}" > "${WORK}/out" 2> "${WORK}/err"
RC=$?
OUT="$(cat "${WORK}/out")"
ERR="$(cat "${WORK}/err")"
if [ "${RC}" -ne 0 ]; then
    ok "build-image.sh fails when docker build fails (rc=${RC})"
else
    nope "build-image.sh reported success for a failed docker build" "stderr: ${ERR}"
fi
leaked="$(find "${LEAK_TMP}" -maxdepth 1 -name 'kubectl-nv-mig-upstream.*' 2> /dev/null | wc -l | tr -d ' ')"
assert_eq "${leaked}" "0" "a failed build leaves no staged tree in TMPDIR"

# docker build exiting 0 and leaving no such tag is a real failure mode (a
# buildx driver that exports nothing to the local daemon), so the exit code is
# not taken as evidence on its own.
export FAKE_IMAGE_NEVER_LANDS=true
run_subject "${UP}" mig
assert_rc 1 "up.sh fails when docker build leaves no image behind"
assert_says "is not in the local daemon" "up.sh says the tag never landed"
assert_not_logged "create cluster" "a failed image build creates no cluster"
unset FAKE_IMAGE_NEVER_LANDS

# ---------------------------------------------------------------------------
# Rollout and assertion timeouts
# ---------------------------------------------------------------------------

expect_timeout

# Every case below sets MOKKA_READY_TIMEOUT=2 and a one-second poll, so a
# subject that honours its own deadline is done in a few seconds. The bound is
# generous against a loaded machine and still far short of a subject that
# spins: without it a broken deadline check hangs this suite instead of
# failing it, and a hung suite reports nothing at all.
BOUND=45

bounded_case() {
    local want_rc="$1" name="$2"
    shift 2
    run_subject_bounded "${BOUND}" "$@"
    if [ "${RC}" -eq 124 ]; then
        nope "${name}: did not finish within ${BOUND}s; its deadline is not being honoured"
        return
    fi
    assert_rc "${want_rc}" "${name}"
    if [ "${ELAPSED}" -le "${BOUND}" ]; then
        ok "${name}: finished in ${ELAPSED}s, inside the ${BOUND}s bound"
    else
        nope "${name}: took ${ELAPSED}s, over the ${BOUND}s bound"
    fi
}

export FAKE_DS_STATUS_PLUGIN="2 0 2"
bounded_case 5 "up.sh times out when the mock device plugin never becomes ready" "${UP}" mig
assert_says "kube-system/nvidia-device-plugin-mock: desired=2 ready=0" \
    "the timeout names the DaemonSet that is holding the lane up"
unset FAKE_DS_STATUS_PLUGIN

export FAKE_DS_STATUS_GFD="2 0 2"
bounded_case 5 "up.sh times out when GFD never becomes ready" "${UP}" mig
assert_says "gpu-operator/gpu-feature-discovery: desired=2 ready=0" \
    "the timeout names GFD"
unset FAKE_DS_STATUS_GFD

# desired has to equal the worker count, not merely equal ready and updated.
#
# This is the only check in the lane that sees the privileged device plugin
# land somewhere it was never meant to. The manifest tolerates every taint, so
# the control-plane taint does not keep it off; the only thing that does is the
# mokka.nvidia.com/type label, which kind-mig.yaml puts on the workers alone.
# Drop that label and the DaemonSet schedules on all three nodes, every pod
# comes up Ready, and "ready == desired" is perfectly true at desired=3.
export FAKE_DS_STATUS="3 3 3"
bounded_case 5 "up.sh fails a DaemonSet that is Ready on three nodes, not two" "${UP}" mig
assert_says "desired=3 ready=3 updated=3, want desired=ready=updated=${EXPECTED_WORKERS}" \
    "the failure names desired=3 against the ${EXPECTED_WORKERS} workers"
unset FAKE_DS_STATUS

# And the mirror: a DaemonSet that only reached one worker is equally
# self-consistent and equally wrong.
export FAKE_DS_STATUS="1 1 1"
bounded_case 5 "up.sh fails a DaemonSet that is Ready on one node, not two" "${UP}" mig
assert_says "desired=1 ready=1 updated=1, want desired=ready=updated=${EXPECTED_WORKERS}" \
    "the failure names desired=1 against the ${EXPECTED_WORKERS} workers"
unset FAKE_DS_STATUS

# A worker short of its partitions is the failure this lane exists to catch:
# seven slices rather than fourteen is one board that did not get carved.
export FAKE_ALLOCATABLE=7
bounded_case 5 "up.sh fails a worker advertising 7 partitions instead of 14" "${UP}" mig
assert_says "allocatable nvidia.com/gpu is '7', expected 14" \
    "the failure names what was seen and what was wanted"
unset FAKE_ALLOCATABLE

# Capacity and allocatable coming apart is how a node that registered its
# partitions and then lost the plugin serving them looks.
export FAKE_CAPACITY=0
bounded_case 5 "up.sh fails a worker whose capacity does not match its allocatable" "${UP}" mig
assert_says "capacity nvidia.com/gpu is '0', expected 14" "the failure names the capacity"
unset FAKE_CAPACITY

# The whole point of the lane: a layout the plugin publishes under a strategy
# other than single is not what this lane claims to be.
export FAKE_MIG_STRATEGY=mixed
bounded_case 5 "up.sh fails a worker labelled with a strategy other than single" "${UP}" mig
assert_says "nvidia.com/mig.strategy is 'mixed', expected single" "the failure names the strategy"
unset FAKE_MIG_STRATEGY

export FAKE_MIG_CAPABLE=false
bounded_case 5 "up.sh fails a worker that is not MIG capable" "${UP}" mig
assert_says "nvidia.com/mig.capable is 'false', expected true" \
    "the failure names the capability label"
unset FAKE_MIG_CAPABLE

# The three GFD labels the lane records but does not pin a value for. Each is
# checked for presence, and each needs a case of its own: a single "the labels
# are there" check passes while two of the three are missing.
export FAKE_GPU_MEMORY=""
bounded_case 5 "up.sh fails a worker with no nvidia.com/gpu.memory label" "${UP}" mig
assert_says "label nvidia.com/gpu.memory is absent" "the failure names the memory label"
unset FAKE_GPU_MEMORY

export FAKE_GPU_COUNT=""
bounded_case 5 "up.sh fails a worker with no nvidia.com/gpu.count label" "${UP}" mig
assert_says "label nvidia.com/gpu.count is absent" "the failure names the count label"
unset FAKE_GPU_COUNT

export FAKE_GPU_PRODUCT=""
bounded_case 5 "up.sh fails a worker with no nvidia.com/gpu.product label" "${UP}" mig
assert_says "label nvidia.com/gpu.product is absent" "the failure names the product label"
unset FAKE_GPU_PRODUCT

expect_converge

# ---------------------------------------------------------------------------
# down.sh
# ---------------------------------------------------------------------------

run_subject "${DOWN}" mig
assert_rc 2 "down.sh refuses a lane whose cluster does not exist"
assert_says "no such cluster: kubectl-nv-mig" "down.sh names the cluster it looked for"
assert_not_logged "delete cluster" "down.sh deletes nothing when there is nothing to delete"

FAKE_KIND_CLUSTERS="$(printf 'bench-mokka\nkubectl-nv-mig\n')"
export FAKE_KIND_CLUSTERS
run_subject "${DOWN}" mig
assert_rc 0 "down.sh deletes the lane cluster"
assert_logged "kind delete cluster --name kubectl-nv-mig" "down.sh names the lane cluster"
assert_not_logged "bench-mokka" "down.sh never names a cluster that is not the lane's"

if [ "$(sentinel_hash)" = "${SENTINEL_BEFORE}" ]; then
    ok "down.sh leaves the default kubeconfig byte-identical"
else
    nope "down.sh modified the default kubeconfig" \
        "before: ${SENTINEL_BEFORE}" "after:  $(sentinel_hash)"
fi
unset FAKE_KIND_CLUSTERS

# ---------------------------------------------------------------------------
# The files beside the scripts
# ---------------------------------------------------------------------------
#
# lib.sh and mig-nvml-mock-values.yaml both name the image. They are two files,
# so they can drift; a chart pointed at a tag nothing built pulls from Docker
# Hub and fails on a node with no network.

values_repo="$(sed -n 's/^  repository: *//p' "${SCRIPT_DIR}/mig-nvml-mock-values.yaml")"
values_tag="$(sed -n 's/^  tag: *//p' "${SCRIPT_DIR}/mig-nvml-mock-values.yaml")"
assert_eq "${values_repo}:${values_tag}" "${PINNED_IMAGE}" \
    "mig-nvml-mock-values.yaml names the image lib.sh builds"

# lib.sh is read by sourcing it rather than by grepping it, because what is
# being checked is the VALUE the scripts get, and the tag is derived rather
# than written out. A grep for a literal would report whatever the file says
# and miss a derivation that produces something else.
lib_values() {
    (
        MOKKA_PROG="lib-values"
        # shellcheck source=SCRIPTDIR/lib.sh
        . "${LIB}"
        printf '%s|%s|%s|%s\n' "${MIG_UPSTREAM_SHA}" \
            "${MIG_NVML_MOCK_IMAGE}" "${MIG_NVML_MOCK_IMAGE_TAG}" \
            "${MIG_EXPECTED_GPUS}"
    )
}
IFS='|' read -r lib_sha lib_repo lib_tag lib_expected <<< "$(lib_values)"

assert_eq "${lib_repo}:${lib_tag}" "${PINNED_IMAGE}" "lib.sh names the pinned image"
assert_eq "${lib_sha}" "${PINNED_SHA}" \
    "lib.sh pins the upstream commit this suite was written against"

# The tag has to FOLLOW the pin, not sit beside it. Checked against lib.sh's
# own SHA as well as against the literal above: together they say the tag is
# eight characters of whatever commit lib.sh names, so a bumped pin that left
# a stale tag behind fails here rather than reusing an image built from the
# old commit.
assert_eq "${lib_tag}" "mig-${lib_sha:0:8}" \
    "lib.sh derives the image tag from the commit it pins"
assert_eq "${values_tag}" "mig-${lib_sha:0:8}" \
    "mig-nvml-mock-values.yaml carries the tag derived from lib.sh's commit"

# lib.sh and this suite have to agree on what a worker advertises, or the
# script gates one number and the e2e suite asserts another.
assert_eq "${lib_expected}" "${EXPECTED_GPUS}" \
    "lib.sh expects the ${EXPECTED_GPUS} partitions this suite was written against"

# The layout has to be the uniform one --mig-strategy=single accepts, and the
# arithmetic has to hold: boards x slices = what up.sh asserts.
mig_boards="$(sed -n 's/^MIG_BOARDS="\(.*\)"$/\1/p' "${LIB}")"
mig_slices="$(sed -n 's/^MIG_SLICES_PER_BOARD="\(.*\)"$/\1/p' "${LIB}")"
mig_expected="$(sed -n 's/^MIG_EXPECTED_GPUS="\(.*\)"$/\1/p' "${LIB}")"
assert_eq "$(( mig_boards * mig_slices ))" "${mig_expected}" \
    "MIG_EXPECTED_GPUS is boards times slices per board"
assert_eq "${mig_expected}" "${EXPECTED_GPUS}" "the lane expects ${EXPECTED_GPUS} partitions per worker"

# The values file has to carry the same layout the pins describe.
values_count="$(sed -n 's/^  count: *//p' "${SCRIPT_DIR}/mig-nvml-mock-values.yaml" | head -1)"
assert_eq "${values_count}" "${mig_boards}" "mig-nvml-mock-values.yaml installs MIG_BOARDS boards"
if grep -qE '^ *- profile: "1g\.10gb"' "${SCRIPT_DIR}/mig-nvml-mock-values.yaml" &&
    grep -qE "^ *count: ${mig_slices}\$" "${SCRIPT_DIR}/mig-nvml-mock-values.yaml"; then
    ok "mig-nvml-mock-values.yaml carves ${mig_slices} x 1g.10gb per board"
else
    nope "mig-nvml-mock-values.yaml does not carve ${mig_slices} x 1g.10gb per board"
fi

# The overlay has to actually override the two values it exists for. A file
# that is present and says nothing leaves the operator's own device plugin
# running, which then races the mock one for nvidia.com/gpu.
#
# Parsed, not grepped. Two independent greps for "^devicePlugin:" and for
# "^  enabled: false" are both satisfied by a file where `enabled: false` sits
# under something else entirely, which is a values file that leaves the
# operator's device plugin running: two plugins then race for nvidia.com/gpu
# on every worker. The question is what the key resolves to, so the file is
# loaded and the keys are read.
overlay="${SCRIPT_DIR}/mig-gpu-operator-values.yaml"
cat > "${WORK}/overlay-check.py" <<'PY'
import sys
try:
    import yaml
except ImportError:
    print("SKIP pyyaml is not installed")
    raise SystemExit(0)

doc = yaml.safe_load(open(sys.argv[1])) or {}
problems = []

plugin = doc.get("devicePlugin")
if not isinstance(plugin, dict):
    problems.append("devicePlugin is %r, want a mapping" % (plugin,))
elif plugin.get("enabled") is not False:
    problems.append("devicePlugin.enabled is %r, want False" % (plugin.get("enabled"),))

mig = doc.get("mig")
if not isinstance(mig, dict):
    problems.append("mig is %r, want a mapping" % (mig,))
elif mig.get("strategy") != "single":
    problems.append("mig.strategy is %r, want 'single'" % (mig.get("strategy"),))

# The overlay exists to override exactly these two. Anything else in it is a
# change to the shared GPU Operator configuration that was made in the MIG
# lane's file and not in the lane everybody else runs.
extra = sorted(set(doc) - {"devicePlugin", "mig"})
if extra:
    problems.append("overrides keys beyond devicePlugin and mig: %s" % ",".join(extra))

print("OK" if not problems else "BAD " + "; ".join(problems))
PY
overlay_report="$(python3 "${WORK}/overlay-check.py" "${overlay}")"
case "${overlay_report}" in
    OK) ok "the overlay resolves devicePlugin.enabled to false and mig.strategy to single, and overrides nothing else" ;;
    SKIP*) echo "SKIP - ${overlay_report#SKIP } ; the overlay was not parsed" ;;
    *) nope "the overlay does not say what it has to" "${overlay_report}" ;;
esac

# The kind topology, read per node rather than by counting matches anywhere in
# the file. A count is satisfied by two labels on the control plane and none
# on a worker, which is the arrangement that puts the privileged device plugin
# on the control plane and leaves a worker with no plugin at all.
cat > "${WORK}/kind-check.py" <<'PY'
import sys
try:
    import yaml
except ImportError:
    print("SKIP pyyaml is not installed")
    raise SystemExit(0)

doc = yaml.safe_load(open(sys.argv[1])) or {}
want_workers = int(sys.argv[2])
label = "mokka.nvidia.com/type"
problems = []

nodes = doc.get("nodes") or []
workers = [n for n in nodes if n.get("role") == "worker"]
controls = [n for n in nodes if n.get("role") == "control-plane"]

if len(workers) != want_workers:
    problems.append("%d worker node(s), want %d" % (len(workers), want_workers))
if len(controls) != 1:
    problems.append("%d control-plane node(s), want 1" % len(controls))

for i, node in enumerate(workers):
    got = (node.get("labels") or {}).get(label)
    if got != "sgpu":
        problems.append("worker %d has %s=%r, want 'sgpu'" % (i, label, got))

for i, node in enumerate(controls):
    labels = node.get("labels") or {}
    if label in labels:
        problems.append(
            "control-plane %d carries %s=%r; the device-plugin DaemonSet "
            "tolerates every taint, so this label is the only thing keeping "
            "it off that node" % (i, label, labels[label]))

print("OK" if not problems else "BAD " + "; ".join(problems))
PY
kind_report="$(python3 "${WORK}/kind-check.py" "${SCRIPT_DIR}/kind-mig.yaml" "${EXPECTED_WORKERS}")"
case "${kind_report}" in
    OK) ok "kind-mig.yaml labels both workers sgpu and leaves the control plane unlabelled" ;;
    SKIP*) echo "SKIP - ${kind_report#SKIP } ; kind-mig.yaml was not parsed" ;;
    *) nope "kind-mig.yaml's node labels are not what the lane needs" "${kind_report}" ;;
esac

# ---------------------------------------------------------------------------
# The copied manifest is still the upstream one
# ---------------------------------------------------------------------------
#
# Skipped, loudly, when no clone holding the commit is reachable: reporting a
# pass nobody earned is worse than reporting that the check did not run.

MANIFEST="${SCRIPT_DIR}/device-plugin-mock-mig.yaml"
REAL_CLONE="${MIG_UPSTREAM_CLONE_REAL:-${HOME}/src/github/nvidia/k8s-test-infra}"
if [ -n "${REAL_GIT}" ] &&
    [ "$("${REAL_GIT}" -C "${REAL_CLONE}" cat-file -t "${PINNED_SHA}" 2> /dev/null)" = "commit" ]; then
    # The one deliberate difference is applied to the UPSTREAM side, not
    # forgiven on both: the copy replaced every spaced typographic em dash with
    # ": " because hack/lint-words.sh rejects the dash. Normalising upstream
    # the same way keeps this a byte comparison of the whole file, comments
    # included, rather than of its non-comment lines.
    # Two normalisations, both applied to the UPSTREAM side rather than
    # forgiven on both, and both checked below to have fired:
    #
    #   1. every spaced typographic em dash becomes ": ", because
    #      hack/lint-words.sh rejects the dash anywhere in this repository;
    #   2. the device plugin's :v0.18.2 tag becomes the digest that tag
    #      resolved to, because this manifest runs that image privileged and a
    #      tag is not a pin.
    #
    # Normalising rather than skipping lines keeps this a byte comparison of
    # the whole file, comments included.
    EM_DASH="$(printf '\342\200\224')"
    PLUGIN_TAGGED="nvcr.io/nvidia/k8s-device-plugin:v0.18.2"
    PLUGIN_PINNED="nvcr.io/nvidia/k8s-device-plugin@${PINNED_DEVICE_PLUGIN_DIGEST}"
    "${REAL_GIT}" -C "${REAL_CLONE}" show \
        "${PINNED_SHA}:tests/e2e/go/assets/device-plugin-mock-mig.yaml" \
        > "${WORK}/upstream-raw.yaml" 2> /dev/null
    sed -e "s/ ${EM_DASH} /: /g" -e "s|${PLUGIN_TAGGED}|${PLUGIN_PINNED}|g" \
        "${WORK}/upstream-raw.yaml" > "${WORK}/upstream-manifest.yaml"
    sed -n '/^# UPSTREAM-FILE-BEGINS$/,$p' "${MANIFEST}" | tail -n +2 \
        > "${WORK}/local-manifest.yaml"

    # A normalisation that matched nothing would make the comparison pass for
    # the wrong reason, so each is checked to have fired. If upstream ever
    # stops using the dashes, or renames the image, the counts change and this
    # says so rather than the diff below reporting something unrelated.
    before="$(LC_ALL=C grep -c -- "${EM_DASH}" "${WORK}/upstream-raw.yaml")"
    after="$(LC_ALL=C grep -c -- "${EM_DASH}" "${WORK}/upstream-manifest.yaml")"
    assert_eq "${before}/${after}" "3/0" \
        "the em dash normalisation turns upstream's three dashes into none"

    tag_before="$(grep -cF -- "${PLUGIN_TAGGED}" "${WORK}/upstream-raw.yaml")"
    tag_after="$(grep -cF -- "${PLUGIN_TAGGED}" "${WORK}/upstream-manifest.yaml")"
    digest_after="$(grep -cF -- "${PLUGIN_PINNED}" "${WORK}/upstream-manifest.yaml")"
    assert_eq "${tag_before}/${tag_after}/${digest_after}" "1/0/1" \
        "the digest normalisation replaces upstream's one tagged image reference"
    if diff -u "${WORK}/upstream-manifest.yaml" "${WORK}/local-manifest.yaml" > "${WORK}/manifest.diff" 2>&1; then
        ok "device-plugin-mock-mig.yaml is byte identical to upstream at ${PINNED_SHA}"
    else
        nope "device-plugin-mock-mig.yaml has drifted from upstream" "$(cat "${WORK}/manifest.diff")"
    fi
else
    echo "SKIP - no clone holding ${PINNED_SHA} at ${REAL_CLONE}; the manifest was not compared"
fi

# Whatever the comparison above could do, these two have to hold: the manifest
# is the one the values overlay's comment points at, and it runs the plugin
# under the strategy the lane asserts.
if grep -qF -- '--mig-strategy=single' "${MANIFEST}"; then
    ok "the manifest runs the plugin with --mig-strategy=single"
else
    nope "the manifest does not pass --mig-strategy=single"
fi
# The image runs privileged and bind-mounts over its own /proc/driver, so a
# tag is not a pin: whoever can move it can change what runs privileged on
# every worker. It is named by digest, and the tag must not appear on the
# image line at all.
if grep -qF "nvcr.io/nvidia/k8s-device-plugin@${PINNED_DEVICE_PLUGIN_DIGEST}" "${MANIFEST}"; then
    ok "the manifest pins the device plugin image by digest"
else
    nope "the manifest does not pin the device plugin by digest" \
        "image line: $(grep -F 'k8s-device-plugin' "${MANIFEST}" | head -1)"
fi
if grep -qE '^ *image: *nvcr\.io/nvidia/k8s-device-plugin:' "${MANIFEST}"; then
    nope "the manifest still names the device plugin by tag" \
        "image line: $(grep -F 'k8s-device-plugin' "${MANIFEST}" | head -1)"
else
    ok "the manifest names the device plugin by digest and not by tag"
fi

# The nodeSelector and the kind topology are one mechanism in two files: the
# label kind-mig.yaml puts on the workers, checked per node above, is what
# keeps the privileged plugin off the control plane, which tolerates
# everything. This end asserts the manifest still selects on that same key.
cat > "${WORK}/selector-check.py" <<'PY'
import sys
try:
    import yaml
except ImportError:
    print("SKIP")
    raise SystemExit(0)
doc = yaml.safe_load(open(sys.argv[1])) or {}
sel = (((doc.get("spec") or {}).get("template") or {}).get("spec") or {}).get("nodeSelector") or {}
print(";".join("%s=%s" % kv for kv in sorted(sel.items())) or "<none>")
PY
plugin_selector="$(python3 "${WORK}/selector-check.py" "${MANIFEST}")"
case "${plugin_selector}" in
    SKIP) echo "SKIP - pyyaml is not installed; the manifest's nodeSelector was not parsed" ;;
    *) assert_eq "${plugin_selector}" "mokka.nvidia.com/type=sgpu" \
        "the manifest selects the nodes kind-mig.yaml labels" ;;
esac

# ---------------------------------------------------------------------------
# The workflow and the lane agree
# ---------------------------------------------------------------------------
#
# The workflow checks out NVIDIA/k8s-test-infra at a SHA of its own, and the
# lane scripts refuse any checkout that is not at theirs. Those two SHAs
# meeting is what makes the job run at all, and nothing but this check is
# watching them: a bumped pin in lib.sh with the workflow left behind fails in
# CI, several minutes in, with a message about a commit mismatch and no hint
# that the workflow is the stale one.
#
# The diagnostics path is the same story in a smaller way. The Makefile writes
# the dump to E2E_MIG_DIAG_DIR and the workflow uploads a path it names
# separately; if they drift, a failed nightly run uploads nothing and says
# "no files found".

WORKFLOW="${SCRIPT_DIR}/../../../.github/workflows/e2e-mig.yaml"
MAKEFILE="${SCRIPT_DIR}/../../../Makefile"

if [ -f "${WORKFLOW}" ]; then
    workflow_sha="$(sed -n 's/^ *MIG_UPSTREAM_SHA: *\([0-9a-f]\{40\}\) *$/\1/p' "${WORKFLOW}" | head -1)"
    assert_eq "${workflow_sha}" "${lib_sha}" \
        "the workflow checks out the commit lib.sh pins"

    # Nothing in the workflow may name a branch or latest for that repository:
    # the job builds and then runs a container image out of whatever it gets.
    if grep -nE 'ref: *(main|master|latest)' "${WORKFLOW}" > /dev/null; then
        nope "the workflow checks out a moving reference" \
            "$(grep -nE 'ref: *(main|master|latest)' "${WORKFLOW}")"
    else
        ok "the workflow checks out no branch and no latest"
    fi

    # The dump the Makefile writes and the artifact the workflow uploads are
    # the same directory.
    # The make variable reference is matched literally, so the single quotes
    # around the sed script are what is wanted.
    # shellcheck disable=SC2016
    diag_dir="$(sed -n 's/^E2E_MIG_DIAG_DIR ?= *\$(CURDIR)\/\(.*\)$/\1/p' "${MAKEFILE}" | head -1)"
    # The directory is named with the lane variable in it; substituting the
    # default lane gives the path a default run actually writes to. The
    # pattern is held in a variable because "$(" inside ${var//...} opens a
    # command substitution whatever escaping is tried on it.
    # A literal make variable reference, matched as text.
    # shellcheck disable=SC2016
    lane_ref='$(MIG_LANE)'
    diag_dir="${diag_dir//${lane_ref}/mig}"
    upload_path="$(sed -n 's/^ *path: *\(bin\/[^ ]*\) *$/\1/p' "${WORKFLOW}" | head -1)"
    assert_eq "${upload_path%/}" "${diag_dir%/}" \
        "the workflow uploads the directory the e2e-mig target dumps into"
else
    nope "no workflow at ${WORKFLOW}"
fi

# ---------------------------------------------------------------------------
# Static analysis of the scripts under test
# ---------------------------------------------------------------------------

if command -v shellcheck > /dev/null 2>&1; then
    for dir in / "${SCRIPT_DIR}"; do
        if (cd "${dir}" && shellcheck -x \
            "${SCRIPT_DIR}/up.sh" "${SCRIPT_DIR}/down.sh" \
            "${SCRIPT_DIR}/build-image.sh" "${SCRIPT_DIR}/image.sh" \
            "${SCRIPT_DIR}/lib.sh" "${SCRIPT_DIR}/up_test.sh" \
            "${SCRIPT_DIR}/make-target_test.sh"); then
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
