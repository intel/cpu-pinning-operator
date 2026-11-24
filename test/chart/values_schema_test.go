// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// schemaPath is the chart's JSON schema, which Helm applies on template, install and
// upgrade.
func schemaPath() string { return filepath.Join(chartUnderTest(), "values.schema.json") }

// renderValues runs `helm template` with extra arguments and returns the output and exit
// error.
func renderValues(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	helm := helmBin(t)
	args := append([]string{"template", "t", chartUnderTest(), "-f", policyTargetValues, "-n", "cpu-pinning-operator-system"}, extra...)
	out, err := exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		requireSubcharts(t, string(out))
	}
	return string(out), err
}

// valuesFileArg writes a values overlay to a temp file and returns the `-f` pair.
func valuesFileArg(t *testing.T, body string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write values: %v", err)
	}
	return []string{"-f", p}
}

// schemaNode is the part of a schema node this file walks.
type schemaNode struct {
	Properties           map[string]schemaNode `json:"properties"`
	AdditionalProperties json.RawMessage       `json:"additionalProperties"`
	Ref                  string                `json:"$ref"`
}

// TestValuesSchemaCoversEveryValue proves every value in values.yaml has a schema
// property, descending wherever the schema enumerates properties. A node without
// properties is deliberately free-form.
func TestValuesSchemaCoversEveryValue(t *testing.T) {
	valuesPath := filepath.Join(chartUnderTest(), "values.yaml")
	data, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("read %s: %v", valuesPath, err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		t.Fatalf("parse %s: %v", valuesPath, err)
	}
	if len(values) == 0 {
		t.Fatalf("%s parsed to no keys — the guard would pass vacuously", valuesPath)
	}

	schema := wholeSchema(t)
	var missing []string
	walkCoverage(values, schema.Properties, schema, "", &missing)
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("values.yaml keys absent from %s, so nothing validates them:\n  %s",
			schemaPath(), strings.Join(missing, "\n  "))
	}
}

// walkCoverage records every values path with no matching schema property;
// `additionalProperties` and property-less nodes are terminal.
func walkCoverage(values map[string]any, properties map[string]schemaNode, root wholeSchemaDoc, path string, missing *[]string) {
	for key, value := range values {
		here := key
		if path != "" {
			here = path + "." + key
		}
		node, ok := properties[key]
		if !ok {
			*missing = append(*missing, here)
			continue
		}
		node = resolve(node, root)
		child, isMap := value.(map[string]any)
		if !isMap || len(node.Properties) == 0 {
			continue
		}
		walkCoverage(child, node.Properties, root, here, missing)
	}
}

// resolve follows a local $ref into the schema's definitions.
func resolve(node schemaNode, root wholeSchemaDoc) schemaNode {
	name, ok := strings.CutPrefix(node.Ref, "#/definitions/")
	if !ok {
		return node
	}
	return root.Definitions[name]
}

type wholeSchemaDoc struct {
	Properties  map[string]schemaNode `json:"properties"`
	Definitions map[string]schemaNode `json:"definitions"`
}

func wholeSchema(t *testing.T) wholeSchemaDoc {
	t.Helper()
	data, err := os.ReadFile(schemaPath())
	if err != nil {
		t.Fatalf("read %s: %v", schemaPath(), err)
	}
	var doc wholeSchemaDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", schemaPath(), err)
	}
	if len(doc.Properties) == 0 {
		t.Fatalf("%s declares no top-level properties", schemaPath())
	}
	return doc
}

// TestValuesSchemaRejectsInvalidValues proves wrong-case enums, out-of-range integers,
// wrong types and enum typos fail `helm template` with a message naming the setting.
func TestValuesSchemaRejectsInvalidValues(t *testing.T) {
	cases := map[string]struct {
		args   []string
		values string // overlay written to a values file, for cases --set cannot express
		want   string
	}{
		"lowercase failurePolicy": {
			args: []string{"--set", "webhook.mutatingWebhookConfiguration.failurePolicy=ignore"},
			want: "webhook.mutatingWebhookConfiguration.failurePolicy",
		},
		"lowercase failurePolicy on the validating guard": {
			args: []string{"--set", "webhook.validatingWebhookConfiguration.failurePolicy=fail"},
			want: "webhook.validatingWebhookConfiguration.failurePolicy",
		},
		"admission timeout above the Kubernetes maximum": {
			args: []string{"--set", "webhook.mutatingWebhookConfiguration.timeoutSeconds=60"},
			want: "webhook.mutatingWebhookConfiguration.timeoutSeconds",
		},
		"port outside the TCP range": {
			args: []string{"--set", "metrics.port=70000"},
			want: "metrics.port",
		},
		"string where the readiness secure boolean is expected": {
			args: []string{"--set-string", "balloonsReady.secure=false"},
			want: "balloonsReady.secure",
		},
		"string where the readiness external-cert boolean is expected": {
			args: []string{"--set-string", "balloonsReady.allowExternalServingCert=true"},
			want: "balloonsReady.allowExternalServingCert",
		},
		"empty CA duration": {
			args: []string{"--set-string", "certManager.ca.duration="},
			want: "certManager.ca.duration",
		},
		"negative replica count": {
			args: []string{"--set", "replicaCount=-1"},
			want: "replicaCount",
		},
		"negative poll budget": {
			args: []string{"--set", "initContainer.balloons.waitTimeout=-5"},
			want: "initContainer.balloons.waitTimeout",
		},
		"string where an integer is expected": {
			args: []string{"--set-string", "healthProbe.port=8081"},
			want: "healthProbe.port",
		},
		"boolean gate given a non-boolean": {
			args: []string{"--set-string", "metrics.enabled=yes"},
			want: "metrics.enabled",
		},
		"cpuAllocation typo": {
			args: []string{"--set", "balloonsClusterConfig.balloon.cpuAllocation=physical_cores"},
			want: "balloonsClusterConfig.balloon.cpuAllocation",
		},
		"htSiblingPolicy typo": {
			args: []string{"--set", "balloonsClusterConfig.balloon.htSiblingPolicy=excluded"},
			want: "balloonsClusterConfig.balloon.htSiblingPolicy",
		},
		"capacity-check mode in the wrong case": {
			args: []string{"--set", "balloonPinningPolicy.capacityCheck.mode=Enforce"},
			want: "balloonPinningPolicy.capacityCheck.mode",
		},
		"drift policy typo": {
			args: []string{"--set", "balloonPinningPolicy.driftPolicy=restarts"},
			want: "balloonPinningPolicy.driftPolicy",
		},
		"node-feature mode typo": {
			args: []string{"--set", "balloonPinningPolicy.scheduling.nodeFeatures.cpu-pinning-operator\\.intel\\.com/amx=preferred"},
			want: "nodeFeatures",
		},
		"pull policy typo": {
			args: []string{"--set", "image.pullPolicy=always"},
			want: "image.pullPolicy",
		},
		"empty image repository": {
			args: []string{"--set", "image.repository="},
			want: "image.repository",
		},
		// An unquoted 1.10 is a YAML number that narrows to 1.1.
		"unquoted numeric image tag": {
			values: "image:\n  tag: 1.10\n",
			want:   "image.tag",
		},
		"log level the manager cannot parse": {
			args: []string{"--set", "log.level=warn"},
			want: "log.level",
		},
		"reserved percent above 100": {
			args: []string{"--set", "balloonsClusterConfig.balloon.reserved.cpus=null",
				"--set", "balloonsClusterConfig.balloon.reserved.percent=120"},
			want: "balloonsClusterConfig.balloon.reserved.percent",
		},
		// A bare string would read as "no peers", opening the webhook port to any source.
		"webhook ingress peers given a string instead of a peer list": {
			args: []string{"--set-string", "networkPolicy.webhook.from=10.0.0.0/28"},
			want: "networkPolicy.webhook.from",
		},
		"monitoring toggle given a word that spells no boolean": {
			args: []string{"--set-string", "metrics.serviceMonitor.enabled=maybe"},
			want: "metrics.serviceMonitor.enabled",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			args := append([]string(nil), tc.args...)
			if tc.values != "" {
				args = append(args, valuesFileArg(t, tc.values)...)
			}
			out, err := renderValues(t, args...)
			if err == nil {
				t.Fatalf("render succeeded on a value the schema must reject:\n%s", out)
			}
			// Pinned to the schema's message, so a template-side `fail` cannot keep this
			// green after the schema constraint is dropped.
			if !strings.Contains(out, "don't meet the specifications of the schema") {
				t.Errorf("render failed, but not on the schema — the constraint may have moved or gone:\n%s", out)
			}
			// Helm >= 3.18 names the field as a JSON pointer, older releases as a dotted
			// path.
			if !strings.Contains(out, tc.want) && !strings.Contains(out, "/"+strings.ReplaceAll(tc.want, ".", "/")) {
				t.Errorf("rejection does not name %q, so the message does not say what to fix:\n%s", tc.want, out)
			}
		})
	}
}

// TestValuesSchemaAcceptsShippedValueSets proves the schema accepts the defaults, the
// OpenShift overlay and a realistic install overlay.
func TestValuesSchemaAcceptsShippedValueSets(t *testing.T) {
	const realInstall = `replicaCount: 2
image:
  repository: registry.example.com/team/cpu-pinning-operator
  tag: 24h
  pullPolicy: Always
metrics:
  enabled: true
  port: 8080
  secure: true
  serviceMonitor:
    enabled: true
  prometheusRule:
    enabled: true
grafanaDashboard:
  enabled: true
  lokiLogs:
    enabled: true
  lokiLabels:
    namespace: namespace
    workload: app
    node: node_name
    pluginWorkload: app
webhook:
  reserveAuthority:
    enabled: true
`

	valueSets := map[string][]string{
		"chart defaults":           nil,
		"OpenShift overlay":        {"-f", filepath.Join(chartUnderTest(), "values-openshift.yaml")},
		"a real install's values":  valuesFileArg(t, realInstall),
		"single replica, no PDB":   valuesFileArg(t, "replicaCount: 1\npodDisruptionBudget:\n  enabled: false\n"),
		"percent-sized reserved":   valuesFileArg(t, "balloonsClusterConfig:\n  balloon:\n    reserved:\n      cpus: null\n      percent: 5\n"),
		"measured-sized reserved":  valuesFileArg(t, "balloonsClusterConfig:\n  balloon:\n    reserved:\n      cpus: null\n      measured:\n        headroomPercent: 50\n        min: 8\n"),
		"density mode":             valuesFileArg(t, "balloonsClusterConfig:\n  balloon:\n    htSiblingPolicy: reserved\n"),
		"no target namespaces":     {"--set-json", `webhook.targetNamespaces=[]`, "--set", "balloonPinningPolicy.create=false"},
		"label-selector targeting": {"--set-json", `webhook.namespaceSelector={"matchLabels":{"pinning":"enabled"}}`},
		"webhook ingress pinned to control-plane ipBlocks": {"--set-json",
			`networkPolicy.webhook.from=[{"ipBlock":{"cidr":"10.0.0.0/28"}},{"ipBlock":{"cidr":"172.20.9.3/32"}}]`},
		"integer log verbosity":    {"--set", "log.level=2"},
		"quoted integer verbosity": {"--set-string", "log.level=2"},
		"capitalised log level":    {"--set", "log.level=Info"},
		// The manager's zap level map holds debug, info, error and panic, looked up
		// lowercased.
		"panic log level":          {"--set", "log.level=panic"},
		"quoted numeric image tag": {"--set-string", "image.tag=1.10"},
		"numeric sidecar label":    {"--set", "grafanaDashboard.sidecarLabelValue=1"},
		"boolean sidecar label":    {"--set", "grafanaDashboard.sidecarLabelValue=true"},
		"parked manager":           {"--set", "replicaCount=0"},
		"no-op disruption budget":  {"--set", "podDisruptionBudget.minAvailable=0"},
		// The init container appends the unit only to a bare number.
		"poll budget as a duration": {"--set", "initContainer.balloons.waitTimeout=5m"},
		"poll budget as seconds":    {"--set", "initContainer.balloons.waitTimeout=120"},
		"percentage minAvailable":   {"--set-string", "podDisruptionBudget.minAvailable=50%"},
		"balloons subchart off":     {"--set", "nri-resource-policy-balloons.enabled=false"},
		"bundled cert-manager":      {"--set", "cert-manager.enabled=true"},
	}

	for name, args := range valueSets {
		t.Run(name, func(t *testing.T) {
			if out, err := renderValues(t, args...); err != nil {
				t.Fatalf("a value set the chart already renders was rejected: %v\n%s", err, out)
			}
		})
	}
}

// TestValuesSchemaAcceptsEveryMonitoringToggleForm proves the nullable monitoring toggles
// accept unset, booleans, boolean strings and "", and reject other words. The shipped
// default is a null that reaches validation as one.
func TestValuesSchemaAcceptsEveryMonitoringToggleForm(t *testing.T) {
	for _, key := range []string{"metrics.serviceMonitor.enabled", "metrics.prometheusRule.enabled"} {
		t.Run(key, func(t *testing.T) {
			t.Run("the shipped null default", func(t *testing.T) {
				if out, err := renderValues(t); err != nil {
					t.Fatalf("the shipped default was rejected: %v\n%s", err, out)
				}
			})
			for _, form := range []string{"true", "false"} {
				t.Run("typed "+form, func(t *testing.T) {
					if out, err := renderValues(t, "--set", key+"="+form); err != nil {
						t.Fatalf("%s=%s was rejected: %v\n%s", key, form, err, out)
					}
				})
			}
			// Terraform's helm_release and --set-string spell booleans as strings.
			for _, form := range []string{"true", "false", "True", "FALSE", ""} {
				t.Run("string "+form, func(t *testing.T) {
					if out, err := renderValues(t, "--set-string", key+"="+form); err != nil {
						t.Fatalf("%s=%q as a string was rejected: %v\n%s", key, form, err, out)
					}
				})
			}
		})
	}
}
