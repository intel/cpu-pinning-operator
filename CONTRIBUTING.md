<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Contributing

Thanks for helping improve the CPU Pinning Operator. Bug reports, fixes, tests and
doc corrections are all welcome.

## Reporting bugs and requesting features

Open a [GitHub issue](https://github.com/intel/cpu-pinning-operator/issues).

| Issue | Include |
|---|---|
| Bug | operator and chart version, Kubernetes version, NRI balloons plugin version, the `BalloonPinningPolicy` and pod spec involved, what you expected, what happened, and operator logs. Redact names, labels, annotations and env values you would not publish |
| Feature | the workload problem you are solving, and what you would want the operator to do |

**Security vulnerabilities never go in a public issue.** Report them privately as
described in [SECURITY.md](SECURITY.md).

## Development setup

You need the Go version in [`go.mod`](go.mod) (currently 1.26), `make`, and `helm` for
the chart targets.

| Command | What it does |
|---|---|
| `make hooks` | once per clone: enables the pre-commit gate in `.githooks/` (gofmt, `go vet`, unit tests, and generated-code drift when `controller-gen` is installed) |
| `make test` | unit tests, all packages, including the doc link checks |
| `make vet` | `go vet`, with and without the `e2e` build tag |
| `make lint` | golangci-lint with `.golangci.yml` (CI enforces the same config) |
| `make helm-lint` | lints the chart in `deploy/cpu-pinning-operator/` |
| `make generate` | regenerates CRDs, deepcopy code, and the RBAC/webhook baselines; run it when you change `api/v1alpha1/*_types.go` or a `+kubebuilder:rbac`/`+kubebuilder:webhook` marker, and commit the result |
| `make test-crd` | the tests that need a real API server (downloads envtest binaries) |
| `make test-all` | `test`, `test-crd` and `test-e2e-guards`; with `E2E_CLUSTER=1` also `test-validation` |
| `make test-validation` | the gated security e2e tests against the installed release, in four passes ([e2e-internals.md](docs/internal/e2e-internals.md#validation-run)) |

End-to-end tests (`go test -tags e2e ./test/e2e/...`, or `make test-e2e`) need a real
cluster with the NRI balloons plugin and the operator installed. `make test-e2e-guards`
runs the parts that need no cluster. See
[development.md](docs/internal/development.md#testing) and
[e2e-internals.md](docs/internal/e2e-internals.md).

Package layout and code generation are covered in
[development.md](docs/internal/development.md).

## Pull requests

- **One logical change per PR.** Branch from `main`.
- **Tests and docs ship with the change.** Add or extend the `*_test.go` beside the
  code you changed; cluster-observable behavior also gets an e2e case. Fix any doc
  your change makes untrue, with the smallest edit that makes it right.
- **Commit subjects** follow [Conventional Commits](https://www.conventionalcommits.org/):
  `fix(webhook): ...`, `feat(chart): ...`, `docs: ...`, `test(e2e): ...`.
- New files carry the SPDX header used throughout the repo.
- Fill in the [pull request template](.github/PULL_REQUEST_TEMPLATE.md), including how
  you verified the change.

Run `make test`, `make lint` and `make test-e2e-guards` before you push. CI runs the
first two; the pre-commit hook runs the third. Every PR must pass these checks:

| Workflow | Checks |
|---|---|
| `unit-tests` | `make vet`, `make test`, CRD install on a real API server, `make generate` drift |
| `linter` | golangci-lint |
| `sec-code` | gosec, govulncheck |
| `sec-fuzz` | the Go fuzz targets |
| `sec-iac` | Trivy config scan of the chart and Dockerfile, zizmor on the workflows |
| `sec-secrets` | gitleaks |
| `trivy-image` | Trivy scan of the built container image |

Expect review questions on placement and admission changes, where a wrong answer pins
workloads to the wrong CPUs. A PR merges when the checks are green and a maintainer
approves it.

## License

The project is licensed under [Apache-2.0](LICENSE). Contributions are accepted
under the same license.
