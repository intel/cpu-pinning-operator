#!/usr/bin/env bash
# Copyright (C) 2026 Intel Corporation
# SPDX-License-Identifier: Apache-2.0
#
# Generate NOTICE file with third-party attributions.
#
# This script creates a consolidated NOTICE file that includes attribution
# notices for all third-party dependencies as required by their licenses. The
# license and NOTICE files come from ./hack/list-third-party-licenses.sh files, so
# NOTICE covers the same files the container image ships under /licenses/.
#
# Usage:
#   ./hack/generate-notice.sh > NOTICE
#
# Requirements:
#   - Bash 3.0+
#   - Go toolchain (go list)

set -euo pipefail

FILES=$(bash "$(dirname "$0")/list-third-party-licenses.sh" files)
GO_LICENSE="$(go env GOROOT)/LICENSE"

cat << 'EOF'
cpu-pinning-operator
Copyright (C) 2026 Intel Corporation

This product includes software developed by Intel Corporation.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

===============================================================================

This product includes third-party software governed by separate license terms.
The Go modules linked into the operator binary, and their licenses, are listed
in docs/third-party-licenses.md.

The container image carries this file and LICENSE under /licenses/, and under
/licenses/third_party/ every license file, PATENTS file and upstream NOTICE file
of every Go module linked into the operator binary, one directory per module
path, plus the Go standard library's license at /licenses/third_party/std/LICENSE.
The Helm chart carries this file and LICENSE at its root.

The following open source licenses apply to third-party components:

-------------------------------------------------------------------------------
Apache License 2.0 Dependencies
-------------------------------------------------------------------------------

The following dependencies are licensed under the Apache License 2.0:

EOF
awk -F '\t' '$3 == "main" && $4 == "Apache-2.0" { print "  - " $1 " (" $2 ")" }' <<<"$FILES"
echo ""

# section prints a titled block holding one file's text.
section() {
    echo "-------------------------------------------------------------------------------"
    echo "$1"
    echo "-------------------------------------------------------------------------------"
    echo ""
    cat "$2"
    echo ""
}

echo "The following dependencies include upstream NOTICE attributions:"
echo ""
while IFS=$'\t' read -r MODULE_PATH VERSION ROLE TYPE FILE REL; do
    if [[ "$ROLE" == "notice" ]]; then
        section "$MODULE_PATH ($VERSION) - $REL" "$FILE"
    fi
done <<<"$FILES"

# Full text of every license file that is not the Apache License 2.0 alone
# (the Apache text is the repository's LICENSE).
section "Go standard library" "$GO_LICENSE"
while IFS=$'\t' read -r MODULE_PATH VERSION ROLE TYPE FILE REL; do
    if [[ "$ROLE" == "notice" || "$TYPE" == "Apache-2.0" ]]; then
        continue
    fi
    if [[ "$ROLE" == "main" ]]; then
        section "$MODULE_PATH ($VERSION)" "$FILE"
    else
        section "$MODULE_PATH ($VERSION) - $REL" "$FILE"
    fi
done <<<"$FILES"

cat << 'EOF'

-------------------------------------------------------------------------------
Components distributed with the operator that are not Go modules
-------------------------------------------------------------------------------

The packaged Helm chart includes these upstream chart archives, unmodified,
under charts/. Both are licensed under the Apache License 2.0:

  - cert-manager v1.20.3 (cert-manager-v1.20.3.tgz)
    https://charts.jetstack.io/charts/cert-manager-v1.20.3.tgz
  - nri-resource-policy-balloons v0.15.2
    (nri-resource-policy-balloons-helm-chart-v0.15.2.tgz)
    https://github.com/containers/nri-plugins/releases/download/v0.15.2/nri-resource-policy-balloons-helm-chart-v0.15.2.tgz

The container image is built on gcr.io/distroless/static:nonroot, which
contains these Debian 13 packages under their own licenses:

  - base-files: GPL-2.0-or-later
  - ca-certificates: GPL-2.0-or-later, GPL-2.0-only, MPL-2.0
  - netbase: GPL-2.0-only
  - tzdata, tzdata-legacy: public domain
  - media-types: ad-hoc license

Each package's copyright and license text is in the image at
/usr/share/doc/<package>/copyright, and its exact version is recorded at
/var/lib/dpkg/status.d/<package>. The corresponding source code for that version
is available from the Debian archive at https://snapshot.debian.org/ (searchable
at https://sources.debian.org/).

===============================================================================

This NOTICE file includes:
- List of the Apache-2.0 dependencies (full Apache-2.0 license text available
  in the repository's LICENSE file)
- Upstream NOTICE files of the linked dependencies
- Full license texts of the Go standard library and of every other license file
  of the linked dependencies (MIT, BSD, ISC, and the non-Apache parts of
  dependencies under more than one license)
- The upstream Helm chart archives and base image packages shipped with the
  operator, and their licenses

To regenerate this file:
    ./hack/generate-notice.sh > NOTICE

To update the dependency inventory:
    ./hack/list-third-party-licenses.sh markdown > docs/third-party-licenses.md
EOF
