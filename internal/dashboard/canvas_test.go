// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// twoNumaFourCoreHT builds 2 NUMA nodes × 2 cores × 2 threads; sibling of c is c+4.
func twoNumaFourCoreHT() map[int]topology.CPUInfo {
	return map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0},
		1: {CPU: 1, NUMA: 0, Core: 1},
		2: {CPU: 2, NUMA: 1, Core: 2},
		3: {CPU: 3, NUMA: 1, Core: 3},
		4: {CPU: 4, NUMA: 0, Core: 0}, // sibling of cpu0
		5: {CPU: 5, NUMA: 0, Core: 1}, // sibling of cpu1
		6: {CPU: 6, NUMA: 1, Core: 2}, // sibling of cpu2
		7: {CPU: 7, NUMA: 1, Core: 3}, // sibling of cpu3
	}
}

// testMachine is a realistic two-socket machine's baked facts.
func testMachine() Machine {
	return Machine{
		MemoryPerNUMABytes: map[int]int64{0: 270088753152, 1: 270088753152},
		PackageTopology:    "1_1",
		MaxSpan:            2,
		NodeClass:          "cpu-amx",
		AMX:                true,
		CPUAllocation:      "physical-cores",
		ReservedCPUs:       2,
	}
}

func TestGroupByNUMA(t *testing.T) {
	groups := groupByNUMA(twoNumaFourCoreHT())
	if len(groups) != 2 {
		t.Fatalf("expected 2 NUMA groups, got %d", len(groups))
	}
	if groups[0].numa != 0 || groups[1].numa != 1 {
		t.Fatalf("NUMA groups not sorted ascending: %d, %d", groups[0].numa, groups[1].numa)
	}
	// NUMA 0 cores 0,1 each with two sorted siblings.
	if len(groups[0].cores) != 2 {
		t.Fatalf("NUMA 0 expected 2 cores, got %d", len(groups[0].cores))
	}
	c0 := groups[0].cores[0]
	if c0.core != 0 || len(c0.cpus) != 2 || c0.cpus[0] != 0 || c0.cpus[1] != 4 {
		t.Fatalf("core 0 siblings wrong: %+v", c0)
	}
	// NUMA 1 core 2 groups cpu 2 and 6.
	c2 := groups[1].cores[0]
	if c2.core != 2 || c2.cpus[0] != 2 || c2.cpus[1] != 6 {
		t.Fatalf("core 2 siblings wrong: %+v", c2)
	}
}

// TestDashboardUIDFitsGrafana pins the node-name headroom under Grafana's 40-character
// UID cap, and keeps per-node UIDs out of the fleet dashboard's "cpu-pinning-fleet".
func TestDashboardUIDFitsGrafana(t *testing.T) {
	if uid := DashboardUID(strings.Repeat("n", 27)); len(uid) > 40 {
		t.Errorf("a 27-character node name gives a %d-character UID %q, want at most 40", len(uid), uid)
	}
	if strings.HasPrefix("cpu-pinning-fleet", DashboardUID("")) {
		t.Errorf("prefix %q lets a node named after the fleet dashboard take its UID", DashboardUID(""))
	}
}

func TestRenderDashboardJSON(t *testing.T) {
	raw, err := RenderDashboardJSON("worker-1", Datasources{}, testMachine(), twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if dash["uid"] != "cpu-pin-node-worker-1" {
		t.Errorf("uid = %v, want cpu-pin-node-worker-1", dash["uid"])
	}
	panels, _ := dash["panels"].([]any)
	if len(panels) < 2 {
		t.Fatalf("expected the canvas panel plus metric panels, got %d panel(s)", len(panels))
	}
	panel := canvasPanel(t, panels)
	// query must be pinned to this node
	targets := panel["targets"].([]any)
	expr := targets[0].(map[string]any)["expr"].(string)
	if want := `node="worker-1"`; !containsStr(expr, want) {
		t.Errorf("expr not pinned to node: %q missing %q", expr, want)
	}

	// An empty datasourceUID must leave `current` empty so Grafana auto-selects.
	tmpl := dash["templating"].(map[string]any)["list"].([]any)
	dsVar := tmpl[0].(map[string]any)
	if dsVar["name"] != "datasource" {
		t.Fatalf("first template var = %v, want datasource", dsVar["name"])
	}
	if dsVar["type"] != "datasource" || dsVar["query"] != "prometheus" {
		t.Errorf("datasource var not a prometheus datasource picker: type=%v query=%v", dsVar["type"], dsVar["query"])
	}
	cur, ok := dsVar["current"].(map[string]any)
	if !ok {
		t.Fatalf("datasource var current is not an object: %v", dsVar["current"])
	}
	if len(cur) != 0 {
		t.Errorf("datasource var current = %v, want empty so Grafana auto-selects the default Prometheus datasource", cur)
	}

	root := panel["options"].(map[string]any)["root"].(map[string]any)
	elements := root["elements"].([]any)
	// 8 metric-value cells + 2 NUMA titles + 4 core captions = 14
	var cells, texts int
	cpuNames := map[string]bool{}
	for _, e := range elements {
		el := e.(map[string]any)
		switch el["type"] {
		case "metric-value":
			cells++
			cpuNames[el["name"].(string)] = true
		case "text":
			texts++
		}
	}
	if cells != 8 {
		t.Errorf("metric-value cells = %d, want 8", cells)
	}
	if texts != 6 {
		t.Errorf("text elements = %d, want 6 (2 titles + 4 captions)", texts)
	}
	for _, want := range []string{"cpu0", "cpu4", "cpu2", "cpu6"} {
		if !cpuNames[want] {
			t.Errorf("missing cell %s", want)
		}
	}
}

// canvasPanel returns the one canvas panel among a dashboard's top-level panels.
func canvasPanel(t *testing.T, panels []any) map[string]any {
	t.Helper()
	for _, item := range panels {
		p, ok := item.(map[string]any)
		if ok && p["type"] == "canvas" {
			return p
		}
	}
	t.Fatal("dashboard has no canvas panel")
	return nil
}

// TestNodeDashboardReadingOrder proves the order: glossary, machine identity, then the core map.
func TestNodeDashboardReadingOrder(t *testing.T) {
	raw, err := RenderDashboardJSON("worker-1", Datasources{}, testMachine(), twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}
	panels, _ := dash["panels"].([]any)
	if len(panels) < 4 {
		t.Fatalf("expected the full page, got %d top-level panel(s)", len(panels))
	}

	first := panels[0].(map[string]any)
	if first["type"] != "row" || first["collapsed"] != true {
		t.Errorf("first panel = %v (collapsed %v), want a collapsed row", first["type"], first["collapsed"])
	}
	if title, _ := first["title"].(string); !containsStr(title, "How to read this page") {
		t.Errorf("first row = %q, want the glossary row", title)
	}
	if y := first["gridPos"].(map[string]any)["y"]; y != float64(0) {
		t.Errorf("glossary row y = %v, want 0", y)
	}

	// The first panel carrying data, glossary aside, must be the machine identity.
	var firstData map[string]any
	for _, item := range panels[1:] {
		p := item.(map[string]any)
		if p["type"] == "row" {
			continue
		}
		firstData = p
		break
	}
	if firstData == nil {
		t.Fatal("the page has no data panel after the glossary row")
	}
	if title, _ := firstData["title"].(string); !containsStr(title, "as the operator measured it") {
		t.Errorf("first data panel = %q, want the machine-identity panel", title)
	}

	// The map follows the identity section; nothing precedes the identity panel.
	canvas := canvasPanel(t, panels)
	canvasY := canvas["gridPos"].(map[string]any)["y"].(float64)
	identityY := firstData["gridPos"].(map[string]any)["y"].(float64)
	if !(identityY < canvasY) {
		t.Errorf("identity panel y=%v is not above the core map y=%v", identityY, canvasY)
	}
	for _, item := range panels {
		p := item.(map[string]any)
		if p["type"] == "row" {
			continue
		}
		if y := p["gridPos"].(map[string]any)["y"].(float64); y < identityY {
			t.Errorf("panel %q sits above the machine-identity panel (y=%v)", p["title"], y)
		}
	}
}

// TestMachineIdentityPanel proves baked facts reach the page and the live half reads Prometheus.
func TestMachineIdentityPanel(t *testing.T) {
	raw, err := RenderDashboardJSON("worker-1", Datasources{}, testMachine(), twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	// Baked: shape derived from the topology, plus what only the operator knows.
	for _, want := range []string{
		"NUMA nodes", "Physical cores", "Threads per core", "Logical CPUs",
		"RAM per NUMA node", "251.5 GiB", "cpu-amx", "AMX (amx_tile)",
		"1_1 (NUMA nodes per socket)", "2 NUMA nodes",
	} {
		if !containsStr(string(raw), want) {
			t.Errorf("the machine-identity panel does not state %q", want)
		}
	}
	// Live: kube-state-metrics capacity and versions, and the operator's own gauges.
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}
	var exprs string
	for _, p := range flatten(dash["panels"]) {
		targets, _ := p["targets"].([]any)
		for _, tg := range targets {
			expr, _ := tg.(map[string]any)["expr"].(string)
			exprs += expr + "\n"
		}
	}
	for _, want := range []string{
		`kube_node_info{node="worker-1"}`,
		`kube_node_status_capacity{node="worker-1", resource="cpu"}`,
		`kube_node_status_capacity{node="worker-1", resource="memory"}`,
		`max by (node) (cpu_pinning_node_reserved_cpus{node="worker-1"})`,
		`max by (node, policy) (cpu_pinning_ht_sibling_policy{node="worker-1"} == 1)`,
	} {
		if !containsStr(exprs, want) {
			t.Errorf("no panel queries %q; the live half of the identity section is missing it", want)
		}
	}
}

// TestMemoryPerNUMAText proves slightly different NUMA memory is shown as a range, not labelled an imbalance.
func TestMemoryPerNUMAText(t *testing.T) {
	for _, tc := range []struct {
		name string
		mem  map[int]int64
		want string
	}{
		{"equal", map[int]int64{0: 270088753152, 1: 270088753152}, "251.5 GiB"},
		{"firmware skew", map[int]int64{0: 270088753152, 1: 270518870016}, "251.5 GiB – 251.9 GiB"},
		{"none", nil, notReported},
	} {
		if got := memoryPerNUMAText(tc.mem); got != tc.want {
			t.Errorf("%s: memoryPerNUMAText = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestMachineFactsMarkdownMissing proves every missing fact says so instead of rendering blank.
func TestMachineFactsMarkdownMissing(t *testing.T) {
	md := machineFactsMarkdown(Machine{}, shapeOf(groupByNUMA(twoNumaFourCoreHT()), nil))
	if n := countStr(md, notReported); n < 3 {
		t.Errorf("only %d fact(s) rendered as %q; missing memory, class, span and package "+
			"topology must all say so:\n%s", n, notReported, md)
	}
	if !containsStr(md, "none found by the probe") {
		t.Errorf("a machine without ISA features must say so:\n%s", md)
	}
}

// TestNodeDashboardMetricPanels proves the metric panels are scoped to this node,
// carry descriptions, and link back to the cluster page.
func TestNodeDashboardMetricPanels(t *testing.T) {
	raw, err := RenderDashboardJSON("worker-1", Datasources{}, testMachine(), twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	panels := flatten(dash["panels"])
	types := map[string]int{}
	ids := map[float64]string{}
	for _, p := range panels {
		typ, _ := p["type"].(string)
		types[typ]++

		id, ok := p["id"].(float64)
		if !ok {
			t.Errorf("panel %q has no id", p["title"])
		} else if prev, dup := ids[id]; dup {
			t.Errorf("panel id %v used twice (%q and %q); Grafana drops duplicates", id, prev, p["title"])
		} else {
			ids[id] = fmt.Sprint(p["title"])
		}

		// Rows are section headers; Grafana shows no description for them.
		if typ != "row" {
			if desc, _ := p["description"].(string); len(desc) == 0 {
				t.Errorf("panel %q (%s) has no description", p["title"], typ)
			}
		}

		targets, _ := p["targets"].([]any)
		for _, tg := range targets {
			expr, _ := tg.(map[string]any)["expr"].(string)
			if expr == "" {
				continue
			}
			if !containsStr(expr, `node="worker-1"`) {
				t.Errorf("panel %q query is not scoped to this machine: %s", p["title"], expr)
			}
		}
	}

	// Five tables, three gauges, two text panels; no piechart or timeseries.
	for typ, want := range map[string]int{
		"canvas": 1, "bargauge": 1, "gauge": 3, "table": 5,
		"piechart": 0, "timeseries": 0, "text": 2,
	} {
		if types[typ] != want {
			t.Errorf("panel type %q appears %d time(s), want %d", typ, types[typ], want)
		}
	}
	// The stat tiles, including both container counts.
	if types["stat"] != 7 {
		t.Errorf("stat panels = %d, want 7", types["stat"])
	}

	// No metric carries the mode, so it is baked in.
	var modeTitle string
	for _, p := range panels {
		if title, _ := p["title"].(string); containsStr(title, "allocation mode") {
			modeTitle = title
		}
	}
	if !containsStr(modeTitle, "physical-cores") {
		t.Errorf("no panel states the resolved CPU-allocation mode; got title %q", modeTitle)
	}

	// The pinned listing reads workload_* labels and excludes the shared pool and idle CPUs.
	var tableExpr string
	for _, p := range panels {
		title, _ := p["title"].(string)
		if p["type"] == "table" && containsStr(title, "Holding dedicated CPUs") {
			targets, _ := p["targets"].([]any)
			tableExpr, _ = targets[0].(map[string]any)["expr"].(string)
		}
	}
	for _, want := range []string{
		"workload_namespace", "workload_pod", "workload_container", `balloon!~"free|reserved"`,
	} {
		if !containsStr(tableExpr, want) {
			t.Errorf("pinned listing query %q is missing %q", tableExpr, want)
		}
	}
	// No panel may still list the pods a second time at a coarser grain.
	for _, p := range panels {
		if title, _ := p["title"].(string); containsStr(title, "Pods pinned on this machine") {
			t.Error("\"Pods pinned on this machine\" is back; the pinned listing already states " +
				"every pod it did, plus the balloon and the container")
		}
	}

	// The cluster dashboard links here by uid; this page links back.
	links, _ := dash["links"].([]any)
	if len(links) == 0 {
		t.Fatal("per-node dashboard has no link back to the cluster dashboard")
	}
	url, _ := links[0].(map[string]any)["url"].(string)
	if !containsStr(url, "/d/cpu-pinning-fleet") {
		t.Errorf("back-link url = %q, want it to resolve to /d/cpu-pinning-fleet", url)
	}
}

// TestPoolMembershipSection proves both pools are listed from the per-container
// metric, scoped to this node with max, and the reserved title names its CPUs.
func TestPoolMembershipSection(t *testing.T) {
	// 5 pinnable, 2 reserved, 8 logical: distinct numbers pin the dedicated gauge's denominator.
	m := testMachine()
	m.PinnableCPUs = map[int]bool{0: true, 1: true, 2: true, 4: true, 5: true}
	raw, err := RenderDashboardJSON("worker-1", Datasources{}, m, twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}
	panels := flatten(dash["panels"])

	find := func(want string) map[string]any {
		t.Helper()
		for _, p := range panels {
			if title, _ := p["title"].(string); containsStr(title, want) {
				return p
			}
		}
		t.Fatalf("no panel titled %q", want)
		return nil
	}

	shared := find("Sharing the reserved pool CPUs")
	pinned := find("Holding dedicated CPUs")
	meter := find("Reserved pool usage")
	count := find("Containers sharing")

	// testMachine reserves 2 of 8 CPUs; titles carry the baked denominators.
	if title, _ := shared["title"].(string); !containsStr(title, "(2 of them)") {
		t.Errorf("the reserved listing's title %q does not name the pool its rows share", title)
	}
	if title, _ := count["title"].(string); !containsStr(title, "those 2 CPUs") {
		t.Errorf("the count tile's title %q does not say what the containers are sharing", title)
	}
	// The container count is a titled panel, not a countRows footer.
	footer, _ := shared["options"].(map[string]any)["footer"].(map[string]any)
	if footer["countRows"] == true {
		t.Error("the reserved listing still uses countRows, which puts an unlabelled COUNT " +
			"under the Namespace column")
	}
	if footer["show"] != true || fmt.Sprint(footer["fields"]) != "["+usedColumn+"]" {
		t.Errorf("the reserved listing's footer is %v; the total belongs under %q, where it is "+
			"the pool's usage against the size in the title", footer, usedColumn)
	}
	if expr, _ := count["targets"].([]any)[0].(map[string]any)["expr"].(string); !containsStr(
		expr, "count(max by (workload_namespace, workload_pod, workload_container)") {
		t.Errorf("the count tile does not count pool members: %s", expr)
	}

	for label, p := range map[string]map[string]any{"reserved": shared, "pinned": pinned} {
		if p["type"] != "table" {
			t.Errorf("%s listing is a %v, want a table", label, p["type"])
		}
		targets, _ := p["targets"].([]any)
		if len(targets) != 2 {
			t.Fatalf("%s listing has %d queries, want the listing plus the usage column",
				label, len(targets))
		}
		expr, _ := targets[0].(map[string]any)["expr"].(string)
		if !containsStr(expr, "cpu_pinning_pool_membership") {
			t.Errorf("%s listing does not read cpu_pinning_pool_membership: %s", label, expr)
		}
		// Only the leader exports the NRT gauges, but a standby may start: max, not sum.
		if !containsStr(expr, "max by (") || containsStr(expr, "sum by (") {
			t.Errorf("%s listing must collapse the gauge with max by, not sum by: %s", label, expr)
		}
		// workload_* labels are not shadowed by the scrape target.
		for _, want := range []string{"workload_namespace", "workload_pod", "workload_container"} {
			if !containsStr(expr, want) {
				t.Errorf("%s listing query is missing %q: %s", label, want, expr)
			}
		}
		// Usage is joined on one PromQL-built key: joinByField takes one field.
		usage, _ := targets[1].(map[string]any)["expr"].(string)
		if !containsStr(usage, "container_cpu_usage_seconds_total") {
			t.Errorf("%s listing has no cores-used query: %s", label, usage)
		}
		if !containsStr(usage, "[$__range]") || containsStr(usage, "[$__interval]") {
			t.Errorf("%s usage query must average over [$__range]; [$__interval] is ~30s at a 6h "+
				"range and returns nothing: %s", label, usage)
		}
		// Usage is narrowed to pool members: cAdvisor keeps series for gone containers.
		if !containsStr(usage, "and on (namespace, pod, container)") ||
			!containsStr(usage, "cpu_pinning_pool_membership") {
			t.Errorf("%s usage query is not intersected with the pool's members, so a departed "+
				"container would join as a row with no pod: %s", label, usage)
		}
		// Millicores, so idle (0 mCPU) differs from missing (empty).
		if !containsStr(usage, ") * 1000") {
			t.Errorf("%s usage query is in cores, not the millicores the column is headed with: "+
				"%s", label, usage)
		}
		defs := p["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
		if defs["decimals"] != float64(0) {
			t.Errorf("%s listing shows %v decimals; millicores are integers and 0.000 is false "+
				"precision", label, defs["decimals"])
		}
		// Absent must stay distinguishable from idle.
		if nv, _ := defs["noValue"].(string); nv == "" || containsStr(nv, "0") {
			t.Errorf("%s listing empty state = %q; a missing scrape must not read as a zero",
				label, nv)
		}
		// Truncated pod names make two replicas of one Deployment indistinguishable.
		var widened []string
		for _, o := range p["fieldConfig"].(map[string]any)["overrides"].([]any) {
			m := o.(map[string]any)
			for _, prop := range m["properties"].([]any) {
				if prop.(map[string]any)["id"] == "custom.width" {
					widened = append(widened, m["matcher"].(map[string]any)["options"].(string))
				}
			}
		}
		for _, want := range []string{"Pod", "Container"} {
			found := false
			for _, got := range widened {
				if got == want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s listing gives the %s column no width, so a long name truncates "+
					"mid-name; widened: %v", label, want, widened)
			}
		}
		if !containsStr(expr, `"key"`) || !containsStr(usage, `"key"`) {
			t.Errorf("%s listing and its usage query do not share the join key: %s / %s",
				label, expr, usage)
		}
		var joined bool
		for _, tr := range p["transformations"].([]any) {
			m := tr.(map[string]any)
			if m["id"] == "joinByField" &&
				m["options"].(map[string]any)["byField"] == "key" {
				joined = true
			}
		}
		if !joined {
			t.Errorf("%s listing does not join its two frames on the container key", label)
		}
	}

	// The shared cpuset size is in the title, not a column; drop both "Value" and "Value #A".
	var sharedExclude map[string]any
	for _, tr := range shared["transformations"].([]any) {
		m := tr.(map[string]any)
		if m["id"] == "organize" {
			sharedExclude, _ = m["options"].(map[string]any)["excludeByName"].(map[string]any)
		}
	}
	for _, want := range []string{"Value", "Value #A"} {
		if sharedExclude[want] != true {
			t.Errorf("the reserved listing keeps %q, which is the same cpuset size on every row; "+
				"the pool's CPU count belongs in the title", want)
		}
	}
	if sort, _ := shared["options"].(map[string]any)["sortBy"].([]any); len(sort) == 0 ||
		sort[0].(map[string]any)["displayName"] != usedColumn ||
		sort[0].(map[string]any)["desc"] != true {
		t.Errorf("the reserved listing is not sorted by usage descending (%v), so the container "+
			"driving the pool is not the top row", sort)
	}

	// The pinned side keeps both numbers: dedicated vs used is the waste signal.
	var pinnedRename map[string]any
	for _, tr := range pinned["transformations"].([]any) {
		m := tr.(map[string]any)
		if m["id"] == "organize" {
			pinnedRename, _ = m["options"].(map[string]any)["renameByName"].(map[string]any)
		}
	}
	for _, want := range []string{"Dedicated CPUs", usedColumn} {
		found := false
		for _, got := range pinnedRename {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the pinned listing has no %q column; %v", want, pinnedRename)
		}
	}

	// Both gauges share bands; each measures its own pool against its own capacity.
	pinnedMeter := find("Dedicated CPUs occupied")
	pinnedCount := find("Containers holding them")
	// The pool colours belong to the count tiles; the gauges are coloured by their bands.
	wantColor := map[string]string{"reserved": ColorReserved, "dedicated": ColorPinned}
	for label, g := range map[string]map[string]any{"reserved": meter, "dedicated": pinnedMeter} {
		if g["type"] != "gauge" {
			t.Errorf("the %s gauge is a %v", label, g["type"])
		}
		defs := g["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
		opts, _ := g["options"].(map[string]any)
		// Threshold colouring, ring drawn, scale from zero.
		if color, _ := defs["color"].(map[string]any); color["mode"] != "thresholds" {
			t.Errorf("the %s gauge's colour mode is %v, want thresholds", label, color["mode"])
		}
		if opts["showThresholdMarkers"] != true {
			t.Errorf("the %s gauge does not draw its ring, so the arc carries no scale", label)
		}
		if defs["min"] != float64(0) {
			t.Errorf("the %s gauge's min is %v, want 0", label, defs["min"])
		}
		// A whole count against the pool, never a percentage.
		if defs["unit"] != "short" || defs["decimals"] != float64(0) {
			t.Errorf("the %s gauge is unit=%v decimals=%v, want a whole number", label,
				defs["unit"], defs["decimals"])
		}
		// Percentage bands serve both baked and live capacities.
		thr, _ := defs["thresholds"].(map[string]any)
		if thr["mode"] != "percentage" {
			t.Errorf("the %s gauge's bands are %v-mode; absolute steps cannot follow a pool "+
				"whose size arrives with the data", label, thr["mode"])
		}
		if !sameSteps(thr, poolUsageSteps()) {
			t.Errorf("the %s gauge's bands are %v, want poolUsageSteps() = %v", label, thr,
				poolUsageSteps())
		}
		if nv, _ := defs["noValue"].(string); nv == "" {
			t.Errorf("the %s gauge has no empty state", label)
		}
	}
	// The reserved gauge's value is CPU time inside the fixed cpuset, in millicores.
	reservedExpr, _ := meter["targets"].([]any)[0].(map[string]any)["expr"].(string)
	for _, want := range []string{
		"container_cpu_usage_seconds_total", "cpu_pinning_pool_membership",
		"and on (namespace, pod, container)", ") * 1000",
	} {
		if !containsStr(reservedExpr, want) {
			t.Errorf("the reserved gauge's query is missing %q: %s", want, reservedExpr)
		}
	}
	// The baked reserved size is in the title: testMachine reserves 2 CPUs.
	if title, _ := meter["title"].(string); !containsStr(title, "of 2000 mCPU") {
		t.Errorf("the reserved gauge's title %q does not name the pool it is drawn against",
			title)
	}
	if mx := meter["fieldConfig"].(map[string]any)["defaults"].(map[string]any)["max"]; mx != float64(2000) {
		t.Errorf("the reserved gauge's max is %v, want 2000 — the pool in millicores, so the "+
			"fill is how much of it is in use", mx)
	}
	// The dedicated gauge is occupancy, not CPU time.
	pmTargets, _ := pinnedMeter["targets"].([]any)
	if len(pmTargets) != 1 {
		t.Fatalf("the dedicated gauge has %d queries, want just the occupancy count",
			len(pmTargets))
	}
	occupied, _ := pmTargets[0].(map[string]any)["expr"].(string)
	// Distinct CPUs: summing shared cpusets double-counts.
	if !containsStr(occupied, "count(max by (cpu) (cpu_pinning_cpu_assignment{") {
		t.Errorf("the dedicated gauge does not count the distinct CPUs assigned to workload "+
			"balloons: %s", occupied)
	}
	for _, banned := range []string{"container_cpu_usage_seconds_total", "* 1000"} {
		if containsStr(occupied, banned) {
			t.Errorf("the dedicated gauge's value contains %q, so it is CPU time or millicores "+
				"rather than the CPUs occupied: %s", banned, occupied)
		}
	}
	// The max is the baked pinnable pool, also in the title; without one the arc is always full.
	pmDefs := pinnedMeter["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
	if pmDefs["max"] != float64(5) {
		t.Errorf("the dedicated gauge's max is %v, want the 5 CPUs the policy leaves pinnable "+
			"rather than the 8 the machine has or the 6 the reserved cpuset leaves", pmDefs["max"])
	}
	if title, _ := pinnedMeter["title"].(string); !containsStr(title, "of 5 pinnable") {
		t.Errorf("the dedicated gauge's title %q does not name the pool it is drawn against",
			title)
	}

	// Each table totals usage in millicores.
	for label, tbl := range map[string]map[string]any{"reserved": shared, "pinned": pinned} {
		f, _ := tbl["options"].(map[string]any)["footer"].(map[string]any)
		if f["show"] != true {
			t.Errorf("the %s listing has no footer total, and the gauges no longer carry "+
				"usage at all", label)
		}
		if !containsStr(fmt.Sprint(f["fields"]), usedColumn) {
			t.Errorf("the %s listing does not total %q: %v", label, usedColumn, f["fields"])
		}
	}
	// Each count tile wears its pool's colour.
	for label, tile := range map[string]map[string]any{"reserved": count, "dedicated": pinnedCount} {
		defs := tile["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
		color, _ := defs["color"].(map[string]any)
		if color["mode"] != "fixed" || color["fixedColor"] != wantColor[label] {
			t.Errorf("the %s count tile is coloured %v, want the pool's fixed %q", label,
				color, wantColor[label])
		}
		if _, hasThresholds := defs["thresholds"]; hasThresholds {
			t.Errorf("the %s count tile carries thresholds, so its colour would move with a "+
				"count that is neither good nor bad", label)
		}
		// A stat renders its value in the field colour only in this mode.
		if mode, _ := tile["options"].(map[string]any)["colorMode"].(string); mode != "value" {
			t.Errorf("the %s count tile renders its value in %q, so the pool colour never "+
				"reaches the number", label, mode)
		}
	}
	if cExpr, _ := pinnedCount["targets"].([]any)[0].(map[string]any)["expr"].(string); !containsStr(
		cExpr, `balloon!~"free|reserved"`) {
		t.Errorf("the dedicated count tile is not scoped to the pinned balloons: %s", cExpr)
	}
	// Both totals: the gauge's numerator and denominator.
	pf, _ := pinned["options"].(map[string]any)["footer"].(map[string]any)
	if fmt.Sprint(pf["fields"]) != "["+usedColumn+" Dedicated CPUs]" {
		t.Errorf("the pinned listing totals %v; it needs both the CPUs held and the CPU used",
			pf["fields"])
	}

	// Every description stays within MaxDescriptionChars.
	for _, p := range []map[string]any{meter, count, shared, pinned, pinnedMeter, pinnedCount} {
		desc, _ := p["description"].(string)
		if n := len(desc); n < 60 || n > 200 {
			t.Errorf("panel %q has a %d-character description; the page's budget is 73-160, "+
				"and this section is held to at most 200:\n%s", p["title"], n, desc)
		}
	}

	// Each usage selector targets only its own pool.
	for label, want := range map[string]string{
		"reserved": `balloon="reserved"`, "pinned": `balloon!~"free|reserved"`,
	} {
		tbl := shared
		if label == "pinned" {
			tbl = pinned
		}
		usage, _ := tbl["targets"].([]any)[1].(map[string]any)["expr"].(string)
		if !containsStr(usage, want) {
			t.Errorf("the %s listing's usage column is not narrowed with %q, so the other "+
				"pool's containers would be counted into this total: %s", label, want, usage)
		}
	}

	sharedExpr, _ := shared["targets"].([]any)[0].(map[string]any)["expr"].(string)
	if !containsStr(sharedExpr, `balloon="reserved"`) {
		t.Errorf("the reserved listing is not filtered to the reserved pool: %s", sharedExpr)
	}
	pinnedExpr, _ := pinned["targets"].([]any)[0].(map[string]any)["expr"].(string)
	if !containsStr(pinnedExpr, `balloon!~"free|reserved"`) {
		t.Errorf("the pinned listing does not exclude the reserved pool and free CPUs: %s", pinnedExpr)
	}
	// The pinned listing names the balloon.
	if !containsStr(pinnedExpr, "max by (balloon,") {
		t.Errorf("the pinned listing does not group by balloon: %s", pinnedExpr)
	}

	// The section is its own row, before the expert detail.
	top, _ := dash["panels"].([]any)
	rowY := map[string]float64{}
	for _, item := range top {
		p := item.(map[string]any)
		if p["type"] != "row" {
			continue
		}
		rowY[fmt.Sprint(p["title"])] = p["gridPos"].(map[string]any)["y"].(float64)
	}
	poolY, ok := rowY["Who shares which CPUs"]
	if !ok {
		t.Fatalf("the page has no pool-membership row; rows are %v", rowY)
	}
	if !(rowY["Workloads on this machine"] < poolY && poolY < rowY["Expert detail"]) {
		t.Errorf("the pool row sits at y=%v, not between workloads (%v) and expert detail (%v)",
			poolY, rowY["Workloads on this machine"], rowY["Expert detail"])
	}
}

// TestBalloonSummaryAgreesWithTheListings proves the roll-up has no __name__ column
// and counts reserved containers from pool_membership.
func TestBalloonSummaryAgreesWithTheListings(t *testing.T) {
	raw, err := RenderDashboardJSON("worker-1", Datasources{}, testMachine(), twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	for _, p := range flatten(dash["panels"]) {
		if title, _ := p["title"].(string); containsStr(title, "Balloons on this machine") {
			summary = p
		}
	}
	if summary == nil {
		t.Fatal("the page has no per-balloon roll-up")
	}

	exprs := ""
	for _, tg := range summary["targets"].([]any) {
		e, _ := tg.(map[string]any)["expr"].(string)
		exprs += e + "\n"
	}
	// An aggregation drops __name__ at the source.
	if !containsStr(exprs, "max by (node, balloon) (cpu_pinning:cpu_used:per_balloon{") {
		t.Errorf("the roll-up reads the recording rule raw, so Grafana renders a __name__ "+
			"column holding the rule's own name:\n%s", exprs)
	}
	// cpu_assignment cannot name reserved-pool pods.
	if !containsStr(exprs, "(cpu_pinning_pool_membership{") {
		t.Errorf("the roll-up counts pods from cpu_assignment, whose reserved series carry "+
			"empty workload labels, so the reserved row reads as having no pods:\n%s", exprs)
	}
	var excluded map[string]any
	for _, tr := range summary["transformations"].([]any) {
		m := tr.(map[string]any)
		if m["id"] == "organize" {
			excluded, _ = m["options"].(map[string]any)["excludeByName"].(map[string]any)
		}
	}
	if excluded["__name__"] != true {
		t.Error("the roll-up does not hide __name__, so a future raw read would leak the " +
			"metric name back into the table as a column")
	}

	// One unit across the section: containers, not pods.
	if !containsStr(exprs, "max by (balloon, workload_namespace, workload_pod, workload_container)") {
		t.Errorf("the roll-up does not count containers; the listings below it have one row "+
			"per container, so a pod count reads as a contradiction:\n%s", exprs)
	}
	var renamed map[string]any
	for _, tr := range summary["transformations"].([]any) {
		m := tr.(map[string]any)
		if m["id"] == "organize" {
			renamed, _ = m["options"].(map[string]any)["renameByName"].(map[string]any)
		}
	}
	if renamed["Value #B"] != "Containers" {
		t.Errorf("the roll-up's count column is headed %v; the unit has to be on the header, "+
			"because an unqualified count reads as contradicting the per-container listings",
			renamed["Value #B"])
	}
}

// TestCoreMapUnchangedByPoolMembership proves the core map reads only cpu_assignment;
// container-cardinality series would give one CPU several rows.
func TestCoreMapUnchangedByPoolMembership(t *testing.T) {
	raw, err := RenderDashboardJSON("worker-1", Datasources{}, testMachine(), twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}
	canvas := canvasPanel(t, dash["panels"].([]any))
	targets := canvas["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("the core map has %d queries, want the one owner pivot", len(targets))
	}
	expr, _ := targets[0].(map[string]any)["expr"].(string)
	if expr != ownerExpr("worker-1") {
		t.Errorf("the core map's query changed:\n got %s\nwant %s", expr, ownerExpr("worker-1"))
	}
	if containsStr(expr, "cpu_pinning_pool_membership") {
		t.Error("the core map reads the per-container gauge; its pivot is keyed on cpu, so a " +
			"container-cardinality series would put several rows on one CPU")
	}
}

// TestNodeDashboardLayout proves, across topology sizes, that no panels overlap the map or each other.
func TestNodeDashboardLayout(t *testing.T) {
	for _, cores := range []int{1, 4, 32, 128} {
		cpus := map[int]topology.CPUInfo{}
		for c := 0; c < cores; c++ {
			cpus[c] = topology.CPUInfo{CPU: c, NUMA: c % 2, Core: c}
			cpus[c+cores] = topology.CPUInfo{CPU: c + cores, NUMA: c % 2, Core: c}
		}
		raw, err := RenderDashboardJSON("worker-1", Datasources{}, testMachine(), cpus)
		if err != nil {
			t.Fatalf("%d cores: %v", cores, err)
		}
		var dash map[string]any
		if err := json.Unmarshal(raw, &dash); err != nil {
			t.Fatalf("%d cores: %v", cores, err)
		}
		// A collapsed row's children are laid out inside it.
		occupied := map[[2]int]string{}
		panels, _ := dash["panels"].([]any)
		for _, item := range panels {
			p := item.(map[string]any)
			g := p["gridPos"].(map[string]any)
			x, y := int(g["x"].(float64)), int(g["y"].(float64))
			w, h := int(g["w"].(float64)), int(g["h"].(float64))
			// Grafana's grid is 24 columns; overflow silently wraps.
			if x+w > 24 {
				t.Errorf("%d cores: panel %q spans columns %d..%d, past the 24-column grid",
					cores, p["title"], x, x+w)
			}
			for dx := 0; dx < w; dx++ {
				for dy := 0; dy < h; dy++ {
					cell := [2]int{x + dx, y + dy}
					if prev, dup := occupied[cell]; dup {
						t.Fatalf("%d cores: %q overlaps %q at grid cell %v",
							cores, p["title"], prev, cell)
					}
					occupied[cell] = fmt.Sprint(p["title"])
				}
			}
		}
	}
}

// TestCoreMapFillsItsWidth proves full rows use most of the canvas and small machines are not stretched.
func TestCoreMapFillsItsWidth(t *testing.T) {
	rightEdge := func(t *testing.T, coresPerNUMA int) (edge, rows int) {
		t.Helper()
		cpus := map[int]topology.CPUInfo{}
		total := coresPerNUMA * 2
		for c := 0; c < total; c++ {
			cpus[c] = topology.CPUInfo{CPU: c, NUMA: c % 2, Core: c}
			cpus[c+total] = topology.CPUInfo{CPU: c + total, NUMA: c % 2, Core: c}
		}
		elements, _ := buildElements(groupByNUMA(cpus), nil)
		for _, e := range elements {
			el := e.(map[string]any)
			if el["type"] != "metric-value" {
				continue
			}
			pl := el["placement"].(map[string]any)
			if r := pl["left"].(int) + pl["width"].(int); r > edge {
				edge = r
			}
		}
		return edge, (coresPerNUMA + coresPerRow - 1) / coresPerRow
	}

	// Full rows use most of the canvas and never overflow it.
	edge, rows := rightEdge(t, 112)
	if edge > canvasW {
		t.Errorf("112 cores per NUMA node: the map runs to %dpx, past the %dpx canvas", edge, canvasW)
	}
	if edge < canvasW*3/4 {
		t.Errorf("112 cores per NUMA node: the map only reaches %dpx of %dpx — it is not "+
			"filling the width it is given", edge, canvasW)
	}
	if rows != 7 {
		t.Errorf("112 cores per NUMA node lay out in %d rows, want 7", rows)
	}

	// Four cores fit one row and must not be padded out to the full width.
	if edge, rows = rightEdge(t, 4); rows != 1 || edge > 4*(coreW+gapX)+leftPad {
		t.Errorf("4 cores per NUMA node: %d row(s), right edge %dpx — a small machine must "+
			"draw a short row, not a stretched one", rows, edge)
	}
}

// TestCanvasGridHeightFitsTheMap proves the panel neither clips the map nor leaves dead space.
func TestCanvasGridHeightFitsTheMap(t *testing.T) {
	for _, pxH := range []int{100, 300, 964, 2000} {
		h := canvasGridH(pxH)
		content := h*gridRowPxH + (h-1)*gridRowGapPxH - panelChromePxH
		if content < pxH {
			t.Errorf("pxH=%d: %d grid rows hold %dpx, which clips the map", pxH, h, content)
		}
		if pxH > 300 && content > pxH+3*(gridRowPxH+gridRowGapPxH) {
			t.Errorf("pxH=%d: %d grid rows hold %dpx, over three rows of dead space", pxH, h, content)
		}
	}
}

// TestPerNUMABarCarriesItsDenominator proves each bar's max is its NUMA capacity,
// named in the title, with fullness colouring.
func TestPerNUMABarCarriesItsDenominator(t *testing.T) {
	raw, err := RenderDashboardJSON("worker-1", Datasources{}, testMachine(), twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}
	var panel map[string]any
	for _, p := range flatten(dash["panels"]) {
		if title, _ := p["title"].(string); containsStr(title, "How full each NUMA node is") {
			panel = p
		}
	}
	if panel == nil {
		t.Fatal("no panel says how full each NUMA node is")
	}
	if panel["type"] != "bargauge" {
		t.Errorf("the per-NUMA panel is a %v, want a bar gauge", panel["type"])
	}
	// twoNumaFourCoreHT: 2 cores x 2 threads in each NUMA node.
	if title, _ := panel["title"].(string); !containsStr(title, "of 4 CPUs each") {
		t.Errorf("the per-NUMA title %q does not name the capacity the bars are drawn against", title)
	}
	defs := panel["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
	if defs["max"] != float64(4) {
		t.Errorf("per-NUMA max = %v, want 4 (the CPUs one NUMA node has)", defs["max"])
	}
	if defs["min"] != float64(0) {
		t.Errorf("per-NUMA min = %v, want 0", defs["min"])
	}
	if mode, _ := panel["options"].(map[string]any)["displayMode"].(string); mode != "lcd" {
		t.Errorf("per-NUMA displayMode = %q; the panel has to read as a meter, not a list", mode)
	}

	// Colour is fullness, so the bars say more than their length.
	var colours []string
	thr, _ := defs["thresholds"].(map[string]any)
	for _, st := range thr["steps"].([]any) {
		if c, _ := st.(map[string]any)["color"].(string); c != "" {
			colours = append(colours, c)
		}
	}
	for _, want := range []string{"green", "orange", "red"} {
		found := false
		for _, got := range colours {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("per-NUMA thresholds %v do not colour fullness %s", colours, want)
		}
	}

	// One query, and an unclaimed NUMA node must still read 0 rather than vanish.
	targets := panel["targets"].([]any)
	if len(targets) != 1 {
		t.Errorf("per-NUMA panel has %d queries; one bar per NUMA node needs one", len(targets))
	}
	expr, _ := targets[0].(map[string]any)["expr"].(string)
	if !containsStr(expr, numaCapacity("worker-1")+" * 0)") {
		t.Errorf("the per-NUMA query lost its zero fallback, so an unclaimed NUMA node would go "+
			"missing instead of reading 0: %s", expr)
	}
}

// Uneven NUMA nodes get per-bar capacities and no single capacity in the title.
func TestPerNUMAUnevenMachineLabelsEachBar(t *testing.T) {
	even := shapeOf(groupByNUMA(twoNumaFourCoreHT()), nil)
	if !even.evenNUMA() {
		t.Error("a machine with equal NUMA nodes is reported as uneven")
	}
	if got := even.numaMaxOverrides(); len(got) != 0 {
		t.Errorf("even machine got %d per-NUMA override(s), want none", len(got))
	}

	// NUMA 0: two cores with two threads. NUMA 1: one core with two threads.
	uneven := map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0}, 1: {CPU: 1, NUMA: 0, Core: 1},
		2: {CPU: 2, NUMA: 0, Core: 0}, 3: {CPU: 3, NUMA: 0, Core: 1},
		4: {CPU: 4, NUMA: 1, Core: 2}, 5: {CPU: 5, NUMA: 1, Core: 2},
	}
	s := shapeOf(groupByNUMA(uneven), nil)
	if s.evenNUMA() {
		t.Fatal("a machine with a 4-CPU and a 2-CPU NUMA node is reported as even")
	}
	got := s.numaMaxOverrides()
	if len(got) != 2 {
		t.Fatalf("uneven machine got %d override(s), want one per NUMA node", len(got))
	}
	want := map[string]int{"NUMA 0": 4, "NUMA 1": 2}
	for _, o := range got {
		m := o.(map[string]any)
		name := m["matcher"].(map[string]any)["options"].(string)
		val := m["properties"].([]any)[0].(map[string]any)["value"].(int)
		if want[name] != val {
			t.Errorf("%s max = %d, want %d", name, val, want[name])
		}
	}

	raw, err := RenderDashboardJSON("worker-1", Datasources{}, testMachine(), uneven)
	if err != nil {
		t.Fatal(err)
	}
	if containsStr(string(raw), "CPUs each") {
		t.Error("the per-NUMA title names one capacity on a machine that has two")
	}
}

// flatten returns every panel including the children of collapsed rows.
func flatten(v any) []map[string]any {
	list, _ := v.([]any)
	var out []map[string]any
	for _, item := range list {
		p, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, p)
		out = append(out, flatten(p["panels"])...)
	}
	return out
}

func TestRenderDashboardJSON_NoTopology(t *testing.T) {
	if _, err := RenderDashboardJSON("empty", Datasources{}, testMachine(), map[int]topology.CPUInfo{}); err == nil {
		t.Error("expected error for empty topology, got nil")
	}
}

// An unknown mode must be said to be unknown rather than guessed at.
func TestRenderDashboardJSON_UnknownAllocationMode(t *testing.T) {
	raw, err := RenderDashboardJSON("worker-1", Datasources{}, Machine{}, twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(string(raw), "allocation mode: not reported") {
		t.Error("an empty cpuAllocation must render as 'not reported', not as a guess")
	}
}

func TestRenderDashboardJSON_DatasourceOverride(t *testing.T) {
	raw, err := RenderDashboardJSON("worker-1", Datasources{PrometheusUID: "prometheus-mimir"}, testMachine(), twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}
	tmpl := dash["templating"].(map[string]any)["list"].([]any)
	cur := tmpl[0].(map[string]any)["current"].(map[string]any)
	if cur["value"] != "prometheus-mimir" {
		t.Errorf("datasource var current.value = %v, want prometheus-mimir (override)", cur["value"])
	}
	// `text` is a data source NAME; a uid there breaks the variable.
	if _, ok := cur["text"]; ok {
		t.Errorf("pinned datasource var carries text=%v; a uid is not a name", cur["text"])
	}
}

// testLoki is a promtail-labelled Loki config.
func testLoki() LokiConfig {
	return LokiConfig{
		Enabled:       true,
		NodeLabel:     "node_name",
		PluginLabel:   "app",
		PluginName:    "nri-resource-policy-balloons",
		WorkloadLabel: "app",
	}
}

// TestMachineLogSection proves the log section reads this node's plugin stream,
// comes last, and is omitted without Loki.
func TestMachineLogSection(t *testing.T) {
	t.Run("absent without Loki", func(t *testing.T) {
		raw, err := RenderDashboardJSON("worker-1", Datasources{}, testMachine(), twoNumaFourCoreHT())
		if err != nil {
			t.Fatal(err)
		}
		var dash map[string]any
		if err := json.Unmarshal(raw, &dash); err != nil {
			t.Fatal(err)
		}
		for _, p := range flatten(dash["panels"]) {
			if p["type"] == "logs" {
				t.Error("logs panel present with no Loki configured; it would query a data source that does not exist")
			}
		}
		for _, v := range dash["templating"].(map[string]any)["list"].([]any) {
			switch v.(map[string]any)["name"] {
			case "loki":
				t.Error("loki template var present with no Loki configured")
			case "logsource", "benign":
				t.Error("a log-panel picker is present with no log panel to drive")
			}
		}
		if containsStr(string(raw), `"type": "loki"`) {
			t.Error("dashboard references a Loki data source with no Loki configured")
		}
	})

	t.Run("absent when the label names are missing", func(t *testing.T) {
		// Enabled but unlabelled would select every stream in Loki.
		raw, err := RenderDashboardJSON("worker-1", Datasources{Loki: LokiConfig{Enabled: true}},
			testMachine(), twoNumaFourCoreHT())
		if err != nil {
			t.Fatal(err)
		}
		if containsStr(string(raw), `"type": "logs"`) {
			t.Error("log panel rendered without the stream labels to filter on")
		}
	})

	t.Run("rendered last, scoped to this machine", func(t *testing.T) {
		raw, err := RenderDashboardJSON("worker-1", Datasources{Loki: testLoki()},
			testMachine(), twoNumaFourCoreHT())
		if err != nil {
			t.Fatal(err)
		}
		var dash map[string]any
		if err := json.Unmarshal(raw, &dash); err != nil {
			t.Fatal(err)
		}
		panels, _ := dash["panels"].([]any)
		var logs map[string]any
		maxY := -1.0
		var lastTitle string
		for _, item := range panels {
			p := item.(map[string]any)
			if p["type"] == "logs" {
				logs = p
			}
			if y := p["gridPos"].(map[string]any)["y"].(float64); y >= maxY {
				maxY, lastTitle = y, fmt.Sprint(p["title"])
			}
		}
		if logs == nil {
			t.Fatal("no logs panel with Loki configured")
		}
		if !containsStr(lastTitle, "plugin and operator logs") {
			t.Errorf("the bottom panel is %q; the logs belong at the expert-detail end", lastTitle)
		}
		// :raw keeps the fragments verbatim; the line filter goes outside the braces.
		expr := logs["targets"].([]any)[0].(map[string]any)["expr"].(string)
		if want := "{${logsource:raw}} ${benign:raw}"; expr != want {
			t.Errorf("logs LogQL = %q, want %q", expr, want)
		}

		vars := map[string]map[string]any{}
		for _, v := range dash["templating"].(map[string]any)["list"].([]any) {
			m := v.(map[string]any)
			vars[m["name"].(string)] = m
		}
		if loki, ok := vars["loki"]; !ok {
			t.Error("logs panel present but no loki data source variable to bind it")
		} else if cur := loki["current"].(map[string]any); len(cur) != 0 {
			t.Errorf("unpinned loki var current = %v, want empty so Grafana auto-selects", cur)
		}

		src, ok := vars["logsource"]
		if !ok {
			t.Fatal("no logsource picker; the panel would have nothing to interpolate")
		}
		opts := src["options"].([]any)
		if len(opts) != 2 {
			t.Fatalf("logsource has %d option(s), want the plugin and the operator", len(opts))
		}
		first := opts[0].(map[string]any)
		// The plugin stream is the default.
		if !containsStr(first["text"].(string), "on this machine") || first["selected"] != true {
			t.Errorf("first logsource option = %v, want the plugin on this machine selected", first)
		}
		if want := `app="nri-resource-policy-balloons", node_name="worker-1"`; first["value"] != want {
			t.Errorf("plugin fragment = %v, want %q", first["value"], want)
		}
		// The operator option must not claim to be per-machine: its lines carry no node.
		second := opts[1].(map[string]any)
		if !containsStr(second["text"].(string), "all machines") {
			t.Errorf("operator option = %v, want its label to say it is fleet-wide", second)
		}
		if containsStr(fmt.Sprint(second["value"]), "node_name") {
			t.Errorf("operator fragment %v filters on the node label; that selects the replica "+
				"running here, not lines about here", second["value"])
		}
		// Grafana re-parses the query on load and splits it on commas.
		query := src["query"].(string)
		if !containsStr(query, `\,`) {
			t.Errorf("logsource query %q does not escape the comma inside a selector fragment", query)
		}
	})

	// The benign messages are hidden by default, stated, and one click from the raw stream.
	t.Run("benign plugin errors hidden by default, and reachable", func(t *testing.T) {
		raw, err := RenderDashboardJSON("worker-1", Datasources{Loki: testLoki()},
			testMachine(), twoNumaFourCoreHT())
		if err != nil {
			t.Fatal(err)
		}
		var dash map[string]any
		if err := json.Unmarshal(raw, &dash); err != nil {
			t.Fatal(err)
		}
		var benign map[string]any
		for _, v := range dash["templating"].(map[string]any)["list"].([]any) {
			if m := v.(map[string]any); m["name"] == "benign" {
				benign = m
			}
		}
		if benign == nil {
			t.Fatal("no benign filter picker; the panel interpolates ${benign:raw} and would query garbage")
		}
		// Grafana re-parses `query` on load, so it must carry both fragments.
		query := benign["query"].(string)
		for _, want := range []string{BenignPluginFilter, UnfilteredLineFilter} {
			if !containsStr(query, want) {
				t.Errorf("benign query %q does not carry %q, which is what Grafana re-parses", query, want)
			}
		}
		opts := benign["options"].([]any)
		if len(opts) != 2 {
			t.Fatalf("benign has %d option(s), want hidden and unfiltered", len(opts))
		}
		hidden := opts[0].(map[string]any)
		if hidden["selected"] != true {
			t.Errorf("first benign option = %v, want it selected: the noise is hidden by default", hidden)
		}
		for _, want := range []string{"libmem: unknown allocation", "PodResources API Get method disabled"} {
			if !containsStr(hidden["value"].(string), want) {
				t.Errorf("default benign filter %q does not exclude %q", hidden["value"], want)
			}
		}
		if f := hidden["value"].(string); !containsStr(f, "!=") {
			t.Errorf("benign filter %q is not a LogQL exclusion; it would match the noise instead of dropping it", f)
		}
		// An empty value makes Grafana splice in the option label.
		shown := opts[1].(map[string]any)
		if v, _ := shown["value"].(string); strings.TrimSpace(v) == "" {
			t.Errorf("unfiltered option value = %q, want a line filter that matches everything", v)
		}
		if !containsStr(shown["text"].(string), "unfiltered") {
			t.Errorf("second benign option = %v, want its label to say it shows everything", shown)
		}
		// The parser splits on commas and " : ", so neither may appear unescaped.
		for _, o := range opts {
			m := o.(map[string]any)
			if v := m["value"].(string); containsStr(v, " : ") {
				t.Errorf("benign fragment %q contains the \" : \" separator Grafana splits on", v)
			}
			if n := m["text"].(string); containsStr(n, ",") {
				t.Errorf("benign option name %q contains a comma; Grafana would split it into two options", n)
			}
		}
		// Stated where it is suppressed, and honest that nothing was dropped upstream.
		var logs map[string]any
		for _, p := range flatten(dash["panels"]) {
			if p["type"] == "logs" {
				logs = p
			}
		}
		desc := logs["description"].(string)
		for _, want := range []string{"libmem: unknown allocation",
			"PodResources API Get method disabled", "docs/observability.md"} {
			if !containsStr(desc, want) {
				t.Errorf("logs panel description does not name %q; a reader cannot tell what is hidden", want)
			}
		}
	})
}

// TestOwnerExpr pins the label_replace order in ownerExpr.
func TestOwnerExpr(t *testing.T) {
	expr := ownerExpr("worker-1")
	// The full-pod seed precedes the Deployment-prefix replace.
	seedIdx := indexOf(expr, `"workload_pod", "^(.+)$"`)
	// pod-prefix derivation must precede the free/reserved balloon replace.
	podIdx := indexOf(expr, `"workload_pod", "^(.+)-[a-z0-9]+-[a-z0-9]{5}$"`)
	balloonIdx := indexOf(expr, `"balloon", "^(free|reserved)$"`)
	if seedIdx < 0 || podIdx < 0 || balloonIdx < 0 {
		t.Fatalf("expr missing a label_replace stage: %q", expr)
	}
	if !(seedIdx < podIdx) {
		t.Errorf("full-workload_pod seed must run before the Deployment-prefix replace so bare pod names still get an owner; got seed@%d, pod@%d in %q", seedIdx, podIdx, expr)
	}
	if !(podIdx < balloonIdx) {
		t.Errorf("balloon replace must run after the pod replace so idle cores keep free/reserved; got pod@%d, balloon@%d in %q", podIdx, balloonIdx, expr)
	}
	// A trailing replace caps the owner token so long names fit the core box.
	if !containsStr(expr, `"owner", "${1}…", "owner", "^(.{12}).+$"`) {
		t.Errorf("expr missing the owner-length cap: %q", expr)
	}
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func countStr(haystack, needle string) int {
	n := 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			n++
		}
	}
	return n
}

func containsStr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// sameSteps compares rendered (float64) thresholds with fullnessSteps (int) output.
func sameSteps(got any, want map[string]any) bool {
	g, ok := got.(map[string]any)
	if !ok {
		return false
	}
	gs, _ := g["steps"].([]any)
	ws, _ := want["steps"].([]any)
	if len(gs) != len(ws) || len(gs) == 0 {
		return false
	}
	for i := range gs {
		gm, _ := gs[i].(map[string]any)
		wm, _ := ws[i].(map[string]any)
		if gm["color"] != wm["color"] {
			return false
		}
		if wm["value"] == nil {
			if gm["value"] != nil {
				return false
			}
			continue
		}
		wv, _ := wm["value"].(int)
		gv, _ := gm["value"].(float64)
		if gv != float64(wv) {
			return false
		}
	}
	return true
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestNodeTilesColourOnlyJudgements proves only judgeable numbers carry threshold
// colours; inventory stays neutral (fixed pool colours are allowed).
func TestNodeTilesColourOnlyJudgements(t *testing.T) {
	raw, err := RenderDashboardJSON("worker-1", Datasources{}, testMachine(), twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}

	// Matched by title prefix: titles carry machine-specific denominators.
	verdicts := map[string][]string{
		// Amber, not red: narrower pods still fit.
		"Free CPUs the widest span cannot use": {"green", "orange"},
		"CPUs booked to pods that are gone":    {"green", "red"},
		"Reserved pool: asked for vs provisi":  {"green", "orange", "red"},
		"NUMA nodes with room left":            {"red", "text"},
		// The two pool gauges carry percentage bands.
		"Reserved pool usage":     {"green", "orange", "red"},
		"Dedicated CPUs occupied": {"green", "orange", "red"},
	}
	facts := []string{
		"Containers sharing", "Containers holding them", "Half-used physical cores",
		"Admissions in flight", "Balloons on this machine", "Sharing the reserved pool CPUs",
		"Holding dedicated CPUs", "The same machine, live from Prometheus",
		"How CPUs are counted here",
	}

	seen := map[string]bool{}
	for _, p := range flatten(dash["panels"]) {
		title, _ := p["title"].(string)
		fc, _ := p["fieldConfig"].(map[string]any)
		defs, _ := fc["defaults"].(map[string]any)
		thr, _ := defs["thresholds"].(map[string]any)
		// A table or text panel carries no thresholds at all, which is neutral.
		stepList, _ := thr["steps"].([]any)
		var colours []string
		for _, st := range stepList {
			c, _ := st.(map[string]any)["color"].(string)
			colours = append(colours, c)
		}

		for prefix, want := range verdicts {
			if !containsStr(title, prefix) {
				continue
			}
			seen[prefix] = true
			if !sameStrings(colours, want) {
				t.Errorf("panel %q colours %v, want %v", title, colours, want)
			}
		}
		for _, prefix := range facts {
			if !containsStr(title, prefix) {
				continue
			}
			seen[prefix] = true
			for _, c := range colours {
				if c != "text" && c != "" {
					t.Errorf("panel %q is inventory and carries the verdict colour %q; the "+
						"judgement about it belongs to a ratio, not to the count", title, c)
				}
			}
		}
	}
	for prefix := range verdicts {
		if !seen[prefix] {
			t.Errorf("no panel titled %q...; the colour rule was checked against a title that "+
				"no longer exists", prefix)
		}
	}
	for _, prefix := range facts {
		if !seen[prefix] {
			t.Errorf("no panel titled %q...; the colour rule was checked against a title that "+
				"no longer exists", prefix)
		}
	}
}

// TestPoolUsageStepsArePercentages proves the pool gauges' steps are in percentage mode.
func TestPoolUsageStepsArePercentages(t *testing.T) {
	st := poolUsageSteps()
	if st["mode"] != "percentage" {
		t.Fatalf("pool-usage bands are %v-mode, want percentage", st["mode"])
	}
	list := st["steps"].([]any)
	if len(list) != 3 {
		t.Fatalf("%d step(s), want base, amber and red", len(list))
	}
	if c := list[0].(map[string]any); c["color"] != "green" || c["value"] != nil {
		t.Errorf("base step = %v, want green from zero", c)
	}
	watch := list[1].(map[string]any)
	full := list[2].(map[string]any)
	if watch["color"] != "orange" || full["color"] != "red" {
		t.Errorf("bands run %v then %v, want orange then red", watch["color"], full["color"])
	}
	w, _ := watch["value"].(int)
	f, _ := full["value"].(int)
	if !(0 < w && w < f && f < 100) {
		t.Errorf("bands at %d%% and %d%%; both have to leave green and red a share of the "+
			"ring", w, f)
	}
	// Low enough to act on, high enough that half-used is not critical.
	if w < 50 || f > 95 {
		t.Errorf("bands at %d%% and %d%%: amber below half a pool would call a healthy pool "+
			"bad, and red above 95%% arrives too late to act on", w, f)
	}
}

// TestReservedGaugeStaysNeutralWithoutADenominator proves an unknown reserved size
// gives neutral bands, while the dedicated gauge keeps its own.
func TestReservedGaugeStaysNeutralWithoutADenominator(t *testing.T) {
	m := testMachine()
	m.ReservedCPUs = 0
	raw, err := RenderDashboardJSON("worker-1", Datasources{}, m, twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, p := range flatten(dash["panels"]) {
		title, _ := p["title"].(string)
		if !containsStr(title, "Reserved pool usage") {
			continue
		}
		checked++
		defs, _ := p["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
		if _, ok := defs["max"]; ok {
			t.Errorf("%q has a max of %v on a machine whose pool size is unknown", title,
				defs["max"])
		}
		thr, _ := defs["thresholds"].(map[string]any)
		if thr["mode"] == "percentage" {
			t.Errorf("%q keeps percentage bands with no denominator to measure them against, "+
				"so any reading renders at the top of its own range", title)
		}
		for _, st := range thr["steps"].([]any) {
			if c, _ := st.(map[string]any)["color"].(string); c != "text" {
				t.Errorf("%q colours an unscaled number %q", title, c)
			}
		}
	}
	if checked != 1 {
		t.Errorf("checked %d reserved gauge(s), want exactly one", checked)
	}
}

// TestFullnessStepsStayApart proves the steps stay distinct at small capacities.
func TestFullnessStepsStayApart(t *testing.T) {
	for _, capacity := range []int{1, 2, 3, 4, 8, 16, 112, 224} {
		st := fullnessSteps(capacity)["steps"].([]any)
		if len(st) != 3 {
			t.Fatalf("capacity %d: %d step(s), want base, filling and full", capacity, len(st))
		}
		filling := st[1].(map[string]any)["value"].(int)
		full := st[2].(map[string]any)["value"].(int)
		if filling < 1 {
			t.Errorf("capacity %d: the filling step is at %d, so an empty NUMA node reads amber",
				capacity, filling)
		}
		if full <= filling {
			t.Errorf("capacity %d: filling=%d and full=%d collide, so one colour never appears",
				capacity, filling, full)
		}
		if capacity > 4 && full > capacity {
			t.Errorf("capacity %d: the full step is at %d, which no bar can reach", capacity, full)
		}
	}
}

// TestPageFollowsThePinnablePool proves per-NUMA numbers use the pinnable pool when siblings are excluded.
func TestPageFollowsThePinnablePool(t *testing.T) {
	m := testMachine()
	// twoNumaFourCoreHT: representatives 0-3, siblings 4-7 excluded from the pool.
	m.PinnableCPUs = map[int]bool{0: true, 1: true, 2: true, 3: true}

	raw, err := RenderDashboardJSON("worker-1", Datasources{}, m, twoNumaFourCoreHT())
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}

	var perNUMA, identity map[string]any
	for _, p := range flatten(dash["panels"]) {
		title, _ := p["title"].(string)
		switch {
		case containsStr(title, "How full each NUMA node is"):
			perNUMA = p
		case containsStr(title, "as the operator measured it"):
			identity = p
		}
	}
	if perNUMA == nil || identity == nil {
		t.Fatal("the per-NUMA panel or the machine identity panel is missing")
	}

	// Each NUMA node has 4 logical CPUs but only 2 a workload can be placed on.
	if title, _ := perNUMA["title"].(string); !containsStr(title, "of 2 CPUs each") {
		t.Errorf("per-NUMA title %q names the hardware total; it must name the pinnable pool", title)
	}
	defs := perNUMA["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
	if defs["max"] != float64(2) {
		t.Errorf("per-NUMA max = %v, want 2 (the CPUs of that NUMA node a workload can use)", defs["max"])
	}
	expr, _ := perNUMA["targets"].([]any)[0].(map[string]any)["expr"].(string)
	if !containsStr(expr, "cpu_pinning_node_pinnable_cpus") {
		t.Errorf("the per-NUMA query does not read the pinnable pool, so it can claim room "+
			"the plugin will not hand out: %s", expr)
	}

	// The smaller denominator is stated, not just applied.
	content, _ := identity["options"].(map[string]any)["content"].(string)
	if !containsStr(content, "4 of 8") {
		t.Errorf("the machine panel does not report the pinnable pool beside the hardware total: %s", content)
	}

	// Excluded threads get fixed cells, not missing data.
	elements, _ := buildElements(groupByNUMA(twoNumaFourCoreHT()), m.PinnableCPUs)
	excluded := 0
	for _, e := range elements {
		el := e.(map[string]any)
		if el["type"] != "metric-value" {
			continue
		}
		text := el["config"].(map[string]any)["text"].(map[string]any)
		switch el["name"] {
		case "cpu4", "cpu5", "cpu6", "cpu7":
			if text["fixed"] != excludedLabel {
				t.Errorf("%v is out of the pinning pool but its cell reads %v", el["name"], text)
			}
			excluded++
		default:
			if text["mode"] != "field" {
				t.Errorf("%v is in the pool, so its cell must read the assignment metric, got %v",
					el["name"], text)
			}
		}
	}
	if excluded != 4 {
		t.Errorf("marked %d cells as out of the pool, want 4", excluded)
	}
}

// The free-CPU fallback uses the pinnable pool and is clamped at 0.
func TestFreePerNUMAIsPoolBasedAndClamped(t *testing.T) {
	expr := freePerNUMA("worker-1")
	if !containsStr(expr, "cpu_pinning_node_pinnable_cpus") {
		t.Errorf("the free-CPU fallback does not read the pinnable pool: %s", expr)
	}
	if !containsStr(expr, "clamp_min(") {
		t.Errorf("the free-CPU fallback is not clamped at 0: %s", expr)
	}
}

// An unresolved pool falls back to the hardware total.
func TestPageWithoutAPinnablePoolUsesTheHardwareTotal(t *testing.T) {
	s := shapeOf(groupByNUMA(twoNumaFourCoreHT()), nil)
	if s.cpusPerNUMAMax != 4 || s.pinnableCPUs != 8 {
		t.Errorf("unresolved pool: per-NUMA max = %d, pinnable total = %d, want 4 and 8",
			s.cpusPerNUMAMax, s.pinnableCPUs)
	}
	if got := machineFactsMarkdown(testMachine(), s); !containsStr(got, "8 (every logical CPU)") {
		t.Errorf("the machine panel does not report the pool as the full machine: %s", got)
	}
}
