// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestMetricsRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()

	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			t.Fatalf("failed to register collector: %v", err)
		}
	}

	if _, err := reg.Gather(); err != nil {
		t.Fatalf("Gather() returned error: %v", err)
	}
}

// TestCollectorsReachTheServedRegistry proves every collector is registered with ctrlmetrics.Registry.
func TestCollectorsReachTheServedRegistry(t *testing.T) {
	for i, c := range collectors {
		name := collectorName(t, c)
		if name == "" {
			// A collector with no Desc registers as unchecked, so the probe below would misfire.
			t.Errorf("collectors[%d] describes no metric, so its registration cannot be judged", i)
			continue
		}
		// AlreadyRegisteredError means it is registered; nil means it was not (undo the probe).
		var already prometheus.AlreadyRegisteredError
		err := ctrlmetrics.Registry.Register(c)
		switch {
		case err == nil:
			ctrlmetrics.Registry.Unregister(c)
			t.Errorf("%s is declared but not registered with the registry the operator serves /metrics from", name)
		case !errors.As(err, &already):
			t.Errorf("registering %s reported %v, which is neither success nor the already-registered signal", name, err)
		}
	}
}

// collectorName is a collector's metric names, comma-separated.
func collectorName(t *testing.T, c prometheus.Collector) string {
	t.Helper()
	return strings.Join(slices.Sorted(maps.Keys(labelsOf(t, c))), ",")
}

// TestEveryDeclaredMetricIsInTheCollectorList proves every metric variable in the
// package source is in the collectors list.
func TestEveryDeclaredMetricIsInTheCollectorList(t *testing.T) {
	declared, listed := metricVarsInSource(t)

	// Self-check: a scan that reads nothing would pass vacuously.
	if len(declared) == 0 {
		t.Fatal("source scan found no metric variables at all; the scan is broken, not the code")
	}
	if len(listed) != len(collectors) {
		t.Fatalf("source scan read %d entries in the collectors list, but it holds %d at run "+
			"time; the scan is reading the wrong declaration", len(listed), len(collectors))
	}

	for _, name := range declared {
		if !slices.Contains(listed, name) {
			t.Errorf("%s is declared but missing from the collectors list, so init() never registers it", name)
		}
	}
}

// metricVarsInSource returns the names of the package-level variables assigned a
// prometheus constructor, and the names listed in the collectors slice literal.
func metricVarsInSource(t *testing.T) (declared, listed []string) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("reading the package source: %v", err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
					continue
				}
				switch v := value.Values[0].(type) {
				case *ast.CallExpr: // Name = prometheus.NewXxx(...)
					if isPrometheusConstructor(v.Fun) {
						declared = append(declared, value.Names[0].Name)
					}
				case *ast.CompositeLit: // collectors = []prometheus.Collector{...}
					if value.Names[0].Name != "collectors" {
						continue
					}
					for _, element := range v.Elts {
						if id, ok := element.(*ast.Ident); ok {
							listed = append(listed, id.Name)
						}
					}
				}
			}
		}
	}
	return declared, listed
}

// isPrometheusConstructor reports whether the expression is a prometheus.New* call.
func isPrometheusConstructor(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "prometheus" && strings.HasPrefix(sel.Sel.Name, "New")
}

// TestPinBlockedCauseValues pins the cause label values that docs and the shipped alert rely on.
func TestPinBlockedCauseValues(t *testing.T) {
	cases := []struct{ got, want string }{
		{PinBlockedNoBalloon, "no-balloon"},
		{PinBlockedNoCapacity, "no-capacity"},
		{PinBlockedOther, "plugin-error"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("cause label = %q, want %q", c.got, c.want)
		}
	}
}

func TestMetricLabelConsistency(t *testing.T) {

	t.Run("PodsPinned", func(t *testing.T) {
		PodsPinned.WithLabelValues("default", "balloon-a", "inference-policy")
	})
	t.Run("PodsSkipped", func(t *testing.T) {
		PodsSkipped.WithLabelValues("default", ReasonFractional)
		PodsSkipped.WithLabelValues("default", ReasonNoBalloon)
		PodsSkipped.WithLabelValues("default", ReasonExcluded)
		PodsSkipped.WithLabelValues("default", ReasonAmbiguous)
	})
	t.Run("CapacityCheckFailures", func(t *testing.T) {
		CapacityCheckFailures.WithLabelValues("worker-1", "default", "capacity", "enforce")
		CapacityCheckFailures.WithLabelValues("worker-1", "default", "memory", "warn")
	})
	t.Run("WebhookBypasses", func(t *testing.T) {
		WebhookBypasses.WithLabelValues("default", BypassNodeLookup)
		WebhookBypasses.WithLabelValues("default", BypassCapacityCheck)
		WebhookBypasses.WithLabelValues("default", BypassMarshal)
	})
	t.Run("DriftEvictions", func(t *testing.T) {
		DriftEvictions.WithLabelValues("worker-1", "default", DriftEvictionEvicted)
		DriftEvictions.WithLabelValues("worker-1", "default", DriftEvictionRateLimited)
		DriftEvictions.WithLabelValues("worker-1", "default", DriftEvictionPDBBlocked)
		DriftEvictions.WithLabelValues("worker-1", "default", DriftEvictionError)
	})
	t.Run("PodsDrifted", func(t *testing.T) {
		PodsDrifted.WithLabelValues("worker-1", "default")
	})
	t.Run("PinningBlocked", func(t *testing.T) {
		PinningBlocked.WithLabelValues("worker-1", "default", PinBlockedNoBalloon)
		PinningBlocked.WithLabelValues("worker-1", "default", PinBlockedNoCapacity)
		PinningBlocked.WithLabelValues("worker-1", "default", PinBlockedOther)
	})
	t.Run("NodeProvisioning", func(t *testing.T) {
		NodeProvisioning.WithLabelValues("worker-1", NodeProvisioned)
		NodeProvisioning.WithLabelValues("worker-1", NodeUpdated)
		NodeProvisioning.WithLabelValues("worker-1", NodeDeprovisioned)
		NodeProvisioning.WithLabelValues("worker-1", NodeProvisionFailed)
	})
	t.Run("WebhookAdmissionSeconds", func(t *testing.T) {
		WebhookAdmissionSeconds.WithLabelValues("pinned")
		WebhookAdmissionSeconds.WithLabelValues("skipped")
		WebhookAdmissionSeconds.WithLabelValues("denied")
		WebhookAdmissionSeconds.WithLabelValues("error")
	})
	t.Run("NodeReservedCPUs", func(t *testing.T) {
		NodeReservedCPUs.WithLabelValues("node-a")
	})
	t.Run("ReservedPoolUtilizationRatio", func(t *testing.T) {
		ReservedPoolUtilizationRatio.WithLabelValues("node-a")
	})
	t.Run("NodeHTSiblingPolicy", func(t *testing.T) {
		NodeHTSiblingPolicy.WithLabelValues("node-a", "held-back")
		NodeHTSiblingPolicy.WithLabelValues("node-a", "reserved-pool")
	})
	t.Run("ReservationsRecorded", func(t *testing.T) {
		ReservationsRecorded.WithLabelValues("node-a")
	})
	t.Run("ReservationsLive", func(t *testing.T) {
		ReservationsLive.WithLabelValues("node-a")
	})
	t.Run("BalloonsPolicyReconcileErrors", func(t *testing.T) {
		BalloonsPolicyReconcileErrors.WithLabelValues("node-a")
	})
	t.Run("CPUAssignment", func(t *testing.T) {
		CPUAssignment.WithLabelValues("node-a", "0", "0", "0", "free", "", "")
		CPUAssignment.WithLabelValues("node-a", "1", "0", "0", "reserved", "", "")
	})
	t.Run("PoolMembership", func(t *testing.T) {
		PoolMembership.WithLabelValues("node-a", "reserved", "kube-system", "kube-proxy-x", "kube-proxy")
		PoolMembership.WithLabelValues("node-a", "balloon-span1", "inference", "demo-x", "demo")
	})
	t.Run("GhostAllocations", func(t *testing.T) {
		GhostAllocations.WithLabelValues("node-a")
	})
	t.Run("NodeLogicalCPUs", func(t *testing.T) {
		NodeLogicalCPUs.WithLabelValues("node-a", "0")
	})
	t.Run("NodePinnableCPUs", func(t *testing.T) {
		NodePinnableCPUs.WithLabelValues("node-a", "0")
	})
	t.Run("CPUFragmentation", func(t *testing.T) {
		CPUFragmentation.WithLabelValues("node-a")
	})
	t.Run("PluginNotReady", func(t *testing.T) {
		PluginNotReady.WithLabelValues("node-a")
	})
}

func TestMetricRecording(t *testing.T) {
	reg := prometheus.NewRegistry()

	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			t.Fatalf("failed to register collector: %v", err)
		}
	}

	PodsPinned.WithLabelValues("default", "balloon-a", "inference-policy").Inc()
	PodsSkipped.WithLabelValues("default", ReasonFractional).Inc()
	CapacityCheckFailures.WithLabelValues("worker-1", "default", "capacity", "enforce").Inc()
	WebhookBypasses.WithLabelValues("default", BypassNodeLookup).Inc()
	DriftEvictions.WithLabelValues("worker-1", "default", DriftEvictionEvicted).Inc()
	PodsDrifted.WithLabelValues("worker-1", "default").Inc()
	PinningBlocked.WithLabelValues("worker-1", "default", PinBlockedNoCapacity).Inc()
	NodeProvisioning.WithLabelValues("worker-1", NodeProvisioned).Inc()
	WebhookAdmissionSeconds.WithLabelValues("pinned").Observe(0.05)
	ReservationsRecorded.WithLabelValues("node-a").Inc()
	ReservationsLive.WithLabelValues("node-a").Set(1)
	NodeReservedCPUs.WithLabelValues("node-a").Set(4)
	ReservedPoolUtilizationRatio.WithLabelValues("node-a").Set(0.5)
	NodeHTSiblingPolicy.WithLabelValues("node-a", "held-back").Set(1)
	BalloonsPolicyReconcileErrors.WithLabelValues("node-a").Inc()
	CPUAssignment.WithLabelValues("node-a", "0", "0", "0", "free", "", "").Set(1)
	PoolMembership.WithLabelValues("node-a", "reserved", "kube-system", "kube-proxy-x", "kube-proxy").Set(6)
	NodeLogicalCPUs.WithLabelValues("node-a", "0").Set(16)
	NodePinnableCPUs.WithLabelValues("node-a", "0").Set(8)
	CPUFragmentation.WithLabelValues("node-a").Set(2)
	PluginNotReady.WithLabelValues("node-a").Inc()
	GhostAllocations.WithLabelValues("node-a").Set(0)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() returned error: %v", err)
	}

	wantNames := map[string]bool{
		namespace + "_pods_pinned_total":                     false,
		namespace + "_pods_skipped_total":                    false,
		namespace + "_capacity_check_failures_total":         false,
		namespace + "_webhook_bypasses_total":                false,
		namespace + "_drift_evictions_total":                 false,
		namespace + "_pods_drifted_total":                    false,
		namespace + "_pods_pinning_blocked_total":            false,
		namespace + "_node_provisioning_total":               false,
		namespace + "_webhook_admission_seconds":             false,
		namespace + "_webhook_reservations_recorded_total":   false,
		namespace + "_webhook_reservations_live":             false,
		namespace + "_node_reserved_cpus":                    false,
		namespace + "_reserved_pool_utilization_ratio":       false,
		namespace + "_ht_sibling_policy":                     false,
		namespace + "_balloonspolicy_reconcile_errors_total": false,
		namespace + "_cpu_assignment":                        false,
		namespace + "_pool_membership":                       false,
		namespace + "_node_logical_cpus":                     false,
		namespace + "_node_pinnable_cpus":                    false,
		namespace + "_cpu_fragmentation":                     false,
		namespace + "_ghost_allocations":                     false,
		namespace + "_webhook_plugin_not_ready_total":        false,
	}

	for _, fam := range families {
		if _, ok := wantNames[fam.GetName()]; ok {
			wantNames[fam.GetName()] = true
		}
	}

	for name, found := range wantNames {
		if !found {
			t.Errorf("expected metric family %q not found in Gather() output", name)
		}
	}
}
