<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

## What

<!--
Bullet the concrete changes, ideally one per touched file/component, e.g.:
- `internal/webhook/pod_webhook.go` — gates on `requests.memory ≤ per-NUMA floor × span`.
- `deploy/.../deployment.yaml` — plumbs the new flag.
-->

Closes #<!-- issue number -->  <!-- or: Related to #<issue number> -->

## Why

<!-- The impact in one short paragraph: what breaks / is hard today, and what this fixes. -->

## Design notes & guardrails

<!--
Optional but expected for placement/preflight or security-sensitive changes.
Call out the invariants you held to (e.g. no resource inflation — span scales
capacity, never the request; gate and filter share one predicate; missing data →
skip not deny) and any trade-off a reviewer should scrutinize. Delete if trivial.
-->

## Checklist

- [ ] **No doc contradicts this change** — named any `docs/*.md` I corrected, or "none needed" (the common case for refactors, tests, and internal renames). A new knob is one row in the `docs/configuration.md` table, not a new section.
- [ ] **Tests added/updated** in the owning `internal/*` package; e2e case + `docs/internal/e2e-internals.md` row if the behavior is cluster-observable.
- [ ] **`make generate` run** and regenerated artifacts committed — only if I touched an `api/v1alpha1/*_types.go` type.
- [ ] **Pre-commit hook passed** (`make hooks` enabled; gofmt + vet + `make test` green; CI re-runs these on the PR).

## How I verified

<!--
Concrete evidence, not "tests pass". Name the test(s), paste the key command +
result, and — for cluster-observable behavior, if you have NRI hardware — the
hardware check (node, CR, observed output). e.g.:
- `go test ./internal/webhook/ -run TestMemoryFits` → PASS
- `helm template ... --set flag=true` → renders <X>; off → nothing
- <node>: node stamped <label=value>; <request> at span N → <observed admission result>
-->

## Notes for the reviewer

<!-- Anything non-obvious: design trade-offs, what to scrutinize, follow-ups deferred to a separate issue. -->
