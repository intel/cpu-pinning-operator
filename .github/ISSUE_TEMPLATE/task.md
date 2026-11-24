---
name: Task
about: A backlog task for the operator. Keep the title a readable sentence.
title: ""
labels: []
---

<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0

Title: a descriptive sentence — what the task delivers — e.g.
"Admit pods against live per-NUMA occupancy".
Keep the body self-contained: link anything a reader needs to start.
-->

## Summary

<!-- One or two sentences: what this task delivers. -->

## Why it matters

<!-- The impact, not the mechanics: how this improves the operator or unblocks
adoption — capacity reclaimed, a silent failure eliminated, an outage risk
removed. Specific, not "it's good hygiene". -->

## Context

<!-- The problem being solved. For work that generalizes existing behavior, say what
must be preserved as the default/example. Enough that someone can start without asking. -->

## Affected code (starting points)

<!-- Concrete files/functions the change is expected to touch, e.g.
internal/webhook/pod_webhook.go, api/v1alpha1/balloonpinningpolicy_types.go. -->

## Acceptance criteria

- [ ] <!-- Observable, testable outcome 1 -->
- [ ] <!-- Observable, testable outcome 2 -->
- [ ] No doc left contradicting the change — name the `docs/*.md` corrected, or "none needed"
- [ ] Tests added/updated in the owning `internal/*` package (+ e2e case + `docs/internal/e2e-internals.md` if cluster-observable)
- [ ] `make generate` run if an `api/v1alpha1/*_types.go` type changed (CRD YAMLs committed)
- [ ] Pre-commit hook green (`make hooks`; gofmt + vet + `make test`)

## Dependencies

<!-- Link real issue numbers with a few descriptive words:
"Blocked by #NNN (needs the NUMA probe)", "Blocks #NNN", "Related to #NNN". -->

## Notes

<!-- Design constraints, links to docs/, known gotchas. -->
