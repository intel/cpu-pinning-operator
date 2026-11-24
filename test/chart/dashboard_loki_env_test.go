// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestDashboardLokiEnv proves the Loki values reach the operator container; an empty
// label name would select every stream.
func TestDashboardLokiEnv(t *testing.T) {
	helm := helmBin(t)

	render := func(t *testing.T, extra ...string) string {
		t.Helper()
		args := []string{"template", "t", "../../deploy/cpu-pinning-operator", "-f", policyTargetValues,
			"--show-only", "templates/configmap.yaml"}
		args = append(args, extra...)
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			requireSubcharts(t, string(out))
			t.Fatalf("helm template failed: %v\n%s", err, out)
		}
		return string(out)
	}

	value := func(t *testing.T, out, key string) string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, key+":") {
				return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, key+":")), `"`)
			}
		}
		t.Fatalf("%s not found in render:\n%s", key, out)
		return ""
	}

	t.Run("off and labelled by default", func(t *testing.T) {
		out := render(t)
		if got := value(t, out, "DASHBOARD_LOKI_ENABLED"); got != "false" {
			t.Errorf("DASHBOARD_LOKI_ENABLED = %q, want false: an install without Loki must "+
				"carry no Loki variable at all", got)
		}
		// The label names ship even with the section off.
		for key, want := range map[string]string{
			"DASHBOARD_LOKI_NODE_LABEL":   "k8s_node_name",
			"DASHBOARD_LOKI_PLUGIN_LABEL": "k8s_daemonset_name",
			"DASHBOARD_LOKI_PLUGIN_NAME":  "nri-resource-policy-balloons",
		} {
			if got := value(t, out, key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
	})

	t.Run("promtail scheme reaches the operator", func(t *testing.T) {
		out := render(t,
			"--set", "grafanaDashboard.lokiLogs.enabled=true",
			"--set", "grafanaDashboard.lokiDatasourceUid=loki",
			"--set", "grafanaDashboard.lokiLabels.node=node_name",
			"--set", "grafanaDashboard.lokiLabels.pluginWorkload=app")
		for key, want := range map[string]string{
			"DASHBOARD_LOKI_ENABLED":        "true",
			"DASHBOARD_LOKI_DATASOURCE_UID": "loki",
			"DASHBOARD_LOKI_NODE_LABEL":     "node_name",
			"DASHBOARD_LOKI_PLUGIN_LABEL":   "app",
		} {
			if got := value(t, out, key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
	})
}

// TestDatasourcePinsCarryTheUIDOnly proves a pinned data source sets only the uid:
// Grafana's `current.text` is the source's name, not its uid.
func TestDatasourcePinsCarryTheUIDOnly(t *testing.T) {
	raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n"+
		"  datasourceUid: openshift-thanos\n  lokiDatasourceUid: my-loki\n")
	for name, wantUID := range map[string]string{
		"datasource": "openshift-thanos",
		"loki":       "my-loki",
	} {
		cur := varCurrent(t, raw, name)
		if cur["value"] != wantUID {
			t.Errorf("%s var current.value = %v, want %q", name, cur["value"], wantUID)
		}
		if _, ok := cur["text"]; ok {
			t.Errorf("%s var pin carries text=%v; that field is the data source's name, "+
				"which the chart does not know", name, cur["text"])
		}
	}
}

// TestLogsPanelSeesBothNamespaces proves the logs panel selects on both the release
// namespace and the balloons namespace.
func TestLogsPanelSeesBothNamespaces(t *testing.T) {
	t.Run("self-installed subchart: one namespace", func(t *testing.T) {
		raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n"+
			"  lokiLabels:\n    namespace: namespace\n"+
			"nri-resource-policy-balloons:\n  enabled: true\n")
		if expr := logsExpr(t, raw); !strings.Contains(expr, `namespace=~"default"`) {
			t.Errorf("logs panel LogQL %q does not scope to the release namespace", expr)
		}
	})

	t.Run("balloons pre-installed elsewhere: both namespaces", func(t *testing.T) {
		raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n"+
			"  lokiLabels:\n    namespace: namespace\n"+
			"nri-resource-policy-balloons:\n  enabled: false\n")
		expr := logsExpr(t, raw)
		if !strings.Contains(expr, "kube-system") {
			t.Errorf("logs panel LogQL %q cannot see a balloons plugin outside the operator's "+
				"namespace, so its option would select nothing", expr)
		}
	})

	t.Run("explicit balloons namespace wins", func(t *testing.T) {
		raw := fleetDashboardJSON(t, "grafanaDashboard:\n  enabled: true\n  lokiLogs:\n    enabled: true\n"+
			"  lokiLabels:\n    namespace: namespace\n"+
			"initContainer:\n  balloons:\n    namespace: nri\n")
		if expr := logsExpr(t, raw); !strings.Contains(expr, "nri") {
			t.Errorf("logs panel LogQL %q ignores the configured balloons namespace", expr)
		}
	})
}
