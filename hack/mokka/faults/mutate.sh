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
# Runs the mutants in mutants.txt against faults_test.sh.
#
# A mutant is KILLED when the harness fails against it and SURVIVED when it
# does not. A survivor is a hole: it is a change to a script that writes to a
# live cluster that no test notices.
#
# Every mutant is applied to a COPY of this directory, never to the tree. The
# copy is what the harness runs against, because faults_test.sh resolves its
# subjects relative to its own path.
#
# Two things are checked before a result is believed, and both have bitten
# this kind of script before:
#
#   - that the mutation APPLIED, by diffing the copy against the original. A
#     search string that no longer matches produces a mutant identical to the
#     subject, which then "survives" and reports a hole that is not there.
#   - that it changed ONLY what it meant to, by counting the changed lines. A
#     greedy replacement that rewrites half a file tells you nothing about the
#     line you were asking about.
#
# Usage: hack/mokka/faults/mutate.sh [id]...
# Exit: 0 every mutant was killed, 1 at least one survived or misapplied.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MUTANTS="${SCRIPT_DIR}/mutants.txt"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mokka-faults-mutate.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

WANTED=("$@")

# How long one mutant's harness run may take before it is stopped. A healthy
# run is about a minute.
MUTATE_TIMEOUT="${MUTATE_TIMEOUT:-240}"

KILLED=0
SURVIVED=0
BROKEN=0

printf '%-6s %-10s %-20s %s\n' "ID" "RESULT" "FILE" "DETAIL"

while IFS=$'\t' read -r id file search replace; do
    case "${id}" in '' | '#'*) continue ;; esac

    if [ "${#WANTED[@]}" -gt 0 ]; then
        hit=false
        for w in "${WANTED[@]}"; do
            [ "${w}" = "${id}" ] && hit=true
        done
        [ "${hit}" = true ] || continue
    fi

    copy="${WORK}/${id}"
    rm -rf "${copy}"
    mkdir -p "${copy}"
    # The trailing /. copies the CONTENTS; without it cp nests the directory
    # inside the destination and the harness finds no subjects at all.
    command cp -R "${SCRIPT_DIR}/." "${copy}/"

    if ! MUT_FILE="${copy}/${file}" MUT_SEARCH="${search}" MUT_REPLACE="${replace}" \
        python3 -I -c '
import os, sys
path = os.environ["MUT_FILE"]
search = os.environ["MUT_SEARCH"].replace("\\n", "\n")
replace = os.environ["MUT_REPLACE"].replace("\\n", "\n")
text = open(path).read()
if search not in text:
    sys.exit("search string not found")
if text.count(search) != 1:
    sys.exit("search string matches %d times, want 1" % text.count(search))
open(path, "w").write(text.replace(search, replace, 1))
' 2> "${copy}/.apply-error"; then
        printf '%-6s %-10s %-20s %s\n' "${id}" "BROKEN" "${file}" \
            "$(cat "${copy}/.apply-error")"
        BROKEN=$((BROKEN + 1))
        continue
    fi

    changed="$(diff "${SCRIPT_DIR}/${file}" "${copy}/${file}" | grep -c '^[<>]')"
    if [ "${changed}" -eq 0 ]; then
        printf '%-6s %-10s %-20s %s\n' "${id}" "BROKEN" "${file}" "the mutation changed nothing"
        BROKEN=$((BROKEN + 1))
        continue
    fi
    if [ "${changed}" -gt 12 ]; then
        printf '%-6s %-10s %-20s %s\n' "${id}" "BROKEN" "${file}" \
            "the mutation changed ${changed} lines, which is too wide to be about one thing"
        BROKEN=$((BROKEN + 1))
        continue
    fi

    # Bounded, because some of these mutants are about the bounds. M21 and M22
    # replace a deadline check with `if false`, and a wait with no deadline
    # does not fail the harness, it hangs it. A run that has to be stopped did
    # not pass, which is what KILLED means, and the reason says which it was.
    "${copy}/faults_test.sh" > "${copy}/.out" 2>&1 &
    harness=$!
    elapsed=0
    timed_out=false
    while kill -0 "${harness}" 2> /dev/null; do
        if [ "${elapsed}" -ge "${MUTATE_TIMEOUT}" ]; then
            kill -TERM "${harness}" 2> /dev/null
            sleep 1
            kill -KILL "${harness}" 2> /dev/null
            timed_out=true
            break
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    wait "${harness}"
    rc=$?

    detail="$(grep -E '^passed:' "${copy}/.out" | tail -1)"
    if [ "${timed_out}" = true ]; then
        printf '%-6s %-10s %-20s %s\n' "${id}" "KILLED" "${file}" \
            "the harness hung and was stopped after ${MUTATE_TIMEOUT}s"
        KILLED=$((KILLED + 1))
    elif [ "${rc}" -eq 0 ]; then
        printf '%-6s %-10s %-20s %s\n' "${id}" "SURVIVED" "${file}" "${detail}"
        SURVIVED=$((SURVIVED + 1))
    else
        printf '%-6s %-10s %-20s %s\n' "${id}" "KILLED" "${file}" "${detail}"
        KILLED=$((KILLED + 1))
    fi
done < "${MUTANTS}"

echo
echo "killed: ${KILLED}  survived: ${SURVIVED}  broken: ${BROKEN}"
[ "${SURVIVED}" -eq 0 ] && [ "${BROKEN}" -eq 0 ]
