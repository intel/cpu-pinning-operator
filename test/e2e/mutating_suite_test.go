// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// e2eMutatingGate opts in the suite that mutates shared cluster singletons.
const e2eMutatingGate = "E2E_MUTATING"

// TestGatedMutatingSuite runs the shared-singleton-mutating scenarios serially,
// settling the node back to baseline between them so none sees another's transition.
func TestGatedMutatingSuite(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(e2eMutatingGate) == "" {
		skipNotSelected(at, "set %s=1 to run the shared-singleton-mutating e2e suite (it mutates the cluster BalloonsClusterConfig / default BalloonPinningPolicy and settles the node to baseline between scenarios)", e2eMutatingGate)
	}
	c := newClient(at)
	node := balloonsEnabledNode(at, c)

	scenarios := []struct {
		name string
		fn   func(t *testing.T, c client.Client, node string)
	}{
		{"ContainerSelectionGlobSingleHit", runContainerSelectionGlobSingleHit},
		{"ContainerSelectionAmbiguousWholePod", runContainerSelectionAmbiguousWholePod},
		{"ContainerSelectionLargestContainer", runContainerSelectionLargestContainer},
		{"CapacityCheckEnforceDeniesOverCapacityPod", runCapacityCheckEnforceDeniesOverCapacityPod},
		{"CapacityCheckWarnAdmitsOverCapacityPod", runCapacityCheckWarnAdmitsOverCapacityPod},
		{"CapacityCheckOffSkipsCheck", runCapacityCheckOffSkipsCheck},
		{"DensityModeAbsorbsSiblingsIntoReserved", runDensityModeAbsorbsSiblingsIntoReserved},
		{"DensityModeAdmitsPinnedPodUnderEnforce", runDensityModeAdmitsPinnedPodUnderEnforce},
		{"ReservedFootprintPhysicalCoresDividesByThreads", runReservedFootprintPhysicalCoresDividesByThreads},
		{"NodeConfigOverrideAppliesPerNode", runNodeConfigOverrideAppliesPerNode},
		{"MeasuredReservedSizing", runMeasuredReservedSizing},
		{"ReservedCpusetCannotOverrideExcludeCpuset", runReservedCpusetCannotOverrideExcludeCpuset},
	}

	settle := newBaselineSettler(t, c, node)

	// The node-render settle does not guard spec.match, and a leaked container glob
	// silently defeats every later admission subtest, so the runner re-asserts it too.
	policyKey := client.ObjectKey{Name: installedPolicyName(t, c)}
	origMatch, hadMatch, haveBaseline := snapshotPolicyMatchBaseline(t, c, policyKey)

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			allureCase(t)
			// Registered first, so it runs last among the subtest's own cleanups: after
			// the scenario restored the policy, BalloonsClusterConfig or DaemonSet, and
			// before the next scenario starts. A failure is the subtest's, by name.
			t.Cleanup(func() {
				// Match first, so the webhook index is at baseline before any pod is admitted.
				if haveBaseline {
					restorePolicyMatchBaseline(t, c, policyKey, origMatch, hadMatch)
				}
				settle(t)
			})
			sc.fn(t, c, node)
		})
	}
}

// snapshotPolicyMatchBaseline reads the policy's spec.match before any subtest
// mutates it. A read failure disables the repair rather than aborting the suite.
func snapshotPolicyMatchBaseline(t *testing.T, c client.Client, key client.ObjectKey) (origMatch map[string]interface{}, hadMatch, haveBaseline bool) {
	t.Helper()
	bpp := newUnstructured(bppGVK)
	if err := c.Get(context.Background(), key, bpp); err != nil {
		if apierrors.IsNotFound(err) {
			t.Logf("baseline match: BalloonPinningPolicy %q not found; runner-level match repair disabled (subtest cleanup still restores its own mutation)", key.Name)
		} else {
			t.Logf("baseline match: get BalloonPinningPolicy %q failed (%v); runner-level match repair disabled", key.Name, err)
		}
		return nil, false, false
	}
	origMatch, hadMatch, _ = unstructured.NestedMap(bpp.Object, "spec", "match")
	return origMatch, hadMatch, true
}

// restorePolicyMatchBaseline re-asserts the baseline match between subtests and
// records a failure as residue, so the next scenario fails with the reason.
func restorePolicyMatchBaseline(t *testing.T, c client.Client, key client.ObjectKey, origMatch map[string]interface{}, hadMatch bool) {
	t.Helper()
	if err := restorePolicyMatch(c, key, origMatch, hadMatch, e2eSettleTimeout); err != nil {
		policyMatchResidue = fmt.Sprintf("the mutating suite's between-subtest match repair failed: %v", err)
		t.Errorf("baseline match: %v; the next scenario would see a non-baseline match", err)
	}
}

// newBaselineSettler snapshots the quiesced render as the baseline mode and returns
// a func that waits, logging to the given test, for the node to return to it. The
// reserved CPUs are left out: measured sizing legitimately changes them. Logs
// instead of failing on timeout.
func newBaselineSettler(t *testing.T, c client.Client, node string) func(*testing.T) {
	t.Helper()
	waitForNodeSettled(t, c, node)
	base := readNodeRenderState(context.Background(), c, node, balloonsNS())
	// cpuAllocation may be unpopulated at snapshot time; matching on "" would never settle.
	matchBaselineCPUAllocation := base.cpuAllocation != ""
	return func(t *testing.T) {
		if !waitForNodeRenderSettled(c, node, func(st nodeRenderState) bool {
			if st.availableCPU != base.availableCPU {
				return false
			}
			if matchBaselineCPUAllocation && st.cpuAllocation != base.cpuAllocation {
				return false
			}
			return true
		}) {
			t.Logf("settle: node %s did not return to its baseline render (availableResources.cpu=%q cpuAllocation=%q) within %s; the next scenario may observe stale node state",
				node, base.availableCPU, base.cpuAllocation, e2eSettleTimeout)
		}
	}
}
