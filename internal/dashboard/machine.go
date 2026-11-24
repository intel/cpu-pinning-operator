// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// The "what this machine is" section. The shape is derived from the per-CPU
// topology (so it matches the map), operator-only facts are baked in
// (kube_node_labels is empty on stock kube-state-metrics), the rest is read live.
package dashboard

import (
	"fmt"
	"sort"
	"strings"
)

// Machine is what the operator knows about a machine that no metric carries, as
// of the last provisioning pass.
type Machine struct {
	// MemoryPerNUMABytes is the probe's MemTotal per NUMA node, nil when none was reported.
	MemoryPerNUMABytes map[int]int64
	// PackageTopology is the per-package NUMA-node count, "_"-joined ("1_1").
	PackageTopology string
	// MaxSpan is the widest span the machine's own balloon definitions serve.
	MaxSpan int
	// NodeClass is the hardware class the machine classified to.
	NodeClass string
	// AMX records the amx_tile ISA feature the operator's probe found.
	AMX bool
	// CPUAllocation is the resolved mode (physical-cores | hyperthreads).
	CPUAllocation string
	// ReservedCPUs is the provisioned reserved pool in logical CPUs; 0 means unknown.
	ReservedCPUs int
	// PinnableCPUs is the allocatable pool (balloons.PinnableCPUs), the per-NUMA
	// denominator; nil uses the hardware total.
	PinnableCPUs map[int]bool
}

// shape is the machine's size, derived from the same topology as the core map.
type shape struct {
	numaNodes      int
	physicalCores  int
	threadsPerCore int
	logicalCPUs    int
	// coresPerNUMA is 0 when NUMA nodes hold different core counts.
	coresPerNUMA int
	// cpusPerNUMA is each NUMA node's pinnable CPU count and cpusPerNUMAMax the
	// largest: the per-NUMA bar gauge's max (logical counts without a pinnable set).
	cpusPerNUMAMax int
	cpusPerNUMA    map[int]int
	// pinnableCPUs is the machine's pinnable total, 0 when unknown.
	pinnableCPUs int
}

func shapeOf(groups []numaGroup, pinnable map[int]bool) shape {
	s := shape{numaNodes: len(groups), threadsPerCore: 1, cpusPerNUMA: map[int]int{}}
	sameCores := true
	for i, g := range groups {
		cpus, pinnableCPUs := 0, 0
		for _, c := range g.cores {
			s.physicalCores++
			cpus += len(c.cpus)
			if len(c.cpus) > s.threadsPerCore {
				s.threadsPerCore = len(c.cpus)
			}
			for _, cpu := range c.cpus {
				if pinnable == nil || pinnable[cpu] {
					pinnableCPUs++
				}
			}
		}
		s.logicalCPUs += cpus
		s.pinnableCPUs += pinnableCPUs
		s.cpusPerNUMA[g.numa] = pinnableCPUs
		if pinnableCPUs > s.cpusPerNUMAMax {
			s.cpusPerNUMAMax = pinnableCPUs
		}
		if i > 0 && len(g.cores) != len(groups[0].cores) {
			sameCores = false
		}
	}
	if sameCores && len(groups) > 0 {
		s.coresPerNUMA = len(groups[0].cores)
	}
	return s
}

// notReported is what an absent baked fact renders as, so a blank is not read as zero.
const notReported = "not reported"

// formatBytes renders a memory size in GiB, or TiB from a terabyte up.
func formatBytes(b int64) string {
	const gib = 1 << 30
	if b <= 0 {
		return notReported
	}
	if g := float64(b) / gib; g < 1024 {
		return fmt.Sprintf("%.1f GiB", g)
	}
	return fmt.Sprintf("%.2f TiB", float64(b)/(gib*1024))
}

// memoryPerNUMAText renders one figure when NUMA nodes agree, else the range,
// without calling it unbalanced (firmware reservations differ slightly).
func memoryPerNUMAText(mem map[int]int64) string {
	if len(mem) == 0 {
		return notReported
	}
	nodes := make([]int, 0, len(mem))
	for n := range mem {
		nodes = append(nodes, n)
	}
	sort.Ints(nodes)
	lo, hi := mem[nodes[0]], mem[nodes[0]]
	for _, n := range nodes {
		if mem[n] < lo {
			lo = mem[n]
		}
		if mem[n] > hi {
			hi = mem[n]
		}
	}
	if lo == hi {
		return formatBytes(lo)
	}
	return fmt.Sprintf("%s – %s", formatBytes(lo), formatBytes(hi))
}

func orNotReported(s string) string {
	if strings.TrimSpace(s) == "" {
		return notReported
	}
	return s
}

// machineFactsMarkdown renders the baked identity table: hardware on the left,
// provisioning on the right.
func machineFactsMarkdown(m Machine, s shape) string {
	cores := fmt.Sprintf("%d", s.physicalCores)
	if s.coresPerNUMA > 0 && s.numaNodes > 1 {
		cores = fmt.Sprintf("%d (%d per NUMA node)", s.physicalCores, s.coresPerNUMA)
	}
	span := notReported
	switch {
	case m.MaxSpan == 1:
		span = "1 NUMA node"
	case m.MaxSpan > 1:
		span = fmt.Sprintf("%d NUMA nodes", m.MaxSpan)
	}
	isa := "none found by the probe"
	if m.AMX {
		isa = "AMX (amx_tile)"
	}
	pkg := notReported
	if p := strings.TrimSpace(m.PackageTopology); p != "" {
		pkg = p + " (NUMA nodes per socket)"
	}

	rows := [][4]string{
		{"NUMA nodes", fmt.Sprintf("%d", s.numaNodes), "Machine class", orNotReported(m.NodeClass)},
		{"Physical cores", cores, "ISA features", isa},
		{"Threads per core", fmt.Sprintf("%d", s.threadsPerCore), "Widest balloon span", span},
		{"Logical CPUs", fmt.Sprintf("%d", s.logicalCPUs), "Package topology", pkg},
		{"RAM per NUMA node", memoryPerNUMAText(m.MemoryPerNUMABytes), "CPUs a workload can use", pinnableText(s)},
	}
	bold := func(s string) string {
		if s == "" {
			return ""
		}
		return "**" + s + "**"
	}
	var b strings.Builder
	b.WriteString("| Hardware | | Provisioned as | |\n| --- | --- | --- | --- |\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", bold(r[0]), r[1], bold(r[2]), r[3])
	}
	b.WriteString("\n*Measured when the operator provisioned this machine and written into " +
		"this page. Changing any of it re-provisions the machine, which regenerates the page. " +
		"The \"live from Prometheus\" panel is the same machine as the cluster reports it now.*\n")
	return b.String()
}

// pinnableText states the pinnable pool, with the hardware total when they differ.
func pinnableText(s shape) string {
	switch {
	case s.pinnableCPUs <= 0:
		return notReported
	case s.pinnableCPUs == s.logicalCPUs:
		return fmt.Sprintf("%d (every logical CPU)", s.pinnableCPUs)
	default:
		return fmt.Sprintf("%d of %d (the rest is out of the pinning pool)",
			s.pinnableCPUs, s.logicalCPUs)
	}
}

// evenNUMA reports whether every NUMA node holds the same number of pinnable CPUs.
func (s shape) evenNUMA() bool {
	for _, n := range s.cpusPerNUMA {
		if n != s.cpusPerNUMAMax {
			return false
		}
	}
	return true
}

// numaMaxOverrides gives each bar its own NUMA node's max and colours, only when
// NUMA nodes differ in size.
func (s shape) numaMaxOverrides() []any {
	if s.evenNUMA() {
		return []any{}
	}
	numas := make([]int, 0, len(s.cpusPerNUMA))
	for n := range s.cpusPerNUMA {
		numas = append(numas, n)
	}
	sort.Ints(numas)
	out := make([]any, 0, len(numas))
	for _, n := range numas {
		out = append(out, map[string]any{
			"matcher": map[string]any{"id": "byName", "options": fmt.Sprintf("NUMA %d", n)},
			"properties": []any{
				map[string]any{"id": "max", "value": s.cpusPerNUMA[n]},
				map[string]any{"id": "thresholds", "value": fullnessSteps(s.cpusPerNUMA[n])},
			},
		})
	}
	return out
}

// fullnessSteps colours a per-NUMA bar by fraction of its capacity; the steps are
// forced apart because on small NUMA nodes they round to the same integer.
func fullnessSteps(capacity int) map[string]any {
	filling := max(1, capacity*3/4)
	full := max(filling+1, capacity*95/100)
	return steps("green", nil, "orange", filling, "red", full)
}

// Pool-usage gauge breakpoints as percentages: Grafana resolves them against the
// field's min/max, so they work for pools sized live.
const (
	// poolUsageWatchPercent is where a pool's headroom is thinning.
	poolUsageWatchPercent = 70
	// poolUsageFullPercent is where there is effectively no headroom left.
	poolUsageFullPercent = 85
)

// poolUsageSteps colours the percentage of a pool's own capacity in use.
func poolUsageSteps() map[string]any {
	st := steps("green", nil, "orange", poolUsageWatchPercent, "red", poolUsageFullPercent)
	st["mode"] = "percentage"
	return st
}

// Heights of the identity section's panels; the baked one matches the two live ones stacked.
const (
	identityH    = 7
	liveFactsH   = 4
	settingsH    = identityH - liveFactsH
	identityRowH = 1 + identityH
)

// machineIdentityPanels builds the "what this machine is" section at grid row y.
func machineIdentityPanels(node string, m Machine, s shape, y int) []any {
	return []any{
		row(idIdentityRow, "What this machine is", y),
		map[string]any{
			"type":  "text",
			"title": "The machine, as the operator measured it",
			"description": "This machine's hardware and the settings it was provisioned with: " +
				"the shape every number further down this page is a fraction of. Reads no " +
				"metrics — no metric carries node labels, so the operator writes these in when " +
				"it provisions the machine, and they can be stale. The live panel beside it " +
				"shows the same machine as the cluster reports it now; the two disagreeing " +
				"means it moved since.",
			"gridPos": gridPos(0, y+1, 12, identityH),
			"id":      idIdentityText,
			"options": map[string]any{"mode": "markdown", "content": machineFactsMarkdown(m, s)},
		},
		liveFactsPanel(node, y+1),
		settingsPanel(node, m, y+1+liveFactsH),
	}
}

// liveFactsPanel shows node capacity and software versions from Prometheus; each
// query keeps only the shown labels so new labels cannot add columns.
func liveFactsPanel(node string, y int) map[string]any {
	return map[string]any{
		"type":  "table",
		"title": "The same machine, live from Prometheus",
		"description": "What the cluster reports for this machine right now: the node capacity " +
			"the scheduler goes by, and the versions a support question needs. Needs " +
			"kube-state-metrics — empty means it is not running, not that the machine is gone. " +
			"Logical CPUs disagreeing with the measured panel means the machine changed shape " +
			"and has not been re-provisioned since.",
		"datasource": prom(),
		"gridPos":    gridPos(12, y, 12, liveFactsH),
		"id":         idLiveFacts,
		"options": map[string]any{
			"showHeader": true,
			"cellHeight": "sm",
			"footer": map[string]any{
				"show": false, "reducer": []any{"sum"}, "countRows": false, "fields": "",
			},
		},
		"fieldConfig": map[string]any{
			"defaults": map[string]any{
				"unit": "short", "decimals": 0,
				"custom":  map[string]any{"align": "auto", "cellOptions": map[string]any{"type": "auto"}, "inspect": false},
				"noValue": "kube-state-metrics is not reporting this machine",
			},
			"overrides": []any{map[string]any{
				"matcher":    map[string]any{"id": "byName", "options": "RAM"},
				"properties": []any{map[string]any{"id": "unit", "value": "bytes"}},
			}},
		},
		"targets": []any{
			map[string]any{
				"refId":      "A",
				"datasource": prom(),
				"expr": fmt.Sprintf(`max by (node, kernel_version, os_image, `+
					`container_runtime_version, kubelet_version) (kube_node_info{node=%q})`, node),
				"instant": true,
				"format":  "table",
			},
			map[string]any{
				"refId":      "B",
				"datasource": prom(),
				"expr": fmt.Sprintf(
					`max by (node) (kube_node_status_capacity{node=%q, resource="cpu"})`, node),
				"instant": true,
				"format":  "table",
			},
			map[string]any{
				"refId":      "C",
				"datasource": prom(),
				"expr": fmt.Sprintf(
					`max by (node) (kube_node_status_capacity{node=%q, resource="memory"})`, node),
				"instant": true,
				"format":  "table",
			},
		},
		"transformations": []any{
			map[string]any{
				"id":      "joinByField",
				"options": map[string]any{"byField": "node", "mode": "outer"},
			},
			map[string]any{
				"id": "organize",
				"options": map[string]any{
					// Value #A is kube_node_info's constant 1; the labels are the payload.
					"excludeByName": map[string]any{
						"Time": true, "Time 1": true, "Time 2": true, "Time 3": true,
						"node": true, "Value #A": true,
					},
					"indexByName": map[string]any{
						"Value #B": 0, "Value #C": 1, "kernel_version": 2,
						"os_image": 3, "container_runtime_version": 4, "kubelet_version": 5,
					},
					"renameByName": map[string]any{
						"Value #B":                  "Logical CPUs",
						"Value #C":                  "RAM",
						"kernel_version":            "Kernel",
						"os_image":                  "OS",
						"container_runtime_version": "Container runtime",
						"kubelet_version":           "Kubelet",
					},
				},
			},
		},
	}
}

// settingsPanel shows the settings that change how other numbers are computed; the
// allocation mode is baked into the title, the rest is live.
func settingsPanel(node string, m Machine, y int) map[string]any {
	return map[string]any{
		"type":  "table",
		"title": "How CPUs are counted here — allocation mode: " + allocationModeText(m.CPUAllocation),
		"description": "The settings that change how every other number here is computed. " +
			"Each core's second thread is `held-back` (left to host work), `reserved-pool` (it IS " +
			"the reserved pool), `allocatable` (a pinned pod can use it), or `no-siblings` (one " +
			"thread per core). The title's mode is the node label " +
			cpuAllocationLabel + "; changing it re-provisions the machine and regenerates " +
			"this page.",
		"datasource": prom(),
		"gridPos":    gridPos(12, y, 12, settingsH),
		"id":         idSettings,
		"options": map[string]any{
			"showHeader": true,
			"cellHeight": "sm",
			"footer": map[string]any{
				"show": false, "reducer": []any{"sum"}, "countRows": false, "fields": "",
			},
		},
		"fieldConfig": map[string]any{
			"defaults": map[string]any{
				"unit": "short", "decimals": 0,
				"custom":  map[string]any{"align": "auto", "cellOptions": map[string]any{"type": "auto"}, "inspect": false},
				"noValue": "not reported — this machine may not be provisioned",
			},
			"overrides": []any{},
		},
		"targets": []any{
			map[string]any{
				"refId":      "A",
				"datasource": prom(),
				"expr": fmt.Sprintf(
					`max by (node, policy) (cpu_pinning_ht_sibling_policy{node=%q} == 1)`, node),
				"instant": true,
				"format":  "table",
			},
			map[string]any{
				"refId":      "B",
				"datasource": prom(),
				"expr":       fmt.Sprintf(`max by (node) (cpu_pinning_node_reserved_cpus{node=%q})`, node),
				"instant":    true,
				"format":     "table",
			},
		},
		"transformations": []any{
			map[string]any{
				"id":      "joinByField",
				"options": map[string]any{"byField": "node", "mode": "outer"},
			},
			map[string]any{
				"id": "organize",
				"options": map[string]any{
					"excludeByName": map[string]any{
						"Time": true, "Time 1": true, "Time 2": true,
						"node": true, "Value #A": true,
					},
					"indexByName": map[string]any{"policy": 0, "Value #B": 1},
					"renameByName": map[string]any{
						"policy":   "Hyperthread siblings",
						"Value #B": "Reserved pool CPUs",
					},
				},
			},
		},
	}
}
