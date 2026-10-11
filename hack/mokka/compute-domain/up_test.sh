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
# Tests for hack/mokka/compute-domain/up.sh and down.sh.
#
# No cluster is created. Every external tool is replaced by a PATH shim that
# records its argv into a log and answers from fixture variables, following
# hack/mokka/up_test.sh.
#
# Two shims do more than record, because without them a test here would be
# theater:
#
#   kind    writes a kubeconfig to whichever path the real kind would have
#           written to, so the sentinel test can fail.
#   curl    writes the bytes a fixture names to its -o path, so the checksum
#           branch can be driven both ways rather than asserted about.
#
# The subject is resolved relative to THIS file, never through $HOME.
#
# Usage: hack/mokka/compute-domain/up_test.sh
# Exit: 0 all tests pass, 1 at least one failed.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UP="${SCRIPT_DIR}/up.sh"
DOWN="${SCRIPT_DIR}/down.sh"

PASS=0
FAIL=0

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mokka-cd-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

# ---------------------------------------------------------------------------
# Shims
# ---------------------------------------------------------------------------

# make_shims <dir> <tool>...
# Writes one recording shim per named tool. A tool left out of the list is
# absent from the shim directory, which is how the missing-tool path is driven.
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

# A values file that is not there is a broken lane, and helm says so. The
# shim says so too, or a subject that pointed --values at the wrong path
# would pass every test here.
prev=""
for a in "$@"; do
    if [ "${prev}" = "--values" ] && [ ! -f "${a}" ]; then
        echo "Error: open ${a}: no such file or directory" >&2
        exit 1
    fi
    prev="${a}"
done

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

# The node this query is about, taken from the argv rather than guessed, so a
# per-node fixture can answer differently for different nodes.
node_of() {
    local prev="" a
    for a in "$@"; do
        if [ "${prev}" = "node" ]; then
            printf '%s' "${a}"
            return
        fi
        prev="${a}"
    done
}

case "${args}" in
    *"get nodes"*)
        nodes="${FAKE_WORKERS-kubectl-nv-compute-domain-worker kubectl-nv-compute-domain-worker2 kubectl-nv-compute-domain-worker3 kubectl-nv-compute-domain-worker4}"
        case "${args}" in
            *'!node-role.kubernetes.io/control-plane'*) ;;
            *) nodes="kubectl-nv-compute-domain-control-plane ${nodes}" ;;
        esac
        printf '%s\n' ${nodes}
        exit 0
        ;;
    *"get daemonset"*)
        case "${args}" in
            *"--namespace gpu-operator"*) ;;
            *)
                echo 'Error from server (NotFound): daemonsets.apps not found' >&2
                exit 1
                ;;
        esac
        case "${args}" in
            *nvidia-device-plugin-daemonset*) key=DEVICE_PLUGIN ;;
            *gpu-feature-discovery*) key=GFD ;;
            *)
                echo 'Error from server (NotFound): daemonsets.apps not found' >&2
                exit 1
                ;;
        esac
        n="$(bump "ds_${key}")"
        if [ "${n}" -le "${FAKE_DS_NOTREADY_CALLS-0}" ]; then
            printf '%s' "${FAKE_DS_NOTREADY_STATUS-4 0 4}"
            exit 0
        fi
        printf '%s' "${FAKE_DS_STATUS-4 4 4}"
        exit 0
        ;;
    *'allocatable.nvidia\.com/gpu'*)
        echo "${FAKE_ALLOCATABLE-4}"
        exit 0
        ;;
    *'gpu\.clique'*)
        node="$(node_of "$@")"
        # A per-node override wins, which is what lets one worker be given the
        # wrong clique while the others are right.
        var="FAKE_CLIQUE_${node##*-}"
        value="${!var-}"
        if [ -n "${value}" ]; then
            printf '%s' "${value}"
            exit 0
        fi
        if [ -n "${FAKE_CLIQUE_ALL-}" ]; then
            printf '%s' "${FAKE_CLIQUE_ALL}"
            exit 0
        fi
        case "${node}" in
            *-worker|*-worker2) printf '00000000-0000-0000-0000-0000000000ab.0' ;;
            *) printf '00000000-0000-0000-0000-0000000000ab.1' ;;
        esac
        exit 0
        ;;
    *'gpu\.product'*)
        echo "${FAKE_GPU_PRODUCT-NVIDIA-GB200}"
        exit 0
        ;;
    *topograph*)
        echo "${FAKE_TOPOGRAPH_LABEL-ib-leaf-0}"
        exit 0
        ;;
    *"rollout status"*)
        exit "${FAKE_KUEUE_ROLLOUT_RC:-0}"
        ;;
    *"apply -f "*kueue-topology*)
        n="$(bump kueue_apply)"
        if [ "${n}" -le "${FAKE_KUEUE_APPLY_REJECTS-0}" ]; then
            echo 'Error from server (InternalError): failed calling webhook' >&2
            exit 1
        fi
        echo 'topology.kueue.x-k8s.io/kubectl-nv-compute-domain created'
        exit 0
        ;;
    *"create namespace"*)
        if [ -n "${FAKE_NAMESPACE_EXISTS:-}" ]; then
            echo 'Error from server (AlreadyExists): namespaces "kubectl-nv-e2e" already exists' >&2
            exit 1
        fi
        if [ -n "${FAKE_NAMESPACE_RC:-}" ] && [ "${FAKE_NAMESPACE_RC}" -ne 0 ]; then
            echo 'Error from server (Forbidden): namespaces is forbidden' >&2
            exit "${FAKE_NAMESPACE_RC}"
        fi
        echo 'namespace/kubectl-nv-e2e created'
        exit 0
        ;;
esac
exit 0
SHIM
                ;;
            curl)
                cat > "${dir}/curl" <<'SHIM'
#!/bin/bash
echo "curl $*" >> "${MOKKA_CALL_LOG}"

# The real curl writes to the path after -o. So does this one, with the bytes
# the fixture names, which is what makes the checksum branch drivable.
target=""
prev=""
for a in "$@"; do
    case "${prev}" in
        -o|-fsSLo|--output) target="${a}" ;;
    esac
    prev="${a}"
done
# -fsSLo bundles the flag with its value's position, so the loop above sees it
# as the previous word exactly as it does for -o.
if [ -z "${target}" ]; then
    echo "curl shim: no output path in: $*" >&2
    exit 2
fi
if [ -n "${FAKE_CURL_RC:-}" ] && [ "${FAKE_CURL_RC}" -ne 0 ]; then
    exit "${FAKE_CURL_RC}"
fi
printf '%s' "${FAKE_CURL_BODY-kueue manifests}" > "${target}"
exit 0
SHIM
                ;;
            docker)
                cat > "${dir}/docker" <<'SHIM'
#!/bin/bash
echo "docker $*" >> "${MOKKA_CALL_LOG}"
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

# A directory of symlinks to exactly the system tools the scripts and the
# shims need, so a machine with a real kind, helm, kubectl, docker or curl on
# PATH cannot leak one into a test.
MOKKA_SYSTEM_TOOLS="bash sh env date mktemp grep sed awk cat sleep rm mkdir
chmod tr wc head tail cut sort seq dirname basename shasum sha256sum stat ls printf"

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

# run_subject <script> <args...>
# Runs the subject with an isolated PATH, with HOME pointed at a throwaway
# directory holding a sentinel kubeconfig. Sets RC, OUT and ERR.
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

# ---------------------------------------------------------------------------
# Fixtures shared by every case
# ---------------------------------------------------------------------------

SHIM_DIR="${WORK}/bin"
FAKE_HOME="${WORK}/home"
export MOKKA_CALL_LOG="${WORK}/calls.log"
export MOKKA_STATE_DIR="${WORK}/state"
export MOKKA_BIN_DIR="${WORK}/kubeconfigs"
TOOL_DIR="${WORK}/systools"
OUTER_STUBS="${WORK}/outer-stubs"

# Timeouts. A case that is meant to converge needs the wall clock only to not
# run out, because the shims answer on a call count rather than on elapsed
# time; ten seconds against a `date +%s` of one-second resolution is margin
# enough, and it is also the cost of a case the subject will never satisfy.
# That second number matters: a mutant that breaks one assertion makes every
# later case spin to this deadline, so a deadline of a minute turns a mutation
# run into twenty.
expect_converge() {
    export MOKKA_READY_TIMEOUT=10
    export MOKKA_POLL_INTERVAL=0
}

expect_timeout() {
    export MOKKA_READY_TIMEOUT=2
    export MOKKA_POLL_INTERVAL=1
}

expect_converge

PINNED_DIGEST="sha256:8bd910853e49bd46953c95b646d35e4765fa7097b18765040b3d95dbc376f524"
PINNED_KUEUE_SHA256="299579286157359052a4355c421a829c81aced8b9feca52c37aefb87ea93a623"
CLUSTER="kubectl-nv-compute-domain"

# The body the curl shim serves on the happy path, and the digest up.sh is
# told to expect for it. The real pin stays what it is; this is the only way a
# shim-served payload can drive the matching branch at all.
FAKE_BODY="kueue manifests for the lane test"
FAKE_BODY_SHA256="$(printf '%s' "${FAKE_BODY}" | shasum -a 256 | awk '{print $1}')"

mkdir -p "${FAKE_HOME}/.kube"
printf '%s\n' 'SENTINEL do not touch the default kubeconfig' > "${FAKE_HOME}/.kube/config"
SENTINEL_BEFORE="$(sentinel_hash)"

reset_fixtures() {
    unset FAKE_KIND_CLUSTERS FAKE_KIND_RC FAKE_HELM_RELEASES FAKE_HELM_RC
    unset FAKE_WORKERS FAKE_ALLOCATABLE FAKE_GPU_PRODUCT FAKE_TOPOGRAPH_LABEL
    unset FAKE_DS_STATUS FAKE_DS_NOTREADY_CALLS FAKE_DS_NOTREADY_STATUS
    unset FAKE_CLIQUE_ALL FAKE_CLIQUE_worker FAKE_CLIQUE_worker2
    unset FAKE_CLIQUE_worker3 FAKE_CLIQUE_worker4
    unset FAKE_CURL_BODY FAKE_CURL_RC FAKE_KUEUE_ROLLOUT_RC FAKE_KUEUE_APPLY_REJECTS
    unset FAKE_NAMESPACE_EXISTS FAKE_NAMESPACE_RC
    export KUEUE_SHA256="${FAKE_BODY_SHA256}"
    export FAKE_CURL_BODY="${FAKE_BODY}"
    expect_converge
    make_shims "${SHIM_DIR}" kind helm kubectl docker curl
}

make_tool_dir

mkdir -p "${OUTER_STUBS}"
for stub in docker kubectl kind helm curl; do
    printf '#!/bin/bash\necho "OUTER STUB %s SHOULD NEVER RUN" >&2\nexit 0\n' "${stub}" \
        > "${OUTER_STUBS}/${stub}"
    chmod +x "${OUTER_STUBS}/${stub}"
done
PATH="${OUTER_STUBS}:${PATH}"
export PATH

if [ "$(command -v docker)" = "${OUTER_STUBS}/docker" ] &&
    [ "$(command -v curl)" = "${OUTER_STUBS}/curl" ]; then
    ok "the harness PATH does offer a docker and a curl to leak"
else
    nope "the outer stubs are not on the harness PATH, so the isolation test proves nothing" \
        "docker: $(command -v docker)" "curl:   $(command -v curl)"
fi

rm -rf "${SHIM_DIR}"
reset_fixtures

# ---------------------------------------------------------------------------
# The pins are the reviewed ones
# ---------------------------------------------------------------------------
#
# Sourcing lib.sh with an empty environment is the only way to see what a run
# that sets nothing gets. Without this, every digest assertion below could be
# satisfied by whatever the harness exported.

# shellcheck disable=SC2016
# The single quotes are the point: the expansion has to happen in the clean
# child, not here, or the assertion would read the harness's own export.
pinned="$(env -u KUEUE_SHA256 bash -c '. "$1"; printf "%s" "${KUEUE_SHA256}"' _ "${SCRIPT_DIR}/lib.sh")"
if [ "${pinned}" = "${PINNED_KUEUE_SHA256}" ]; then
    ok "an unset environment leaves the reviewed Kueue digest in place"
else
    nope "lib.sh defaults the Kueue digest to ${pinned}, want ${PINNED_KUEUE_SHA256}"
fi

# shellcheck disable=SC2016
pinned_version="$(env -u KUEUE_SHA256 bash -c '. "$1"; printf "%s" "${KUEUE_VERSION}"' _ "${SCRIPT_DIR}/lib.sh")"
if [ "${pinned_version}" = "v0.18.11" ]; then
    ok "the Kueue release is pinned to v0.18.11"
else
    nope "lib.sh pins Kueue to ${pinned_version}, want v0.18.11"
fi

# ---------------------------------------------------------------------------
# Argument validation: only a kubectl-nv-<lane> cluster is ever reached
# ---------------------------------------------------------------------------

run_subject "${UP}" one two
assert_rc 2 "up.sh with two arguments"

run_subject "${UP}" "bad lane"
assert_rc 2 "up.sh with a lane that is not a DNS label"
assert_not_logged "create cluster" "a rejected lane creates no cluster"

run_subject "${UP}" "UPPER"
assert_rc 2 "up.sh rejects an uppercase lane"

run_subject "${UP}" "-leading"
assert_rc 2 "up.sh rejects a lane starting with a hyphen"

# A lane carrying an embedded newline must be rejected on the WHOLE string. A
# line-oriented check accepts this, and the second line names another cluster,
# which is how a teardown reaches the wrong one.
run_subject "${UP}" "$(printf 'x\nbench-mokka')"
assert_rc 2 "up.sh rejects a lane with an embedded newline"
assert_not_logged "create cluster" "a newline lane creates no cluster"

run_subject "${DOWN}" "$(printf 'x\nbench-mokka')"
assert_rc 2 "down.sh rejects a lane with an embedded newline"
assert_not_logged "delete cluster" "a newline lane deletes no cluster"

run_subject "${DOWN}" one two
assert_rc 2 "down.sh with two arguments"

# ---------------------------------------------------------------------------
# Missing tool
# ---------------------------------------------------------------------------

for missing in kind helm kubectl docker curl; do
    rm -rf "${SHIM_DIR}"
    present=""
    for t in kind helm kubectl docker curl; do
        if [ "${t}" != "${missing}" ]; then
            present="${present} ${t}"
        fi
    done
    # shellcheck disable=SC2086
    make_shims "${SHIM_DIR}" ${present}
    run_subject "${UP}"
    assert_rc 3 "up.sh with ${missing} missing, despite a ${missing} on the harness PATH"
    assert_says "required tool not found: ${missing}" \
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
# Refusal paths
# ---------------------------------------------------------------------------

FAKE_KIND_CLUSTERS="$(printf 'bench-mokka\n%s\n' "${CLUSTER}")"
export FAKE_KIND_CLUSTERS
run_subject "${UP}"
assert_rc 4 "up.sh refuses when the cluster already exists"
assert_says "cluster ${CLUSTER} already exists" "up.sh says which cluster is in the way"
assert_says "hack/mokka/compute-domain/down.sh compute-domain" "up.sh points at its own down.sh"

FAKE_KIND_CLUSTERS="$(printf 'bench-mokka\n%s-old\n' "${CLUSTER}")"
export FAKE_KIND_CLUSTERS
run_subject "${UP}"
assert_rc 0 "up.sh does not mistake ${CLUSTER}-old for ${CLUSTER}"

FAKE_KIND_CLUSTERS="$(printf '%s\n' "${CLUSTER}")"
export FAKE_KIND_CLUSTERS
export FAKE_HELM_RELEASES="nvml-mock"
run_subject "${UP}"
assert_rc 4 "up.sh refuses when a Mokka release is installed in the cluster"
assert_says "holds Mokka release nvml-mock" "up.sh names the release in the way"

unset FAKE_KIND_CLUSTERS FAKE_HELM_RELEASES

# ---------------------------------------------------------------------------
# Happy path
# ---------------------------------------------------------------------------

run_subject "${UP}"
assert_rc 0 "up.sh succeeds when every check and assertion passes"

assert_logged "${PINNED_DIGEST}" "the pinned kind node image digest reaches kind"
assert_logged "--config ${SCRIPT_DIR}/kind-compute-domain.yaml" \
    "the four worker kind config reaches kind"
assert_logged "--name ${CLUSTER}" "the cluster name reaches kind"
assert_logged "oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock" \
    "the nvml-mock chart reference reaches helm"
assert_logged "--version 0.4.0" "the nvml-mock chart version 0.4.0 reaches helm"
assert_logged "--version v26.3.3" "the GPU Operator version v26.3.3 reaches helm"
assert_logged "--values ${SCRIPT_DIR}/mokka-values.yaml" \
    "the lane's nvml-mock values file reaches helm"
assert_logged "--values ${SCRIPT_DIR}/../gpu-operator-values.yaml" \
    "the shared GPU Operator values file reaches helm"
assert_logged "--values ${MOKKA_BIN_DIR}/topology-values-compute-domain.yaml" \
    "the rendered topology document reaches helm"

# Settings go in through files, never through --set: the switches interact and
# a command line spread of them is not reviewable.
assert_not_logged "--set " "no helm call carries a --set"

# ---------------------------------------------------------------------------
# The rendered topology document says what the kind config's labels say
# ---------------------------------------------------------------------------

RENDERED="${MOKKA_BIN_DIR}/topology-values-compute-domain.yaml"
if [ -f "${RENDERED}" ]; then
    ok "up.sh rendered the topology document"
    for worker in worker worker2; do
        if grep -qF "            - ${CLUSTER}-${worker}" "${RENDERED}"; then
            ok "the topology document lists ${CLUSTER}-${worker}"
        else
            nope "the topology document does not list ${CLUSTER}-${worker}" \
                "$(cat "${RENDERED}")"
        fi
    done
    # Clique 0 must hold worker and worker2 and nothing else. Checked by the
    # line the clique-1 heading sits on rather than by "the names appear
    # somewhere", which any ordering would satisfy.
    first_clique="$(awk '/- id: 0/{f=1;next} /- id: 1/{f=0} f && /^ *- /{print $2}' "${RENDERED}" | sort | tr '\n' ' ')"
    want_first="${CLUSTER}-worker ${CLUSTER}-worker2 "
    if [ "${first_clique}" = "${want_first}" ]; then
        ok "clique 0 holds exactly the first two workers"
    else
        nope "clique 0 holds '${first_clique}', want '${want_first}'"
    fi
    second_clique="$(awk '/- id: 1/{f=1;next} f && /^ *- /{print $2}' "${RENDERED}" | sort | tr '\n' ' ')"
    want_second="${CLUSTER}-worker3 ${CLUSTER}-worker4 "
    if [ "${second_clique}" = "${want_second}" ]; then
        ok "clique 1 holds exactly the last two workers"
    else
        nope "clique 1 holds '${second_clique}', want '${want_second}'"
    fi
    if grep -qF "${CD_UUID:-00000000-0000-0000-0000-0000000000ab}" "${RENDERED}"; then
        ok "the topology document carries the lane's ComputeDomain UUID"
    else
        nope "the topology document does not carry the lane's ComputeDomain UUID"
    fi
else
    nope "up.sh did not render a topology document at ${RENDERED}"
fi

# The UUID and the cliques in the kind config's static labels have to be the
# same ones, or GPU Feature Discovery and the kubelet would disagree.
KIND_CONFIG="${SCRIPT_DIR}/kind-compute-domain.yaml"
if [ "$(grep -cF 'nvidia.com/gpu.clique: "00000000-0000-0000-0000-0000000000ab.0"' "${KIND_CONFIG}")" -eq 2 ] &&
    [ "$(grep -cF 'nvidia.com/gpu.clique: "00000000-0000-0000-0000-0000000000ab.1"' "${KIND_CONFIG}")" -eq 2 ]; then
    ok "the kind config labels two workers into each clique, with the lane's UUID"
else
    nope "the kind config's static clique labels do not match the rendered topology document" \
        "$(grep -n 'gpu.clique' "${KIND_CONFIG}")"
fi

# ---------------------------------------------------------------------------
# Kueue is pinned, and the checksum is a gate rather than a note
# ---------------------------------------------------------------------------

run_subject "${UP}"
assert_rc 0 "up.sh succeeds with a payload matching the pin"
assert_logged "releases/download/v0.18.11/manifests.yaml" \
    "the pinned Kueue release reaches curl"
assert_logged "apply --server-side -f ${MOKKA_BIN_DIR}/kueue-v0.18.11-compute-domain.yaml" \
    "the checked manifest is the one kubectl applies"
assert_logged "apply -f ${SCRIPT_DIR}/kueue-topology.yaml" \
    "the lane's Topology and ResourceFlavor are applied"
assert_logged "create namespace kubectl-nv-e2e" \
    "the namespace the suite puts its jobs in is created"

export FAKE_CURL_BODY="a different manifest than the one that was reviewed"
run_subject "${UP}"
assert_rc 6 "up.sh refuses a Kueue manifest that does not match its digest"
assert_says "expected ${FAKE_BODY_SHA256}" "up.sh names the digest it expected"
assert_says "nothing was applied" "up.sh says nothing was applied"
assert_not_logged "apply --server-side" "a manifest that failed its digest is never applied"
assert_not_logged "apply -f ${SCRIPT_DIR}/kueue-topology.yaml" \
    "the topology objects are not applied after a digest failure"
export FAKE_CURL_BODY="${FAKE_BODY}"

# ---------------------------------------------------------------------------
# The fabric assertions discriminate
# ---------------------------------------------------------------------------

expect_timeout
export FAKE_CLIQUE_ALL="00000000-0000-0000-0000-000000000001.0"
run_subject "${UP}"
assert_rc 5 "up.sh fails when every worker reports the profile's placeholder clique"
assert_says "expected 00000000-0000-0000-0000-0000000000ab.1" \
    "up.sh names the clique the worker should have had"
unset FAKE_CLIQUE_ALL

# One worker in the wrong clique is the case a per-node assertion catches and
# a "some clique label is set" check does not.
export FAKE_CLIQUE_worker3="00000000-0000-0000-0000-0000000000ab.0"
run_subject "${UP}"
assert_rc 5 "up.sh fails when one worker is in the wrong clique"
assert_says "worker3: label nvidia.com/gpu.clique is '00000000-0000-0000-0000-0000000000ab.0'" \
    "up.sh names the worker that is in the wrong clique"
unset FAKE_CLIQUE_worker3

export FAKE_TOPOGRAPH_LABEL=""
run_subject "${UP}"
assert_rc 5 "up.sh fails when a Topograph label is absent"
assert_says "accelerator.topograph.run/domain is absent" \
    "up.sh names the Topograph label that is absent"
unset FAKE_TOPOGRAPH_LABEL

export FAKE_ALLOCATABLE=8
run_subject "${UP}"
assert_rc 5 "up.sh fails when a worker advertises the wrong GPU count"
assert_says "expected 4 for profile gb200" "up.sh names the count it expected"
unset FAKE_ALLOCATABLE

export FAKE_WORKERS="kubectl-nv-compute-domain-worker kubectl-nv-compute-domain-worker2"
run_subject "${UP}"
assert_rc 5 "up.sh fails when the cluster has fewer workers than the lane needs"
assert_says "has 2 worker(s), expected 4" "up.sh says how many workers it found"
unset FAKE_WORKERS

export FAKE_DS_NOTREADY_CALLS=1000
run_subject "${UP}"
assert_rc 5 "up.sh fails when the operand DaemonSets never roll out"
unset FAKE_DS_NOTREADY_CALLS

reset_fixtures

# ---------------------------------------------------------------------------
# The webhook retry converges rather than failing on the first rejection
# ---------------------------------------------------------------------------

export FAKE_KUEUE_APPLY_REJECTS=3
run_subject "${UP}"
assert_rc 0 "up.sh retries the Kueue objects past a webhook that is not ready yet"
unset FAKE_KUEUE_APPLY_REJECTS

# ---------------------------------------------------------------------------
# The namespace is created idempotently, and only that one failure is tolerated
# ---------------------------------------------------------------------------

export FAKE_NAMESPACE_EXISTS=1
run_subject "${UP}"
assert_rc 0 "up.sh tolerates a namespace that is already there"
assert_says "was already there" "up.sh says the namespace was already there"
unset FAKE_NAMESPACE_EXISTS

export FAKE_NAMESPACE_RC=1
run_subject "${UP}"
assert_rc 1 "up.sh fails when the namespace is refused for any other reason"
assert_says "could not create namespace kubectl-nv-e2e" \
    "up.sh names the namespace it could not create"
unset FAKE_NAMESPACE_RC

# ---------------------------------------------------------------------------
# down.sh deletes its own cluster and nothing else
# ---------------------------------------------------------------------------

reset_fixtures

run_subject "${DOWN}"
assert_rc 2 "down.sh with no cluster of that name"
assert_says "no such cluster: ${CLUSTER}" "down.sh names the cluster it looked for"
assert_not_logged "delete cluster" "down.sh deletes nothing when there is nothing to delete"

FAKE_KIND_CLUSTERS="$(printf 'bench-mokka\nkubectl-nv-mokka\n%s\n%s-old\n' "${CLUSTER}" "${CLUSTER}")"
export FAKE_KIND_CLUSTERS
run_subject "${DOWN}"
assert_rc 0 "down.sh deletes the lane cluster"
assert_logged "kind delete cluster --name ${CLUSTER} " "down.sh deletes its own cluster"
for other in bench-mokka kubectl-nv-mokka "${CLUSTER}-old"; do
    assert_not_logged "delete cluster --name ${other} " "down.sh leaves ${other} alone"
done

FAKE_KIND_CLUSTERS="$(printf '%s-old\n' "${CLUSTER}")"
export FAKE_KIND_CLUSTERS
run_subject "${DOWN}"
assert_rc 2 "down.sh does not mistake ${CLUSTER}-old for ${CLUSTER}"
unset FAKE_KIND_CLUSTERS

# ---------------------------------------------------------------------------
# Neither script touches the default kubeconfig
# ---------------------------------------------------------------------------

reset_fixtures
run_subject "${UP}"
if [ "$(sentinel_hash)" = "${SENTINEL_BEFORE}" ]; then
    ok "up.sh leaves the default kubeconfig byte-identical"
else
    nope "up.sh modified the default kubeconfig" \
        "before: ${SENTINEL_BEFORE}" "after:  $(sentinel_hash)"
fi

FAKE_KIND_CLUSTERS="$(printf '%s\n' "${CLUSTER}")"
export FAKE_KIND_CLUSTERS
run_subject "${DOWN}"
if [ "$(sentinel_hash)" = "${SENTINEL_BEFORE}" ]; then
    ok "down.sh leaves the default kubeconfig byte-identical"
else
    nope "down.sh modified the default kubeconfig" \
        "before: ${SENTINEL_BEFORE}" "after:  $(sentinel_hash)"
fi
unset FAKE_KIND_CLUSTERS

# ---------------------------------------------------------------------------
# Static analysis of the scripts under test
# ---------------------------------------------------------------------------

if command -v shellcheck > /dev/null 2>&1; then
    for dir in / "${SCRIPT_DIR}"; do
        if (cd "${dir}" && shellcheck -x \
            "${SCRIPT_DIR}/up.sh" "${SCRIPT_DIR}/down.sh" \
            "${SCRIPT_DIR}/lib.sh" "${SCRIPT_DIR}/up_test.sh"); then
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
