#!/usr/bin/env bash
# Copyright (C) 2026 Intel Corporation
# SPDX-License-Identifier: Apache-2.0
#
# Classify the output of a failed `go test -fuzz` leg for the sec-fuzz workflow.
#
# Usage:
#   .github/scripts/fuzz/classify-failure.sh <fuzz-target> <output-file>
#
# Prints one word on stdout:
#
#   deadline  the Go fuzzing coordinator surfaced its own shutdown at the
#             -fuzztime boundary and reported nothing else; the leg found no bug
#   failure   anything else — a crash, a build error, an infrastructure failure
#
# and a one-line explanation on stderr. Both verdicts exit 0 (a usage error exits
# 2), so the caller decides what a verdict means for the job.
#
# `deadline` is deliberately narrow, because a tolerance that swallows a real
# failure is worse than a red leg. It requires the run's ONLY failure to be the
# target's own FAIL block, and that block's only detail line to be the
# coordinator's message:
#
#   --- FAIL: FuzzSomething (30.12s)
#       context deadline exceeded
#
# A failing seed adds a `--- FAIL: FuzzSomething/<seed>` line, a crash adds detail
# lines and a "written to testdata" notice, and a build error produces no FAIL
# block at all — each of those stays a failure. test/fuzzgate exercises every one
# of these cases against this script; extend it before changing the rule.
#
# The caller must check for a new corpus file BEFORE asking this script, and treat
# one as a crash whatever the output says. When the fuzztime deadline lands while a
# crasher is being minimized, go test writes the reproducer to testdata/ but
# reports only the deadline, printing no "written to" notice — so the file on disk
# is the only evidence, and this script would call that output a clean shutdown.

set -euo pipefail

if [ "$#" -ne 2 ]; then
    echo "usage: $(basename "$0") <fuzz-target> <output-file>" >&2
    exit 2
fi
target=$1
output=$2

# The name is interpolated into a regex below, and a Go fuzz target is an
# identifier, so reject anything else rather than matching more than intended.
if ! printf '%s' "$target" | grep -qE '^Fuzz[A-Za-z0-9_]+$'; then
    echo "$(basename "$0"): not a fuzz target name: $target" >&2
    exit 2
fi

if [ ! -f "$output" ]; then
    echo "$(basename "$0"): no such output file: $output" >&2
    exit 2
fi

# A crash names itself wherever in the output it lands — including after the FAIL
# block, where a goroutine dump is not indented under it.
if grep -qE 'panic:|fatal error:|written to testdata' "$output"; then
    echo "output reports a crash" >&2
    echo failure
    exit 0
fi

# fails    every "--- FAIL:" line, target and subtest alike
# details  non-blank indented lines under THIS target's FAIL block
# deadline those detail lines that are exactly the coordinator's shutdown message
counts="$(awk -v target="$target" '
    /^--- FAIL: / {
        fails++
        inblock = ($0 ~ "^--- FAIL: " target " \\(")
        next
    }
    inblock && /^[[:space:]]/ {
        line = $0
        sub(/^[[:space:]]+/, "", line)
        sub(/[[:space:]]+$/, "", line)
        if (line != "") {
            details++
            if (line == "context deadline exceeded") { deadline++ }
        }
        next
    }
    { inblock = 0 }
    END { printf "%d %d %d\n", fails, details, deadline }
' "$output")"
read -r fails details deadline <<<"$counts"

if [ "$fails" -eq 1 ] && [ "$details" -eq 1 ] && [ "$deadline" -eq 1 ]; then
    echo "the only failure is $target's own coordinator shutdown at the fuzztime boundary" >&2
    echo deadline
    exit 0
fi

echo "failure detail is not a coordinator shutdown (FAIL lines=$fails, detail lines=$details, deadline lines=$deadline)" >&2
echo failure
