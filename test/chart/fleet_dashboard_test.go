// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v3"

	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/dashboard"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

const fleetDataKey = "cpu-pinning-fleet-overview.json"

// chartUnderTest is this checkout's chart, unless CPU_PINNING_CHART_DIR points at another
// revision (to prove a regression test fails on it).
func chartUnderTest() string {
	if dir := os.Getenv("CPU_PINNING_CHART_DIR"); dir != "" {
		return dir
	}
	return filepath.Join("..", "..", "deploy", "cpu-pinning-operator")
}

// renderFleet renders templates/fleet-dashboard.yaml with the given values.
func renderFleet(t *testing.T, valuesBody string) string {
	t.Helper()
	helm := helmBin(t)
	chartDir := chartUnderTest()
	vf := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(vf, []byte(valuesBody), 0o600); err != nil {
		t.Fatalf("write values: %v", err)
	}
	args := []string{"template", "t", chartDir, "-f", policyTargetValues, "--show-only", "templates/fleet-dashboard.yaml", "-f", vf}
	out, err := exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		s := string(out)
		requireSubcharts(t, s)
		// A gated-empty template still renders its Source header, so any error here is
		// real.
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

func hasConfigMap(out string) bool {
	for _, doc := range strings.Split(out, "\n---") {
		var m map[string]any
		if yaml.Unmarshal([]byte(doc), &m) == nil && m["kind"] == "ConfigMap" {
			return true
		}
	}
	return false
}

// fleetDashboardJSON renders the chart and returns the embedded dashboard JSON.
func fleetDashboardJSON(t *testing.T, valuesBody string) string {
	t.Helper()
	out := renderFleet(t, valuesBody)
	for _, doc := range strings.Split(out, "\n---") {
		var cm struct {
			Data map[string]string `yaml:"data"`
		}
		var probe map[string]any
		if yaml.Unmarshal([]byte(doc), &probe) != nil || probe["kind"] != "ConfigMap" {
			continue
		}
		if err := yaml.Unmarshal([]byte(doc), &cm); err != nil {
			t.Fatalf("parse ConfigMap: %v\n%s", err, doc)
		}
		// ConfigMap order is not guaranteed.
		if raw, ok := cm.Data[fleetDataKey]; ok {
			return raw
		}
	}
	t.Fatalf("no dashboard ConfigMap rendered for values:\n%s\n%s", valuesBody, out)
	return ""
}

// fleetValueSets are the value combinations every structural assertion runs against.
var fleetValueSets = map[string]string{
	"default":              "grafanaDashboard:\n  enabled: true\n",
	"loki logs enabled":    "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n",
	"loki uid pinned":      "grafanaDashboard:\n  enabled: true\n  lokiDatasourceUid: my-loki\n",
	"both datasources set": "grafanaDashboard:\n  enabled: true\n  datasourceUid: openshift-thanos\n  lokiDatasourceUid: my-loki\n",
	"promtail labels":      "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n  lokiLabels:\n    namespace: namespace\n    workload: pod\n",
}

// TestFleetDashboardRendering proves the dashboard renders only with both gates on,
// carries the sidecar label and embeds valid JSON.
func TestFleetDashboardRendering(t *testing.T) {
	t.Run("not rendered by default", func(t *testing.T) {
		if out := renderFleet(t, "{}\n"); hasConfigMap(out) {
			t.Errorf("fleet dashboard rendered with grafanaDashboard.enabled=false:\n%s", out)
		}
	})

	t.Run("not rendered when fleetOverview disabled", func(t *testing.T) {
		out := renderFleet(t, "grafanaDashboard:\n  enabled: true\n  fleetOverview:\n    enabled: false\n")
		if hasConfigMap(out) {
			t.Errorf("fleet dashboard rendered with fleetOverview.enabled=false:\n%s", out)
		}
	})

	t.Run("sidecar label and dashboard identity", func(t *testing.T) {
		out := renderFleet(t, "grafanaDashboard:\n  enabled: true\n")
		var cm struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Data map[string]string `yaml:"data"`
		}
		found := false
		for _, doc := range strings.Split(out, "\n---") {
			var probe map[string]any
			if yaml.Unmarshal([]byte(doc), &probe) != nil || probe["kind"] != "ConfigMap" {
				continue
			}
			found = true
			if err := yaml.Unmarshal([]byte(doc), &cm); err != nil {
				t.Fatalf("parse ConfigMap: %v\n%s", err, doc)
			}
		}
		if !found {
			t.Fatalf("no ConfigMap rendered when grafanaDashboard.enabled=true:\n%s", out)
		}

		if cm.Metadata.Labels["grafana_dashboard"] != "1" {
			t.Errorf("sidecar label grafana_dashboard=%q, want \"1\":\n%s", cm.Metadata.Labels["grafana_dashboard"], out)
		}
		raw, ok := cm.Data[fleetDataKey]
		if !ok {
			t.Fatalf("ConfigMap missing the dashboard data key:\n%s", out)
		}
		var dash map[string]any
		if err := json.Unmarshal([]byte(raw), &dash); err != nil {
			t.Fatalf("embedded dashboard JSON is not valid: %v", err)
		}
		if dash["uid"] != "cpu-pinning-fleet" {
			t.Errorf("dashboard uid = %v, want cpu-pinning-fleet (the per-node dashboards link back to it)", dash["uid"])
		}
	})

	t.Run("machine table links into the per-node dashboards", func(t *testing.T) {
		raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
		// By title: the Failures row carries a table too.
		var table map[string]any
		for _, p := range flattenPanels(t, raw) {
			title, _ := p["title"].(string)
			if p["type"] == "table" && strings.Contains(title, "one row per managed machine") {
				table = p
			}
		}
		if table == nil {
			t.Fatalf("no machine table found; the cluster dashboard has no machine list:\n%s", raw)
		}
		if !strings.Contains(raw, `"byField": "node"`) {
			t.Errorf("machine table does not join its queries on the node label:\n%s", raw)
		}

		wantPrefix := "/d/" + dashboard.DashboardUID("")
		links, _ := json.Marshal(table["fieldConfig"])
		if !strings.Contains(string(links), wantPrefix+`${__value.text}?var-datasource=${datasource}`) {
			t.Errorf("machine table has no data link of the form %s${__value.text}: %s", wantPrefix, links)
		}

		if strings.Contains(raw, "label_values(cpu_pinning_node_logical_cpus, node)") {
			t.Errorf("dashboard still carries the obsolete $node dropdown query:\n%s", raw)
		}
	})

	t.Run("no unreplaced sentinel on any branch", func(t *testing.T) {
		// A leftover __UPPER_SNAKE__ token ships a broken dashboard; Grafana's
		// ${__value...} macros are not sentinels.
		sentinel := regexp.MustCompile(`__[A-Z][A-Z0-9_]*__`)
		for name, values := range fleetValueSets {
			raw := fleetDashboardJSON(t, values)
			if got := sentinel.FindString(raw); got != "" {
				t.Errorf("%s: unreplaced sentinel token %q", name, got)
			}
			var dash map[string]any
			if err := json.Unmarshal([]byte(raw), &dash); err != nil {
				t.Errorf("%s: rendered dashboard is not valid JSON: %v", name, err)
			}
		}
	})
}

// TestFleetDashboardLokiSection proves the Loki variable is absent without Loki and
// present when Loki is configured either way.
func TestFleetDashboardLokiSection(t *testing.T) {
	lokiBits := func(t *testing.T, raw string) (vars []string, hasLogsPanel bool) {
		t.Helper()
		var parsed struct {
			Templating struct {
				List []struct {
					Name string `json:"name"`
				} `json:"list"`
			} `json:"templating"`
		}
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatalf("parse dashboard: %v", err)
		}
		for _, v := range parsed.Templating.List {
			vars = append(vars, v.Name)
		}
		for _, p := range flattenPanels(t, raw) {
			if p["type"] == "logs" {
				hasLogsPanel = true
			}
		}
		return vars, hasLogsPanel
	}

	t.Run("omitted entirely by default", func(t *testing.T) {
		raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
		vars, logs := lokiBits(t, raw)
		for _, unwanted := range []string{"loki", "namespace"} {
			for _, got := range vars {
				if got == unwanted {
					t.Errorf("template var %q present with no Loki configured; it has nothing to resolve to", unwanted)
				}
			}
		}
		if logs {
			t.Error("logs panel present with no Loki configured; it would query a data source that does not exist")
		}
		if strings.Contains(raw, `"type": "loki"`) {
			t.Errorf("dashboard still references a Loki data source with no Loki configured:\n%s", raw)
		}
	})

	t.Run("rendered when lokiLogs.enabled", func(t *testing.T) {
		raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n")
		vars, logs := lokiBits(t, raw)
		if !contains(vars, "loki") || !contains(vars, "logsource") {
			t.Errorf("loki/logsource vars missing with lokiLogs.enabled: %v", vars)
		}
		// The panel scopes itself to the chart's namespaces; a `namespace` variable would
		// miss a plugin installed elsewhere.
		if contains(vars, "namespace") {
			t.Error("the namespace variable is back; the logs panel must scope itself to both " +
				"components' namespaces instead of asking the reader for one")
		}
		if !logs {
			t.Error("logs panel missing with lokiLogs.enabled")
		}
	})

	// A pinned uid alone must render the section (the single-Loki auto-select path).
	t.Run("rendered and pinned when only the uid is set", func(t *testing.T) {
		raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiDatasourceUid: my-loki\n")
		vars, logs := lokiBits(t, raw)
		if !contains(vars, "loki") || !logs {
			t.Fatalf("loki section missing when only lokiDatasourceUid is set: vars=%v logs=%v", vars, logs)
		}
		if cur := varCurrent(t, raw, "loki"); cur["value"] != "my-loki" || cur["selected"] != true {
			t.Errorf("loki var current = %v, want my-loki pinned", cur)
		}
	})

	t.Run("logs query uses the configured stream labels", func(t *testing.T) {
		raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n")
		expr := logsExpr(t, raw)
		// OTLP/k8sattributes labels are the default ingestion path; bare `namespace`
		// labels match nothing there.
		if !strings.Contains(expr, "k8s_namespace_name=") {
			t.Errorf("logs panel LogQL %q does not select on k8s_namespace_name", expr)
		}
		if strings.Contains(expr, "app_kubernetes_io_name") {
			t.Errorf("logs panel LogQL %q uses app_kubernetes_io_name, not a promoted stream label under OTLP", expr)
		}

		raw = fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n  lokiLabels:\n    namespace: namespace\n    workload: pod\n")
		if expr := logsExpr(t, raw); !strings.Contains(expr, `namespace=~`) {
			t.Errorf("promtail override did not rewrite the namespace label; got %q", expr)
		}
		if strings.Contains(raw, "k8s_namespace_name") {
			t.Errorf("promtail override left OTLP labels in the dashboard:\n%s", raw)
		}

		raw = fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n  lokiLabels:\n    namespace: namespace\n    workload: app\n    pluginWorkload: app\n")
		if !strings.Contains(raw, `app=~\"cpu-pinning-operator.*\"`) {
			t.Errorf("the log-source options do not use the configured promtail workload label:\n%s", raw)
		}
		if !strings.Contains(raw, `app=\"nri-resource-policy-balloons\"`) {
			t.Errorf("the log-source options do not use the configured plugin label and name:\n%s", raw)
		}
	})

	// The selector must be interpolated raw: its fragments hold quotes other formats
	// would encode.
	t.Run("logs panel reads the source picker", func(t *testing.T) {
		raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n")
		if expr := logsExpr(t, raw); !strings.Contains(expr, "${logsource:raw}") {
			t.Errorf("logs panel LogQL %q does not interpolate the log-source picker raw", expr)
		}
		var opts []any
		var parsed struct {
			Templating struct {
				List []map[string]any `json:"list"`
			} `json:"templating"`
		}
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatalf("parse dashboard: %v", err)
		}
		for _, v := range parsed.Templating.List {
			if v["name"] == "logsource" {
				opts, _ = v["options"].([]any)
				if q, _ := v["query"].(string); strings.Count(q, ",") != 1 {
					t.Errorf("logsource query %q has a comma inside an option; Grafana splits "+
						"the query on commas and would render broken options", q)
				}
			}
		}
		if len(opts) != 2 {
			t.Fatalf("logsource has %d option(s), want the operator and the plugin", len(opts))
		}
		if first := opts[0].(map[string]any); first["selected"] != true ||
			!strings.Contains(first["text"].(string), "Operator") {
			t.Errorf("first logsource option = %v, want the operator selected on the cluster page", first)
		}
	})

	// The benign-error filter is written twice (chart JSON and Go) and the two must
	// agree.
	t.Run("benign plugin errors filtered as the per-machine pages filter them", func(t *testing.T) {
		raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n")
		if expr := logsExpr(t, raw); !strings.Contains(expr, "${benign:raw}") {
			t.Errorf("logs panel LogQL %q does not interpolate the filter picker; the noise "+
				"would be back on the cluster page", expr)
		}
		var parsed struct {
			Templating struct {
				List []map[string]any `json:"list"`
			} `json:"templating"`
		}
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatalf("parse dashboard: %v", err)
		}
		var benign map[string]any
		for _, v := range parsed.Templating.List {
			if v["name"] == "benign" {
				benign = v
			}
		}
		if benign == nil {
			t.Fatal("no benign filter picker; the panel interpolates ${benign:raw} and would query garbage")
		}
		// Grafana rebuilds `options` from `query` on load, so `query` is what applies.
		q, _ := benign["query"].(string)
		if strings.Count(q, ",") != 1 {
			t.Errorf("benign query %q has a comma inside an option; Grafana splits the query "+
				"on commas and would render broken options", q)
		}
		for _, want := range []string{dashboard.BenignPluginFilter, dashboard.UnfilteredLineFilter} {
			if !strings.Contains(q, want) {
				t.Errorf("benign query %q does not carry %q; Grafana re-parses this field, so "+
					"the two pages would filter differently", q, want)
			}
		}
		opts, _ := benign["options"].([]any)
		if len(opts) != 2 {
			t.Fatalf("benign has %d option(s), want hidden and unfiltered", len(opts))
		}
		hidden := opts[0].(map[string]any)
		if hidden["selected"] != true {
			t.Errorf("first benign option = %v, want it selected: the noise is hidden by default", hidden)
		}
		if got := hidden["value"]; got != dashboard.BenignPluginFilter {
			t.Errorf("chart filter = %q, per-machine filter = %q; the two pages must hide the "+
				"same lines", got, dashboard.BenignPluginFilter)
		}
		if got := opts[1].(map[string]any)["value"]; got != dashboard.UnfilteredLineFilter {
			t.Errorf("chart unfiltered option = %q, want %q", got, dashboard.UnfilteredLineFilter)
		}

		cur := varCurrent(t, raw, "benign")
		if cur["value"] != dashboard.BenignPluginFilter || cur["selected"] != true {
			t.Errorf("benign current = %v, want %q selected", cur, dashboard.BenignPluginFilter)
		}

		desc := logsPanelDescription(t, raw)
		for _, want := range []string{"libmem: unknown allocation",
			"PodResources API Get method disabled", "docs/observability.md"} {
			if !strings.Contains(desc, want) {
				t.Errorf("logs panel description does not name %q; a reader cannot tell what is hidden", want)
			}
		}
	})

	t.Run("logs row is the last row", func(t *testing.T) {
		raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n")
		var dash struct {
			Panels []map[string]any `json:"panels"`
		}
		if err := json.Unmarshal([]byte(raw), &dash); err != nil {
			t.Fatalf("parse dashboard: %v", err)
		}
		var lastRow string
		var maxY float64
		for _, p := range dash.Panels {
			y := p["gridPos"].(map[string]any)["y"].(float64)
			if p["type"] == "row" && y >= maxY {
				maxY, lastRow = y, fmt.Sprint(p["title"])
			}
		}
		if !strings.Contains(lastRow, "Operator logs") {
			t.Errorf("the last row is %q, want the operator-logs row at the bottom", lastRow)
		}
	})
}

// TestFleetDashboardVariablesResolve proves no variable claims a selection with an empty
// value, which Grafana reports as "Datasource was not found".
func TestFleetDashboardVariablesResolve(t *testing.T) {
	noEmptyPins := func(t *testing.T, label, raw string) {
		t.Helper()
		var parsed struct {
			Templating struct {
				List []struct {
					Name    string         `json:"name"`
					Current map[string]any `json:"current"`
				} `json:"list"`
			} `json:"templating"`
		}
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatalf("%s: parse dashboard: %v", label, err)
		}
		for _, v := range parsed.Templating.List {
			sel, _ := v.Current["selected"].(bool)
			val, hasVal := v.Current["value"]
			if sel && (!hasVal || val == "") {
				t.Errorf("%s: template var %q pins an empty value (%v); Grafana reports "+
					"\"Datasource was not found\" for it", label, v.Name, v.Current)
			}
		}
	}
	for name, values := range fleetValueSets {
		noEmptyPins(t, name, fleetDashboardJSON(t, values))
	}

	noEmptyPins(t, "per-node dashboard", renderNodeDashboard(t))

	// The default must leave the Prometheus variable unpinned so Grafana auto-selects
	// (its uid is not "prometheus" on OpenShift).
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
	if cur := varCurrent(t, raw, "datasource"); len(cur) != 0 {
		t.Errorf("datasource var current = %v, want empty so Grafana auto-selects", cur)
	}
}

// TestDashboardPanelDescriptions proves every panel and template variable on both
// dashboards has a description within dashboard.MaxDescriptionChars.
func TestDashboardPanelDescriptions(t *testing.T) {
	check := func(t *testing.T, label, raw string) {
		t.Helper()
		panels := flattenPanels(t, raw)
		if len(panels) < 5 {
			t.Fatalf("%s: only %d panels found; the walk is not finding them", label, len(panels))
		}
		for _, p := range panels {
			if p["type"] == "row" {
				continue
			}
			desc, _ := p["description"].(string)
			if strings.TrimSpace(desc) == "" {
				t.Errorf("%s: panel %q (%v) has no description", label, p["title"], p["type"])
			}
			assertDescriptionFits(t, fmt.Sprintf("%s: the description of panel %q", label, p["title"]), desc)
		}
		for _, v := range templateVars(t, raw) {
			desc, _ := v["description"].(string)
			assertDescriptionFits(t, fmt.Sprintf("%s: the description of variable %q", label, v["name"]), desc)
		}
	}
	check(t, "cluster dashboard", fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n"))
	check(t, "per-node dashboard", renderNodeDashboard(t))
	check(t, "per-node dashboard with logs", renderNodeDashboardWithLogs(t))
}

// assertDescriptionFits fails naming what is too long and by how much.
func assertDescriptionFits(t *testing.T, what, desc string) {
	t.Helper()
	if n := utf8.RuneCountInString(desc); n > dashboard.MaxDescriptionChars {
		t.Errorf("%s is %d characters, over the %d-character maximum by %d:\n%s",
			what, n, dashboard.MaxDescriptionChars, n-dashboard.MaxDescriptionChars, desc)
	}
}

// templateVars returns a dashboard's template variables.
func templateVars(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var dash struct {
		Templating struct {
			List []map[string]any `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal([]byte(raw), &dash); err != nil {
		t.Fatalf("parse dashboard templating: %v", err)
	}
	return dash.Templating.List
}

// TestNodeDashboardCitesTheRealLabel proves the allocation-mode description names the
// label the Node reconciler stamps; dashboard cannot import the controller.
func TestNodeDashboardCitesTheRealLabel(t *testing.T) {
	raw := renderNodeDashboard(t)
	if !strings.Contains(raw, controller.LabelCPUAllocation) {
		t.Errorf("no panel cites the CPU-allocation node label %q that the Node reconciler sets",
			controller.LabelCPUAllocation)
	}
}

// TestDashboardPanelIDsUnique proves panel ids are unique: Grafana silently drops a
// duplicate.
func TestDashboardPanelIDsUnique(t *testing.T) {
	check := func(t *testing.T, label, raw string) {
		t.Helper()
		seen := map[float64]string{}
		for _, p := range flattenPanels(t, raw) {
			id, ok := p["id"].(float64)
			if !ok {
				t.Errorf("%s: panel %q has no id", label, p["title"])
				continue
			}
			if prev, dup := seen[id]; dup {
				t.Errorf("%s: panel id %v used by both %q and %q; Grafana keeps only one",
					label, id, prev, p["title"])
			}
			seen[id] = fmt.Sprint(p["title"])
		}
	}
	check(t, "cluster dashboard", fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n"))
	check(t, "cluster dashboard (no loki)", fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n"))
	check(t, "per-node dashboard", renderNodeDashboard(t))
}

// gaugeMetrics are the NRT-sourced gauges; a standby may export them too, so each must be
// wrapped in `max` first or every number doubles.
// Recording rules are excluded: they already deduplicate with max.
var gaugeMetrics = []string{
	"cpu_pinning_cpu_assignment",
	"cpu_pinning_pool_membership",
	"cpu_pinning_node_logical_cpus",
	"cpu_pinning_node_pinnable_cpus",
	"cpu_pinning_node_reserved_cpus",
	"cpu_pinning_reserved_pool_utilization_ratio",
	"cpu_pinning_ht_sibling_policy",
	"cpu_pinning_ghost_allocations",
	"cpu_pinning_cpu_fragmentation",
	"cpu_pinning_webhook_reservations_live",
}

func TestDashboardGaugeAggregations(t *testing.T) {
	// Self-check: a detector that stops detecting turns this test green.
	for _, tc := range []struct {
		expr string
		want bool // want a finding
	}{
		// Must be caught.
		{`cpu_pinning:cpu_free:per_numa{node="$node"} or (sum by (node, numa) (cpu_pinning_node_logical_cpus{node="$node"}) - 1)`, true},
		{`sum(cpu_pinning_ghost_allocations)`, true},
		// Counting raw series doubles with two exporting replicas.
		{`count by (node, numa) (cpu_pinning_cpu_assignment{balloon!="free"})`, true},
		{`count(cpu_pinning_cpu_assignment)`, true},
		{`avg(cpu_pinning_reserved_pool_utilization_ratio)`, true},
		// No dedup at all.
		{`cpu_pinning_cpu_fragmentation{node="x"}`, true},
		// Must pass.
		{`max by (node, numa) (cpu_pinning_node_logical_cpus{node="n"}) - count by (node, numa) (max by (node, numa, cpu) (cpu_pinning_cpu_assignment{node="n"}))`, false},
		{`sum(max by (node) (cpu_pinning_ghost_allocations))`, false},
		{`count(max by (node) (cpu_pinning_cpu_fragmentation) > 0)`, false},
		{`max by (node, policy) (cpu_pinning_ht_sibling_policy == 1)`, false},
		{`(max by (node) (cpu_pinning_node_logical_cpus) * 0)`, false},
	} {
		got := len(undeduplicatedGauges(tc.expr)) > 0
		if got != tc.want {
			t.Fatalf("detector: finding=%v, want %v, for %q", got, tc.want, tc.expr)
		}
	}

	check := func(t *testing.T, label, raw string) {
		t.Helper()
		checked := 0
		for _, p := range flattenPanels(t, raw) {
			// The canvas panel pivots one series per CPU into one field, so a duplicate
			// exporter collapses rather than adds.
			if p["type"] == "canvas" {
				continue
			}
			for _, expr := range panelExprs(p) {
				checked++
				if got := undeduplicatedGauges(expr); len(got) > 0 {
					t.Errorf("%s: panel %q reads the raw gauge(s) %v without collapsing them "+
						"with max — a second exporting replica would double it: %s",
						label, p["title"], got, expr)
				}
			}
		}
		if checked == 0 {
			t.Errorf("%s: no panel queries were checked; the walk is not finding them", label)
		}
	}
	check(t, "cluster dashboard", fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n"))
	check(t, "per-node dashboard", renderNodeDashboard(t))
}

// usesCPUTotalAsCapacity reports whether expr uses a per-NUMA CPU total as a capacity
// (subtracted from or a ratio denominator), not as inventory.
func usesCPUTotalAsCapacity(expr string) bool {
	if !strings.Contains(expr, "cpu_pinning_node_logical_cpus") {
		return false
	}
	return strings.Contains(expr, "- (count") || strings.Contains(expr, "- count") ||
		strings.Contains(expr, ") / sum") || strings.Contains(expr, ") / max")
}

// TestCapacityDenominatorsReadThePinnablePool proves capacities read
// cpu_pinning_node_pinnable_cpus, not the hardware total, which counts excluded CPUs.
func TestCapacityDenominatorsReadThePinnablePool(t *testing.T) {
	// Self-check: a detector that stops detecting turns this green.
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{`sum(max by (node, numa) (cpu_pinning_node_logical_cpus))`, false},
		{`count(max by (node) (cpu_pinning_node_logical_cpus)) or vector(0)`, false},
		{`max by (node) (cpu_pinning_webhook_reservations_live) or (max by (node) (cpu_pinning_node_logical_cpus) * 0)`, false},
		{`sum(max by (node, numa) (cpu_pinning_node_logical_cpus)) - (count(max by (node, cpu) (cpu_pinning_cpu_assignment)) or vector(0))`, true},
		{`sum by (node) (x) / sum by (node) (max by (node, numa) (cpu_pinning_node_logical_cpus))`, true},
	} {
		if got := usesCPUTotalAsCapacity(tc.expr); got != tc.want {
			t.Fatalf("detector: capacity=%v, want %v, for %q", got, tc.want, tc.expr)
		}
	}

	check := func(t *testing.T, label, raw string) {
		checked := 0
		for _, p := range flattenPanels(t, raw) {
			for _, expr := range panelExprs(p) {
				if !usesCPUTotalAsCapacity(expr) {
					continue
				}
				checked++
				if !strings.Contains(expr, "cpu_pinning_node_pinnable_cpus") {
					t.Errorf("%s: panel %q reads the hardware total as capacity without the "+
						"pinnable pool, so it can report room on a machine that is refusing "+
						"pods: %s", label, p["title"], expr)
				}
			}
		}
		if checked == 0 {
			t.Errorf("%s: no capacity expression was checked; the detector is not finding them", label)
		}
	}
	check(t, "cluster dashboard", fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n"))
	check(t, "per-node dashboard", renderNodeDashboard(t))
}

// TestGlossaryTermsDefinedEverywhere proves every dashboard.GlossaryTerms entry is
// explained in both dashboards and docs/observability.md.
func TestGlossaryTermsDefinedEverywhere(t *testing.T) {
	// Narrowed to the glossary: the terms also occur in ordinary panel descriptions.
	sources := map[string]string{
		"cluster dashboard glossary panel": glossaryPanelText(t,
			fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")),
		"per-node dashboard glossary panel": glossaryPanelText(t, renderNodeDashboard(t)),
		"docs/observability.md glossary":    docsGlossarySection(t),
	}
	for label, body := range sources {
		lower := strings.ToLower(body)
		for _, term := range dashboard.GlossaryTerms {
			if !strings.Contains(lower, strings.ToLower(term)) {
				t.Errorf("%s does not define the term %q", label, term)
			}
		}
	}
}

// glossaryPanelText returns the glossary panel's markdown, found by title since the
// per-machine page has a second text panel.
func glossaryPanelText(t *testing.T, raw string) string {
	t.Helper()
	var found []string
	for _, p := range flattenPanels(t, raw) {
		if p["type"] != "text" {
			continue
		}
		title, _ := p["title"].(string)
		if !strings.Contains(title, "words on this dashboard") {
			continue
		}
		opts, _ := p["options"].(map[string]any)
		content, _ := opts["content"].(string)
		found = append(found, content)
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one text panel carrying the glossary, found %d", len(found))
	}
	if strings.TrimSpace(found[0]) == "" {
		t.Fatal("the glossary text panel is empty")
	}
	return found[0]
}

// TestGlossaryRowIsFirst proves the "how to read this page" row is first and collapsed on
// both dashboards.
func TestGlossaryRowIsFirst(t *testing.T) {
	check := func(t *testing.T, label, raw string) {
		t.Helper()
		var dash struct {
			Panels []map[string]any `json:"panels"`
		}
		if err := json.Unmarshal([]byte(raw), &dash); err != nil {
			t.Fatalf("%s: parse dashboard: %v", label, err)
		}
		if len(dash.Panels) == 0 {
			t.Fatalf("%s: dashboard has no panels", label)
		}
		first := dash.Panels[0]
		title, _ := first["title"].(string)
		if first["type"] != "row" || !strings.Contains(title, "How to read this page") {
			t.Errorf("%s: first panel is %v %q, want the glossary row", label, first["type"], title)
		}
		if first["collapsed"] != true {
			t.Errorf("%s: the glossary row is not collapsed; it would push the data down the page", label)
		}
		if y, _ := first["gridPos"].(map[string]any)["y"].(float64); y != 0 {
			t.Errorf("%s: the glossary row sits at y=%v, want 0", label, y)
		}
	}
	check(t, "cluster dashboard", fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n"))
	check(t, "per-node dashboard", renderNodeDashboard(t))
}

// TestDashboardsCountExactly proves no panel windows a counter with
// increase/rate/irate/delta: the operator's counters are too sparse to extrapolate.
// denseCounters is the exception list: cAdvisor's CPU counter moves continuously, but
// never over [$__interval].
var denseCounters = []string{"container_cpu_usage_seconds_total"}

func TestDashboardsCountExactly(t *testing.T) {
	check := func(t *testing.T, label, raw string) {
		t.Helper()
		checked := 0
		for _, p := range flattenPanels(t, raw) {
			for _, expr := range panelExprs(p) {
				checked++
				if strings.Contains(expr, "[$__interval]") {
					t.Errorf("%s: panel %q reads over [$__interval], which is ~30s at a 6h range "+
						"and returns nothing: %s", label, p["title"], expr)
				}
				if isDenseCounterExpr(expr) {
					continue
				}
				for _, fn := range []string{"increase(", "rate(", "irate(", "delta("} {
					if strings.Contains(expr, fn) {
						t.Errorf("%s: panel %q uses %s on a counter that moves a few times a "+
							"day, so it extrapolates. Read the counter's own samples instead: %s",
							label, p["title"], fn, expr)
					}
				}
			}
		}
		if checked == 0 {
			t.Errorf("%s: no panel queries were checked; the walk is not finding them", label)
		}
	}
	check(t, "cluster dashboard", fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n"))
	check(t, "per-node dashboard", renderNodeDashboard(t))

	// Self-check: an exception list that matches everything would turn this green.
	sparse := `sum(rate(cpu_pinning_node_provisioning_total[$__range]))`
	if isDenseCounterExpr(sparse) {
		t.Errorf("the dense-counter exception matches %q, so it no longer protects the "+
			"operator's own counters", sparse)
	}
	dense := `rate(container_cpu_usage_seconds_total{node="n"}[$__range])`
	if !isDenseCounterExpr(dense) {
		t.Errorf("the dense-counter exception does not match %q, so the per-container usage "+
			"column cannot be expressed", dense)
	}
}

// isDenseCounterExpr reports whether expr reads only counters on denseCounters; mixing in
// an operator `_total` counter keeps no exemption.
func isDenseCounterExpr(expr string) bool {
	stripped := expr
	for _, c := range denseCounters {
		stripped = strings.ReplaceAll(stripped, c, "")
	}
	if stripped == expr {
		return false // reads no dense counter
	}
	return !strings.Contains(stripped, "_total")
}

// behaviourPanels count what the operator has done, read across processes so a rollout
// does not empty them.
var behaviourPanels = []string{
	"Admission decisions",
	"How long a decision takes, out of the 10s allowed",
	"Machine configuration changes",
}

// lastOverTimeWindow captures the trailing window (in days) of every last_over_time read.
var lastOverTimeWindow = regexp.MustCompile(`last_over_time\(\w+\[(\d+)d\]\)`)

// splitBy and dedupBy capture a behaviour panel's outer grouping and its inner
// per-process dedup labels.
var (
	splitBy = regexp.MustCompile(`sum\s+by\s*\(([^)]*)\)`)
	dedupBy = regexp.MustCompile(`max\s+by\s*\(([^)]*)\)`)
)

// labelList splits a PromQL grouping list into its label names.
func labelList(group string) []string {
	var out []string
	for _, label := range strings.Split(group, ",") {
		if label = strings.TrimSpace(label); label != "" {
			out = append(out, label)
		}
	}
	return out
}

// groupsBy reports whether a grouping list carries a label.
func groupsBy(group, label string) bool {
	for _, have := range labelList(group) {
		if have == label {
			return true
		}
	}
	return false
}

// TestOperatorCountsSurviveARestart proves behaviour panels sum each process's last
// value, deduplicated per pod, so a replaced pod keeps contributing.
// No PromQL engine is available, so it pins the shape and evaluates it over a synthetic
// scrape.
func TestOperatorCountsSurviveARestart(t *testing.T) {
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
	found := map[string][]string{}
	for _, p := range flattenPanels(t, raw) {
		title, _ := p["title"].(string)
		for _, want := range behaviourPanels {
			if title == want {
				found[title] = panelExprs(p)
			}
		}
	}
	for _, title := range behaviourPanels {
		exprs, ok := found[title]
		if !ok {
			t.Errorf("the cluster dashboard has no panel %q", title)
			continue
		}
		if len(exprs) == 0 {
			t.Errorf("panel %q carries no query", title)
		}
		for _, expr := range exprs {
			if !strings.Contains(expr, "last_over_time(") {
				t.Errorf("panel %q reads the counter as it stands, so a rollout empties it: %s",
					title, expr)
			}
			// Only `pod` can dedup: pod IPs are recycled, so two processes can share an
			// `instance`.
			if !strings.Contains(expr, "max by (pod") {
				t.Errorf("panel %q does not deduplicate per pod, so one process scraped twice "+
					"counts twice: %s", title, expr)
			}
			if strings.Contains(expr, "instance") {
				t.Errorf("panel %q separates processes by instance, and pod addresses are "+
					"recycled: %s", title, expr)
			}
			// The split label must survive the dedup too, or the panel reports the max
			// where it means the sum.
			for _, split := range splitBy.FindAllStringSubmatch(expr, -1) {
				for _, label := range labelList(split[1]) {
					for _, dedup := range dedupBy.FindAllStringSubmatch(expr, -1) {
						if !groupsBy(dedup[1], label) {
							t.Errorf("panel %q splits by %q but deduplicates over `max by (%s)`, "+
								"which collapses that split and undercounts it: %s",
								title, label, dedup[1], expr)
						}
					}
				}
			}
			for _, m := range lastOverTimeWindow.FindAllStringSubmatch(expr, -1) {
				days, err := strconv.Atoi(m[1])
				if err != nil || days < 7 {
					t.Errorf("panel %q reads over %sd; a window this short drops the processes "+
						"whose counts the panel exists to keep: %s", title, m[1], expr)
				}
			}
		}
	}

	// The first process served 5 and is gone; its successor served 3 and is scraped
	// twice. Expect 8.
	scrape := []processCount{
		{pod: "replaced", result: "pinned", last: 5},
		{pod: "running", result: "pinned", last: 3, live: true},
		{pod: "running", result: "pinned", last: 3, live: true},
	}
	if got := readAcrossProcesses(scrape); got["pinned"] != 8 {
		t.Errorf("the panels' shape reports %v decisions across a pod replacement, want 8 "+
			"(5 from the replaced process, 3 from the running one, counted once)", got["pinned"])
	}
	// Read as exported: the gone process is lost and the duplicate double-counted, giving
	// 6.
	if got := readAsExported(scrape); got["pinned"] != 6 {
		t.Errorf("reading the counter as exported reports %v, want 6; the synthetic scrape no "+
			"longer contains the replacement and the double scrape this test is about",
			got["pinned"])
	}
}

// processCount is one counter series as a scrape leaves it.
type processCount struct {
	pod    string
	result string
	last   float64
	live   bool
}

// readAcrossProcesses computes what the behaviour panels do: last value per process,
// deduplicated per pod, summed per label value.
func readAcrossProcesses(scrape []processCount) map[string]float64 {
	perProcess := map[[2]string]float64{}
	for _, s := range scrape {
		k := [2]string{s.pod, s.result}
		if s.last > perProcess[k] {
			perProcess[k] = s.last
		}
	}
	out := map[string]float64{}
	for k, v := range perProcess {
		out[k[1]] += v
	}
	return out
}

// readAsExported sums the counter as it stands, seeing only processes still scraped.
func readAsExported(scrape []processCount) map[string]float64 {
	out := map[string]float64{}
	for _, s := range scrape {
		if s.live {
			out[s.result] += s.last
		}
	}
	return out
}

// TestFailureBreakdownIsSilentWhenHealthy proves the failure breakdown drops zero rows
// and says so in one line when all are zero.
func TestFailureBreakdownIsSilentWhenHealthy(t *testing.T) {
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
	for _, p := range flattenPanels(t, raw) {
		title, _ := p["title"].(string)
		if !strings.Contains(title, "Failures by kind") {
			continue
		}
		var filtered bool
		trs, _ := p["transformations"].([]any)
		for _, tr := range trs {
			m, _ := tr.(map[string]any)
			if m["id"] == "filterByValue" {
				filtered = true
			}
		}
		if !filtered {
			t.Errorf("panel %q keeps every row; a healthy cluster must not list eight zeros", title)
		}
		if nv, _ := p["fieldConfig"].(map[string]any)["defaults"].(map[string]any)["noValue"].(string); nv == "" {
			t.Errorf("panel %q has no empty-state message, so a healthy cluster reads "+
				"as missing data rather than as nothing wrong", title)
		}
		for _, expr := range panelExprs(p) {
			// These counters do not exist until their first event; `or vector(0)`
			// manufactures zeros.
			if strings.Contains(expr, "vector(0)") {
				t.Errorf("panel %q substitutes a zero for an absent counter, which is the row "+
					"a healthy cluster should not have: %s", title, expr)
			}
		}
		return
	}
	t.Fatal("the cluster dashboard has no panel listing the failures by kind")
}

// TestAdmissionLatencyIsExactAndNotAHeatmap proves admission latency is one tile, a
// quantile over the bucket counts: admissions are too sparse for a heatmap or rate()d
// quantile.
func TestAdmissionLatencyIsExactAndNotAHeatmap(t *testing.T) {
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
	for _, p := range flattenPanels(t, raw) {
		if p["type"] == "heatmap" {
			t.Errorf("panel %q is a heatmap; it renders empty with a NaN legend on a cluster "+
				"that admits a pod occasionally", p["title"])
		}
		for _, expr := range panelExprs(p) {
			if strings.Contains(expr, "cpu_pinning_webhook_admission_seconds") &&
				(strings.Contains(expr, "rate(") || strings.Contains(expr, "increase(")) {
				t.Errorf("panel %q reads the admission histogram as a rate, which is empty "+
					"whenever no admission fell inside the window: %s", p["title"], expr)
			}
		}
	}
}

// TestClusterPageGroupsByMeaning proves the failure breakdown sits behind a collapsed row
// with a top tile that reads clean at zero, and rows are named for what their panels
// mean.
func TestClusterPageGroupsByMeaning(t *testing.T) {
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n")
	var dash struct {
		Panels []map[string]any `json:"panels"`
	}
	if err := json.Unmarshal([]byte(raw), &dash); err != nil {
		t.Fatalf("parse dashboard: %v", err)
	}

	rowTitle := func(want string) map[string]any {
		for _, p := range dash.Panels {
			title, _ := p["title"].(string)
			if p["type"] == "row" && strings.Contains(title, want) {
				return p
			}
		}
		t.Fatalf("the cluster dashboard has no %q row", want)
		return nil
	}

	failures := rowTitle("Failures")
	if failures["collapsed"] != true {
		t.Error("the Failures row is expanded, so a healthy cluster spends screen space on " +
			"a panel that has nothing to say")
	}
	nested, _ := failures["panels"].([]any)
	if len(nested) != 1 {
		t.Errorf("the Failures row holds %d panels; it must hold the failure breakdown and "+
			"nothing else — latency and provisioning are the operator working normally", len(nested))
	}
	for _, n := range nested {
		p, _ := n.(map[string]any)
		if title, _ := p["title"].(string); !strings.Contains(title, "Failures by kind") {
			t.Errorf("the Failures row carries %q, which is not a failure", title)
		}
	}

	behaviour := rowTitle("How the operator is behaving")
	bY := behaviour["gridPos"].(map[string]any)["y"].(float64)
	fY := failures["gridPos"].(map[string]any)["y"].(float64)
	if !(bY < fY) {
		t.Errorf("the behaviour row (y=%v) is not above the Failures row (y=%v)", bY, fY)
	}
	var inBehaviour []string
	for _, p := range dash.Panels {
		g, ok := p["gridPos"].(map[string]any)
		if !ok || p["type"] == "row" {
			continue
		}
		if y := g["y"].(float64); y > bY && y < fY {
			title, _ := p["title"].(string)
			inBehaviour = append(inBehaviour, title)
		}
	}
	for _, want := range []string{"Admission decisions",
		"How long a decision takes", "Machine configuration changes"} {
		found := false
		for _, got := range inBehaviour {
			if strings.Contains(got, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not in the row about how the operator behaves; that row holds %v",
				want, inBehaviour)
		}
	}

	// The per-machine verdict lives in the machine table, which already links each
	// machine.
	table := panelByTitle(t, raw, "Machines — one row per managed machine (click a name to open it)")
	for _, p := range dash.Panels {
		title, _ := p["title"].(string)
		if strings.Contains(title, "needing attention") {
			t.Errorf("panel %q repeats the machine table's verdict without naming a machine",
				title)
		}
	}

	var column string
	for _, tr := range table["transformations"].([]any) {
		m, _ := tr.(map[string]any)
		if m["id"] != "organize" {
			continue
		}
		o, _ := m["options"].(map[string]any)
		rename, _ := o["renameByName"].(map[string]any)
		for field, to := range rename {
			if to == "Status" {
				column = field
			}
		}

		if idx, _ := o["indexByName"].(map[string]any); idx[column] != float64(1) {
			t.Errorf("the verdict column sits at position %v, so a reader scans past the "+
				"machine's shape to reach it", idx[column])
		}
	}
	if column == "" {
		t.Fatal("the machine table has no column saying whether a machine needs acting on, so " +
			"a reader has to read several numbers and combine them")
	}
	var expr string
	for _, tg := range table["targets"].([]any) {
		m, _ := tg.(map[string]any)
		if "Value #"+m["refId"].(string) == column {
			expr, _ = m["expr"].(string)
		}
	}
	// Current-state gauges, so a recovered machine clears itself.
	for _, want := range []string{"cpu_pinning_ghost_allocations", "cpu_pinning_cpu_fragmentation",
		"max by (node)"} {
		if !strings.Contains(expr, want) {
			t.Errorf("the verdict column does not read %s: %s", want, expr)
		}
	}
	// Neither gauge may default to zero: absent is routine, and a 0 would paint the
	// machine OK instead of empty.
	if strings.Contains(expr, "* 0") {
		t.Errorf("the verdict column substitutes 0 for a gauge that has not reported, so an "+
			"unmeasured machine reads OK: %s", expr)
	}
	// Kept out of a "needs acting on now" column: capacity exhaustion is not a defect,
	// and cumulative fault counters never clear.
	for _, banned := range []string{
		"cpu_pinning_pods_pinning_blocked_total",
		"cpu_pinning_capacity_check_failures_total",
		"cpu_pinning_pods_skipped_total",
		"cpu_pinning_balloonspolicy_reconcile_errors_total",
		"cpu_pinning_webhook_plugin_not_ready_total",
	} {
		if strings.Contains(expr, banned) {
			t.Errorf("the verdict column reads %s, which cannot answer whether a machine needs "+
				"acting on now: %s", banned, expr)
		}
	}
	// Both readings fold into one value: 1 shape, 2 fault, 3 both. The ghost gauge
	// carries the doubled term so a fault outranks a shape under the sort.
	if !strings.Contains(expr, "2 * clamp_max(max by (node) (cpu_pinning_ghost_allocations)") {
		t.Errorf("the verdict column does not weight ghost allocations above the fragmentation "+
			"reading, so a machine that is merely a poor shape outranks a faulty one: %s", expr)
	}
	if strings.Contains(expr, "2 * clamp_max(max by (node) (cpu_pinning_cpu_fragmentation)") {
		t.Errorf("the verdict column doubles the fragmentation reading, so a machine placing pods "+
			"normally reads as the fault the mapping names at 2: %s", expr)
	}
	wantStatusColour := map[string]string{"1": "orange", "2": "red", "3": "red"}
	var mapped map[string]bool
	for _, ovr := range table["fieldConfig"].(map[string]any)["overrides"].([]any) {
		o, _ := ovr.(map[string]any)
		if o["matcher"].(map[string]any)["options"] != "Status" {
			continue
		}
		for _, prop := range o["properties"].([]any) {
			pr, _ := prop.(map[string]any)
			if pr["id"] != "mappings" {
				continue
			}
			mapped = map[string]bool{}
			for _, mp := range pr["value"].([]any) {
				opts, _ := mp.(map[string]any)["options"].(map[string]any)
				for value, spec := range opts {
					sp, _ := spec.(map[string]any)
					text, _ := sp["text"].(string)
					mapped[value] = true
					switch value {
					case "0":
						if sp["color"] != "green" {
							t.Errorf("a clean machine reads %q in %v, want green", text, sp["color"])
						}
						if text != strings.ToUpper(text) {
							t.Errorf("the clean state reads %q; it has to stay upper case for the "+
								"ordering below to hold", text)
						}
					default:
						// Red is the fault band, amber the shape band; any reading with a
						// ghost is red.
						if want := wantStatusColour[value]; sp["color"] != want {
							t.Errorf("%q reads in %v, want %v", text, sp["color"], want)
						}
						// The field is the numeric severity, so the sort orders by
						// severity; text is display only.
						if text != strings.ToLower(text[:1])+text[1:] {
							t.Errorf("the reading %q starts upper case, so a string sort would "+
								"rank it below the clean state", text)
						}
					}
				}
			}
		}
	}

	for _, want := range []string{"0", "1", "2", "3"} {
		if !mapped[want] {
			t.Errorf("the verdict column has no text for the reading %q, so a machine in that "+
				"state shows a bare number", want)
		}
	}
	// Descending on the numeric severity, so the worst machine is the first row.
	sort, _ := table["options"].(map[string]any)["sortBy"].([]any)
	if len(sort) == 0 {
		t.Fatal("the machine table has no default sort, so a machine needing attention sits " +
			"wherever its name falls")
	}
	first, _ := sort[0].(map[string]any)
	if first["displayName"] != "Status" || first["desc"] != true {
		t.Errorf("the machine table sorts by %v desc=%v, want the verdict column descending",
			first["displayName"], first["desc"])
	}
	if !strings.HasPrefix(column, "Value #") {
		t.Errorf("the verdict column is %q rather than a query value, so it is not the numeric "+
			"field the severity sort needs", column)
	}
	// The dropped machine must render the column's empty state, not blank.
	var empty string
	for _, ovr := range table["fieldConfig"].(map[string]any)["overrides"].([]any) {
		o, _ := ovr.(map[string]any)
		if o["matcher"].(map[string]any)["options"] != "Status" {
			continue
		}
		for _, prop := range o["properties"].([]any) {
			if pr, _ := prop.(map[string]any); pr["id"] == "noValue" {
				empty, _ = pr["value"].(string)
			}
		}
	}
	if empty == "" {
		t.Error("the verdict column has no empty state, so a machine whose gauges have not " +
			"reported renders a blank cell instead of saying it is unmeasured")
	}
}

// TestClusterTilesColourOnlyJudgements proves only verdict tiles carry threshold colours;
// facts may wear a fixed pool colour but no thresholds.
func TestClusterTilesColourOnlyJudgements(t *testing.T) {
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
	facts := map[string]bool{
		"Logical CPUs": true, "Pinned pods": true,
		// The outcome split's default stays neutral; TestOutcomeBarsColourEveryOutcome
		// pins its per-outcome colours.
		"Admission decisions": true,
		// Its colours name the pool holding a CPU, not a judgement.
		"Who owns the cluster's CPUs": true,
	}
	judgements := map[string]bool{
		"Worst reserved pool": true, "Machines the widest span cannot fill": true,
		// Zero managed machines is acted on, so it takes the verdict palette.
		"Ghost allocations": true, "Machines managed": true,
		// Zero admissible machines is acted on too.
		"Machines that can take a pinned pod": true,
	}
	seen := map[string]bool{}
	for _, p := range flattenPanels(t, raw) {
		title, _ := p["title"].(string)
		fc, _ := p["fieldConfig"].(map[string]any)
		defs, _ := fc["defaults"].(map[string]any)
		thr, _ := defs["thresholds"].(map[string]any)
		steps, _ := thr["steps"].([]any)
		var coloured bool
		for _, st := range steps {
			m, _ := st.(map[string]any)
			if c, _ := m["color"].(string); c != "text" && c != "" {
				coloured = true
			}
		}
		if facts[title] {
			seen[title] = true
			if coloured {
				t.Errorf("panel %q is a fact and carries a verdict colour", title)
			}
		}
		if judgements[title] {
			seen[title] = true
			if !coloured {
				t.Errorf("panel %q is a judgement and renders in neutral text", title)
			}
		}
	}
	for want := range facts {
		if !seen[want] {
			t.Errorf("panel %q not found; the colour rule was checked against a title that "+
				"no longer exists", want)
		}
	}
	for want := range judgements {
		if !seen[want] {
			t.Errorf("panel %q not found; the colour rule was checked against a title that "+
				"no longer exists", want)
		}
	}

	// Zero managed machines is red; above zero stays neutral because no metric says how
	// many nodes should be managed.
	managed := panelByTitle(t, raw, "Machines managed")
	var colours []string
	for _, st := range managed["fieldConfig"].(map[string]any)["defaults"].(map[string]any)["thresholds"].(map[string]any)["steps"].([]any) {
		c, _ := st.(map[string]any)["color"].(string)
		colours = append(colours, c)
	}
	if want := []string{"red", "text"}; !sameStrings(colours, want) {
		t.Errorf("the managed-machines tile colours %v, want %v: only the zero case is "+
			"judgeable without inventing a target", colours, want)
	}
	// An absent series renders the empty state, which reads the same as a red zero.
	for _, expr := range panelExprs(managed) {
		if !strings.Contains(expr, "or vector(0)") {
			t.Errorf("the managed-machines tile cannot reach 0 (%s), so the red step it "+
				"carries would never render", expr)
		}
	}
}

// TestClusterTablesColourOnlyJudgements proves the tables carry the tiles' colour rule:
// verdict columns coloured, inventory columns neutral.
func TestClusterTablesColourOnlyJudgements(t *testing.T) {
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")

	// Verdict columns use their tile's palette. The reserved ratio marks 1 (where the
	// alert fires) and 0.8.
	verdicts := map[string][]string{
		"Reserved pool used": {"green", "orange", "red"},
		// Amber: those CPUs still take narrower pods.
		"Free CPUs the widest span cannot use": {"green", "orange"},
		// Status is coloured by value mappings; thresholds only cover unmapped values.
		"Status": {"green", "red"},
	}
	// Inventory: no share is right, and live reservations return to 0 on their own.
	factColumns := []string{"Pinned share", "Live reservations", "Logical CPUs", "Pinned", "Free"}

	machines := panelByTitle(t, raw, "one row per managed machine")
	for column, want := range verdicts {
		got := columnThresholdColours(t, machines, column)
		if got == nil {
			t.Errorf("the machine table's %q column carries no thresholds of its own, so it "+
				"renders in the table's neutral default", column)
			continue
		}
		if !sameStrings(got, want) {
			t.Errorf("the machine table's %q column colours %v, want %v", column, got, want)
		}
	}
	for _, column := range factColumns {
		for _, c := range columnThresholdColours(t, machines, column) {
			if c != "text" {
				t.Errorf("the machine table's %q column is inventory and carries the verdict "+
					"colour %q", column, c)
			}
		}
	}

	// Failures are coloured by kind, not count; the deliberate skip is not a fault.
	failures := panelByTitle(t, raw, "Failures by kind")
	if cols := columnThresholdColours(t, failures, "How many"); len(cols) > 0 {
		t.Errorf("the failure count carries threshold colours %v; the count is a magnitude "+
			"and the severity is on the kind beside it", cols)
	}
	mapped := map[string]string{}
	for _, prop := range columnProperties(t, failures, "What happened") {
		if prop["id"] != "mappings" {
			continue
		}
		list, _ := prop["value"].([]any)
		for _, m := range list {
			opts, _ := m.(map[string]any)["options"].(map[string]any)
			for name, spec := range opts {
				s, _ := spec.(map[string]any)
				mapped[name], _ = s["color"].(string)
			}
		}
	}
	if len(mapped) == 0 {
		t.Fatal("the failure breakdown maps no severity onto any kind, so a reader cannot " +
			"tell an act-now row from a deliberate skip")
	}
	// Every kind must have a severity, so a new failure kind fails here until classified.
	const deliberate = "Left unpinned on purpose"
	kinds := regexp.MustCompile(`"problem", "([^"]+)"`)
	for _, expr := range panelExprs(failures) {
		for _, m := range kinds.FindAllStringSubmatch(expr, -1) {
			kind := m[1]
			colour, ok := mapped[kind]
			if kind == deliberate {
				if ok {
					t.Errorf("%q is coloured %q; it is a deliberate skip and the panel says so",
						kind, colour)
				}
				continue
			}
			if !ok {
				t.Errorf("failure kind %q has no severity colour; a reader cannot tell whether "+
					"to act on it", kind)
				continue
			}
			if colour != "red" && colour != "orange" {
				t.Errorf("failure kind %q is coloured %q; the palette means act now (red) or "+
					"worth a look (orange)", kind, colour)
			}
		}
	}

	var coloursText bool
	for _, prop := range columnProperties(t, failures, "What happened") {
		if prop["id"] != "custom.cellOptions" {
			continue
		}
		v, _ := prop["value"].(map[string]any)
		if v["type"] == "color-text" {
			coloursText = true
		}
	}
	if !coloursText {
		t.Error("the failure kinds are mapped to colours the cell does not render, so the " +
			"severity is invisible")
	}
}

// TestOutcomeBarsColourEveryOutcome proves every outcome bar is coloured by what it
// means: success green (pinned in the pinned-pool colour), deliberate non-success
// neutral, refusal amber, fault red.
// The panel default stays neutral so an unclassified outcome arrives plain.
func TestOutcomeBarsColourEveryOutcome(t *testing.T) {
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
	for _, tc := range []struct {
		title    string
		fault    string
		outcomes map[string]string
	}{
		// internal/webhook's result values, which are unexported.
		{"Admission decisions", "error", map[string]string{
			"pinned": dashboard.ColorPinned, "skipped": dashboard.ColorFree,
			"denied": "orange", "error": "red",
		}},
		{"Machine configuration changes", metrics.NodeProvisionFailed, map[string]string{
			metrics.NodeProvisioned:     "green",
			metrics.NodeUpdated:         "green",
			metrics.NodeDeprovisioned:   dashboard.ColorFree,
			metrics.NodeProvisionFailed: "red",
		}},
	} {
		p := panelByTitle(t, raw, tc.title)
		if mode, _ := p["options"].(map[string]any)["valueMode"].(string); mode != "color" {
			t.Errorf("%q renders its values in %q, so the fault's own number stays white",
				tc.title, mode)
		}
		defs, _ := p["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
		thr, _ := defs["thresholds"].(map[string]any)
		list, _ := thr["steps"].([]any)
		for _, st := range list {
			if c, _ := st.(map[string]any)["color"].(string); c != "text" {
				t.Errorf("%q colours an unclassified outcome %q; the default has to stay neutral",
					tc.title, c)
			}
		}
		for outcome, want := range tc.outcomes {
			var got string
			// A bargauge colours from thresholds, so the colour must be set in both
			// places.
			var banded bool
			for _, prop := range columnProperties(t, p, outcome) {
				switch prop["id"] {
				case "color":
					v, _ := prop["value"].(map[string]any)
					got, _ = v["fixedColor"].(string)
				case "thresholds":
					v, _ := prop["value"].(map[string]any)
					steps, _ := v["steps"].([]any)
					for _, st := range steps {
						if c, _ := st.(map[string]any)["color"].(string); c == want {
							banded = true
						}
					}
				}
			}
			if got != want {
				t.Errorf("%q paints its %q bar %q, want %q", tc.title, outcome, got, want)
			}
			if !banded {
				t.Errorf("%q gives its %q bar no %q threshold band, so the bar itself stays grey",
					tc.title, outcome, want)
			}
		}
		// Bar length is the share of all outcomes, which survives a restart; the total
		// arrives as a second query via configFromData.
		if defs["min"] != float64(0) {
			t.Errorf("%q has min %v, so its bars are not shares of anything", tc.title, defs["min"])
		}
		targets, _ := p["targets"].([]any)
		if len(targets) != 2 {
			t.Fatalf("%q has %d queries, want the split plus the total it is a share of",
				tc.title, len(targets))
		}
		total, _ := targets[1].(map[string]any)["expr"].(string)
		if !strings.Contains(total, "sum(") {
			t.Errorf("%q sizes itself from %q, which is still split by label rather than the "+
				"total", tc.title, total)
		}
		// Both halves read the counter so it survives a rollout: last sample per `pod`
		// (not `instance`, IPs recycle), max by inside the sum, fixed window.
		// The dedup must also carry the split label;
		// TestDurableCountsDeduplicateOnEveryLabel holds it to every label.
		split := targets[0].(map[string]any)["expr"].(string)
		label := split[strings.Index(split, "sum by (")+len("sum by (") : strings.Index(split, ")")]
		for _, expr := range []string{split, total} {
			kept := dedupLabels(expr)
			for _, need := range []string{"pod", label} {
				if !kept[need] {
					t.Errorf("%q reads %q, which collapses %q instead of keeping one series per "+
						"outcome per replica", tc.title, expr, need)
				}
			}
		}
		for _, expr := range []string{split, total} {
			for _, want := range []string{"last_over_time(", "max by (pod", "[30d]"} {
				if !strings.Contains(expr, want) {
					t.Errorf("%q reads %q without %s, so a rolling restart drops the counts of "+
						"every replica it replaced", tc.title, expr, want)
				}
			}
			for _, banned := range []string{"increase(", "rate(", "$__range"} {
				if strings.Contains(expr, banned) {
					t.Errorf("%q reads %q with %s; these counters move a few times a day, so an "+
						"extrapolation reports a fraction of a discrete event", tc.title, expr, banned)
				}
			}
		}
		var sized bool
		for _, tr := range p["transformations"].([]any) {
			m, _ := tr.(map[string]any)
			if m["id"] != "configFromData" {
				continue
			}
			o, _ := m["options"].(map[string]any)
			if o["configRefId"] != targets[1].(map[string]any)["refId"] {
				t.Errorf("%q configures itself from %v, not from its total query", tc.title,
					o["configRefId"])
			}
			for _, mp := range o["mappings"].([]any) {
				if mm, _ := mp.(map[string]any); mm["handlerKey"] == "max" {
					sized = true
				}
			}
		}
		if !sized {
			t.Errorf("%q does not take its max from the total, so its bars read as magnitudes",
				tc.title)
		}
	}
}

// TestLatencyValuesReadAsWholeMilliseconds proves the latency panel shows no false
// precision ("248 ms", not "248.500 ms"); its absolute-second thresholds are left alone.
func TestLatencyValuesReadAsWholeMilliseconds(t *testing.T) {
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
	p := panelByTitle(t, raw, "How long a decision takes, out of the 10s allowed")
	defs, _ := p["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
	if defs["unit"] != "s" || defs["decimals"] != float64(0) {
		t.Errorf("the latency panel is unit=%v decimals=%v, want seconds rendered to whole "+
			"milliseconds", defs["unit"], defs["decimals"])
	}
}

// TestOwnershipColoursMatchTheCoreMap proves pool identity colours agree between the
// fleet JSON and the per-machine core map's Go constants.
func TestOwnershipColoursMatchTheCoreMap(t *testing.T) {
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")

	fixedColorOf := func(props []map[string]any) (string, string) {
		for _, pr := range props {
			if pr["id"] != "color" {
				continue
			}
			v, _ := pr["value"].(map[string]any)
			mode, _ := v["mode"].(string)
			c, _ := v["fixedColor"].(string)
			return mode, c
		}
		return "", ""
	}

	bar := panelByTitle(t, raw, "Who owns the cluster's CPUs")
	defs, _ := bar["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
	col, _ := defs["color"].(map[string]any)
	if col["mode"] != "fixed" || col["fixedColor"] != dashboard.ColorPinned {
		t.Errorf("the ownership bar's pinned series is %v, want the core map's fixed %q",
			col, dashboard.ColorPinned)
	}
	for series, want := range map[string]string{
		"reserved pool": dashboard.ColorReserved, "free": dashboard.ColorFree,
	} {
		mode, got := fixedColorOf(columnProperties(t, bar, series))
		if mode != "fixed" || got != want {
			t.Errorf("the ownership bar's %q series is %s/%q, want fixed/%q", series, mode, got, want)
		}
	}

	for _, tc := range []struct{ panel, column, want string }{
		{"Pinned pods", "", dashboard.ColorPinned},
		{"one row per managed machine", "Pinned", dashboard.ColorPinned},
		{"one row per managed machine", "Free", dashboard.ColorFree},
		{"one row per managed machine", "Reserved CPUs", dashboard.ColorReserved},
		{"one row per managed machine", "Pinned share", dashboard.ColorPinned},
	} {
		p := panelByTitle(t, raw, tc.panel)
		var mode, got string
		if tc.column == "" {
			d, _ := p["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
			c, _ := d["color"].(map[string]any)
			mode, _ = c["mode"].(string)
			got, _ = c["fixedColor"].(string)
			// A stat renders its value in the field colour only in this mode.
			if m, _ := p["options"].(map[string]any)["colorMode"].(string); m != "value" {
				t.Errorf("%q renders its value in %q, so the pool colour never reaches the "+
					"number", tc.panel, m)
			}
			// An identity colour must not also come from thresholds, or one source
			// silently wins.
			if _, has := d["thresholds"]; has {
				t.Errorf("%q carries both a fixed pool colour and thresholds", tc.panel)
			}
		} else {
			mode, got = fixedColorOf(columnProperties(t, p, tc.column))
		}
		if mode != "fixed" {
			t.Errorf("%q / %q colour mode is %q, want fixed: a pool colour that moved with "+
				"the value would read as a verdict", tc.panel, tc.column, mode)
		}
		if got != tc.want {
			t.Errorf("%q / %q is coloured %q, want %q — one concept, one colour across both "+
				"dashboards", tc.panel, tc.column, got, tc.want)
		}
	}
}

// panelByTitle returns the one panel whose title contains want.
func panelByTitle(t *testing.T, raw, want string) map[string]any {
	t.Helper()
	for _, p := range flattenPanels(t, raw) {
		if title, _ := p["title"].(string); strings.Contains(title, want) {
			return p
		}
	}
	t.Fatalf("the cluster dashboard has no panel titled %q...", want)
	return nil
}

// columnProperties returns the field-override properties a table sets on one column.
func columnProperties(t *testing.T, panel map[string]any, column string) []map[string]any {
	t.Helper()
	fc, _ := panel["fieldConfig"].(map[string]any)
	overrides, _ := fc["overrides"].([]any)
	var out []map[string]any
	for _, o := range overrides {
		m, _ := o.(map[string]any)
		matcher, _ := m["matcher"].(map[string]any)
		if matcher["id"] != "byName" || matcher["options"] != column {
			continue
		}
		props, _ := m["properties"].([]any)
		for _, p := range props {
			if pm, ok := p.(map[string]any); ok {
				out = append(out, pm)
			}
		}
	}
	return out
}

// columnThresholdColours returns the threshold colours one table column overrides, or nil
// when it inherits the default.
func columnThresholdColours(t *testing.T, panel map[string]any, column string) []string {
	t.Helper()
	for _, prop := range columnProperties(t, panel, column) {
		if prop["id"] != "thresholds" {
			continue
		}
		thr, _ := prop["value"].(map[string]any)
		steps, _ := thr["steps"].([]any)
		var out []string
		for _, s := range steps {
			c, _ := s.(map[string]any)["color"].(string)
			out = append(out, c)
		}
		return out
	}
	return nil
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

// TestClusterCPUBarReconcilesWithHardware proves the CPU ownership bar sums to the
// fleet's logical CPU total, with the excluded segment taken per machine and NUMA node
// before the fleet sum (overrides vary the mode per node).
func TestClusterCPUBarReconcilesWithHardware(t *testing.T) {
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
	var bar map[string]any
	for _, p := range flattenPanels(t, raw) {
		if title, _ := p["title"].(string); strings.Contains(title, "owns the cluster's CPUs") {
			bar = p
		}
	}
	if bar == nil {
		t.Fatal("the cluster dashboard has no CPU ownership bar")
	}
	legends := map[string]string{} // legend -> expr
	targets, _ := bar["targets"].([]any)
	for _, tg := range targets {
		m, _ := tg.(map[string]any)
		legend, _ := m["legendFormat"].(string)
		expr, _ := m["expr"].(string)
		legends[legend] = expr
	}
	known := map[string]bool{"pinned to workloads": true, "reserved pool": true, "free": true}
	for want := range known {
		if _, ok := legends[want]; !ok {
			t.Errorf("the bar lost its %q segment; it no longer splits every CPU by who holds it", want)
		}
	}
	if len(legends) != 4 {
		t.Fatalf("the bar has %d segments, want the three owners plus the CPUs held out of "+
			"the pinnable pool, without which it sums to the pinnable pools and not to the "+
			"hardware: %v", len(legends), legends)
	}
	var heldOut, label string
	for legend, expr := range legends {
		if !known[legend] {
			heldOut, label = expr, legend
		}
	}
	if !strings.Contains(heldOut,
		"max by (node, numa) (cpu_pinning_node_logical_cpus) - max by (node, numa) (cpu_pinning_node_pinnable_cpus)") {
		t.Errorf("the held-out segment does not subtract the pinnable pool per machine and "+
			"NUMA node, so it is wrong on a fleet that mixes hyperthread policies: %s", heldOut)
	}
	// The label is for a reader who has never seen this operator.
	for _, jargon := range []string{"pinnable", "cpuset", "sibling", "exclude", "policy", "hyperthread"} {
		if strings.Contains(strings.ToLower(label), jargon) {
			t.Errorf("the held-out segment is labelled %q, which names the operator's internal "+
				"term %q instead of what the CPUs mean to a reader", label, jargon)
		}
	}
	// Every segment needs its own colour. The core map's out-of-pool colour is not
	// reused: as a bar it is below 3:1 contrast on the dark theme.
	byColour := map[string][]string{}
	for legend, colour := range barSegmentColours(t, bar, legends) {
		byColour[colour] = append(byColour[colour], legend)
	}
	for colour, segs := range byColour {
		if len(segs) > 1 {
			t.Errorf("segments %v all render in %s, so the bar cannot be read", segs, colour)
		}
	}
}

// barSegmentColours maps each segment's legend to its colour: the panel default unless a
// byName override names it.
func barSegmentColours(t *testing.T, bar map[string]any, legends map[string]string) map[string]string {
	t.Helper()
	fc, _ := bar["fieldConfig"].(map[string]any)
	defs, _ := fc["defaults"].(map[string]any)
	defColour, _ := defs["color"].(map[string]any)
	fallback, _ := defColour["fixedColor"].(string)
	if fallback == "" {
		t.Fatal("the CPU ownership bar sets no default segment colour")
	}
	out := map[string]string{}
	for legend := range legends {
		out[legend] = fallback
	}
	overrides, _ := fc["overrides"].([]any)
	for _, o := range overrides {
		m, _ := o.(map[string]any)
		matcher, _ := m["matcher"].(map[string]any)
		name, _ := matcher["options"].(string)
		if matcher["id"] != "byName" {
			continue
		}
		if _, ok := out[name]; !ok {
			t.Errorf("a colour override names %q, which is not a segment of the bar", name)
			continue
		}
		props, _ := m["properties"].([]any)
		for _, pr := range props {
			p, _ := pr.(map[string]any)
			if p["id"] != "color" {
				continue
			}
			v, _ := p["value"].(map[string]any)
			if c, _ := v["fixedColor"].(string); c != "" {
				out[name] = c
			}
		}
	}
	return out
}

// TestAdmissibilityTileCountsMachines proves the admissibility tile reduces to one value
// per machine before counting (free CPUs are not equivalent across HT policies) and reads
// 0, not empty, on a saturated fleet.
func TestAdmissibilityTileCountsMachines(t *testing.T) {
	// Self-check: a bare `sum(` must be caught; `sum by (node)` must not.
	if !strings.Contains(`count(sum(cpu_pinning:cpu_free:per_numa) > 0)`, "sum(") {
		t.Fatal("detector: the fleet-sum check no longer matches a bare sum")
	}
	if strings.Contains(`count(sum by (node) (cpu_pinning:cpu_free:per_numa) > 0)`, "sum(") {
		t.Fatal("detector: the fleet-sum check matches the per-machine sum by (node) form")
	}

	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
	var tile map[string]any
	for _, p := range flattenPanels(t, raw) {
		if title, _ := p["title"].(string); strings.Contains(title, "can take a pinned pod") {
			tile = p
		}
	}
	if tile == nil {
		t.Fatal("the cluster dashboard has no tile counting the machines that can still " +
			"accept a pinned pod")
	}
	exprs := panelExprs(tile)
	if len(exprs) != 1 {
		t.Fatalf("the admissibility tile has %d queries, want one", len(exprs))
	}
	expr := exprs[0]
	if !strings.Contains(expr, "by (node)") {
		t.Errorf("the admissibility tile never reduces per machine, so it answers from fleet "+
			"totals: %s", expr)
	}

	if strings.Contains(expr, "sum(") {
		t.Errorf("the admissibility tile sums across the fleet, which adds free CPUs that are "+
			"not equivalent units across hyperthread policies: %s", expr)
	}
	if !strings.Contains(expr, "* 0)") {
		t.Errorf("the admissibility tile has no zero term, so a fleet with no room left reports "+
			"no data instead of 0: %s", expr)
	}
	defs, _ := tile["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
	if nv, _ := defs["noValue"].(string); nv == "" {
		t.Error("the admissibility tile has no empty-state message, so a fleet that reports " +
			"nothing reads as a fleet with no room")
	}
	thr, _ := defs["thresholds"].(map[string]any)
	steps, _ := thr["steps"].([]any)
	if len(steps) < 2 {
		t.Fatalf("the admissibility tile carries %d threshold step(s); zero machines with room "+
			"is the reading on this tile that needs colour", len(steps))
	}
	if c, _ := steps[0].(map[string]any)["color"].(string); c != "red" {
		t.Errorf("the admissibility tile's base colour is %q; zero machines able to take a "+
			"pinned pod has to read red", c)
	}
	var green bool
	for _, s := range steps[1:] {
		m, _ := s.(map[string]any)
		if m["color"] == "green" && m["value"] == float64(1) {
			green = true
		}
	}
	if !green {
		t.Errorf("the admissibility tile never turns green from one machine upward: %v", steps)
	}
}

// TestDashboardGeometry proves no panel runs past column 24 or overlaps another; Grafana
// does both silently.
func TestDashboardGeometry(t *testing.T) {
	check := func(t *testing.T, label, raw string) {
		t.Helper()
		var dash struct {
			Panels []map[string]any `json:"panels"`
		}
		if err := json.Unmarshal([]byte(raw), &dash); err != nil {
			t.Fatalf("%s: parse dashboard: %v", label, err)
		}
		// A collapsed row's children are laid out inside the row.
		occupied := map[[2]int]string{}
		for _, p := range dash.Panels {
			g, ok := p["gridPos"].(map[string]any)
			if !ok {
				t.Errorf("%s: panel %q has no gridPos", label, p["title"])
				continue
			}
			x, y := int(g["x"].(float64)), int(g["y"].(float64))
			w, h := int(g["w"].(float64)), int(g["h"].(float64))
			if x+w > 24 {
				t.Errorf("%s: panel %q spans columns %d..%d, past the 24-column grid",
					label, p["title"], x, x+w)
			}
			for dx := 0; dx < w; dx++ {
				for dy := 0; dy < h; dy++ {
					cell := [2]int{x + dx, y + dy}
					if prev, dup := occupied[cell]; dup {
						t.Errorf("%s: panel %q overlaps %q at grid cell %v",
							label, p["title"], prev, cell)
					}
					occupied[cell] = fmt.Sprint(p["title"])
				}
			}
		}
	}
	check(t, "cluster dashboard", fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n"))
	check(t, "cluster dashboard (no loki)", fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n"))
	check(t, "per-node dashboard", renderNodeDashboard(t))
}

// TestAdmissionLatencyCarriesItsScale proves admission latency is drawn against its
// budget (8s alert, 10s API server limit).
func TestAdmissionLatencyCarriesItsScale(t *testing.T) {
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
	for _, p := range flattenPanels(t, raw) {
		title, _ := p["title"].(string)
		if !strings.Contains(title, "How long a decision takes") {
			continue
		}
		if p["type"] != "bargauge" {
			t.Errorf("the admission-latency panel is a %v; a scale needs a panel that draws "+
				"the value against its limit", p["type"])
		}
		defs, _ := p["fieldConfig"].(map[string]any)["defaults"].(map[string]any)
		if defs["max"] != float64(10) {
			t.Errorf("admission-latency max = %v, want 10 (the API server's timeout)", defs["max"])
		}
		var steps []float64
		thr, _ := defs["thresholds"].(map[string]any)
		list, _ := thr["steps"].([]any)
		for _, s := range list {
			if v, ok := s.(map[string]any)["value"].(float64); ok {
				steps = append(steps, v)
			}
		}
		if !containsFloat(steps, 8) {
			t.Errorf("admission-latency thresholds %v do not mark 8s, where "+
				"CPUPinningWebhookLatencyHigh fires", steps)
		}
		return
	}
	t.Fatal("the cluster dashboard has no admission-latency panel")
}

func containsFloat(list []float64, want float64) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// docsGlossarySection returns the glossary section of docs/observability.md.
func docsGlossarySection(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "observability.md"))
	if err != nil {
		t.Fatalf("read docs/observability.md: %v", err)
	}
	const anchor = `<a id="glossary"></a>`
	start := strings.Index(string(body), anchor)
	if start < 0 {
		t.Fatalf("docs/observability.md has no %s anchor to hold the glossary", anchor)
	}
	rest := string(body)[start+len(anchor):]

	if h := strings.Index(rest, "\n## "); h >= 0 {
		rest = rest[h+len("\n## "):]
	}
	if end := strings.Index(rest, "\n## "); end > 0 {
		rest = rest[:end]
	}
	if strings.TrimSpace(rest) == "" {
		t.Fatal("the glossary section of docs/observability.md is empty")
	}
	return rest
}

// renderNodeDashboard renders a per-node dashboard from a small synthetic topology.
func renderNodeDashboard(t *testing.T) string {
	t.Helper()
	cpus := map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0}, 1: {CPU: 1, NUMA: 0, Core: 1},
		2: {CPU: 2, NUMA: 1, Core: 2}, 3: {CPU: 3, NUMA: 1, Core: 3},
		4: {CPU: 4, NUMA: 0, Core: 0}, 5: {CPU: 5, NUMA: 0, Core: 1},
		6: {CPU: 6, NUMA: 1, Core: 2}, 7: {CPU: 7, NUMA: 1, Core: 3},
	}
	machine := dashboard.Machine{
		MemoryPerNUMABytes: map[int]int64{0: 270088753152, 1: 270088753152},
		PackageTopology:    "1_1",
		MaxSpan:            2,
		NodeClass:          "cpu-amx",
		AMX:                true,
		CPUAllocation:      "physical-cores",
		ReservedCPUs:       2,
	}
	raw, err := dashboard.RenderDashboardJSON("worker-1", dashboard.Datasources{}, machine, cpus)
	if err != nil {
		t.Fatalf("render per-node dashboard: %v", err)
	}
	return string(raw)
}

// renderNodeDashboardWithLogs renders the per-node dashboard with the Loki section on.
func renderNodeDashboardWithLogs(t *testing.T) string {
	t.Helper()
	cpus := map[int]topology.CPUInfo{
		0: {CPU: 0, NUMA: 0, Core: 0}, 1: {CPU: 1, NUMA: 0, Core: 1},
		2: {CPU: 2, NUMA: 1, Core: 2}, 3: {CPU: 3, NUMA: 1, Core: 3},
	}
	ds := dashboard.Datasources{Loki: dashboard.LokiConfig{
		Enabled:       true,
		NodeLabel:     "k8s_node_name",
		PluginLabel:   "k8s_daemonset_name",
		PluginName:    "nri-resource-policy-balloons",
		WorkloadLabel: "k8s_deployment_name",
	}}
	machine := dashboard.Machine{
		MemoryPerNUMABytes: map[int]int64{0: 270088753152, 1: 270088753152},
		PackageTopology:    "1_1",
		MaxSpan:            2,
		CPUAllocation:      "physical-cores",
		ReservedCPUs:       2,
	}
	raw, err := dashboard.RenderDashboardJSON("worker-1", ds, machine, cpus)
	if err != nil {
		t.Fatalf("render per-node dashboard with logs: %v", err)
	}
	return string(raw)
}

// flattenPanels returns every panel, including children of collapsed rows.
func flattenPanels(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var dash struct {
		Panels []map[string]any `json:"panels"`
	}
	if err := json.Unmarshal([]byte(raw), &dash); err != nil {
		t.Fatalf("parse dashboard panels: %v", err)
	}
	var out []map[string]any
	var walk func(ps []map[string]any)
	walk = func(ps []map[string]any) {
		for _, p := range ps {
			out = append(out, p)
			nested, ok := p["panels"].([]any)
			if !ok {
				continue
			}
			var children []map[string]any
			for _, n := range nested {
				if m, ok := n.(map[string]any); ok {
					children = append(children, m)
				}
			}
			walk(children)
		}
	}
	walk(dash.Panels)
	return out
}

func panelExprs(p map[string]any) []string {
	targets, _ := p["targets"].([]any)
	var out []string
	for _, tg := range targets {
		m, ok := tg.(map[string]any)
		if !ok {
			continue
		}
		if expr, ok := m["expr"].(string); ok && expr != "" {
			out = append(out, expr)
		}
	}
	return out
}

func varCurrent(t *testing.T, raw, name string) map[string]any {
	t.Helper()
	var parsed struct {
		Templating struct {
			List []struct {
				Name    string         `json:"name"`
				Current map[string]any `json:"current"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("parse dashboard templating: %v", err)
	}
	for _, v := range parsed.Templating.List {
		if v.Name == name {
			if v.Current == nil {
				return map[string]any{}
			}
			return v.Current
		}
	}
	t.Fatalf("dashboard has no template var %q", name)
	return nil
}

func logsExpr(t *testing.T, raw string) string {
	t.Helper()
	for _, p := range flattenPanels(t, raw) {
		if p["type"] != "logs" {
			continue
		}
		if exprs := panelExprs(p); len(exprs) > 0 {
			return exprs[0]
		}
	}
	t.Fatalf("no logs panel with a query found:\n%s", raw)
	return ""
}

// logsPanelDescription returns the logs panel's description.
func logsPanelDescription(t *testing.T, raw string) string {
	t.Helper()
	for _, p := range flattenPanels(t, raw) {
		if p["type"] == "logs" {
			d, _ := p["description"].(string)
			return d
		}
	}
	t.Fatalf("no logs panel found:\n%s", raw)
	return ""
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// undeduplicatedGauges reports the raw gauges in expr not immediately enclosed by `max`.
func undeduplicatedGauges(expr string) []string {
	var hits []string
	for _, metric := range gaugeMetrics {
		for i := 0; i < len(expr); {
			j := strings.Index(expr[i:], metric)
			if j < 0 {
				break
			}
			at := i + j
			i = at + len(metric)
			// A longer metric name may contain a shorter one; only a selector boundary
			// counts.
			if at+len(metric) < len(expr) && isMetricChar(expr[at+len(metric)]) {
				continue
			}
			if enclosingCall(expr, at) != "max" {
				hits = append(hits, metric)
				break
			}
		}
	}
	return hits
}

func isMetricChar(c byte) bool {
	return c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isSpaceByte(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// enclosingCall returns the name of the call or aggregation most closely enclosing
// position at, skipping plain grouping parens; "" if none.
func enclosingCall(expr string, at int) string {
	depth := 0
	for i := at - 1; i >= 0; i-- {
		switch expr[i] {
		case ')':
			depth++
		case '(':
			if depth > 0 {
				depth--
				continue
			}
			if name, ok := callNameBefore(expr, i); ok && name != "" {
				return name
			}
			// A plain grouping paren: keep looking outward.
		}
	}
	return ""
}

// callNameBefore reads the identifier before the '(' at open, stepping over `by
// (...)`/`without (...)`; ok is false for shapes it does not understand.
func callNameBefore(expr string, open int) (string, bool) {
	j := open - 1
	for j >= 0 && isSpaceByte(expr[j]) {
		j--
	}
	if j >= 0 && expr[j] == ')' {
		// Possibly `<agg> by (labels) (`: skip the label list.
		depth := 0
		for ; j >= 0; j-- {
			if expr[j] == ')' {
				depth++
			} else if expr[j] == '(' {
				depth--
				if depth == 0 {
					j--
					break
				}
			}
		}
		for j >= 0 && isSpaceByte(expr[j]) {
			j--
		}
		kw, k := identBefore(expr, j)
		if kw != "by" && kw != "without" {
			return "", false
		}
		j = k
		for j >= 0 && isSpaceByte(expr[j]) {
			j--
		}
	}
	name, _ := identBefore(expr, j)
	return name, true
}

// identBefore returns the identifier ending at index end and the index just before it.
func identBefore(expr string, end int) (string, int) {
	j := end
	for j >= 0 && isMetricChar(expr[j]) {
		j--
	}
	return expr[j+1 : end+1], j
}

// behaviourPanelCollectors maps each behaviour-panel metric to the shipped collector
// declaring its labels; histograms by base name.
var behaviourPanelCollectors = map[string]prometheus.Collector{
	"cpu_pinning_node_provisioning_total":   metrics.NodeProvisioning,
	"cpu_pinning_webhook_admission_seconds": metrics.WebhookAdmissionSeconds,
}

// durableDedup captures a behaviour reading's inner max labels and its metric.
var durableDedup = regexp.MustCompile(`max by \(([^)]*)\) \(last_over_time\((\w+)\[\d+d\]\)\)`)

// dedupLabels returns every label a durable reading deduplicates on.
func dedupLabels(expr string) map[string]bool {
	kept := map[string]bool{}
	for _, m := range durableDedup.FindAllStringSubmatch(expr, -1) {
		for _, l := range strings.Split(m[1], ",") {
			kept[strings.TrimSpace(l)] = true
		}
	}
	return kept
}

// collectorLabels reads a collector's declared variable labels out of its Desc.
var collectorLabelsRe = regexp.MustCompile(`variableLabels: \{([^}]*)\}`)

func collectorLabels(t *testing.T, c prometheus.Collector) []string {
	t.Helper()
	ch := make(chan *prometheus.Desc, 8)
	go func() {
		c.Describe(ch)
		close(ch)
	}()
	var out []string
	for d := range ch {
		m := collectorLabelsRe.FindStringSubmatch(d.String())
		if m == nil {
			// Desc.String() is client_golang's format; fail rather than read no labels
			// and pass.
			t.Fatalf("cannot read labels out of Desc: %s", d.String())
		}
		for _, l := range strings.Split(m[1], ",") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
	}
	return out
}

// TestDurableCountsDeduplicateOnEveryLabel proves each behaviour panel's `max by` names
// every label its counter declares, or the panel undercounts.
func TestDurableCountsDeduplicateOnEveryLabel(t *testing.T) {
	// Self-check: an unparseable expression must not turn this green.
	for _, probe := range []string{
		`sum by (outcome) (max by (pod, outcome) (last_over_time(cpu_pinning_node_provisioning_total[30d])))`,
		`histogram_quantile(0.99, sum by (le) (max by (pod, result) ` +
			`(last_over_time(cpu_pinning_webhook_admission_seconds_bucket[30d]))))`,
	} {
		if len(missingDedupLabels(t, "Probe", probe)) == 0 {
			t.Fatalf("detector found nothing wrong with a dedup that drops a label the counter carries: %s", probe)
		}
	}

	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n")
	checked := 0
	for _, p := range flattenPanels(t, raw) {
		title, _ := p["title"].(string)
		for _, want := range behaviourPanels {
			if title != want {
				continue
			}
			for _, expr := range panelExprs(p) {
				for _, f := range missingDedupLabels(t, title, expr) {
					t.Error(f)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatalf("no behaviour-panel query was checked; the panels are %v", behaviourPanels)
	}
}

// missingDedupLabels reports the counter labels a reading's `max by (...)` drops.
func missingDedupLabels(t *testing.T, title, expr string) []string {
	t.Helper()
	var findings []string
	for _, m := range durableDedup.FindAllStringSubmatch(expr, -1) {
		kept := map[string]bool{}
		for _, l := range strings.Split(m[1], ",") {
			kept[strings.TrimSpace(l)] = true
		}
		metric := m[2]
		collector, ok := behaviourPanelCollectors[metric]
		// Histogram buckets carry `le`; dropping it collapses the distribution.
		var extra []string
		if !ok {
			for _, suffix := range []string{"_bucket", "_count", "_sum"} {
				base, found := strings.CutSuffix(metric, suffix)
				if !found {
					continue
				}
				collector, ok = behaviourPanelCollectors[base]
				if suffix == "_bucket" {
					extra = append(extra, "le")
				}
				break
			}
		}
		if !ok {
			findings = append(findings, fmt.Sprintf(
				"panel %q reads %s, which behaviourPanelCollectors does not map to a shipped "+
					"collector, so its labels are not checked: %s", title, metric, expr))
			continue
		}
		for _, l := range append(collectorLabels(t, collector), extra...) {
			if !kept[l] {
				findings = append(findings, fmt.Sprintf(
					"panel %q deduplicates %s on %v, which drops its %q label: `max` keeps one "+
						"series per group, so the panel reports one %s instead of their total: %s",
					title, metric, m[1], l, l, expr))
			}
		}
	}
	return findings
}
