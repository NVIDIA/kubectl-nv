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
# Renders the agent evaluation results into the README.
#
# Reads every eval/results/<date>.json and replaces whatever sits between the
# two markers in the README with a table of one row per recorded run. The
# markers are replaced rather than appended to, so running this twice gives
# the same file.
#
# Usage: hack/eval/render-results.sh [--results <dir>] [--readme <file>]
#
# Exit codes:
#   0  the README was rendered
#   2  the command line was wrong, or the README has no markers
#   3  a required tool is missing, or a results file is unreadable

set -uo pipefail

PROG="$(basename "${BASH_SOURCE[0]}")"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

RESULTS_DIR="${REPO_ROOT}/eval/results"
README="${REPO_ROOT}/README.md"

START_MARKER='<!-- eval-results:start -->'
END_MARKER='<!-- eval-results:end -->'

die() {
    local code="$1"
    shift
    printf '%s: %s\n' "${PROG}" "$*" >&2
    exit "${code}"
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --results)
            [ "$#" -ge 2 ] || die 2 "--results needs a directory"
            RESULTS_DIR="$2"
            shift 2
            ;;
        --readme)
            [ "$#" -ge 2 ] || die 2 "--readme needs a file"
            README="$2"
            shift 2
            ;;
        -h | --help)
            sed -n '17,29p' "${BASH_SOURCE[0]}"
            exit 0
            ;;
        *)
            die 2 "unknown argument: $1"
            ;;
    esac
done

command -v jq > /dev/null 2>&1 || die 3 "required tool not found: jq"
[ -r "${README}" ] || die 2 "no README at ${README}"

grep -qF -- "${START_MARKER}" "${README}" || die 2 "${README} has no ${START_MARKER}"
grep -qF -- "${END_MARKER}" "${README}" || die 2 "${README} has no ${END_MARKER}"

# ---------------------------------------------------------------------------
# The table
# ---------------------------------------------------------------------------

BODY="$(mktemp "${TMPDIR:-/tmp}/kubectl-nv-eval-render.XXXXXX")" || die 3 "could not make a temporary file"
trap 'rm -f "${BODY}" "${BODY}.out"' EXIT

files=()
if [ -d "${RESULTS_DIR}" ]; then
    while IFS= read -r f; do
        [ -n "${f}" ] && files+=("${f}")
    done < <(find "${RESULTS_DIR}" -type f -name '*.json' 2> /dev/null | sort)
fi

if [ "${#files[@]}" -eq 0 ]; then
    {
        printf '%s\n' 'No evaluation run has been recorded yet. The table appears here once'
        printf '%s\n' 'a run is recorded under eval/results; see [eval/README.md](eval/README.md).'
    } > "${BODY}"
else
    {
        printf '%s\n' '| Date | Agent | Cases | Passed | Codes missed |'
        printf '%s\n' '|---|---|---|---|---|'
        for f in "${files[@]}"; do
            row="$(jq -r '
                [ (((.startedAt // "") | split("T"))[0]),
                  (.agent // "unknown"),
                  (.summary.cases | tostring),
                  (.summary.passed | tostring),
                  ([.cases[] | select(.pass | not) | .code] | if length == 0 then "none" else join(", ") end)
                ] | "| " + join(" | ") + " |"' < "${f}" 2> /dev/null)"
            [ -n "${row}" ] || die 3 "could not read ${f}"
            printf '%s\n' "${row}"
        done
    } > "${BODY}"
fi

# ---------------------------------------------------------------------------
# Splice it in
# ---------------------------------------------------------------------------
#
# awk rather than sed: the body is arbitrary Markdown and sed would read a
# backslash or an ampersand in it as a replacement directive.
awk -v start="${START_MARKER}" -v end="${END_MARKER}" -v body="${BODY}" '
    $0 == start {
        print
        print ""
        while ((getline line < body) > 0) {
            print line
        }
        close(body)
        print ""
        skipping = 1
        next
    }
    $0 == end {
        skipping = 0
        print
        next
    }
    !skipping { print }
' "${README}" > "${BODY}.out" || die 3 "could not render ${README}"

cat "${BODY}.out" > "${README}" || die 3 "could not write ${README}"

printf '%s: rendered %d result file(s) into %s\n' "${PROG}" "${#files[@]}" "${README}" >&2
