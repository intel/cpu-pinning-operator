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

| Module | Version | License | License File |
|--------|---------|---------|--------------|
| cel.dev/expr | v0.25.2 | Apache-2.0 | LICENSE |
| github.com/antlr4-go/antlr/v4 | v4.13.1 | BSD-3-Clause | LICENSE |
| github.com/beorn7/perks | v1.0.1 | MIT | LICENSE |
| github.com/blang/semver/v4 | v4.0.0 | MIT | LICENSE |
| github.com/cenkalti/backoff/v5 | v5.0.3 | MIT | LICENSE |
| github.com/cespare/xxhash/v2 | v2.3.0 | MIT | LICENSE.txt |
| github.com/containerd/log | v0.1.0 | Apache-2.0 | LICENSE |
| github.com/containerd/nri | v0.12.2 | Apache-2.0 | LICENSE |
| github.com/containerd/ttrpc | v1.2.9 | Apache-2.0 | LICENSE |
| github.com/containers/nri-plugins/pkg/topology | v0.0.0-20261106164433-cbad332146ea | Apache-2.0 | LICENSE |
| github.com/containers/nri-plugins | v0.15.1 | Apache-2.0 | LICENSE |
| github.com/davecgh/go-spew | v1.1.2-0.20180830191138-d8f796af33cc | ISC | LICENSE |
| github.com/emicklei/go-restful/v3 | v3.13.0 | MIT | LICENSE |
| github.com/evanphx/json-patch/v5 | v5.9.11 | BSD-3-Clause | LICENSE |
| github.com/felixge/httpsnoop | v1.0.4 | MIT | LICENSE.txt |
| github.com/fsnotify/fsnotify | v1.10.1 | BSD-3-Clause | LICENSE |
| github.com/fxamacker/cbor/v2 | v2.9.1 | MIT | LICENSE |
| github.com/go-logr/logr | v1.4.4 | Apache-2.0 | LICENSE |
| github.com/go-logr/stdr | v1.2.2 | Apache-2.0 | LICENSE |
| github.com/go-logr/zapr | v1.3.0 | Apache-2.0 | LICENSE |
| github.com/go-openapi/jsonpointer | v1.0.0 | Apache-2.0 | LICENSE |
| github.com/go-openapi/jsonreference | v1.0.0 | Apache-2.0 | LICENSE |
| github.com/go-openapi/swag/cmdutils | v0.27.1 | Apache-2.0 | LICENSE |
| github.com/go-openapi/swag/conv | v0.27.1 | Apache-2.0 | LICENSE |
| github.com/go-openapi/swag/fileutils | v0.27.1 | Apache-2.0 | LICENSE |
| github.com/go-openapi/swag/jsonutils | v0.27.1 | Apache-2.0 | LICENSE |
| github.com/go-openapi/swag/loading | v0.27.1 | Apache-2.0 | LICENSE |
| github.com/go-openapi/swag/mangling | v0.27.1 | Apache-2.0 | LICENSE |
| github.com/go-openapi/swag/netutils | v0.27.1 | Apache-2.0 | LICENSE |
| github.com/go-openapi/swag/pools | v0.27.1 | Apache-2.0 | LICENSE |
| github.com/go-openapi/swag/stringutils | v0.27.1 | Apache-2.0 | LICENSE |
| github.com/go-openapi/swag/typeutils | v0.27.1 | Apache-2.0 | LICENSE |
| github.com/go-openapi/swag/yamlutils | v0.27.1 | Apache-2.0 | LICENSE |
| github.com/go-openapi/swag | v0.27.1 | Apache-2.0 | LICENSE |
| github.com/google/cel-go | v0.30.0 | Apache-2.0 AND BSD-3-Clause | LICENSE |
| github.com/google/gnostic-models | v0.7.0 | Apache-2.0 | LICENSE |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause | LICENSE |
| github.com/grpc-ecosystem/grpc-gateway/v2 | v2.29.0 | BSD-3-Clause | LICENSE |
| github.com/intel/goresctrl | v0.14.0 | Apache-2.0 | LICENSE |
| github.com/json-iterator/go | v1.1.12 | MIT | LICENSE |
| github.com/knqyf263/go-plugin | v0.9.0 | MIT | LICENSE |
| github.com/modern-go/concurrent | v0.0.0-20180306012644-bacd9c7ef1dd | Apache-2.0 | LICENSE |
| github.com/modern-go/reflect2 | v1.0.3-0.20250322232337-35a7c28c31ee | Apache-2.0 | LICENSE |
| github.com/munnerz/goautoneg | v0.0.0-20191010083416-a7dc8b61c822 | BSD-3-Clause | LICENSE |
| github.com/opencontainers/runtime-spec | v1.3.0 | Apache-2.0 | LICENSE |
| github.com/pmezard/go-difflib | v1.0.1-0.20181226105442-5d4384ee4fb2 | BSD-3-Clause | LICENSE |
| github.com/prometheus/client_golang | v1.25.0 | Apache-2.0 | LICENSE |
| github.com/prometheus/client_model | v0.6.3 | Apache-2.0 | LICENSE |
| github.com/prometheus/common | v0.72.0 | Apache-2.0 | LICENSE |
| github.com/prometheus/procfs | v0.22.0 | Apache-2.0 | LICENSE |
| github.com/sirupsen/logrus | v1.10.2 | MIT | LICENSE |
| github.com/spf13/cobra | v1.10.2 | Apache-2.0 | LICENSE.txt |
| github.com/spf13/pflag | v1.0.10 | BSD-3-Clause | LICENSE |
| github.com/tetratelabs/wazero | v1.11.0 | Apache-2.0 | LICENSE |
| github.com/x448/float16 | v0.8.4 | MIT | LICENSE |
| go.opentelemetry.io/auto/sdk | v1.2.1 | Apache-2.0 | LICENSE |
| go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp | v0.69.0 | Apache-2.0 AND BSD-3-Clause | LICENSE |
| go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc | v1.45.0 | Apache-2.0 AND BSD-3-Clause | LICENSE |
| go.opentelemetry.io/otel/exporters/otlp/otlptrace | v1.45.0 | Apache-2.0 AND BSD-3-Clause | LICENSE |
| go.opentelemetry.io/otel/log | v1.47.0 | Apache-2.0 AND BSD-3-Clause | LICENSE |
| go.opentelemetry.io/otel/metric | v1.47.0 | Apache-2.0 AND BSD-3-Clause | LICENSE |
| go.opentelemetry.io/otel/sdk | v1.47.0 | Apache-2.0 AND BSD-3-Clause | LICENSE |
| go.opentelemetry.io/otel/trace | v1.47.0 | Apache-2.0 AND BSD-3-Clause | LICENSE |
| go.opentelemetry.io/otel | v1.47.0 | Apache-2.0 AND BSD-3-Clause | LICENSE |
| go.opentelemetry.io/proto/otlp | v1.11.0 | Apache-2.0 | LICENSE |
| go.uber.org/multierr | v1.11.0 | MIT | LICENSE.txt |
| go.uber.org/zap | v1.28.0 | MIT | LICENSE |
| go.yaml.in/yaml/v2 | v2.4.4 | Apache-2.0 AND MIT | LICENSE, LICENSE.libyaml |
| go.yaml.in/yaml/v3 | v3.0.5 | Apache-2.0 AND MIT | LICENSE |
| golang.org/x/exp | v0.0.0-20260410095643-746e56fc9e2f | BSD-3-Clause | LICENSE |
| golang.org/x/mod | v0.42.0 | BSD-3-Clause | LICENSE |
| golang.org/x/net | v0.62.0 | BSD-3-Clause | LICENSE |
| golang.org/x/oauth2 | v0.37.0 | BSD-3-Clause | LICENSE |
| golang.org/x/sync | v0.24.0 | BSD-3-Clause | LICENSE |
| golang.org/x/sys | v0.49.0 | BSD-3-Clause | LICENSE |
| golang.org/x/term | v0.47.0 | BSD-3-Clause | LICENSE |
| golang.org/x/text | v0.43.0 | BSD-3-Clause | LICENSE |
| golang.org/x/time | v0.16.0 | BSD-3-Clause | LICENSE |
| gomodules.xyz/jsonpatch/v2 | v2.4.0 | Apache-2.0 | LICENSE |
| google.golang.org/genproto/googleapis/api | v0.0.0-20260803160001-6ac0973c030d | Apache-2.0 | LICENSE |
| google.golang.org/genproto/googleapis/rpc | v0.0.0-20260803160001-6ac0973c030d | Apache-2.0 | LICENSE |
| google.golang.org/grpc | v1.83.2 | Apache-2.0 | LICENSE |
| google.golang.org/protobuf | v1.36.12 | BSD-3-Clause | LICENSE |
| gopkg.in/evanphx/json-patch.v4 | v4.13.0 | BSD-3-Clause | LICENSE |
| gopkg.in/inf.v0 | v0.9.1 | BSD-3-Clause | LICENSE |
| k8s.io/api | v0.37.2 | Apache-2.0 | LICENSE |
| k8s.io/apiextensions-apiserver | v0.37.2 | Apache-2.0 | LICENSE |
| k8s.io/apimachinery | v0.37.2 | Apache-2.0 | LICENSE |
| k8s.io/apiserver | v0.37.2 | Apache-2.0 | LICENSE |
| k8s.io/client-go | v0.37.2 | Apache-2.0 | LICENSE |
| k8s.io/component-base | v0.37.2 | Apache-2.0 | LICENSE |
| k8s.io/component-helpers | v0.37.2 | Apache-2.0 | LICENSE |
| k8s.io/klog/v2 | v2.140.0 | Apache-2.0 | LICENSE |
| k8s.io/kube-openapi | v0.0.0-20260721132016-d427ff9ee9ad | Apache-2.0 AND BSD-3-Clause AND MIT | LICENSE, pkg/internal/third_party/go-json-experiment/json/LICENSE, pkg/internal/third_party/govalidator/LICENSE, pkg/validation/errors/LICENSE, pkg/validation/spec/LICENSE, pkg/validation/strfmt/LICENSE |
| k8s.io/kubelet | v0.37.0 | Apache-2.0 | LICENSE |
| k8s.io/streaming | v0.37.2 | Apache-2.0 | LICENSE |
| k8s.io/utils | v0.0.0-20260626114624-be93311217bd | Apache-2.0 | LICENSE, third_party/forked/golang/btree/LICENSE |
| sigs.k8s.io/apiserver-network-proxy/konnectivity-client | v0.36.0 | Apache-2.0 | LICENSE |
| sigs.k8s.io/controller-runtime | v0.25.3 | Apache-2.0 | LICENSE |
| sigs.k8s.io/json | v0.0.0-20250730193827-2d320260d730 | Apache-2.0 AND BSD-3-Clause | LICENSE |
| sigs.k8s.io/randfill | v1.0.0 | Apache-2.0 | LICENSE |
| sigs.k8s.io/structured-merge-diff/v6 | v6.4.2 | Apache-2.0 | LICENSE |
| sigs.k8s.io/yaml | v1.6.0 | Apache-2.0 AND BSD-3-Clause AND MIT | LICENSE |
