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
# Rejects typographic dashes and retired naming in the sources and docs.
#
# Usage: lint-words.sh [root]   (root defaults to the repository root)
#
# Only plain ASCII hyphens belong in this repository: the typographic dashes
# render badly in terminals and diffs. The word list covers names that must not
# appear in public text; the product is Mokka and its devices are simulated
# GPUs.
#
# The patterns are assembled from printf escapes so that this file does not
# match itself. Everything here is portable to the BSD grep on macOS, which has
# no -P.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${1:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

EM_DASH="$(printf '\xe2\x80\x94')"
EN_DASH="$(printf '\xe2\x80\x93')"
WORDS="$(printf 'm\x6fck gpu')|$(printf 'libc\x75da')|$(printf 'dsx s\x69m')"

PATTERN="${EM_DASH}|${EN_DASH}|${WORDS}"

hits="$(grep -r -n -I -i -E \
    --include='*.go' \
    --include='*.md' \
    --include='*.yaml' \
    --include='*.yml' \
    --include='*.sh' \
    --exclude-dir='vendor' \
    --exclude-dir='.git' \
    --exclude-dir='dist' \
    --exclude-dir='graphify-out' \
    -e "${PATTERN}" \
    "${ROOT}" 2>/dev/null)"

if [ -n "${hits}" ]; then
    echo "forbidden dash or word found:"
    echo "${hits}"
    echo
    echo "use an ASCII hyphen instead of a typographic dash;"
    echo "the product is Mokka and the devices are simulated GPUs."
    exit 1
fi

exit 0
