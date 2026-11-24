// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// The per-machine page's log section, last on the page. It defaults to the
// balloons plugin's stream: the plugin applies the pinning, and only its lines
// carry the node as a stream label.
package dashboard

import (
	"fmt"
	"strings"
)

// LokiConfig is where the log section reads from, mirroring the chart's
// grafanaDashboard Loki values. Without Loki the section is omitted, or every
// panel would report "Datasource was not found".
type LokiConfig struct {
	Enabled bool
	// DatasourceUID pins the `loki` variable; empty lets Grafana auto-select.
	DatasourceUID string
	// NodeLabel is the stream label carrying the machine a line came from
	// (k8s_node_name under OTLP/k8sattributes, node_name under promtail).
	NodeLabel string
	// PluginLabel and PluginName select the balloons plugin's stream.
	PluginLabel string
	PluginName  string
	// WorkloadLabel is the stream label carrying the operator's workload name.
	WorkloadLabel string
}

// on reports whether the section can resolve. Label names are required, or the
// selector would match every stream.
func (l LokiConfig) on() bool {
	if l.NodeLabel == "" || l.PluginLabel == "" || l.PluginName == "" || l.WorkloadLabel == "" {
		return false
	}
	return l.Enabled || l.DatasourceUID != ""
}

// operatorWorkloadPattern matches the operator's workload under any release name.
const operatorWorkloadPattern = "cpu-pinning-operator.*"

// Two benign ERROR messages from the balloons plugin, produced by normal operation:
//
//   - benignReleaseLine: releasing memory for a container that never pinned any
//     (every non-span balloon); a real allocation failure logs "allocMem" instead.
//     The filter also hides this line for span balloons, where it can mean real
//     bookkeeping trouble.
//   - benignPodResAPILine: kubelet's PodResources Get is behind an alpha gate;
//     the plugin falls back to List.
//
// They are hidden only in the panel; Loki keeps the lines.
const (
	benignReleaseLine   = "libmem: unknown allocation"
	benignPodResAPILine = "PodResources API Get method disabled"

	// UnfilteredLineFilter matches every line; not "", since an empty custom-variable
	// value makes Grafana substitute the option label.
	UnfilteredLineFilter = `|= ""`
)

// BenignPluginFilter hides both messages. Exported so a chart test can check the
// cluster dashboard filters identically.
var BenignPluginFilter = fmt.Sprintf("!= %q != %q", benignReleaseLine, benignPodResAPILine)

// logSourceVar picks the plugin's stream (this machine) or the operator's
// (fleet-wide, its lines carry no node). Options are selector fragments.
func logSourceVar(node string, l LokiConfig) map[string]any {
	return customVar("logsource", "Logs from",
		"Which log the panel below reads: the balloons plugin on this machine, "+
			"or the operator's own log across the whole fleet.",
		// The plugin is the default: pinning failures show up there first.
		[]varOption{
			{"Balloons plugin on this machine", fmt.Sprintf("%s=%q, %s=%q",
				l.PluginLabel, l.PluginName, l.NodeLabel, node)},
			{"Operator (all machines)", fmt.Sprintf("%s=~%q", l.WorkloadLabel, operatorWorkloadPattern)},
		})
}

// benignFilterVar toggles hiding the benign plugin messages, independent of the source.
func benignFilterVar() map[string]any {
	return customVar("benign", "Plugin's benign errors",
		"Whether the panel below hides the two uninformative balloons-plugin ERROR "+
			"messages: the libmem release of an allocation the plugin never "+
			"made, and the kubelet's disabled PodResources Get method. They are nearly "+
			"every ERROR line the plugin writes, so hiding them keeps its level from "+
			"reading as a permanent fault. Choose *Shown* for the unfiltered stream; "+
			"nothing is dropped on the way into Loki.",
		[]varOption{
			{"Hidden", BenignPluginFilter},
			{"Shown (unfiltered stream)", UnfilteredLineFilter},
		})
}

// varOption is one custom-variable choice: its label and LogQL fragment.
type varOption struct{ name, fragment string }

// customVar builds a Grafana `custom` variable of LogQL fragments; the first is
// the default. Grafana splits on commas, then the last " : ", so fragment commas
// are escaped, fragments must not contain " : ", and names must not contain commas.
func customVar(name, label, description string, opts []varOption) map[string]any {
	var query string
	list := make([]any, 0, len(opts))
	for i, o := range opts {
		if i > 0 {
			query += ","
		}
		query += o.name + " : " + strings.ReplaceAll(o.fragment, ",", `\,`)
		list = append(list, map[string]any{
			"text": o.name, "value": o.fragment, "selected": i == 0,
		})
	}
	return map[string]any{
		"name": name, "label": label, "type": "custom",
		"description": description,
		"query":       query, "hide": 0, "multi": false, "includeAll": false,
		"options": list,
		"current": map[string]any{
			"text": opts[0].name, "value": opts[0].fragment, "selected": true,
		},
	}
}

// lokiVar builds the `loki` datasource variable, following datasourceVar: an empty
// uid leaves `current` empty so Grafana auto-selects.
func lokiVar(uid string) map[string]any {
	return map[string]any{
		"name": "loki", "label": "Loki data source", "type": "datasource",
		"description": "Loki data source the machine-log panel reads. Left unset by default " +
			"so Grafana selects the default Loki data source on import.",
		"query": "loki", "hide": 0,
		"current": pinnedDatasource(uid),
	}
}

// machineLogPanels is the log section's row header and panel, starting at grid row y.
func machineLogPanels(y int) []any {
	return []any{
		map[string]any{
			"type": "row", "title": "Logs from this machine", "collapsed": false,
			"gridPos": gridPos(0, y, 24, 1), "id": idLogRow,
		},
		map[string]any{
			"type":  "logs",
			"title": "Balloons plugin and operator logs",
			"description": "The balloons plugin logs only on pod create and remove, so an empty " +
				"panel over a short range is normal; breakage looks like no lines over 24 hours. " +
				"The **Logs from** picker sets the selector. Two benign messages are hidden, " +
				"`libmem: unknown allocation` and `PodResources API Get method disabled` — see " +
				"docs/observability.md.",
			"datasource": map[string]any{"type": "loki", "uid": "${loki}"},
			"gridPos":    gridPos(0, y+1, 24, 10),
			"id":         idLogPanel,
			"options": map[string]any{
				"showTime":         true,
				"wrapLogMessage":   true,
				"enableLogDetails": true,
				"dedupStrategy":    "none",
				"sortOrder":        "Descending",
			},
			"targets": []any{map[string]any{
				"refId":      "A",
				"datasource": map[string]any{"type": "loki", "uid": "${loki}"},
				// :raw inserts the fragments verbatim; other formats would encode them.
				"expr": "{${logsource:raw}} ${benign:raw}",
			}},
		},
	}
}
