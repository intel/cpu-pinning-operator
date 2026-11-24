// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v3"

	"github.com/intel/cpu-pinning-operator/internal/metrics"
)

// operatorNamespace is the namespace the scrape target attaches to every sample: the
// operator's own.
const operatorNamespace = "cpu-pinning-system"

// targetLabels are the labels Prometheus Operator puts on a ServiceMonitor scrape;
// internal/metrics carries the same list.
var targetLabels = map[string]string{
	"namespace": operatorNamespace,
	"pod":       "cpu-pinning-operator-0",
	"container": "manager",
	"service":   "cpu-pinning-operator-metrics",
	"endpoint":  "metrics",
	"job":       "cpu-pinning-operator-metrics",
	"instance":  "10.0.0.1:8080",
}

// scrape returns the label set Prometheus stores for one exposed sample: target labels
// win and a clashing metric label becomes exported_<name> (honorLabels unset).
func scrape(exposed map[string]string) map[string]string {
	stored := map[string]string{}
	for k, v := range exposed {
		if _, collides := targetLabels[k]; collides {
			k = "exported_" + k
		}
		stored[k] = v
	}
	for k, v := range targetLabels {
		stored[k] = v
	}
	return stored
}

// scrapedSeries increments the real CounterVec once per label tuple and returns each
// series as Prometheus would store it. The vec is package-global, so it is reset first.
func scrapedSeries(t *testing.T, name string, vec *prometheus.CounterVec, labelSets ...[]string) []map[string]string {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(vec); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	vec.Reset()
	for _, labels := range labelSets {
		vec.WithLabelValues(labels...).Inc()
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []map[string]string
	for _, fam := range families {
		for _, m := range fam.GetMetric() {
			exposed := map[string]string{"__name__": fam.GetName()}
			for _, lp := range m.GetLabel() {
				exposed[lp.GetName()] = lp.GetValue()
			}
			out = append(out, scrape(exposed))
		}
	}
	return out
}

// blockedSeries reports one blocked pod per node and namespace pair.
func blockedSeries(t *testing.T, nodes, namespaces []string) []map[string]string {
	t.Helper()
	var sets [][]string
	for _, node := range nodes {
		for _, ns := range namespaces {
			sets = append(sets, []string{node, ns, metrics.PinBlockedNoCapacity})
		}
	}
	return scrapedSeries(t, "PinningBlocked", metrics.PinningBlocked, sets...)
}

// groupBy reproduces `sum by (...)`. A label no series carries collapses everything into
// one group.
func groupBy(series []map[string]string, by []string) map[string]map[string]string {
	groups := map[string]map[string]string{}
	for _, s := range series {
		key := make([]string, 0, len(by))
		labels := map[string]string{}
		for _, l := range by {
			if v, ok := s[l]; ok {
				labels[l] = v
				key = append(key, l+"="+v)
			}
		}
		groups[strings.Join(key, ",")] = labels
	}
	return groups
}

var sumByRe = regexp.MustCompile(`sum by \(([^)]*)\)`)

// alertRule is one rendered alert, keyed the way PrometheusRule spells it.
type alertRule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Annotations map[string]string `yaml:"annotations"`
}

// renderAlerts returns the rendered PrometheusRule's alerts by name.
func renderAlerts(t *testing.T) map[string]alertRule {
	t.Helper()
	helm := helmBin(t)
	vf := filepath.Join(t.TempDir(), "values.yaml")
	values := "metrics:\n  enabled: true\n  prometheusRule:\n    enabled: true\n    alerts: true\n"
	if err := os.WriteFile(vf, []byte(values), 0o600); err != nil {
		t.Fatalf("write values: %v", err)
	}
	out, err := exec.Command(helm, "template", "t", chartUnderTest(), "-f", policyTargetValues,
		"--show-only", "templates/prometheusrule.yaml", "-f", vf).CombinedOutput()
	if err != nil {
		requireSubcharts(t, string(out))
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	var pr struct {
		Spec struct {
			Groups []struct {
				Rules []alertRule `yaml:"rules"`
			} `yaml:"groups"`
		} `yaml:"spec"`
	}
	for _, doc := range strings.Split(string(out), "\n---") {
		if !strings.Contains(doc, "kind: PrometheusRule") {
			continue
		}
		if err := yaml.Unmarshal([]byte(doc), &pr); err != nil {
			t.Fatalf("parse PrometheusRule: %v\n%s", err, doc)
		}
	}
	alerts := map[string]alertRule{}
	for _, g := range pr.Spec.Groups {
		for _, r := range g.Rules {
			if r.Alert != "" {
				alerts[r.Alert] = r
			}
		}
	}
	if len(alerts) == 0 {
		t.Fatal("no alerts rendered with metrics.prometheusRule.alerts=true")
	}
	return alerts
}

// renderSummary evaluates an annotation with $labels bound to the firing group's labels.
func renderSummary(t *testing.T, annotation string, labels map[string]string) string {
	t.Helper()
	tmpl, err := template.New("summary").Parse("{{ $labels := . }}" + annotation)
	if err != nil {
		t.Fatalf("parse annotation %q: %v", annotation, err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, labels); err != nil {
		t.Fatalf("execute annotation %q: %v", annotation, err)
	}
	return sb.String()
}

// TestPinningBlockedAlertNamesWorkloadNamespace proves blocked pods in different tenants
// fire separate alerts naming the workload namespace, not the scrape target's.
func TestPinningBlockedAlertNamesWorkloadNamespace(t *testing.T) {
	const nsA, nsB = "inference", "sglang"

	alert, ok := renderAlerts(t)["CPUPinningPinningBlocked"]
	if !ok {
		t.Fatal("CPUPinningPinningBlocked not rendered")
	}
	m := sumByRe.FindStringSubmatch(alert.Expr)
	if m == nil {
		t.Fatalf("CPUPinningPinningBlocked expr has no `sum by (...)`, cannot fire per namespace: %s", alert.Expr)
	}
	var by []string
	for _, l := range strings.Split(m[1], ",") {
		by = append(by, strings.TrimSpace(l))
	}

	const node = "worker-1"
	series := blockedSeries(t, []string{node}, []string{nsA, nsB})
	if len(series) != 2 {
		t.Fatalf("two blocked pods in two namespaces produced %d series, want 2", len(series))
	}
	// The exposed label must survive the scrape intact, not arrive as exported_namespace.
	for _, s := range series {
		if _, shadowed := s["exported_namespace"]; shadowed {
			t.Errorf("cpu_pinning_pods_pinning_blocked_total still declares a label the target shadows: %v", s)
		}
		// The target attaches no node, so `node` reaches the rule under its own name.
		if got := s["node"]; got != node {
			t.Errorf("node label reads %q after the scrape, want %q (exported_node=%q)", got, node, s["exported_node"])
		}
	}

	groups := groupBy(series, by)
	var fired []string
	for _, labels := range groups {
		fired = append(fired, renderSummary(t, alert.Annotations["summary"], labels))
	}
	sort.Strings(fired)
	if len(groups) != 2 {
		t.Fatalf("blocked pods in %q and %q fired %d alert(s) grouping by %v, want 2: %v",
			nsA, nsB, len(groups), by, fired)
	}
	for _, ns := range []string{nsA, nsB} {
		want := fmt.Sprintf("Pinned pods cannot start in %s on %s (%s)", ns, node, metrics.PinBlockedNoCapacity)
		if !slices.Contains(fired, want) {
			t.Errorf("no alert summary reads %q; got %v", want, fired)
		}
	}
	for _, s := range fired {
		if strings.Contains(s, operatorNamespace) {
			t.Errorf("alert summary names the operator's namespace instead of the workload's: %q", s)
		}
	}
}

// TestPinningBlockedAlertNamesNode proves two machines blocking pods in one namespace
// fire two alerts, each naming its node.
func TestPinningBlockedAlertNamesNode(t *testing.T) {
	const ns = "inference"
	nodeA, nodeB := "worker-1", "worker-2"

	alert, ok := renderAlerts(t)["CPUPinningPinningBlocked"]
	if !ok {
		t.Fatal("CPUPinningPinningBlocked not rendered")
	}
	m := sumByRe.FindStringSubmatch(alert.Expr)
	if m == nil {
		t.Fatalf("CPUPinningPinningBlocked expr has no `sum by (...)`, cannot fire per node: %s", alert.Expr)
	}
	var by []string
	for _, l := range strings.Split(m[1], ",") {
		by = append(by, strings.TrimSpace(l))
	}

	series := blockedSeries(t, []string{nodeA, nodeB}, []string{ns})
	if len(series) != 2 {
		t.Fatalf("one blocked pod on each of two nodes produced %d series, want 2", len(series))
	}

	groups := groupBy(series, by)
	var fired []string
	for _, labels := range groups {
		fired = append(fired, renderSummary(t, alert.Annotations["summary"], labels))
	}
	sort.Strings(fired)
	if len(groups) != 2 {
		t.Fatalf("pods blocked on %q and %q in one namespace fired %d alert(s) grouping by %v, want 2: %v",
			nodeA, nodeB, len(groups), by, fired)
	}
	for _, node := range []string{nodeA, nodeB} {
		want := fmt.Sprintf("Pinned pods cannot start in %s on %s (%s)", ns, node, metrics.PinBlockedNoCapacity)
		if !slices.Contains(fired, want) {
			t.Errorf("no alert summary reads %q; got %v", want, fired)
		}
	}
}

var (
	labelRef   = regexp.MustCompile(`\$labels\.(\w+)`)
	byClause   = regexp.MustCompile(`by \(([^)]*)\)`)
	aggregator = regexp.MustCompile(`\b(sum|max|min|avg|count|topk|bottomk|quantile|group|stddev|stdvar)\s*\(`)
)

// keptLabels returns the labels an alert expr leaves in $labels, or nil when every label
// survives.
// `by (...)` keeps the named labels; an aggregation without `by` keeps none; no
// aggregation keeps all, the target's included. `without (...)` is treated as keeping
// none: over-strict, and no shipped rule uses it.
func keptLabels(expr string) map[string]bool {
	if m := byClause.FindStringSubmatch(expr); m != nil {
		kept := map[string]bool{}
		for _, l := range strings.Split(m[1], ",") {
			kept[strings.TrimSpace(l)] = true
		}
		return kept
	}
	if aggregator.MatchString(expr) {
		return map[string]bool{}
	}
	return nil
}

// annotationLabelFindings reports every $labels reference in an alert's annotations that
// would not render what its author meant.
func annotationLabelFindings(name, expr string, annotations map[string]string) []string {
	kept := keptLabels(expr)
	var findings []string
	for key, text := range annotations {
		for _, ref := range labelRef.FindAllStringSubmatch(text, -1) {
			l := ref[1]
			switch {
			case kept != nil && !kept[l]:
				findings = append(findings, fmt.Sprintf(
					"%s %s interpolates $labels.%s, which its expr does not keep — renders <no value>: %s",
					name, key, l, expr))
			case targetLabels[l] != "":
				findings = append(findings, fmt.Sprintf(
					"%s %s interpolates $labels.%s, a label the scrape target owns — it would always read %q",
					name, key, l, targetLabels[l]))
			}
		}
	}
	return findings
}

// TestAlertAnnotationsOnlyUseGroupedLabels proves every annotation interpolates only
// labels its expr keeps, never one the scrape target owns.
func TestAlertAnnotationsOnlyUseGroupedLabels(t *testing.T) {
	// Self-check: a detector that skips a shape turns this green.
	for _, tc := range []struct {
		expr, summary string
		want          bool // want a finding
	}{
		// The shipped shape.
		{`sum by (node, workload_namespace, cause) (increase(cpu_pinning_pods_pinning_blocked_total[15m])) > 0`,
			"blocked in {{ $labels.workload_namespace }} on {{ $labels.node }}", false},
		// Grouped on a label the target owns.
		{`sum by (namespace, cause) (increase(cpu_pinning_pods_pinning_blocked_total[15m])) > 0`,
			"blocked in {{ $labels.namespace }}", true},
		// A bare aggregation keeps no labels.
		{`sum(rate(cpu_pinning_webhook_bypasses_total[5m])) > 0`,
			"bypassed in {{ $labels.workload_namespace }}", true},
		{`sum(rate(cpu_pinning_webhook_bypasses_total[5m])) > 0`, "pods admitted unpinned", false},
		// No aggregation: workload labels survive, target labels must not be named.
		{`increase(cpu_pinning_node_provisioning_total{outcome="failed"}[15m]) > 0`,
			"failed on {{ $labels.node }}", false},
		{`increase(cpu_pinning_node_provisioning_total{outcome="failed"}[15m]) > 0`,
			"failed in {{ $labels.namespace }}", true},
	} {
		got := len(annotationLabelFindings("Probe", tc.expr, map[string]string{"summary": tc.summary})) > 0
		if got != tc.want {
			t.Fatalf("detector: finding=%v, want %v, for expr %q summary %q", got, tc.want, tc.expr, tc.summary)
		}
	}

	for name, rule := range renderAlerts(t) {
		for _, f := range annotationLabelFindings(name, rule.Expr, rule.Annotations) {
			t.Error(f)
		}
	}
}

// provisionFailedSeries reports one failed topology probe per node.
func provisionFailedSeries(t *testing.T, nodes ...string) []map[string]string {
	t.Helper()
	var sets [][]string
	for _, node := range nodes {
		sets = append(sets, []string{node, metrics.NodeProvisionFailed})
	}
	return scrapedSeries(t, "NodeProvisioning", metrics.NodeProvisioning, sets...)
}

// replicaOf returns the series as scraped from a second replica: same metric labels,
// different pod and address.
func replicaOf(series map[string]string, pod string) map[string]string {
	copied := map[string]string{}
	for k, v := range series {
		copied[k] = v
	}
	copied["pod"] = pod
	copied["instance"] = "10.0.0.2:8080"
	return copied
}

// TestNodeProvisionFailedAlertNamesNode proves two failed machines fire two alerts naming
// their nodes, and one machine scraped from two replicas fires one.
func TestNodeProvisionFailedAlertNamesNode(t *testing.T) {
	nodeA, nodeB := "worker-1", "worker-2"

	alert, ok := renderAlerts(t)["CPUPinningNodeProvisionFailed"]
	if !ok {
		t.Fatal("CPUPinningNodeProvisionFailed not rendered")
	}
	m := byClause.FindStringSubmatch(alert.Expr)
	if m == nil {
		t.Fatalf("CPUPinningNodeProvisionFailed expr has no `by (...)`, so it cannot fire once per node: %s", alert.Expr)
	}
	var by []string
	for _, l := range strings.Split(m[1], ",") {
		by = append(by, strings.TrimSpace(l))
	}

	series := provisionFailedSeries(t, nodeA, nodeB)
	if len(series) != 2 {
		t.Fatalf("a failure on each of two nodes produced %d series, want 2", len(series))
	}

	groups := groupBy(series, by)
	var fired []string
	for _, labels := range groups {
		fired = append(fired, renderSummary(t, alert.Annotations["summary"], labels))
	}
	sort.Strings(fired)
	if len(groups) != 2 {
		t.Fatalf("failures on %q and %q fired %d alert(s) grouping by %v, want 2: %v",
			nodeA, nodeB, len(groups), by, fired)
	}
	for _, node := range []string{nodeA, nodeB} {
		want := fmt.Sprintf("Node topology provisioning failed on %s", node)
		if !slices.Contains(fired, want) {
			t.Errorf("no alert summary reads %q; got %v", want, fired)
		}
	}

	single := provisionFailedSeries(t, nodeA)
	if len(single) != 1 {
		t.Fatalf("one failing node produced %d series, want 1", len(single))
	}
	ha := []map[string]string{single[0], replicaOf(single[0], "cpu-pinning-operator-1")}
	if n := len(groupBy(ha, by)); n != 1 {
		t.Errorf("one failure on %q scraped from two replicas fired %d alerts grouping by %v, want 1",
			nodeA, n, by)
	}
}

// bypassSeries reports one bypass per namespace and reason pair.
func bypassSeries(t *testing.T, namespaces, reasons []string) []map[string]string {
	t.Helper()
	var sets [][]string
	for _, ns := range namespaces {
		for _, reason := range reasons {
			sets = append(sets, []string{ns, reason})
		}
	}
	return scrapedSeries(t, "WebhookBypasses", metrics.WebhookBypasses, sets...)
}

var (
	offsetRe = regexp.MustCompile(`offset (\d+[smhd])`)
	rateFnRe = regexp.MustCompile(`\b(increase|rate|irate)\(`)
)

// TestWebhookBypassAlertNamesTenant proves bypass alerts fire once per tenant and failing
// step, each naming it, and do not group on node (the counter has none).
func TestWebhookBypassAlertNamesTenant(t *testing.T) {
	const nsA, nsB = "inference", "sglang"

	alert, ok := renderAlerts(t)["CPUPinningWebhookBypass"]
	if !ok {
		t.Fatal("CPUPinningWebhookBypass not rendered")
	}
	m := sumByRe.FindStringSubmatch(alert.Expr)
	if m == nil {
		t.Fatalf("CPUPinningWebhookBypass expr has no `sum by (...)`, so it cannot fire per tenant: %s", alert.Expr)
	}
	var by []string
	for _, l := range strings.Split(m[1], ",") {
		by = append(by, strings.TrimSpace(l))
	}

	series := bypassSeries(t, []string{nsA, nsB}, []string{metrics.BypassNodeLookup})
	if len(series) != 2 {
		t.Fatalf("a bypass in each of two namespaces produced %d series, want 2", len(series))
	}
	for _, s := range series {
		if _, shadowed := s["exported_workload_namespace"]; shadowed {
			t.Errorf("the scrape target shadows the bypass counter's namespace label: %v", s)
		}
		if _, hasNode := s["node"]; hasNode {
			t.Errorf("the bypass counter now carries a node label, so the rule should group on it: %v", s)
		}
	}
	if slices.Contains(by, "node") {
		t.Errorf("CPUPinningWebhookBypass groups on node, which the counter does not carry, so every "+
			"tenant collapses into one alert: %s", alert.Expr)
	}

	fire := func(series []map[string]string) []string {
		var fired []string
		for _, labels := range groupBy(series, by) {
			fired = append(fired, renderSummary(t, alert.Annotations["summary"], labels))
		}
		sort.Strings(fired)
		return fired
	}

	fired := fire(series)
	if len(fired) != 2 {
		t.Fatalf("bypasses in %q and %q fired %d alert(s) grouping by %v, want 2: %v",
			nsA, nsB, len(fired), by, fired)
	}
	for _, ns := range []string{nsA, nsB} {
		if !slices.ContainsFunc(fired, func(s string) bool { return strings.Contains(s, ns) }) {
			t.Errorf("no alert summary names tenant %q; got %v", ns, fired)
		}
	}
	for _, s := range fired {
		if strings.Contains(s, operatorNamespace) {
			t.Errorf("alert summary names the operator's namespace instead of the workload's: %q", s)
		}
	}

	// Two failing steps are two remedies, so two alerts.
	twoReasons := bypassSeries(t, []string{nsA}, []string{metrics.BypassNodeLookup, metrics.BypassMarshal})
	fired = fire(twoReasons)
	if len(fired) != 2 {
		t.Errorf("a node-lookup and a marshal bypass in %q fired %d alert(s) grouping by %v, want 2: %v",
			nsA, len(fired), by, fired)
	}
	for _, reason := range []string{metrics.BypassNodeLookup, metrics.BypassMarshal} {
		if !slices.ContainsFunc(fired, func(s string) bool { return strings.Contains(s, reason) }) {
			t.Errorf("no alert summary names reason %q; got %v", reason, fired)
		}
	}

	// One bypass scraped from two replicas is one event.
	single := bypassSeries(t, []string{nsA}, []string{metrics.BypassNodeLookup})
	ha := []map[string]string{single[0], replicaOf(single[0], "cpu-pinning-operator-1")}
	if n := len(groupBy(ha, by)); n != 1 {
		t.Errorf("one bypass scraped from two replicas fired %d alerts grouping by %v, want 1", n, by)
	}

	// increase()/rate() read 0 for a series born inside their window, missing the first
	// bypass. The shipped shape subtracts the value one window back with `or (... * 0)`
	// for an absent series.
	if rateFnRe.MatchString(alert.Expr) {
		t.Errorf("CPUPinningWebhookBypass reads the counter through increase()/rate(), which cannot see the "+
			"first bypass in a tenant: %s", alert.Expr)
	}
	if !strings.Contains(alert.Expr, "* 0)") {
		t.Errorf("CPUPinningWebhookBypass has no substitute for a series that did not exist a window ago, so a "+
			"first bypass drops out of the subtraction: %s", alert.Expr)
	}
	o := offsetRe.FindStringSubmatch(alert.Expr)
	if o == nil {
		t.Fatalf("CPUPinningWebhookBypass expr has no offset, so it compares the counter against nothing: %s", alert.Expr)
	}
	window, err := time.ParseDuration(o[1])
	if err != nil {
		t.Fatalf("parse offset %q: %v", o[1], err)
	}
	pending, err := time.ParseDuration(alert.For)
	if err != nil {
		t.Fatalf("parse for %q: %v", alert.For, err)
	}
	// The window must outlive `for`, or the difference drops to 0 before the alert
	// matures.
	if window <= pending {
		t.Errorf("CPUPinningWebhookBypass window %v does not outlive its for: %v, so one bypass can clear before it fires",
			window, pending)
	}
}
