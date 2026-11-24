// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/intel/cpu-pinning-operator/internal/dashboard"
)

// shadowedByScrapeTarget are the target labels the chart's ServiceMonitor attaches;
// a metric label of the same name is renamed exported_<name>. `node` is absent
// because the ServiceMonitor sets no targetLabels.
var shadowedByScrapeTarget = []string{
	"container", "endpoint", "instance", "job", "namespace", "pod", "service",
}

// descLabels pulls the variableLabels names out of a Desc's String() form.
var descLabels = regexp.MustCompile(`variableLabels: \{([^}]*)\}`)
var descName = regexp.MustCompile(`fqName: "([^"]*)"`)
var descHelp = regexp.MustCompile(`help: "((?:[^"\\]|\\.)*)"`)

// labelsOf returns a collector's metric names mapped to their variable labels.
func labelsOf(t *testing.T, c prometheus.Collector) map[string][]string {
	t.Helper()
	ch := make(chan *prometheus.Desc, 8)
	go func() {
		c.Describe(ch)
		close(ch)
	}()
	out := map[string][]string{}
	for d := range ch {
		s := d.String()
		name := descName.FindStringSubmatch(s)
		labels := descLabels.FindStringSubmatch(s)
		if name == nil || labels == nil {
			// Desc.String() is client_golang's format: fail rather than read nothing.
			t.Fatalf("cannot read labels out of Desc: %s", s)
		}
		var names []string
		for _, l := range strings.Split(labels[1], ",") {
			if l = strings.TrimSpace(l); l != "" {
				names = append(names, l)
			}
		}
		out[name[1]] = names
	}
	return out
}

// TestNoLabelShadowedByScrapeTarget proves no metric label collides with a scrape-target label.
func TestNoLabelShadowedByScrapeTarget(t *testing.T) {
	// Self-check first: a detector that quietly reads nothing turns this green.
	probe := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "shadowing_probe_total", Help: "detector self-check.",
	}, []string{"namespace", "reason"})
	got := labelsOf(t, probe)[namespace+"_shadowing_probe_total"]
	if len(got) != 2 || got[0] != "namespace" {
		t.Fatalf("detector read %v from a probe declaring [namespace reason]", got)
	}

	for _, c := range collectors {
		for metric, labels := range labelsOf(t, c) {
			for _, l := range labels {
				if slices.Contains(shadowedByScrapeTarget, l) {
					t.Errorf("%s declares label %q, which the scrape target sets: "+
						"Prometheus renames this one to exported_%s. Prefix it (workload_%s).",
						metric, l, l, l)
				}
			}
		}
	}
}

// TestWorkloadNamespaceLabelName pins the label name the shipped PrometheusRule groups on.
func TestWorkloadNamespaceLabelName(t *testing.T) {
	for _, c := range []prometheus.Collector{
		PodsPinned, PodsSkipped, CapacityCheckFailures, WebhookBypasses,
		DriftEvictions, PodsDrifted, PinningBlocked, CPUAssignment,
	} {
		for metric, labels := range labelsOf(t, c) {
			if !slices.Contains(labels, "workload_namespace") {
				t.Errorf("%s has labels %v, want one named \"workload_namespace\"", metric, labels)
			}
		}
	}
}

// helpOf returns a collector's metric names mapped to their Help strings.
func helpOf(t *testing.T, c prometheus.Collector) map[string]string {
	t.Helper()
	ch := make(chan *prometheus.Desc, 8)
	go func() {
		c.Describe(ch)
		close(ch)
	}()
	out := map[string]string{}
	for d := range ch {
		s := d.String()
		name := descName.FindStringSubmatch(s)
		help := descHelp.FindStringSubmatch(s)
		if name == nil || help == nil {
			// Desc.String() is client_golang's format: fail rather than read nothing.
			t.Fatalf("cannot read help out of Desc: %s", s)
		}
		unquoted, err := strconv.Unquote(`"` + help[1] + `"`)
		if err != nil {
			t.Fatalf("cannot unquote help %q: %v", help[1], err)
		}
		out[name[1]] = unquoted
	}
	return out
}

// TestMetricHelpFitsTheDescriptionLimit holds every metric's Help to the panel-description length limit.
func TestMetricHelpFitsTheDescriptionLimit(t *testing.T) {
	// Self-check first: a detector that quietly reads nothing turns this green.
	probe := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "help_probe_total", Help: "detector self-check.",
	}, []string{"reason"})
	if got := helpOf(t, probe)[namespace+"_help_probe_total"]; got != "detector self-check." {
		t.Fatalf("detector read %q from a probe whose help is %q", got, "detector self-check.")
	}

	for _, c := range collectors {
		for metric, help := range helpOf(t, c) {
			if strings.TrimSpace(help) == "" {
				t.Errorf("%s has no Help", metric)
				continue
			}
			if n := utf8.RuneCountInString(help); n > dashboard.MaxDescriptionChars {
				t.Errorf("%s has a %d-character Help, over the %d-character maximum by %d:\n%s",
					metric, n, dashboard.MaxDescriptionChars, n-dashboard.MaxDescriptionChars, help)
			}
		}
	}
}
