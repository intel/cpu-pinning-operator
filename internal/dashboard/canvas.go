// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package dashboard renders a per-node Grafana canvas dashboard: one box per
// physical core, grouped by NUMA node, each CPU cell coloured by its owner.
// Canvas elements are static, so the layout is generated from the node's own
// topology and written to a per-node ConfigMap the Grafana sidecar imports.
package dashboard

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// Pixel layout constants.
const (
	coreW      = 96 // width of one physical-core box
	subH       = 20 // height of one hyperthread half-cell
	captionH   = 14 // caption band under a core box (cpu ids)
	gapX       = 6  // gap between core boxes horizontally
	gapY       = 8  // gap between core rows
	numaTitleH = 24 // NUMA section title height
	numaGapY   = 16 // gap between NUMA blocks
	leftPad    = 8
	topPad     = 8

	// canvasW is the fixed canvas coordinate space; Grafana scales it to the panel.
	canvasW = 2000

	// coresPerRow is the number of core boxes per NUMA row.
	coresPerRow = 16
)

// Pool identity colours, shared by every panel on both dashboards and always
// applied as color.mode "fixed", never via thresholds. fleet-overview.json
// duplicates them by hand; test/chart keeps the two in step.
const (
	// ColorFree is mid-grey, visible in light and dark themes.
	ColorFree = "#4b5563"
	// ColorReserved is the shared pool everything unpinned runs in.
	ColorReserved = "dark-purple"
	// ColorPinned is palette-classic's first colour, the one the map gives a single pinned workload.
	ColorPinned = "#7EB26D"
)

// MaxDescriptionChars limits panel and variable descriptions, alert text and metric Help.
const MaxDescriptionChars = 400

// fixedColor pairs a non-workload owner token with a fixed colour; workloads use the classic palette.
type fixedColor struct {
	name  string
	color string
}

var fixedColors = []fixedColor{
	{"free", ColorFree},
	{"reserved", ColorReserved},
}

// identityColor is a field's fixed pool colour, never a threshold.
func identityColor(color string) map[string]any {
	return map[string]any{"mode": "fixed", "fixedColor": color}
}

// excludedLabel and excludedColor render a CPU outside the allocatable pool; it has
// no assignment series, so it is baked into the cell.
const (
	excludedLabel = "not pinnable"
	excludedColor = "#1f2937"
)

// ownerExpr is the per-CPU PromQL for one node, deriving an `owner` label:
//  1. seed with the full workload_pod (bare pods still get an owner);
//  2. replace with the Deployment prefix so replicas share a colour;
//  3. label free/reserved CPUs (empty workload_pod) by balloon;
//  4. cap the token length to fit the core box.
func ownerExpr(node string) string {
	inner := fmt.Sprintf(`cpu_pinning_cpu_assignment{node=%q}`, node)
	r0 := fmt.Sprintf(`label_replace(%s, "owner", "$1", "workload_pod", "^(.+)$")`, inner)
	r1 := fmt.Sprintf(`label_replace(%s, "owner", "$1", "workload_pod", "^(.+)-[a-z0-9]+-[a-z0-9]{5}$")`, r0)
	r2 := fmt.Sprintf(`label_replace(%s, "owner", "$1", "balloon", "^(free|reserved)$")`, r1)
	return fmt.Sprintf(`label_replace(%s, "owner", "${1}…", "owner", "^(.{12}).+$")`, r2)
}

func valueMappings() []any {
	opts := map[string]any{}
	for i, fc := range fixedColors {
		opts[fc.name] = map[string]any{"color": fc.color, "index": i}
	}
	return []any{map[string]any{"type": "value", "options": opts}}
}

// numaGroup holds a NUMA node's cores; core -> sorted sibling CPU ids.
type numaGroup struct {
	numa  int
	cores []coreGroup
}

type coreGroup struct {
	core int
	cpus []int
}

// groupByNUMA groups CPUs into NUMA -> cores -> siblings, sorted; siblings share
// a Core id, no "+N" numbering is assumed.
func groupByNUMA(cpus map[int]topology.CPUInfo) []numaGroup {
	byNUMA := map[int]map[int][]int{}
	for _, info := range cpus {
		if byNUMA[info.NUMA] == nil {
			byNUMA[info.NUMA] = map[int][]int{}
		}
		byNUMA[info.NUMA][info.Core] = append(byNUMA[info.NUMA][info.Core], info.CPU)
	}
	numas := make([]int, 0, len(byNUMA))
	for n := range byNUMA {
		numas = append(numas, n)
	}
	sort.Ints(numas)

	var out []numaGroup
	for _, n := range numas {
		coreIDs := make([]int, 0, len(byNUMA[n]))
		for c := range byNUMA[n] {
			coreIDs = append(coreIDs, c)
		}
		sort.Ints(coreIDs)
		cores := make([]coreGroup, 0, len(coreIDs))
		for _, c := range coreIDs {
			sib := byNUMA[n][c]
			sort.Ints(sib)
			cores = append(cores, coreGroup{core: c, cpus: sib})
		}
		out = append(out, numaGroup{numa: n, cores: cores})
	}
	return out
}

func headerText(name, text string, left, top, width, height, size int, align string) map[string]any {
	return map[string]any{
		"type": "text",
		"name": name,
		"config": map[string]any{
			"text":   map[string]any{"mode": "fixed", "fixed": text},
			"size":   size,
			"color":  map[string]any{"fixed": "text"},
			"align":  align,
			"valign": "middle",
		},
		"background": map[string]any{"color": map[string]any{"fixed": "transparent"}},
		"border":     map[string]any{"color": map[string]any{"fixed": "transparent"}},
		"placement":  map[string]any{"top": top, "left": left, "width": width, "height": height},
		"constraint": map[string]any{"horizontal": "left", "vertical": "top"},
	}
}

// buildElements lays out the core boxes and returns the elements and pixel height.
// A CPU outside pinnable gets a fixed cell, since it has no assignment series.
func buildElements(groups []numaGroup, pinnable map[int]bool) ([]any, int) {
	var elements []any
	y := topPad

	// The max sibling count sets a uniform box height.
	maxThreads := 1
	for _, g := range groups {
		for _, c := range g.cores {
			if len(c.cpus) > maxThreads {
				maxThreads = len(c.cpus)
			}
		}
	}
	coreBoxH := maxThreads*subH + captionH

	for _, g := range groups {
		elements = append(elements, headerText(
			fmt.Sprintf("numa%d-title", g.numa), fmt.Sprintf("NUMA %d", g.numa),
			leftPad, y, 240, numaTitleH, 15, "left"))
		blockTop := y + numaTitleH
		rows := (len(g.cores) + coresPerRow - 1) / coresPerRow

		for k, c := range g.cores {
			row := k / coresPerRow
			col := k % coresPerRow
			x := leftPad + col*(coreW+gapX)
			boxTop := blockTop + row*(coreBoxH+gapY)

			for t, cpu := range c.cpus {
				field := strconv.Itoa(cpu)
				cellTop := boxTop + t*subH
				text := map[string]any{"field": field, "mode": "field"}
				background := map[string]any{"color": map[string]any{"field": field}}
				if pinnable != nil && !pinnable[cpu] {
					text = map[string]any{"mode": "fixed", "fixed": excludedLabel}
					background = map[string]any{"color": map[string]any{"fixed": excludedColor}}
				}
				elements = append(elements, map[string]any{
					"type": "metric-value",
					"name": "cpu" + field,
					"config": map[string]any{
						"text":   text,
						"size":   10,
						"color":  map[string]any{"fixed": "#ffffff"},
						"align":  "center",
						"valign": "middle",
					},
					"background": background,
					"border":     map[string]any{"color": map[string]any{"fixed": "dark-gray"}, "width": 1},
					"placement":  map[string]any{"top": cellTop, "left": x, "width": coreW, "height": subH},
					"constraint": map[string]any{"horizontal": "left", "vertical": "top"},
				})
			}
			// caption: core id + the sibling CPU ids in this box
			cpuIDs := ""
			for i, cpu := range c.cpus {
				if i > 0 {
					cpuIDs += "/"
				}
				cpuIDs += strconv.Itoa(cpu)
			}
			elements = append(elements, headerText(
				fmt.Sprintf("core%d-cap", c.core),
				fmt.Sprintf("core %d  cpu %s", c.core, cpuIDs),
				x, boxTop+len(c.cpus)*subH, coreW, captionH, 9, "center"))
		}
		blockH := numaTitleH + rows*(coreBoxH+gapY)
		y = blockTop + (blockH - numaTitleH) + numaGapY
	}
	return elements, y + topPad
}

// minCanvasGridH is the floor on the canvas panel's grid height.
const minCanvasGridH = 8

// Grafana's dashboard grid, for canvasGridH. One row of slack: a short panel clips the map.
const (
	gridRowPxH     = 30
	gridRowGapPxH  = 8
	panelChromePxH = 48
)

func canvasGridH(pxH int) int {
	// h*gridRowPxH + (h-1)*gridRowGapPxH - panelChromePxH >= pxH, rounded up.
	perRow := gridRowPxH + gridRowGapPxH
	need := pxH + panelChromePxH + gridRowGapPxH
	h := (need+perRow-1)/perRow + 1
	if h < minCanvasGridH {
		h = minCanvasGridH
	}
	return h
}

// buildPanel assembles the canvas panel for a node, starting at grid row y.
func buildPanel(node string, groups []numaGroup, pinnable map[int]bool, y int) map[string]any {
	elements, pxH := buildElements(groups, pinnable)
	gridH := canvasGridH(pxH)
	return map[string]any{
		"id":    idCanvas,
		"type":  "canvas",
		"title": "Which pod owns which CPU on this machine",
		// The elements are unlabelled, so the description explains the map.
		"description": "This machine's CPUs as the hardware is laid out: one box per physical " +
			"core with its hyperthread siblings stacked inside, grouped by NUMA node. A cell's " +
			"colour is its owner — grey free, near-black not pinnable, purple the reserved " +
			"pool, anything else the pod named in the cell. Healthy is pinned pods in " +
			"contiguous blocks in one NUMA node. Rebuilt from the topology CR, so it lags " +
			"a pod's start.",
		"datasource": map[string]any{"type": "prometheus", "uid": "${datasource}"},
		"gridPos":    map[string]any{"h": gridH, "w": 24, "x": 0, "y": y},
		"options": map[string]any{
			"inlineEditing":     false,
			"showAdvancedTypes": true,
			"panZoom":           true,
			"infinitePan":       false,
			"root": map[string]any{
				"type":       "frame",
				"name":       "root",
				"elements":   elements,
				"background": map[string]any{"color": map[string]any{"fixed": "transparent"}},
				"border":     map[string]any{"color": map[string]any{"fixed": "transparent"}},
				"constraint": map[string]any{"horizontal": "left", "vertical": "top"},
				"placement":  map[string]any{"top": 0, "left": 0, "width": canvasW, "height": pxH},
			},
		},
		"targets": []any{map[string]any{
			"refId":      "A",
			"datasource": map[string]any{"type": "prometheus", "uid": "${datasource}"},
			"expr":       ownerExpr(node),
			"instant":    true,
			"format":     "table",
		}},
		"transformations": []any{
			map[string]any{ // keep only cpu (field name) + owner (field value)
				"id": "organize",
				"options": map[string]any{
					"excludeByName": map[string]any{
						"Time": true, "__name__": true, "job": true,
						"instance": true, "endpoint": true, "container": true,
						"prometheus": true, "service": true, "pod_ip": true,
						"Value": true, "core": true, "numa": true, "node": true,
						"workload_namespace": true, "workload_pod": true, "balloon": true,
						// Drop the scrape target's labels so only cpu + owner survive the pivot.
						"pod": true, "namespace": true,
					},
				},
			},
			map[string]any{ // pivot: one field per CPU, value = the owner token
				"id": "rowsToFields",
				"options": map[string]any{
					"mappings": []any{
						map[string]any{"fieldName": "cpu", "handlerKey": "field.name"},
						map[string]any{"fieldName": "owner", "handlerKey": "field.value"},
					},
				},
			},
		},
		"fieldConfig": map[string]any{
			"defaults": map[string]any{
				"color":    map[string]any{"mode": "palette-classic"},
				"mappings": valueMappings(),
				"custom":   map[string]any{},
			},
			"overrides": []any{},
		},
	}
}

// pinnedDatasource is a datasource variable's `current`: `{}` for an empty uid
// (Grafana auto-selects). A pin sets only `value`: Grafana reads `text` as the
// data source NAME, which a uid is not.
func pinnedDatasource(uid string) map[string]any {
	if uid == "" {
		return map[string]any{}
	}
	return map[string]any{"value": uid, "selected": true}
}

// datasourceVar builds the Prometheus `datasource` template variable; an empty uid auto-selects.
func datasourceVar(uid string) map[string]any {
	current := pinnedDatasource(uid)
	return map[string]any{
		"name": "datasource", "label": "Data source", "type": "datasource",
		"query": "prometheus", "hide": 0,
		"current": current,
	}
}

// RenderDashboardJSON builds the Grafana dashboard JSON for one node from its
// per-CPU topology. machine carries facts no metric reports; ds picks the data
// sources (ds.Loki adds the log section). It errors if the node has no cached CPUs.
//
// Page reading order (a new panel goes in its section, not at the end; the
// glossary panel repeats it):
//
//  0. Glossary, collapsed.
//  1. What this machine is.
//  2. The CPU map.
//  3. This machine's numbers.
//  4. Who shares which CPUs.
//  5. Expert detail.
func RenderDashboardJSON(node string, ds Datasources, machine Machine, cpus map[int]topology.CPUInfo) ([]byte, error) {
	if len(cpus) == 0 {
		return nil, fmt.Errorf("no CPU topology for node %q", node)
	}
	groups := groupByNUMA(cpus)
	shp := shapeOf(groups, machine.PinnableCPUs)

	// Section rows; the collapsed glossary row costs one grid row.
	const glossaryY = 0
	identityY := glossaryY + 1
	mapRowY := identityY + identityRowH
	canvasY := mapRowY + 1

	canvas := buildPanel(node, groups, machine.PinnableCPUs, canvasY)
	// The numbers start under the canvas; floored so panels never stack on the map.
	canvasH, ok := canvas["gridPos"].(map[string]any)["h"].(int)
	if !ok || canvasH < minCanvasGridH {
		canvasH = minCanvasGridH
	}

	panels := []any{glossaryRow(idGlossaryRow, idGlossaryText, glossaryY)}
	panels = append(panels, machineIdentityPanels(node, machine, shp, identityY)...)
	panels = append(panels, row(idMapRow, "This machine's CPU map", mapRowY), canvas)
	metrics := nodePanels(node, machine, shp, canvasY+canvasH)
	panels = append(panels, metrics...)
	// Logs last, and only when they can resolve.
	if ds.Loki.on() {
		panels = append(panels, machineLogPanels(nextFreeRow(metrics))...)
	}

	vars := []any{datasourceVar(ds.PrometheusUID)}
	if ds.Loki.on() {
		vars = append(vars, lokiVar(ds.Loki.DatasourceUID), logSourceVar(node, ds.Loki), benignFilterVar())
	}
	dash := map[string]any{
		"annotations":   map[string]any{"list": []any{}},
		"editable":      true,
		"graphTooltip":  0,
		"title":         "CPU Pinning — machine: " + node,
		"uid":           DashboardUID(node),
		"tags":          []any{"cpu-pinning-operator"},
		"schemaVersion": 39,
		"version":       1,
		"time":          map[string]any{"from": "now-6h", "to": "now"},
		// Back-link to the cluster dashboard, keeping the same datasource.
		"links": []any{map[string]any{
			"title":       "Back to the cluster overview (all machines)",
			"type":        "link",
			"icon":        "dashboard",
			"url":         "/d/cpu-pinning-fleet?var-datasource=${datasource}",
			"targetBlank": false,
			"tooltip":     "Cluster-wide tiles and the table of all managed machines",
		}},
		"templating": map[string]any{"list": vars},
		"panels":     panels,
	}
	return json.MarshalIndent(dash, "", "  ")
}

// Datasources is the Prometheus uid and Loki settings a page's panels read from.
type Datasources struct {
	PrometheusUID string
	Loki          LokiConfig
}

// nextFreeRow is the first grid row below every panel in panels.
func nextFreeRow(panels []any) int {
	end := 0
	for _, item := range panels {
		p, ok := item.(map[string]any)
		if !ok {
			continue
		}
		g, ok := p["gridPos"].(map[string]any)
		if !ok {
			continue
		}
		y, yOK := g["y"].(int)
		h, hOK := g["h"].(int)
		if yOK && hOK && y+h > end {
			end = y + h
		}
	}
	return end
}
