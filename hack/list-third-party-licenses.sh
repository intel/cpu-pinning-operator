#!/usr/bin/env bash
# Copyright (C) 2026 Intel Corporation
# SPDX-License-Identifier: Apache-2.0
#
# List third-party licenses for dependencies linked in the operator binary.
#
# This script enumerates the Go modules linked into the operator binary (./cmd/...),
# finds every license file of each module (all LICENSE*/COPYING* files at the module
# root, plus any license file in a linked package's own directory, such as a forked
# third_party package), identifies the license of each, and prints a consolidated list.
# A module whose files carry several licenses is listed with all of them
# ("Apache-2.0 AND MIT").
#
# Scope: modules linked into the operator binary only. Test-only and build-only
# dependencies declared in go.mod are not linked into the distributed artifact and
# are not listed.
#
# Usage:
#   ./hack/list-third-party-licenses.sh [text|json|markdown]
#   ./hack/list-third-party-licenses.sh copy <dest-dir>
#   ./hack/list-third-party-licenses.sh files
#
# Output format defaults to text if not specified.
#
# copy mode prints the text listing and also copies each module's license files,
# PATENTS file and upstream NOTICE file to <dest-dir>/<module path>/ (a package-level
# license file keeps its path inside the module), and the Go standard library's
# license to <dest-dir>/std/LICENSE. The Dockerfile uses it to ship the texts in the
# image. A module with no recognized license file fails the run, as in every other mode.
#
# files mode prints one tab-separated line per license or NOTICE file:
#   <module path> <version> <role> <license> <absolute file> <file relative to module>
# where role is main (the module's license file), extra (another license file of the
# module) or notice. hack/generate-notice.sh builds NOTICE from it.
#
# Requirements:
#   - Bash 3.0+
#   - Go toolchain (go list)
#   - Local Go module cache populated (go mod download or successful go build)

set -euo pipefail

# The image's build target: build constraints decide which modules link.
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64

OUTPUT_FORMAT="${1:-text}"

if [[ "$OUTPUT_FORMAT" == "copy" ]]; then
    COPY_DEST="${2:?copy mode needs a destination directory}"
    mkdir -p "$COPY_DEST/std"
    cp "$(go env GOROOT)/LICENSE" "$COPY_DEST/std/LICENSE"
fi

# spdx_and joins the license ids on stdin, each once, into one SPDX AND expression.
spdx_and() {
    tr ' ' '\n' | grep -vxE 'AND|' | sort -u | paste -sd ' ' - | sed 's/ / AND /g'
}

# license_type prints the SPDX expression for the licenses a file's text carries.
license_type() {
    local content found=()
    content=$(cat "$1")
    if grep -qiE 'Apache License' <<<"$content" && \
       grep -qiE '(Version 2\.0|2\.0.*January 2004)' <<<"$content"; then
        found+=("Apache-2.0")
    fi
    if grep -qiE 'Redistribution and use in source and binary forms' <<<"$content"; then
        if grep -qiE '(Neither the name|endorse or promote)' <<<"$content"; then
            found+=("BSD-3-Clause")
        else
            found+=("BSD-2-Clause")
        fi
    fi
    if grep -qiE 'ISC License|Permission to use, copy, modify, and(/or)? distribute this software for any purpose' <<<"$content"; then
        found+=("ISC")
    fi
    if grep -qiE '(MIT License|Permission is hereby granted, free of charge)' <<<"$content"; then
        found+=("MIT")
    fi
    if grep -qiE 'Mozilla Public License' <<<"$content"; then
        found+=("MPL-2.0")
    fi
    if grep -qiE 'GNU (AFFERO |LESSER )?GENERAL PUBLIC LICENSE' <<<"$content"; then
        found+=("GPL-family")
    fi
    if [[ ${#found[@]} -eq 0 ]]; then
        echo "Unknown"
    else
        printf '%s\n' "${found[@]}" | spdx_and
    fi
}

# legal_kind prints license, notice or patents for a license, NOTICE or PATENTS file
# name, and nothing for any other name (such as the Go source license.go).
legal_kind() {
    grep -qE '\.go$' <<<"$1" && return 0
    if grep -qiE '^(LICEN[CS]E|COPYING)([.-][A-Za-z0-9]+)?$' <<<"$1"; then
        echo license
    elif grep -qiE '^NOTICE([.-][A-Za-z0-9]+)?$' <<<"$1"; then
        echo notice
    elif grep -qiE '^PATENTS([.-][A-Za-z0-9]+)?$' <<<"$1"; then
        echo patents
    fi
}

# Every package linked into the operator binary, with its module (by replacement if
# replaced) and both directories, tab-separated (a directory may contain spaces). The
# operator's own module is skipped.
if ! PACKAGES=$(go list -deps -f '{{with .Module}}{{if not .Main}}{{if .Replace}}{{.Replace.Path}}@{{.Replace.Version}}{{"\t"}}{{.Replace.Dir}}{{else}}{{.Path}}@{{.Version}}{{"\t"}}{{.Dir}}{{end}}{{"\t"}}{{$.Dir}}{{end}}{{end}}' ./cmd/...); then
    echo "Error: Failed to enumerate dependencies. Ensure 'go list' succeeds and the module cache is populated." >&2
    exit 1
fi
MODULES=$(awk -F '\t' 'NF { print $1 "\t" $2 }' <<<"$PACKAGES" | sort -u)

if [[ -z "$MODULES" ]]; then
    echo "Error: No dependencies found (empty result from go list)." >&2
    exit 1
fi

# Output header
if [[ "$OUTPUT_FORMAT" == "markdown" ]]; then
    cat << 'HEADER'
<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Third-party licenses

This inventory covers the Go modules linked into the operator binary for its
linux/amd64 image target (`go list -deps ./cmd/...`). The Go standard library, also
linked, is BSD-3-Clause and is not in the table. Modules that `go.mod` declares only
for tests or builds are not linked into the binary and are not listed. The upstream
Helm chart archives that the chart packages, and the packages of the image's base,
are listed in `NOTICE`.

Regenerate with `./hack/list-third-party-licenses.sh markdown > docs/third-party-licenses.md`.

HEADER
    echo "| Module | Version | License | License File |"
    echo "|--------|---------|---------|--------------|"
elif [[ "$OUTPUT_FORMAT" == "json" ]]; then
    echo "{"
    echo '  "generated": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'",'
    echo '  "modules": ['
    FIRST=true
fi

# Track modules with unresolved licenses
declare -a UNRESOLVED_MODULES=()

while IFS=$'\t' read -r MODULE_VERSION MODULE_DIR; do
    MODULE_PATH=${MODULE_VERSION%@*}
    VERSION=${MODULE_VERSION##*@}

    if [[ -z "$MODULE_DIR" || ! -d "$MODULE_DIR" ]]; then
        UNRESOLVED_MODULES+=("$MODULE_VERSION (module directory not found)")
        continue
    fi

    # The module's license file, then its other license files at the root and in the
    # directories of its linked packages, as paths relative to the module directory.
    MAIN=""
    for NAME in LICENSE LICENSE.txt LICENSE.md license.md COPYING COPYING.txt COPYING.md; do
        if [[ -f "$MODULE_DIR/$NAME" ]]; then
            MAIN="$NAME"
            break
        fi
    done
    if [[ -z "$MAIN" ]]; then
        UNRESOLVED_MODULES+=("$MODULE_VERSION (no LICENSE file)")
        continue
    fi
    # Every license, NOTICE and PATENTS file at the module root (the empty first line)
    # and in each linked package's own directory.
    EXTRAS=()
    NOTICES=()
    PATENTS=()
    PKG_DIRS=$(awk -F '\t' -v m="$MODULE_VERSION" -v d="$MODULE_DIR" '$1 == m && $3 != d { print substr($3, length(d) + 2) }' <<<"$PACKAGES" | sort -u)
    while IFS= read -r REL_DIR; do
        for FILE in "$MODULE_DIR/${REL_DIR:+$REL_DIR/}"*; do
            NAME=${FILE##*/}
            [[ -f "$FILE" ]] || continue
            REL="${REL_DIR:+$REL_DIR/}$NAME"
            case "$(legal_kind "$NAME")" in
                license) [[ "$REL" == "$MAIN" ]] || EXTRAS+=("$REL") ;;
                notice) NOTICES+=("$REL") ;;
                patents) PATENTS+=("$REL") ;;
            esac
        done
    done <<<"$(printf '\n%s' "$PKG_DIRS")"

    TYPES=()
    for REL in "$MAIN" ${EXTRAS[@]+"${EXTRAS[@]}"}; do
        TYPE=$(license_type "$MODULE_DIR/$REL")
        if [[ "$TYPE" == "Unknown" ]]; then
            UNRESOLVED_MODULES+=("$MODULE_VERSION ($REL found but its license type is not recognized)")
        fi
        TYPES+=("$TYPE")
        if [[ "$OUTPUT_FORMAT" == "files" ]]; then
            ROLE=extra
            [[ "$REL" == "$MAIN" ]] && ROLE=main
            printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$MODULE_PATH" "$VERSION" "$ROLE" "$TYPE" "$MODULE_DIR/$REL" "$REL"
        fi
    done
    if [[ "$OUTPUT_FORMAT" == "files" ]]; then
        for REL in ${NOTICES[@]+"${NOTICES[@]}"}; do
            printf '%s\t%s\tnotice\t-\t%s\t%s\n' "$MODULE_PATH" "$VERSION" "$MODULE_DIR/$REL" "$REL"
        done
    fi
    # Every license of the module, each once, as one SPDX expression.
    LICENSE_TYPE=$(printf '%s\n' "${TYPES[@]}" | spdx_and)
    LICENSE_FILE=$(printf '%s\n' "$MAIN" ${EXTRAS[@]+"${EXTRAS[@]}"} | paste -sd ',' - | sed 's/,/, /g')

    if [[ "$OUTPUT_FORMAT" == "copy" ]]; then
        for REL in "$MAIN" ${EXTRAS[@]+"${EXTRAS[@]}"} ${NOTICES[@]+"${NOTICES[@]}"} ${PATENTS[@]+"${PATENTS[@]}"}; do
            mkdir -p "$(dirname "$COPY_DEST/$MODULE_PATH/$REL")"
            cp "$MODULE_DIR/$REL" "$COPY_DEST/$MODULE_PATH/$REL"
        done
    fi

    # Output in requested format
    if [[ "$OUTPUT_FORMAT" == "markdown" ]]; then
        echo "| $MODULE_PATH | $VERSION | $LICENSE_TYPE | $LICENSE_FILE |"
    elif [[ "$OUTPUT_FORMAT" == "json" ]]; then
        [[ "$FIRST" == "false" ]] && echo "    ,"
        echo "    {"
        echo "      \"path\": \"$MODULE_PATH\","
        echo "      \"version\": \"$VERSION\","
        echo "      \"license\": \"$LICENSE_TYPE\","
        echo "      \"license_file\": \"$LICENSE_FILE\""
        echo -n "    }"
        FIRST=false
    elif [[ "$OUTPUT_FORMAT" != "files" ]]; then
        echo "$MODULE_PATH@$VERSION: $LICENSE_TYPE ($LICENSE_FILE)"
    fi

done <<< "$MODULES"

# Output footer
if [[ "$OUTPUT_FORMAT" == "json" ]]; then
    echo ""
    echo "  ]"
    echo "}"
fi

# Report unresolved modules to stderr
if [[ ${#UNRESOLVED_MODULES[@]} -gt 0 ]]; then
    echo "" >&2
    echo "Error: ${#UNRESOLVED_MODULES[@]} module(s) with unresolved licenses:" >&2
    for UNRESOLVED in "${UNRESOLVED_MODULES[@]}"; do
        echo "  - $UNRESOLVED" >&2
    done
    exit 1
fi
