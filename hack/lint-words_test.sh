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

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LINT="${SCRIPT_DIR}/lint-words.sh"

EM_DASH="$(printf '\xe2\x80\x94')"
EN_DASH="$(printf '\xe2\x80\x93')"
BANNED_WORD="$(printf 'M\x6fck GPU')"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/lint-words-test.XXXXXX")" || { echo "cannot create a temporary directory"; exit 2; }
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

# Every fixture carries a clean, in-scope file so that a tree is never empty.
new_tree() {
    local root
    root="$(mktemp -d "${WORK}/tree.XXXXXX")" || exit 2
    printf '# Title\n\nA clean line of prose.\n' > "${root}/clean.md"
    echo "${root}"
}

# 1. An em dash in a markdown file is rejected.
t1="$(new_tree)"
printf 'A sentence %s with a long dash.\n' "${EM_DASH}" > "${t1}/doc.md"
run_case "em dash in .md" 1 "${t1}"

# 2. An en dash in a Go file is rejected.
t2="$(new_tree)"
printf 'package main\n\n// Range 1 %s 9.\nfunc main() {}\n' "${EN_DASH}" > "${t2}/main.go"
run_case "en dash in .go" 1 "${t2}"

# 3. A banned product phrase in a YAML file is rejected, case-insensitively.
t3="$(new_tree)"
printf 'metadata:\n  description: "%s node"\n' "${BANNED_WORD}" > "${t3}/config.yaml"
run_case "banned phrase in .yaml" 1 "${t3}"

# 4. A tree with nothing forbidden passes.
t4="$(new_tree)"
printf 'package main\n\nfunc main() {}\n' > "${t4}/main.go"
printf 'metadata:\n  name: kubectl-nv\n' > "${t4}/config.yaml"
printf '#!/bin/bash\necho hello\n' > "${t4}/script.sh"
run_case "clean tree" 0 "${t4}"

# 5. A forbidden byte under vendor/ is out of scope and does not fail the tree.
t5="$(new_tree)"
mkdir -p "${t5}/vendor/example.com/pkg"
printf 'A vendored sentence %s with a long dash.\n' "${EM_DASH}" \
    > "${t5}/vendor/example.com/pkg/README.md"
run_case "em dash only under vendor" 0 "${t5}"

echo
if [ "${failures}" -ne 0 ]; then
    echo "${failures} of ${cases} cases failed"
    exit 1
fi
echo "all ${cases} cases passed"
