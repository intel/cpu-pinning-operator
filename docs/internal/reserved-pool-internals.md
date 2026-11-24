<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Reserved-pool internals

Sizing and cpuset emission live in code, and the comments there are the source of
truth. This page holds only what those comments cannot: where to look, and the one
trap that costs an hour.

| what | where |
|---|---|
| Resolve the count from `spec.balloon.reserved` | `internal/balloons/reserved.go` — `ResolveReservedCPUs` |
| Clamp order, and the physical-cores conversion | `internal/balloons/reserved.go` — `ResolveReservedCPUs` and `reservedFromSpec` |
| Pick the cpuset, NUMA-balanced | `internal/balloons/policy.go` — `reservedCPUSet` |
| The pool's per-NUMA share, as admission subtracts it | `internal/balloons/reserved.go` — `ReservedPerNUMA` |
| Route namespaces onto the pool | `internal/balloons/policy.go` — `defaultReservedPoolNamespaces` |

> [!WARNING]
> `spec.balloon.reserved.percent` is already a fraction of the **physical** core
> count. Unlike the logical-CPU baselines it is not divided by threads-per-core in
> `physical-cores` mode. Dividing twice turns 25% into 12.5% and silently halves a
> user's reserved pool.

The count applied to each node is reported on `BalloonsClusterConfig` status as
`perNode[].reservedCPUs`, and the resolved mode as `perNode[].cpuAllocation`.

User-facing sizing options are in [configuration.md](../configuration.md).
