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
# Tests for hack/lint-words.sh.
#
# The forbidden byte sequences and words are built with printf escapes so that
# this file does not itself contain anything lint-words.sh rejects.
#
# Exit codes under test: 0 clean, 1 findings, 2 the check could not run.
#
# lint-words.sh has two modes. Inside a git work tree it scans tracked files
# only; anywhere else it walks the tree. Both are exercised below, because a
# fixture that covers only one leaves the other unguarded.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LINT="${SCRIPT_DIR}/lint-words.sh"

EM_DASH="$(printf '\xe2\x80\x94')"
EN_DASH="$(printf '\xe2\x80\x93')"
BANNED_GPU="$(printf 'M\x6fck GPU')"
BANNED_LIB="$(printf 'libc\x75da')"
BANNED_SIM="$(printf 'DSX S\x69m')"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/lint-words-test.XXXXXX")" || {
    echo "cannot create a temporary directory"
    exit 2
}
trap 'rm -rf "${WORK}"' EXIT

failures=0
cases=0

# run_case <name> <expected-rc> <fixture-root>
run_case() {
    local name="$1" expected="$2" root="$3"
    local log="${WORK}/$(echo "${name}" | tr ' /' '__').log"
    cases=$((cases + 1))

    bash "${LINT}" "${root}" > "${log}" 2>&1
    local rc=$?

    if [ "${rc}" -eq "${expected}" ]; then
        echo "PASS ${name} (rc=${rc})"
    else
        echo "FAIL ${name}: want rc=${expected}, got rc=${rc}"
        sed 's/^/    /' "${log}"
        failures=$((failures + 1))
    fi
}

# A plain directory, so lint-words.sh walks it. Every fixture carries a clean,
# in-scope file so that a tree is never empty.
new_tree() {
    local root
    root="$(mktemp -d "${WORK}/tree.XXXXXX")" || exit 2
    printf '# Title\n\nA clean line of prose.\n' > "${root}/clean.md"
    echo "${root}"
}

# A git work tree, so lint-words.sh scans tracked files only. The clean file is
# staged; each caller decides what to track beyond that.
new_repo() {
    local root
    root="$(mktemp -d "${WORK}/repo.XXXXXX")" || exit 2
    git -C "${root}" init -q > /dev/null 2>&1 || exit 2
    printf '# Title\n\nA clean line of prose.\n' > "${root}/clean.md"
    git -C "${root}" add clean.md > /dev/null 2>&1 || exit 2
    echo "${root}"
}

#
# Patterns. One fixture per alternative, so dropping any one of them is caught.
#

# 1. An em dash in a markdown file is rejected.
t1="$(new_tree)"
printf 'A sentence %s with a long dash.\n' "${EM_DASH}" > "${t1}/doc.md"
run_case "em dash in .md" 1 "${t1}"

# 2. An en dash in a Go file is rejected.
t2="$(new_tree)"
printf 'package main\n\n// Range 1 %s 9.\nfunc main() {}\n' "${EN_DASH}" > "${t2}/main.go"
run_case "en dash in .go" 1 "${t2}"

# 3. A banned device phrase in a YAML file is rejected, case-insensitively.
t3="$(new_tree)"
printf 'metadata:\n  description: "%s node"\n' "${BANNED_GPU}" > "${t3}/config.yaml"
run_case "banned device phrase in .yaml" 1 "${t3}"

# 4. The banned driver library name is rejected.
t4="$(new_tree)"
printf 'The plugin does not link against %s at all.\n' "${BANNED_LIB}" > "${t4}/doc.md"
run_case "banned library name in .md" 1 "${t4}"

# 5. The banned simulator name is rejected, case-insensitively.
t5="$(new_tree)"
printf 'Tested on the %s rig.\n' "${BANNED_SIM}" > "${t5}/doc.md"
run_case "banned simulator name in .md" 1 "${t5}"

#
# File types in scope.
#

# 6. Shell scripts are scanned.
t6="$(new_tree)"
printf '#!/bin/bash\n# step one %s step two\necho hello\n' "${EM_DASH}" > "${t6}/script.sh"
run_case "em dash in .sh" 1 "${t6}"

# 7. The .yml spelling is scanned as well as .yaml.
t7="$(new_tree)"
printf 'name: CI\n# a comment %s with a long dash\n' "${EM_DASH}" > "${t7}/workflow.yml"
run_case "em dash in .yml" 1 "${t7}"

# 8. A tree with nothing forbidden passes.
t8="$(new_tree)"
printf 'package main\n\nfunc main() {}\n' > "${t8}/main.go"
printf 'metadata:\n  name: kubectl-nv\n' > "${t8}/config.yaml"
printf '#!/bin/bash\necho hello\n' > "${t8}/script.sh"
run_case "clean tree" 0 "${t8}"

#
# Directories out of scope when walking a plain tree.
#

# 9. Dependencies are not ours to rewrite.
t9="$(new_tree)"
mkdir -p "${t9}/vendor/example.com/pkg"
printf 'A vendored sentence %s with a long dash.\n' "${EM_DASH}" \
    > "${t9}/vendor/example.com/pkg/README.md"
run_case "em dash only under vendor" 0 "${t9}"

# 10. Git's own store holds old blobs and commit messages.
t10="$(new_tree)"
mkdir -p "${t10}/.git/info"
printf 'A sentence %s from an old revision.\n' "${EM_DASH}" > "${t10}/.git/info/notes.md"
run_case "em dash only under .git" 0 "${t10}"

# 11. GoReleaser output is a build artifact.
t11="$(new_tree)"
mkdir -p "${t11}/dist/kubectl-nv_linux_amd64"
printf 'A built sentence %s with a long dash.\n' "${EM_DASH}" \
    > "${t11}/dist/kubectl-nv_linux_amd64/README.md"
run_case "em dash only under dist" 0 "${t11}"

# 12. A generated code graph is not source either.
t12="$(new_tree)"
mkdir -p "${t12}/graphify-out"
printf 'A generated sentence %s with a long dash.\n' "${EM_DASH}" \
    > "${t12}/graphify-out/GRAPH_REPORT.md"
run_case "em dash only under graphify-out" 0 "${t12}"

#
# Inside a git work tree only tracked files count.
#

# 13. An untracked file is somebody's scratch space, not what the repo ships.
r13="$(new_repo)"
printf 'A scratch sentence %s with a long dash.\n' "${EM_DASH}" > "${r13}/scratch.md"
run_case "untracked em dash in a git tree" 0 "${r13}"

# 14. The same file once it is staged is part of the repository.
r14="$(new_repo)"
printf 'A staged sentence %s with a long dash.\n' "${EM_DASH}" > "${r14}/staged.md"
git -C "${r14}" add staged.md > /dev/null 2>&1 || exit 2
run_case "tracked em dash in a git tree" 1 "${r14}"

# 15. Tracked dependencies stay out of scope in a git tree too.
r15="$(new_repo)"
mkdir -p "${r15}/vendor/example.com/pkg"
printf 'A vendored sentence %s with a long dash.\n' "${EM_DASH}" \
    > "${r15}/vendor/example.com/pkg/README.md"
git -C "${r15}" add vendor > /dev/null 2>&1 || exit 2
run_case "tracked em dash under vendor in a git tree" 0 "${r15}"

# 16. Tracked shell scripts are scanned in a git tree.
r16="$(new_repo)"
printf '#!/bin/bash\n# step one %s step two\necho hello\n' "${EM_DASH}" > "${r16}/script.sh"
git -C "${r16}" add script.sh > /dev/null 2>&1 || exit 2
run_case "tracked em dash in .sh in a git tree" 1 "${r16}"

# 17. So are tracked .yml files.
r17="$(new_repo)"
printf 'name: CI\n# a comment %s with a long dash\n' "${EM_DASH}" > "${r17}/workflow.yml"
git -C "${r17}" add workflow.yml > /dev/null 2>&1 || exit 2
run_case "tracked em dash in .yml in a git tree" 1 "${r17}"

# 18. A git tree with nothing forbidden passes.
r18="$(new_repo)"
printf 'package main\n\nfunc main() {}\n' > "${r18}/main.go"
git -C "${r18}" add main.go > /dev/null 2>&1 || exit 2
run_case "clean git tree" 0 "${r18}"

# 19. Matching stays case-insensitive in a git tree, not only when walking.
r19="$(new_repo)"
printf 'metadata:\n  description: "%s node"\n' "${BANNED_GPU}" > "${r19}/config.yaml"
git -C "${r19}" add config.yaml > /dev/null 2>&1 || exit 2
run_case "tracked banned device phrase in a git tree" 1 "${r19}"

#
# Failure to run is not success.
#

# 20. A root that is not there cannot be reported clean.
run_case "root does not exist" 2 "${WORK}/no-such-directory"

# 21. A file in the index but missing from disk makes grep fail. A checker that
# cannot read what it was told to read has not found the tree clean.
r21="$(new_repo)"
printf '# Notes\n\nNothing forbidden here.\n' > "${r21}/gone.md"
git -C "${r21}" add gone.md > /dev/null 2>&1 || exit 2
rm -f "${r21}/gone.md"
run_case "tracked file missing from disk" 2 "${r21}"

echo
if [ "${failures}" -ne 0 ]; then
    echo "${failures} of ${cases} cases failed"
    exit 1
fi
echo "all ${cases} cases passed"
