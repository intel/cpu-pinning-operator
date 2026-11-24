// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// The per-node dashboard's metric panels, below the canvas; every query has the
// node baked in. Rules for every query:
//   - a standby replica may also export the NRT gauges, so collapse them with
//     `max by`, never `sum by`;
//   - group on workload_namespace, never `namespace` (the scrape target owns it).
//
// cAdvisor metrics are the kubelet's and may be absent; panels must render without them.
package dashboard

import (
	"fmt"
	"strings"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
)

// cpuAllocationLabel is the node label carrying the resolved CPU-allocation mode.
const cpuAllocationLabel = cpopv1alpha1.LabelPrefix + "cpu-allocation"

// Panel ids for all three files, in one place: Grafana silently drops a panel on
// an id collision. 8 and 9 are free.
const (
	idCanvas       = 1
	idNumbersRow   = 2
	idPerNUMA      = 3
	idReservedPool = 4
	idStranded     = 5
	idGhosts       = 6
	idSettings     = 7
	idBalloonTable = 10
	idExpertRow    = 11
	idGlossaryRow  = 13
	idGlossaryText = 14
	idNUMARoom     = 15
	idSplitCores   = 16
	idInFlight     = 17
	idIdentityRow  = 18
	idIdentityText = 19
	idLiveFacts    = 20
	idMapRow       = 21
	idLogRow       = 22
	idLogPanel     = 23
	// The pool-membership section.
	idPoolRow         = 12
	idPoolSharedTable = 24
	idPoolPinnedTable = 25
	idPoolUsedGauge   = 26
	idPoolCountStat   = 27
	idPoolPinnedGauge = 28
	idPoolPinnedCount = 29
	// One row per pool.
	idPoolSharedRow = 30
	idPoolPinnedRow = 31
)

// GlossaryTerms must be defined on both dashboards and in docs/observability.md;
// a chart test enforces it.
var GlossaryTerms = []string{
	"balloon",
	"span",
	"NUMA",
	"reserved pool",
	"hyperthread sibling",
	"stranded capacity",
	"drift",
	"bypass",
	"ghost allocation",
	"pinned",
	"skipped",
	"denied",
}

// prom is the `datasource` template variable reference every panel uses.
func prom() map[string]any {
	return map[string]any{"type": "prometheus", "uid": "${datasource}"}
}

func gridPos(x, y, w, h int) map[string]any {
	return map[string]any{"x": x, "y": y, "w": w, "h": h}
}

func steps(pairs ...any) map[string]any {
	out := make([]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{"color": pairs[i], "value": pairs[i+1]})
	}
	return map[string]any{"mode": "absolute", "steps": out}
}

// target builds a one-query panel's Prometheus target; instant queries for current values.
func target(expr, legend string, instant bool) map[string]any {
	t := map[string]any{
		"refId":        "A",
		"datasource":   prom(),
		"expr":         expr,
		"legendFormat": legend,
	}
	if instant {
		t["instant"] = true
	}
	return t
}

// usedPerNUMA and freePerNUMA prefer the recording rules, falling back to
// de-duplicated raw aggregations; `capacity * 0` turns an absent series into 0.
// used is not clamped: an overrun bar shows a container keeping an excluded sibling.
func usedPerNUMA(node string) string {
	capacity := numaCapacity(node)
	claimed := fmt.Sprintf(`count by (node, numa) (max by (node, numa, cpu) `+
		`(cpu_pinning_cpu_assignment{node=%q, balloon!="free"}))`, node)
	return fmt.Sprintf(`cpu_pinning:cpu_used:per_numa{node=%q} or %s or (%s * 0)`,
		node, claimed, capacity)
}

// The fallback is clamped at 0: an excluded sibling still claimed could make free negative.
func freePerNUMA(node string) string {
	capacity := numaCapacity(node)
	claimed := fmt.Sprintf(`count by (node, numa) (max by (node, numa, cpu) `+
		`(cpu_pinning_cpu_assignment{node=%q, balloon!="free"}))`, node)
	return fmt.Sprintf(`cpu_pinning:cpu_free:per_numa{node=%q} or clamp_min(%s - (%s or (%s * 0)), 0)`,
		node, capacity, claimed, capacity)
}

// numaCapacity is the per-NUMA pinnable total, falling back to the hardware total;
// parenthesised because `or` binds looser than arithmetic.
func numaCapacity(node string) string {
	return fmt.Sprintf(`(max by (node, numa) (cpu_pinning_node_pinnable_cpus{node=%[1]q})`+
		` or max by (node, numa) (cpu_pinning_node_logical_cpus{node=%[1]q}))`, node)
}

// numaNodesWithFreeRoom counts NUMA nodes with a free CPU: an upper bound on a new
// balloon's span, not a promise.
func numaNodesWithFreeRoom(node string) string {
	return fmt.Sprintf(`count((%s) > 0)`, freePerNUMA(node))
}

// splitCores counts physical cores with one thread claimed and another free.
func splitCores(node string) string {
	claimed := fmt.Sprintf(`count by (core) (max by (core, cpu) `+
		`(cpu_pinning_cpu_assignment{node=%q, balloon!="free"}))`, node)
	total := fmt.Sprintf(`count by (core) (max by (core, cpu) `+
		`(cpu_pinning_cpu_assignment{node=%q}))`, node)
	return fmt.Sprintf(`count(((%[1]s) > 0) and ((%[2]s) > (%[1]s)))`, claimed, total)
}

// nodePanels builds the sections below the canvas, from grid row startY. s supplies
// the per-NUMA scales; m the reserved pool size for titles.
func nodePanels(node string, m Machine, s shape, startY int) []any {
	panels := []any{row(idNumbersRow, "This machine's numbers", startY)}

	// One bar per NUMA node, its track that node's capacity, coloured by fullness.
	perNUMATitle := "How full each NUMA node is"
	if s.evenNUMA() && s.cpusPerNUMAMax > 0 {
		perNUMATitle = fmt.Sprintf("How full each NUMA node is, of %d CPUs each", s.cpusPerNUMAMax)
	}
	panels = append(panels, map[string]any{
		"type":  "bargauge",
		"title": perNUMATitle,
		"description": "CPUs claimed on each NUMA node, drawn against the CPUs that NUMA node " +
			"has, so a full bar is a full NUMA node and the gap is what is still free. Healthy " +
			"is bars of similar length; one long bar beside a short one is the imbalance that " +
			"strands capacity, because a max-span balloon takes the same count on every NUMA " +
			"node it spans. " +
			"A NUMA node with nothing claimed reads 0 rather than going missing.",
		"datasource": prom(),
		"gridPos":    gridPos(0, startY+1, 10, 6),
		"id":         idPerNUMA,
		"options": map[string]any{
			// lcd reads as a meter rather than as a row of numbers, at 2 NUMA nodes
			// and at 8.
			"displayMode":   "lcd",
			"orientation":   "horizontal",
			"showUnfilled":  true,
			"valueMode":     "color",
			"minVizHeight":  16,
			"minVizWidth":   8,
			"reduceOptions": map[string]any{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
		},
		"fieldConfig": map[string]any{
			"defaults": map[string]any{
				"unit": "short", "decimals": 0, "min": 0, "max": s.cpusPerNUMAMax,
				"color":      map[string]any{"mode": "thresholds"},
				"noValue":    "no topology reported for this machine",
				"thresholds": fullnessSteps(s.cpusPerNUMAMax),
			},
			"overrides": s.numaMaxOverrides(),
		},
		"targets": []any{target(usedPerNUMA(node), "NUMA {{numa}}", true)},
	})

	panels = append(panels, map[string]any{
		"type":  "gauge",
		"title": "Reserved pool: asked for vs provisioned",
		"description": "What everything unpinned here asks for, over the reserved pool it was " +
			"given. Above 1 those pods contend for the same CPUs — raise the reserved sizing, " +
			"do not unpin workloads.",
		"datasource": prom(),
		"gridPos":    gridPos(10, startY+1, 4, 6),
		"id":         idReservedPool,
		"options": map[string]any{
			"showThresholdLabels":  false,
			"showThresholdMarkers": true,
			"reduceOptions":        map[string]any{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
		},
		"fieldConfig": map[string]any{
			"defaults": map[string]any{
				"unit": "none", "min": 0, "max": 2, "decimals": 2,
				"color":   map[string]any{"mode": "thresholds"},
				"noValue": "not reported for this machine",
				// Same steps as the cluster page; red at 1, where the oversubscribed alert fires.
				"thresholds": steps("green", nil, "orange", 0.8, "red", 1),
			},
			"overrides": []any{},
		},
		"targets": []any{target(
			fmt.Sprintf(`max by (node) (cpu_pinning_reserved_pool_utilization_ratio{node=%q})`, node),
			"reserved pool used", true)},
	})

	panels = append(panels, map[string]any{
		"type":  "stat",
		"title": "Free CPUs the widest span cannot use",
		"description": "Free CPUs too unevenly spread across this machine's NUMA nodes for a " +
			"max-span balloon to absorb, because such a balloon takes the same count on every " +
			"NUMA node it spans. A narrower pod can still be placed on them, so this is a limit " +
			"on the widest shape and not a fault. The core map above shows where the holes are. " +
			"Always 0 on a single-NUMA machine.",
		"datasource": prom(),
		"gridPos":    gridPos(14, startY+1, 5, 6),
		"id":         idStranded,
		"options": map[string]any{
			"colorMode": "value", "graphMode": "area", "justifyMode": "auto",
			"textMode":      "auto",
			"reduceOptions": map[string]any{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
		},
		"fieldConfig": map[string]any{
			"defaults": map[string]any{
				"unit": "short", "decimals": 0,
				"color":   map[string]any{"mode": "thresholds"},
				"noValue": "not reported",
				// Healthy is 0 (green); amber from 1, where CPUPinningNodeFragmented fires.
				"thresholds": steps("green", nil, "orange", 1),
			},
			"overrides": []any{},
		},
		"targets": []any{target(
			fmt.Sprintf(`max by (node) (cpu_pinning_cpu_fragmentation{node=%q})`, node),
			"free CPUs the widest span cannot use", false)},
	})

	panels = append(panels, map[string]any{
		"type":  "stat",
		"title": "CPUs booked to pods that are gone",
		"description": "CPUs the balloons plugin still holds for pods that no longer exist, so " +
			"new pods can be refused while those CPUs sit idle. Healthy is 0; restart the " +
			"balloons plugin on this machine. An empty tile is unknown, not clean.",
		"datasource": prom(),
		"gridPos":    gridPos(19, startY+1, 5, 6),
		"id":         idGhosts,
		"options": map[string]any{
			"colorMode": "value", "graphMode": "none", "justifyMode": "auto",
			"textMode":      "auto",
			"reduceOptions": map[string]any{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
		},
		"fieldConfig": map[string]any{
			"defaults": map[string]any{
				"unit": "short", "decimals": 0,
				"color": map[string]any{"mode": "thresholds"},
				// Absent reads as unknown; only a reported 0 is green.
				"noValue":    "unknown — not reported",
				"thresholds": steps("green", nil, "red", 1),
			},
			"overrides": []any{},
		},
		"targets": []any{target(
			fmt.Sprintf(`max by (node) (cpu_pinning_ghost_allocations{node=%q})`, node),
			"ghost allocations", true)},
	})

	// Who is in each pool and how much they use.
	panels = append(panels, row(idPoolRow, "Who shares which CPUs", startY+7))
	panels = append(panels, poolMembershipPanels(node, m, s, startY+8)...)

	// Expert detail last; each count names its total.
	panels = append(panels, row(idExpertRow, "Expert detail", startY+32))

	panels = append(panels, map[string]any{
		"type":  "stat",
		"title": fmt.Sprintf("NUMA nodes with room left (of %d)", s.numaNodes),
		"description": "How many NUMA nodes still have a free CPU — an upper bound on the span " +
			"a new balloon could be built from, not a promise that a given pod fits. Nothing " +
			"here knows how many CPUs per NUMA node the next pod will ask for.",
		"datasource": prom(),
		"gridPos":    gridPos(0, startY+33, 8, 4),
		"id":         idNUMARoom,
		"options": map[string]any{
			"colorMode": "value", "graphMode": "none", "justifyMode": "auto",
			"textMode":      "auto",
			"reduceOptions": map[string]any{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
		},
		"fieldConfig": map[string]any{
			"defaults": map[string]any{
				"unit": "short", "decimals": 0,
				"color":      map[string]any{"mode": "thresholds"},
				"noValue":    "no per-NUMA data for this machine",
				"thresholds": steps("red", nil, "text", 1),
			},
			"overrides": []any{},
		},
		"targets": []any{target(numaNodesWithFreeRoom(node), "NUMA nodes with a free CPU", true)},
	})

	panels = append(panels, map[string]any{
		"type":  "stat",
		"title": fmt.Sprintf("Half-used physical cores (of %d)", s.physicalCores),
		"description": "Physical cores with one thread claimed and the other free, out of this " +
			"machine's physical cores. This is where the hyperthread-sibling policy shows up, and " +
			"it is the usual reason free CPUs cannot be pinned: under `exclude` those free threads " +
			"are not offered to anyone.",
		"datasource": prom(),
		"gridPos":    gridPos(8, startY+33, 8, 4),
		"id":         idSplitCores,
		"options": map[string]any{
			"colorMode": "value", "graphMode": "none", "justifyMode": "auto",
			"textMode":      "auto",
			"reduceOptions": map[string]any{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
		},
		"fieldConfig": map[string]any{
			"defaults": map[string]any{
				"unit": "short", "decimals": 0,
				"color":      map[string]any{"mode": "thresholds"},
				"noValue":    "no assignments published for this machine",
				"thresholds": steps("text", nil),
			},
			"overrides": []any{},
		},
		"targets": []any{target(splitCores(node), "cores with a free sibling", true)},
	})

	panels = append(panels, map[string]any{
		"type":  "stat",
		"title": "Admissions in flight (0 at rest)",
		"description": "Reservations the webhook is holding for pods it has just admitted but " +
			"whose CPUs are not yet visible in the topology. 0 is the resting value and the " +
			"sparkline shows the return to it; a value stuck above 0 means pods were admitted " +
			"and never started.",
		"datasource": prom(),
		"gridPos":    gridPos(16, startY+33, 8, 4),
		"id":         idInFlight,
		"options": map[string]any{
			"colorMode": "value", "graphMode": "area", "justifyMode": "auto",
			"textMode":      "auto",
			"reduceOptions": map[string]any{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
		},
		"fieldConfig": map[string]any{
			"defaults": map[string]any{
				"unit": "short", "decimals": 0,
				"color":      map[string]any{"mode": "thresholds"},
				"noValue":    "not reported for this machine",
				"thresholds": steps("text", nil),
			},
			"overrides": []any{},
		},
		"targets": []any{target(
			fmt.Sprintf(`max by (node) (cpu_pinning_webhook_reservations_live{node=%q})`, node),
			"reservations held", false)},
	})

	return panels
}

// balloonSummary is the per-balloon roll-up: CPUs held and containers in it. Both
// are aggregated so a raw recording rule's __name__ does not become a column. The
// count reads pool_membership, since cpu_assignment's reserved series have empty
// workload labels; it counts containers, not pods.
func balloonSummary(node string, y int) map[string]any {
	return map[string]any{
		"type":  "table",
		"title": "Balloons on this machine",
		"description": "Each balloon holding CPUs here, the CPUs it holds and the containers in " +
			"it. `reserved` is the pool for everything unpinned. Free CPUs are not a balloon.",
		"datasource": prom(),
		"gridPos":    gridPos(0, y, 24, 6),
		"id":         idBalloonTable,
		"options": map[string]any{
			"showHeader": true,
			"cellHeight": "sm",
			"footer": map[string]any{
				"show": true, "reducer": []any{"sum"}, "countRows": false,
				"fields": []any{"CPUs", "Containers"},
			},
			"sortBy": []any{map[string]any{"displayName": "CPUs", "desc": true}},
		},
		"fieldConfig": map[string]any{
			"defaults": map[string]any{
				"unit": "short", "decimals": 0,
				"custom":  map[string]any{"align": "auto", "cellOptions": map[string]any{"type": "auto"}, "inspect": false},
				"noValue": "not reported",
			},
			// The balloon name carries the pool colour; the counts stay uncoloured.
			"overrides": []any{poolWidth("Balloon", 260), balloonNameColors()},
		},
		"targets": []any{
			map[string]any{
				"refId":      "A",
				"datasource": prom(),
				"expr": fmt.Sprintf(`max by (node, balloon) (cpu_pinning:cpu_used:per_balloon{node=%[1]q}) or `+
					`count by (node, balloon) (max by (node, balloon, cpu) `+
					`(cpu_pinning_cpu_assignment{node=%[1]q, balloon!="free"}))`, node),
				"instant": true,
				"format":  "table",
			},
			map[string]any{
				"refId":      "B",
				"datasource": prom(),
				"expr": fmt.Sprintf(`count by (balloon) `+
					`(max by (balloon, workload_namespace, workload_pod, workload_container) `+
					`(cpu_pinning_pool_membership{node=%q}))`, node),
				"instant": true,
				"format":  "table",
			},
		},
		"transformations": []any{
			map[string]any{
				"id":      "joinByField",
				"options": map[string]any{"byField": "balloon", "mode": "outer"},
			},
			map[string]any{
				"id": "organize",
				"options": map[string]any{
					"excludeByName": map[string]any{
						"Time":   true,
						"Time 1": true, "Time 2": true, "node": true,
						// Also guards against a raw recording-rule __name__ column.
						"__name__": true,
					},
					"indexByName": map[string]any{"balloon": 0, "Value #A": 1, "Value #B": 2},
					"renameByName": map[string]any{
						"balloon":  "Balloon",
						"Value #A": "CPUs",
						"Value #B": "Containers",
					},
				},
			},
		},
	}
}

// poolListing is the per-container listing for one balloon selector plus a single
// join key, since Grafana's joinByField takes one field and a container's identity is three.
func poolListing(node, groupBy, selector string) string {
	inner := fmt.Sprintf(`max by (%s) (cpu_pinning_pool_membership{node=%q, %s})`, groupBy, node, selector)
	return fmt.Sprintf(`label_join(%s, "key", "/", `+
		`"workload_namespace", "workload_pod", "workload_container")`, inner)
}

// millicoresPerCore converts cAdvisor's cores to millicores, the unit of requests and limits.
const millicoresPerCore = 1000

// containerMillicoresUsed is each pool member's CPU use over the range, reduced to
// the join key. It is narrowed to pool members, since cAdvisor keeps series for gone
// containers. Without cAdvisor the column is absent, not zero. [$__range], since
// [$__interval] is too short at long ranges and returns nothing.
func containerMillicoresUsed(node, selector string) string {
	return fmt.Sprintf(`max by (key) (label_join((%s) * %d, "key", "/", "namespace", "pod", "container"))`,
		coresUsedByMembers(node, selector), millicoresPerCore)
}

// poolMemberCount is how many containers are in one pool, read live.
func poolMemberCount(node, selector string) string {
	return fmt.Sprintf(`count(max by (workload_namespace, workload_pod, workload_container) `+
		`(cpu_pinning_pool_membership{node=%q, %s}))`, node, selector)
}

// cadvisorCores is per-container CPU usage; container!="" drops rollups that would double totals.
func cadvisorCores(node string) string {
	return fmt.Sprintf(`sum by (namespace, pod, container) `+
		`(rate(container_cpu_usage_seconds_total{node=%q, container!=""}[$__range]))`, node)
}

// coresUsedByMembers keeps the cAdvisor usage of one pool's members (`and on`), with
// workload_* labels relabelled onto cAdvisor's names.
func coresUsedByMembers(node, selector string) string {
	inner := fmt.Sprintf(`max by (workload_namespace, workload_pod, workload_container) `+
		`(cpu_pinning_pool_membership{node=%q, %s})`, node, selector)
	relabelled := fmt.Sprintf(`label_replace(%s, "namespace", "$1", "workload_namespace", "(.+)")`, inner)
	relabelled = fmt.Sprintf(`label_replace(%s, "pod", "$1", "workload_pod", "(.+)")`, relabelled)
	relabelled = fmt.Sprintf(`label_replace(%s, "container", "$1", "workload_container", "(.+)")`, relabelled)
	return fmt.Sprintf(`%s and on (namespace, pod, container) %s`, cadvisorCores(node), relabelled)
}

// reservedMillicoresUsed is what everything unpinned on the machine burns, in millicores.
func reservedMillicoresUsed(node string) string {
	return fmt.Sprintf(`sum(%s) * %d`, coresUsedByMembers(node, `balloon="reserved"`),
		millicoresPerCore)
}

// dedicatedCPUs counts distinct pinned CPUs from cpu_assignment; summing cpusets
// double-counts containers sharing one.
func dedicatedCPUs(node string) string {
	return fmt.Sprintf(`count(max by (cpu) `+
		`(cpu_pinning_cpu_assignment{node=%q, balloon!~"free|reserved"}))`, node)
}

// The dedicated gauge shows occupancy, not CPU time: pinned CPUs are held even when idle.

// usedColumn names its unit: Grafana has no millicore unit.
const usedColumn = "Used (mCPU)"

// poolWidth fits a full Deployment pod name, so replicas stay distinguishable.
func poolWidth(field string, px int) map[string]any {
	return map[string]any{
		"matcher":    map[string]any{"id": "byName", "options": field},
		"properties": []any{map[string]any{"id": "custom.width", "value": px}},
	}
}

// balloonNameColors colours the Balloon column pinned, with `reserved` mapped to the shared pool colour.
func balloonNameColors() map[string]any {
	return map[string]any{
		"matcher": map[string]any{"id": "byName", "options": "Balloon"},
		"properties": []any{
			map[string]any{"id": "color", "value": identityColor(ColorPinned)},
			map[string]any{"id": "custom.cellOptions", "value": map[string]any{"type": "color-text"}},
			map[string]any{"id": "mappings", "value": []any{
				map[string]any{"type": "value", "options": map[string]any{
					"reserved": map[string]any{"color": ColorReserved, "index": 0},
				}},
			}},
		},
	}
}

// poolMembershipPanels builds "who shares which CPUs" from grid row y: the roll-up,
// then one row per pool with its gauge, container count and listing (from
// pool_membership, per container). The reserved gauge is CPU time, the dedicated
// one occupancy. No throttling column: cpusets do not cause CFS throttling.
func poolMembershipPanels(node string, m Machine, s shape, y int) []any {
	// Baked sizes go in titles; unset ones are omitted, not shown as zero.
	sharedTitle := "Sharing the reserved pool CPUs"
	countTitle := "Containers sharing them"
	if m.ReservedCPUs > 0 {
		sharedTitle = fmt.Sprintf("Sharing the reserved pool CPUs (%d of them)", m.ReservedCPUs)
		countTitle = fmt.Sprintf("Containers sharing those %d CPUs", m.ReservedCPUs)
	}
	// The two gauges measure different things on purpose: the reserved cpuset is always
	// fully occupied, so it shows CPU time; the dedicated pool shows occupancy.
	pinnableCPUs := s.pinnableCPUs
	reservedTitle := "Reserved pool usage (mCPU)"
	if m.ReservedCPUs > 0 {
		reservedTitle = fmt.Sprintf("Reserved pool usage, of %d mCPU",
			m.ReservedCPUs*millicoresPerCore)
	}
	dedicatedTitle := "Dedicated CPUs occupied"
	if pinnableCPUs > 0 {
		dedicatedTitle = fmt.Sprintf("Dedicated CPUs occupied, of %d pinnable", pinnableCPUs)
	}
	// Shared gauge defaults: percentage threshold bands, so one palette serves both pools.
	usageDefaults := func(poolMax int, noValue string) map[string]any {
		d := map[string]any{
			"unit": "short", "min": 0, "decimals": 0,
			"color":   map[string]any{"mode": "thresholds"},
			"noValue": noValue,
		}
		if poolMax > 0 {
			d["max"] = poolMax
			d["thresholds"] = poolUsageSteps()
		} else {
			// Without a pool size Grafana would paint any reading red; stay neutral.
			d["thresholds"] = steps("text", nil)
		}
		return d
	}

	// Shared by both listings; "Value" is the membership field's name when cAdvisor is absent.
	poolTransformations := func(index map[string]any, rename map[string]any, exclude map[string]any) []any {
		return []any{
			map[string]any{
				"id":      "joinByField",
				"options": map[string]any{"byField": "key", "mode": "outer"},
			},
			map[string]any{
				"id": "organize",
				"options": map[string]any{
					"excludeByName": exclude,
					"indexByName":   index,
					"renameByName":  rename,
				},
			},
		}
	}

	return []any{
		balloonSummary(node, y),
		// The roll-up spans both pools, so it sits outside both rows.
		row(idPoolSharedRow, "Who shares the reserved pool CPUs", y+6),
		map[string]any{
			"type":  "gauge",
			"title": reservedTitle,
			"description": "What everything unpinned here burns, against the pool it shares. " +
				"CPU time, not occupancy: this cpuset is fixed, so all of it is always occupied. " +
				"Near the top those pods compete for what is left.",
			"datasource": prom(),
			"gridPos":    gridPos(0, y+7, 4, 4),
			"id":         idPoolUsedGauge,
			"options": map[string]any{
				"showThresholdLabels":  false,
				"showThresholdMarkers": true,
				"reduceOptions":        map[string]any{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
			},
			"fieldConfig": map[string]any{
				"defaults":  usageDefaults(m.ReservedCPUs*millicoresPerCore, "not reported"),
				"overrides": []any{},
			},
			"targets": []any{target(reservedMillicoresUsed(node), "mCPU in use", true)},
		},
		map[string]any{
			"type":  "stat",
			"title": countTitle,
			"description": "How many containers time-share the reserved pool. Healthy is system " +
				"daemons only. A long list means ordinary pods land here — pin the ones that need CPUs.",
			"datasource": prom(),
			"gridPos":    gridPos(0, y+11, 4, 4),
			"id":         idPoolCountStat,
			"options": map[string]any{
				// The value carries the pool's colour.
				"colorMode": "value", "graphMode": "none", "justifyMode": "auto",
				"textMode":      "auto",
				"reduceOptions": map[string]any{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
			},
			"fieldConfig": map[string]any{
				"defaults": map[string]any{
					"unit": "short", "decimals": 0,
					"color":   identityColor(ColorReserved),
					"noValue": "not reported",
				},
				"overrides": []any{},
			},
			"targets": []any{target(poolMemberCount(node, `balloon="reserved"`), "containers", true)},
		},
		map[string]any{
			"type":  "table",
			"title": sharedTitle,
			"description": "Every unpinned container and the CPU it uses, heaviest first. Healthy is " +
				"a total well under the pool. If not, pin the top rows or raise the reserved sizing.",
			"datasource": prom(),
			"gridPos":    gridPos(4, y+7, 20, 8),
			"id":         idPoolSharedTable,
			"options": map[string]any{
				"showHeader": true,
				"cellHeight": "sm",
				"footer": map[string]any{
					// A sum, not countRows, which would read as a namespace count.
					"show": true, "reducer": []any{"sum"}, "countRows": false,
					"fields": []any{usedColumn},
				},
				"sortBy": []any{map[string]any{"displayName": usedColumn, "desc": true}},
			},
			"fieldConfig": map[string]any{
				"defaults": map[string]any{
					"unit": "short", "decimals": 0,
					"custom":  map[string]any{"align": "auto", "cellOptions": map[string]any{"type": "auto"}, "inspect": false},
					"noValue": "nothing published for this machine",
				},
				"overrides": []any{poolWidth("Pod", 380), poolWidth("Container", 200)},
			},
			"targets": []any{
				map[string]any{
					"refId":      "A",
					"datasource": prom(),
					"expr": poolListing(node,
						"workload_namespace, workload_pod, workload_container", `balloon="reserved"`),
					"instant": true,
					"format":  "table",
				},
				map[string]any{
					"refId":      "B",
					"datasource": prom(),
					"expr":       containerMillicoresUsed(node, `balloon="reserved"`),
					"instant":    true,
					"format":     "table",
				},
			},
			"transformations": poolTransformations(
				map[string]any{
					"workload_namespace": 0, "workload_pod": 1, "workload_container": 2, "Value #B": 3,
				},
				map[string]any{
					"workload_namespace": "Namespace",
					"workload_pod":       "Pod",
					"workload_container": "Container",
					"Value #B":           usedColumn,
				},
				map[string]any{
					"Time": true, "Time 1": true, "Time 2": true, "key": true,
					// The cpuset size, identical on every row here.
					"Value": true, "Value #A": true,
				},
			),
		},
		row(idPoolPinnedRow, "Who holds the dedicated CPUs", y+15),
		map[string]any{
			"type":  "gauge",
			"title": dedicatedTitle,
			"description": "CPUs the pinned workloads occupy, against the pinnable pool: the " +
				"CPUs this machine's generated policy leaves available to pin. Occupancy, not CPU " +
				"time — pinned CPUs are held even when idle.",
			"datasource": prom(),
			"gridPos":    gridPos(0, y+16, 4, 4),
			"id":         idPoolPinnedGauge,
			"options": map[string]any{
				"showThresholdLabels":  false,
				"showThresholdMarkers": true,
				"reduceOptions":        map[string]any{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
			},
			"fieldConfig": map[string]any{
				// No series means nothing pinned. The max is the pinnable pool; unknown takes neutral bands.
				"defaults":  usageDefaults(pinnableCPUs, "no dedicated CPUs on this machine"),
				"overrides": []any{},
			},
			"targets": []any{target(dedicatedCPUs(node), "CPUs occupied", true)},
		},
		map[string]any{
			"type":  "stat",
			"title": "Containers holding them",
			"description": "How many containers hold dedicated CPUs here. Healthy is the pods you " +
				"pinned and no more; the table's totals give the CPUs held and the CPU used.",
			"datasource": prom(),
			"gridPos":    gridPos(0, y+20, 4, 4),
			"id":         idPoolPinnedCount,
			"options": map[string]any{
				"colorMode": "value", "graphMode": "none", "justifyMode": "auto",
				"textMode":      "auto",
				"reduceOptions": map[string]any{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
			},
			"fieldConfig": map[string]any{
				"defaults": map[string]any{
					"unit": "short", "decimals": 0,
					"color":   identityColor(ColorPinned),
					"noValue": "nothing pinned",
				},
				"overrides": []any{},
			},
			"targets": []any{target(poolMemberCount(node, `balloon!~"free|reserved"`), "containers", true)},
		},
		map[string]any{
			"type":  "table",
			"title": "Holding dedicated CPUs",
			"description": "Every pinned container, its balloon, the CPUs held for it and what it " +
				"uses. Healthy is the pods you pinned; dedicated CPUs near 0 mCPU are idle capacity.",
			"datasource": prom(),
			"gridPos":    gridPos(4, y+16, 20, 8),
			"id":         idPoolPinnedTable,
			"options": map[string]any{
				"showHeader": true,
				"cellHeight": "sm",
				"footer": map[string]any{
					// Both numbers: they are the gauge's numerator and denominator.
					"show": true, "reducer": []any{"sum"}, "countRows": false,
					"fields": []any{usedColumn, "Dedicated CPUs"},
				},
				"sortBy": []any{map[string]any{"displayName": "Balloon", "desc": false}},
			},
			"fieldConfig": map[string]any{
				"defaults": map[string]any{
					"unit": "short", "decimals": 0,
					"custom":  map[string]any{"align": "auto", "cellOptions": map[string]any{"type": "auto"}, "inspect": false},
					"noValue": "nothing pinned on this machine",
				},
				"overrides": []any{poolWidth("Pod", 380), poolWidth("Container", 200)},
			},
			"targets": []any{
				map[string]any{
					"refId":      "A",
					"datasource": prom(),
					"expr": poolListing(node,
						"balloon, workload_namespace, workload_pod, workload_container",
						`balloon!~"free|reserved"`),
					"instant": true,
					"format":  "table",
				},
				map[string]any{
					"refId":      "B",
					"datasource": prom(),
					"expr":       containerMillicoresUsed(node, `balloon!~"free|reserved"`),
					"instant":    true,
					"format":     "table",
				},
			},
			"transformations": poolTransformations(
				map[string]any{
					"balloon": 0, "workload_namespace": 1, "workload_pod": 2,
					"workload_container": 3, "Value #A": 4, "Value #B": 5,
				},
				map[string]any{
					"balloon":            "Balloon",
					"workload_namespace": "Namespace",
					"workload_pod":       "Pod",
					"workload_container": "Container",
					// One zone per container, so the cpuset size is its dedicated CPU count.
					"Value":    "Dedicated CPUs",
					"Value #A": "Dedicated CPUs",
					"Value #B": usedColumn,
				},
				map[string]any{"Time": true, "Time 1": true, "Time 2": true, "key": true},
			),
		},
	}
}

// row builds a collapsed=false section header.
func row(id int, title string, y int) map[string]any {
	return map[string]any{
		"type": "row", "title": title, "collapsed": false,
		"gridPos": gridPos(0, y, 24, 1), "id": id,
	}
}

// allocationModeText renders the resolved cpuAllocation, baked in since no metric carries it.
func allocationModeText(cpuAllocation string) string {
	if strings.TrimSpace(cpuAllocation) == "" {
		return "not reported"
	}
	return cpuAllocation
}

// glossaryRow is the first, collapsed row holding the glossary; GlossaryTerms keeps
// it in step with the cluster dashboard and docs.
func glossaryRow(rowID, textID, y int) map[string]any {
	return map[string]any{
		"type": "row", "title": "How to read this page (open me first)",
		"collapsed": true,
		"gridPos":   gridPos(0, y, 24, 1),
		"id":        rowID,
		"panels": []any{map[string]any{
			"type":  "text",
			"title": "What the words on this dashboard mean",
			"description": "Glossary of the operator's vocabulary — balloon, span, NUMA, " +
				"reserved pool, hyperthread sibling, stranded capacity, drift, bypass, ghost " +
				"allocation, pinned vs skipped vs denied — and how the core map is laid out. Reads " +
				"no metrics. The same text is in docs/observability.md.",
			"gridPos": gridPos(0, y+1, 24, 15),
			"id":      textID,
			"options": map[string]any{"mode": "markdown", "content": glossaryMarkdown},
		}},
	}
}

// glossaryMarkdown is the per-node page's glossary.
const glossaryMarkdown = `
# How to read this page

This page is about **one machine**, and it is ordered most important first:

1. **What this machine is** — its shape, and the settings that change how every
   number below is computed. Everything else is a fraction of these.
2. **This machine's CPU map** — who owns which CPU, right now.
3. **This machine's numbers** — how full the machine is, the CPUs booked to pods
   that are gone, and the free CPUs the widest balloon span cannot use.
4. **Who shares which CPUs** — the balloons holding this machine's CPUs, then a
   section per pool, each with its own gauge, container count and per-container
   listing: **who shares the reserved pool CPUs** is everything left unpinned,
   sharing the reserved pool, and **who holds the dedicated CPUs** is everything
   pinned, with the balloon it sits in and what it is using.
5. **Expert detail** — why the numbers above look the way they do.

Colour says one of two things. A **pool** colour is fixed and only labels the
number: grey is free, purple the reserved pool, green pinned, on the CPU map and on
every gauge and count below it. On the map, two colours in one box mean two owners
share that physical core. **Green, amber and red** are a verdict — nothing to do,
worth a look, act now — and appear only where a number can be good or bad; on the
two pool gauges that is amber past 70% of the pool and red past 85%, and on free
CPUs the widest span cannot use it is amber from 1. **The cluster
page is the way back**: use the link at the top of the page.

The operator pins selected pods onto fixed CPUs so they stop competing for CPU
time with other pods on the node. The words it uses:

| Term | What it means |
| --- | --- |
| **Pinned pod** | A pod the operator bound to a fixed set of CPUs. No other pod is placed on those CPUs; host processes and interrupts are not isolated. |
| **Skipped pod** | A pod that matched a pinning policy but was deliberately left unpinned (for example it asked for half a CPU, which cannot be pinned). It still runs, just without dedicated CPUs. |
| **Denied pod** | A pod the operator refused at admission, because there was no room for it, because its span is unsupported, because the balloons plugin on this machine was not ready (under ` + "`capacityCheck.mode: enforce`" + `), or because no machine is provisioned for balloons. A denied pod never starts and never exists, so a pod listing cannot show it; the cluster page counts it and the Event names it. |
| **Balloon** | The named set of CPUs a pinned pod's containers are placed into. Pods sharing a balloon share its CPUs. |
| **Span** | How many NUMA nodes one balloon is allowed to stretch across. ` + "`span1`" + ` stays inside a single NUMA node, which keeps memory access local; a wider span buys room and memory bandwidth at the cost of remote-memory latency. The best span depends on the workload. |
| **NUMA node** | One CPU-and-memory locality domain of the machine. CPUs reach their own NUMA node's memory fastest, so a balloon that stays inside one avoids remote-memory access. |
| **Reserved pool** | CPUs deliberately kept out of pinning, for everything that is *not* pinned: system daemons, kubelet, and ordinary pods. |
| **Hyperthread sibling** | The second thread of a physical core. **Hyperthread siblings** on this machine reads one of four ways: ` + "`held-back`" + ` — kept out of the pinning pool, so no pod is pinned to them (the OS can still run host work there); ` + "`reserved-pool`" + ` — they *are* the reserved pool, so everything unpinned runs on them and no pinned pod can; ` + "`allocatable`" + ` — ordinary CPUs a pinned pod can land on; ` + "`no-siblings`" + ` — this machine runs one thread per core, so there is none. |
| **Stranded capacity** | Free CPUs the machine's *widest* balloon span cannot use, because they are split unevenly across NUMA nodes and a balloon spanning N of them takes the same count on each. A narrower pod still fits on them, so this is a limit on one shape and not a fault. |
| **Drift** | A pod that is still running with the pinning it was given, but the configuration has changed since — so what it runs on no longer matches what it should run on. |
| **Bypass** | A pod the operator meant to pin but admitted **unpinned** after an internal error. It runs on shared CPUs without anyone asking for that. |
| **Ghost allocation** | CPUs still booked to a pod that no longer exists. The capacity is not really in use, but the machine's bookkeeping believes it is, so new pods can be refused. |

Healthy machine, in short: **no ghosts and a reserved pool below 1**, with the
pinned pods you expect holding the CPU counts you expect. Stranded capacity is
amber rather than red: it says the widest balloon shape has nowhere to go, while
every narrower pod still places.

The same glossary is in ` + "`docs/observability.md`" + `.
`
