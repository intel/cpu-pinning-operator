// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// e2eSettleTimeout is generous: flipping a shared singleton re-provisions the node.
	e2eSettleTimeout = 3 * time.Minute
	// e2eSettleQuiesce spaces the two reads that must agree before a render counts as settled.
	e2eSettleQuiesce = 6 * time.Second
)

// e2eSettlePoll is a variable so the cluster-free guards can poll a fake client quickly.
var e2eSettlePoll = 3 * time.Second

// nodeRenderState is what the operator re-renders, asynchronously, after a
// shared-singleton write. The mutating suite waits on it between scenarios.
type nodeRenderState struct {
	availableCPU  string // node BalloonsPolicy spec.availableResources.cpu
	reservedCPU   string // node BalloonsPolicy spec.reservedResources.cpu
	cpuAllocation string // BCC status.perNode[node].cpuAllocation
	found         bool   // the node BalloonsPolicy existed
}

// readNodeRenderState reads a node's render state, swallowing read errors so it is safe in cleanup.
func readNodeRenderState(ctx context.Context, c client.Client, node, balloonsNamespace string) nodeRenderState {
	var st nodeRenderState
	bp := newUnstructured(bpGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: "node." + node, Namespace: balloonsNamespace}, bp); err == nil {
		st.found = true
		st.availableCPU, _, _ = unstructured.NestedString(bp.Object, "spec", "availableResources", "cpu")
		st.reservedCPU, _, _ = unstructured.NestedString(bp.Object, "spec", "reservedResources", "cpu")
	}
	bcc := newUnstructured(bccGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: "cluster"}, bcc); err == nil {
		perNode, _, _ := unstructured.NestedSlice(bcc.Object, "status", "perNode")
		for _, entry := range perNode {
			m, ok := entry.(map[string]interface{})
			if !ok {
				continue
			}
			if name, _, _ := unstructured.NestedString(m, "name"); name == node {
				st.cpuAllocation, _, _ = unstructured.NestedString(m, "cpuAllocation")
				break
			}
		}
	}
	return st
}

// waitForNodeRenderSettled reports whether the node's render matched and stayed
// unchanged across two spaced reads within e2eSettleTimeout. It never fails the test.
func waitForNodeRenderSettled(c client.Client, node string, match func(nodeRenderState) bool) bool {
	ctx := context.Background()
	deadline := time.Now().Add(e2eSettleTimeout)
	balloonsNamespace := balloonsNS() // resolved once, not on every poll
	for {
		// Two failed reads are equal zero states; require found so they don't count as quiesced.
		st := readNodeRenderState(ctx, c, node, balloonsNamespace)
		if st.found && match(st) {
			time.Sleep(e2eSettleQuiesce)
			st2 := readNodeRenderState(ctx, c, node, balloonsNamespace)
			if st2.found && match(st2) && st2 == st {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(e2eSettlePoll)
	}
}

// waitForNodeSettled blocks until the node's render is quiescent; logs instead of failing on timeout.
func waitForNodeSettled(t *testing.T, c client.Client, node string) {
	t.Helper()
	if !waitForNodeRenderSettled(c, node, func(nodeRenderState) bool { return true }) {
		t.Logf("settle: node %s render did not quiesce within %s before the read; it may be mid-reprovision", node, e2eSettleTimeout)
	}
}

// waitForPolicySpecSettled reports whether the policy's observedGeneration caught up
// within timeout. The operator sets it only after the spec is live in the webhook index.
func waitForPolicySpecSettled(c client.Client, key client.ObjectKey, timeout time.Duration) bool {
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for {
		cur := newUnstructured(bppGVK)
		if err := c.Get(ctx, key, cur); err == nil {
			if og, ok, _ := unstructured.NestedInt64(cur.Object, "status", "observedGeneration"); ok && og >= cur.GetGeneration() {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(e2eSettlePoll)
	}
}

// policyMatchResidue names the spec.match a failed restore left live, or "" at baseline.
// A plain variable: tests in this package run serially.
var policyMatchResidue string

// restorePolicyMatch writes spec.match back to baseline and waits until it is live in
// the webhook. It retries because status writes cause Conflicts and guardActivePods
// denies the write while bound pods remain.
func restorePolicyMatch(c client.Client, key client.ObjectKey, want map[string]interface{}, wantPresent bool, timeout time.Duration) error {
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	var lastErr error
	var got map[string]interface{}
	for {
		cur := newUnstructured(bppGVK)
		if err := c.Get(ctx, key, cur); err != nil {
			lastErr = err
		} else {
			var present bool
			got, present, _ = unstructured.NestedMap(cur.Object, "spec", "match")
			if present == wantPresent && (!wantPresent || reflect.DeepEqual(got, want)) {
				og, ok, _ := unstructured.NestedInt64(cur.Object, "status", "observedGeneration")
				if ok && og >= cur.GetGeneration() {
					return nil
				}
				lastErr = fmt.Errorf("observedGeneration %d behind generation %d", og, cur.GetGeneration())
			} else {
				// Write afresh every poll; one rejected write must not end the retrying.
				var err error
				if wantPresent {
					err = unstructured.SetNestedMap(cur.Object, want, "spec", "match")
				} else {
					unstructured.RemoveNestedField(cur.Object, "spec", "match")
				}
				if err == nil {
					err = c.Update(ctx, cur)
				}
				lastErr = err
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("policy %q spec.match is %v, want %v, not restored within %s (last error: %v)", key.Name, got, want, timeout, lastErr)
		}
		time.Sleep(e2eSettlePoll)
	}
}
