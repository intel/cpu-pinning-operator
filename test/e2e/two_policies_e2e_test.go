// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// sglangSpanArgRegex is the SGLang spelling of the tensor-parallel flag.
const sglangSpanArgRegex = `--tp-size(?:=|\s+)(\d+)`

// TestTwoPoliciesPinTheirOwnWorkloads: two policies in two namespaces each pin
// their own pod with their own intent, and deleting one leaves the other intact.
func TestTwoPoliciesPinTheirOwnWorkloads(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(e2eMutatingGate) == "" {
		skipNotSelected(at, "set %s=1 to run the two-policy test (it creates two namespaces and two BalloonPinningPolicies)", e2eMutatingGate)
	}
	c := newClient(at)
	if ms := clusterMaxSpan(at, c); ms < 2 {
		skipNoConfig(at, "cluster max-span is %d; the two policies need spans 1 and 2 to tell their intents apart", ms)
	}
	sfx := utilrand.String(6)
	nsA, nsB := tpNamespace(t, c, "cpu-pinning-e2e-two-a-"+sfx), tpNamespace(t, c, "cpu-pinning-e2e-two-b-"+sfx)
	polA, polB := "e2e-two-vllm-"+sfx, "e2e-two-sglang-"+sfx
	// Each default differs from the pod's TP, so a span equal to the TP proves the argRegex fired.
	tpPolicy(t, c, polA, nsA, "vllm", vllmSpanArgRegex, 2)
	tpPolicy(t, c, polB, nsB, "sglang", sglangSpanArgRegex, 1)
	tpWaitSelector(at, c, nsA, true)
	tpWaitSelector(at, c, nsB, true)
	tpWaitSettled(at, c, polA, polB)

	keyA, keyB := balloonAnnotationBase+"/container.vllm", balloonAnnotationBase+"/container.sglang"
	podA := tpAdmitUntil(t, c, func(i int) *corev1.Pod {
		// Carries the other runtime's flag too, so the wrong intent would read span 2.
		p := vllmPod(fmt.Sprintf("e2e-two-a-%d", i), nsA, "2", "2", 1)
		p.Spec.Containers[0].Args = append(p.Spec.Containers[0].Args, "--tp-size", "2")
		return p
	}, func(p *corev1.Pod) bool { return p.Annotations[keyA] != "" })
	podB := tpAdmitUntil(t, c, func(i int) *corev1.Pod {
		p := vllmPod(fmt.Sprintf("e2e-two-b-%d", i), nsB, "2", "2", 0)
		p.Spec.Containers[0].Name = "sglang"
		p.Spec.Containers[0].Args = []string{"--tp-size", "2", "--tensor-parallel-size", "1"}
		return p
	}, func(p *corev1.Pod) bool { return p.Annotations[keyB] != "" })

	if got := podA.Annotations[keyA]; got != "balloon-span1" {
		at.Errorf("pod %s/%s: %s = %q, want balloon-span1 from %s's --tensor-parallel-size 1", nsA, podA.Name, keyA, got, polA)
	}
	if got := podB.Annotations[keyB]; got != "balloon-span2" {
		at.Errorf("pod %s/%s: %s = %q, want balloon-span2 from %s's --tp-size 2", nsB, podB.Name, keyB, got, polB)
	}
	// The webhook stamps the operator, not the policy; status.boundPods is the attribution.
	for _, p := range []*corev1.Pod{podA, podB} {
		if got := p.Annotations[pinnedByAnnotation]; got != "cpu-pinning-operator" {
			at.Errorf("pod %s/%s: pinned-by = %q, want cpu-pinning-operator", p.Namespace, p.Name, got)
		}
	}

	wantA, wantB := []string{nsA + "/" + podA.Name}, []string{nsB + "/" + podB.Name}
	if !tpWaitBound(t, c, polA, wantA, 2*time.Minute) {
		at.Errorf("policy %s status.boundPods = %v, want only %v", polA, tpBound(t, c, polA), wantA)
	}
	if !tpWaitBound(t, c, polB, wantB, 2*time.Minute) {
		at.Errorf("policy %s status.boundPods = %v, want only %v", polB, tpBound(t, c, polB), wantB)
	}

	ctx := context.Background()
	pa := newUnstructured(bppGVK)
	pa.SetName(polA)
	if err := c.Delete(ctx, pa); err != nil {
		at.Fatalf("delete policy %s: %v", polA, err)
	}
	// The selector shrinking proves the controller reconciled the deletion; B must stay
	// selected, or B's future pods would bypass the webhook.
	tpWaitSelector(at, c, nsA, false)
	tpWaitSelector(at, c, nsB, true)
	if got := tpBound(t, c, polB); !slices.Equal(got, wantB) {
		at.Errorf("after deleting %s, policy %s status.boundPods = %v, want %v", polA, polB, got, wantB)
	}
	fresh := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(podB), fresh); err != nil {
		at.Errorf("after deleting %s, get pod %s/%s: %v", polA, nsB, podB.Name, err)
	} else if fresh.DeletionTimestamp != nil || fresh.Annotations[keyB] != podB.Annotations[keyB] {
		at.Errorf("after deleting %s, pod %s/%s lost its pinning: %s = %q, deleting=%v",
			polA, nsB, podB.Name, keyB, fresh.Annotations[keyB], fresh.DeletionTimestamp != nil)
	}
	// Retried: a replica may serve the deleted rule for a moment.
	late := tpAdmitUntil(t, c, func(i int) *corev1.Pod {
		return vllmPod(fmt.Sprintf("e2e-two-a-late-%d", i), nsA, "2", "2", 1)
	}, func(p *corev1.Pod) bool { return !tpStamped(p) })
	if tpStamped(late) {
		at.Errorf("pod %s/%s in %s's namespace was still stamped after the policy was deleted: %v", nsA, late.Name, polA, late.Annotations)
	}
}

// TestOverlappingPoliciesPickOneDeterministically: when two policies match the same
// pod, the one whose name sorts first wins, whatever the creation order.
func TestOverlappingPoliciesPickOneDeterministically(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(e2eMutatingGate) == "" {
		skipNotSelected(at, "set %s=1 to run the overlapping-policy test (it creates a namespace and two BalloonPinningPolicies)", e2eMutatingGate)
	}
	c := newClient(at)
	if ms := clusterMaxSpan(at, c); ms < 2 {
		skipNoConfig(at, "cluster max-span is %d; the two policies need spans 1 and 2 to tell which one won", ms)
	}
	sfx := utilrand.String(6)
	ns := tpNamespace(t, c, "cpu-pinning-e2e-overlap-"+sfx)
	// A TP=2 vLLM pod reads span 2 under the vLLM regex, the default 1 under the SGLang one.
	// The 00-/01- prefixes fix the name order: polV must win.
	polV, polS := "e2e-overlap-00-vllm-"+sfx, "e2e-overlap-01-sglang-"+sfx
	regex := map[string]string{polV: vllmSpanArgRegex, polS: sglangSpanArgRegex}
	bySpan := map[string]string{"balloon-span2": polV, "balloon-span1": polS}
	key := balloonAnnotationBase + "/container.vllm"
	ctx := context.Background()

	var verdicts []string
	winners := map[string]bool{}
	for round, order := range [][]string{{polV, polS}, {polS, polV}} {
		for _, name := range order {
			tpPolicy(t, c, name, ns, "vllm", regex[name], 1)
		}
		tpWaitSelector(at, c, ns, true)
		tpWaitSettled(at, c, order...)
		// Settled status is the leader's; give every replica's index time to load both.
		time.Sleep(15 * time.Second)

		var pods []*corev1.Pod
		for i := 0; i < 3; i++ {
			p := tpAdmitUntil(t, c, func(j int) *corev1.Pod {
				return vllmPod(fmt.Sprintf("e2e-overlap-r%d-%d-%d", round, i, j), ns, "2", "2", 2)
			}, tpStamped)
			pods = append(pods, p)
			var keys []string
			for k := range p.Annotations {
				if strings.HasPrefix(k, balloonAnnotationBase) {
					keys = append(keys, k)
				}
			}
			winner := bySpan[p.Annotations[key]]
			if len(keys) != 1 || winner == "" {
				at.Errorf("round %d pod %s: balloon annotations %v, want exactly %s = balloon-span2 (%s) or balloon-span1 (%s)",
					round, p.Name, keys, key, polV, polS)
				continue
			}
			winners[winner] = true
			verdicts = append(verdicts, fmt.Sprintf("created %v: pod %s stamped %s by %s", order, p.Name, p.Annotations[key], winner))
		}
		for _, p := range pods {
			owner := tpWaitOneOwner(t, c, ns+"/"+p.Name, order, 2*time.Minute)
			if owner == "" {
				at.Errorf("round %d pod %s: not listed in exactly one of %v status.boundPods", round, p.Name, order)
				continue
			}
			winners[owner] = true
			verdicts = append(verdicts, fmt.Sprintf("created %v: pod %s in %s status.boundPods", order, p.Name, owner))
		}

		for _, p := range pods {
			_ = c.Delete(ctx, p, client.GracePeriodSeconds(0))
		}
		for _, name := range order {
			tpDeletePolicy(at, c, name)
		}
	}

	at.Logf("overlap verdicts:\n  %s", strings.Join(verdicts, "\n  "))
	if len(winners) != 1 || !winners[polV] {
		at.Errorf("overlapping policies %s and %s: winners %v, want only %s, the name that sorts first (verdicts above)",
			polV, polS, winners, polV)
	} else {
		at.Logf("winner: %s for every pod, both creation orders", polV)
	}
}

// tpNamespace creates a namespace and deletes it on cleanup.
func tpNamespace(t *testing.T, c client.Client, name string) string {
	t.Helper()
	if err := e2eCreate(context.Background(), c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		t.Fatalf("create namespace %s: %v", name, err)
	}
	t.Cleanup(func() {
		e2eDelete(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	// An installed policy reaching it (a "*" glob, an empty selector) would compete with
	// the test's own policies and keep the namespace selected after they are deleted.
	if policyTargetsNamespace(name) {
		skipNoConfig(t, "an installed BalloonPinningPolicy already targets new namespace %s (glob or namespaceSelector), so the test's policies cannot be isolated", name)
	}
	return name
}

// tpPolicy creates a policy matching container in ns with a span intent, deleted on cleanup.
func tpPolicy(t *testing.T, c client.Client, name, ns, container, argRegex string, def int) {
	t.Helper()
	p := newUnstructured(bppGVK)
	p.SetName(name)
	p.Object["spec"] = map[string]interface{}{
		"match":      map[string]interface{}{"byContainerName": []interface{}{container}},
		"namespaces": []interface{}{ns},
		"intent": map[string]interface{}{
			"span": map[string]interface{}{"argRegex": argRegex, "default": int64(def)},
		},
	}
	if err := e2eCreate(context.Background(), c, p); err != nil {
		t.Fatalf("create BalloonPinningPolicy %s: %v", name, err)
	}
	t.Cleanup(func() {
		d := newUnstructured(bppGVK)
		d.SetName(name)
		e2eDelete(t, c, d)
	})
}

// tpDeletePolicy deletes a policy and waits until it is gone.
func tpDeletePolicy(t testing.TB, c client.Client, name string) {
	t.Helper()
	ctx := context.Background()
	p := newUnstructured(bppGVK)
	p.SetName(name)
	if err := c.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete policy %s: %v", name, err)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(2 * time.Second) {
		if err := c.Get(ctx, client.ObjectKey{Name: name}, newUnstructured(bppGVK)); apierrors.IsNotFound(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("policy %s still present a minute after delete", name)
		}
	}
}

// tpWaitSelector waits until the live webhook selector does (want) or does not cover ns.
func tpWaitSelector(at *allureT, c client.Client, ns string, want bool) {
	set := labels.Set{"kubernetes.io/metadata.name": ns}
	for deadline := time.Now().Add(2 * time.Minute); liveWebhookSelector(at.T, c, webhookConfigName()).Matches(set) != want; time.Sleep(3 * time.Second) {
		if time.Now().After(deadline) {
			at.Fatalf("webhook %s namespaceSelector covers %q = %v after 2m, want %v", webhookConfigName(), ns, !want, want)
		}
	}
}

// tpWaitSettled waits until each named policy's status caught up with its spec.
func tpWaitSettled(t testing.TB, c client.Client, names ...string) {
	t.Helper()
	for _, n := range names {
		if !waitForPolicySpecSettled(c, client.ObjectKey{Name: n}, 90*time.Second) {
			t.Fatalf("policy %s observedGeneration never caught up; the reconciler did not pick it up", n)
		}
	}
}

// tpAdmitUntil creates pods from build until one satisfies check or 45s pass, deleting
// the misses; it absorbs the lag before every webhook replica sees a policy change.
func tpAdmitUntil(t *testing.T, c client.Client, build func(i int) *corev1.Pod, check func(*corev1.Pod) bool) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for i := 0; ; i++ {
		p := createAndCleanup(t, c, build(i))
		if check(p) || time.Now().After(deadline) {
			return p
		}
		_ = c.Delete(context.Background(), p, client.GracePeriodSeconds(0))
		time.Sleep(3 * time.Second)
	}
}

func tpStamped(p *corev1.Pod) bool {
	for k := range p.Annotations {
		if strings.HasPrefix(k, balloonAnnotationBase) {
			return true
		}
	}
	return false
}

// tpBound returns a policy's status.boundPods as sorted "ns/name".
func tpBound(t *testing.T, c client.Client, policy string) []string {
	t.Helper()
	pol := newUnstructured(bppGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: policy}, pol); err != nil {
		t.Fatalf("get policy %s: %v", policy, err)
	}
	bound, _, _ := unstructured.NestedSlice(pol.Object, "status", "boundPods")
	var out []string
	for _, b := range bound {
		bp, _ := b.(map[string]interface{})
		ns, _, _ := unstructured.NestedString(bp, "namespace")
		name, _, _ := unstructured.NestedString(bp, "name")
		out = append(out, ns+"/"+name)
	}
	slices.Sort(out)
	return out
}

// tpWaitBound waits until a policy's status.boundPods equals want.
func tpWaitBound(t *testing.T, c client.Client, policy string, want []string, timeout time.Duration) bool {
	t.Helper()
	for deadline := time.Now().Add(timeout); !slices.Equal(tpBound(t, c, policy), want); time.Sleep(5 * time.Second) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

// tpWaitOneOwner waits until exactly one of policies lists pod in status.boundPods and
// returns it, or "" at the deadline.
func tpWaitOneOwner(t *testing.T, c client.Client, pod string, policies []string, timeout time.Duration) string {
	t.Helper()
	for deadline := time.Now().Add(timeout); ; time.Sleep(5 * time.Second) {
		var owners []string
		for _, p := range policies {
			if slices.Contains(tpBound(t, c, p), pod) {
				owners = append(owners, p)
			}
		}
		if len(owners) == 1 {
			return owners[0]
		}
		if time.Now().After(deadline) {
			return ""
		}
	}
}
