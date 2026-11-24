// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Negative e2e for a forged AdmissionReview burst against the in-memory reservation
// ledger: a caller posting reviews straight at the webhook port books reservations for
// pods that never exist and denies real pods on capacity. Containment claims checked:
//   - reserves expire after one webhook.reservationTTL, so the denial is bounded;
//   - no pod is created, though each verdict records an Event against the decoded pod,
//     only in that pod's own namespace;
//   - matching is re-checked per namespace, so a review for an untargeted namespace is
//     allowed with no patch.
//
// It says nothing about port reachability: raxAdmit uses the privileged pods/proxy path.
// The ledger is keyed by node, so the blast radius is the shared node by design.
// cpu_pinning_webhook_reservations_live only republishes on a store change, so both
// ends of the measured window are pinned to an admission (see raxWarmGauges).
// Gated behind E2E_FORGED_ADMISSION: it waits out two reservation TTLs. Helpers local
// to this file are prefixed "fab".
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// fabGate opts this test in.
	fabGate = "E2E_FORGED_ADMISSION"

	// One policy targets both namespaces, so they differ only by name.
	fabAttackerNS = "cpu-pinning-e2e-forged-attacker"
	fabVictimNS   = "cpu-pinning-e2e-forged-victim"

	// fabOutsideNS must be untargeted by every policy; policyTargetsNamespace checks it.
	fabOutsideNS = "default"

	// fabEventWait bounds the wait for the burst's Event; the recorder batches.
	fabEventWait = 60 * time.Second
)

// fabPatchPrefixes are the only pod paths a pinning patch may touch. Strict on
// purpose: a new mutation must go red here and be reviewed before it is added.
var fabPatchPrefixes = []string{"/metadata/annotations", "/spec/initContainers", "/spec/affinity", "/spec/volumes"}

// fabCPUIDTokens are the shapes of a leaked CPU-id prediction, matched lowercased. Bare
// "cpu" is absent because the init container declares cpu requests.
var fabCPUIDTokens = []string{"cpuset", "cpus"}

// TestForgedAdmissionBurstContainedByReservationTTL proves a forged burst's reservations
// drain within one TTL, other tenants are decided normally, and answers leak nothing.
func TestForgedAdmissionBurstContainedByReservationTTL(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(fabGate) == "" {
		skipNotSelected(at, "set %s=1 to run the forged-AdmissionReview burst against the reservation ledger; it waits out two full "+
			"webhook.reservationTTL periods and needs a real NRI cluster", fabGate)
	}
	c := newClient(at)                 // skips cleanly when no kubeconfig
	node := balloonsEnabledNode(at, c) // skips cleanly when no node is provisioned

	cs, opNS := fabManager(at)
	ctx := context.Background()
	replica := leaderPodNameAfterHandover(at, cs, opNS) // the authority: its store is the fleet ledger

	scrapeMetricsPerReplicaOrSkip(at, "the manager /metrics endpoints are not reachable via the apiserver pod proxy, so the ledger cannot be "+
		"watched filling and draining. Check metrics.enabled and E2E_METRICS_PORT/E2E_METRICS_SCHEME (see docs/internal/e2e-internals.md)")
	// Checked up front: if a policy targets fabOutsideNS, a patched answer there is the
	// policy, not the forgery widening the match set.
	if policyTargetsNamespace(fabOutsideNS) {
		skipNoConfig(at, "%q is not a provably unmatched namespace on this cluster (a policy targets it by name, glob or namespaceSelector, or "+
			"targeting could not be read), so the namespace-scoping half of this test cannot be judged", fabOutsideNS)
	}

	perNUMA, numa := snPerNUMACapacity(at, c, node)
	hostname := snNodeHostname(at, c, node)
	// A quarter-NUMA pod per review, enough to fill every slot plus two. Whole-NUMA pods
	// would leave one slot per NUMA, which the reserved pool can strand.
	perPod := max(1, perNUMA/4)
	wave := (perNUMA/perPod)*numa + 2
	ttl := raxReservationTTL(at, cs, opNS)
	at.Logf("node %s: %d NUMA node(s) x %d span-1 cores; forging %d AdmissionReview(s) x %d cores at replica %s, reservation TTL %s",
		node, numa, perNUMA, wave, perPod, replica, ttl)

	const containerName = "vllm"
	fabProvision(at, c, containerName, fabAttackerNS, fabVictimNS)
	raxDrainLedger(at, cs, opNS, node)

	// Republish the live-reservation series before reading the baseline.
	if !raxWarmGauges(ctx, at, cs, opNS, fabAttackerNS, node, replica) {
		skipNoConfig(at, "inconclusive: replica %s did not pin a 1-core warm-up pod, so neither a fresh baseline for %s nor a reachability "+
			"check for the direct admissions can be established on %s", replica, raLiveMetric, node)
	}
	mark := scrapeMetricsPerReplicaOrSkip(at, "the manager /metrics endpoints stopped serving between the precondition scrape and the burst; "+
		"the ledger proof needs a baseline taken next to it")
	baseline := raLive(mark[replica], node)

	forged := raxWave(ctx, cs, opNS, replica, fabAttackerNS, node, "forged", wave, perPod)
	burst := raxSummarise(forged)
	// Both probes are decided while the ledger is full.
	victim := raxAdmit(ctx, cs, opNS, replica, fabPod("victim-during", fabVictimNS, node, perPod))
	outside := raxAdmit(ctx, cs, opNS, replica, fabPod("outside-during", fabOutsideNS, node, perPod))

	after := scrapeMetricsPerReplicaOrSkip(at, "inconclusive: the manager /metrics endpoints stopped serving mid-run; the ledger proof needs "+
		"both scrapes")
	booked := raLive(after[replica], node) - baseline
	at.Logf("forged burst at %s: %d/%d answered with a pin, %d refused on capacity, %d unaccountable, %d allowed unpinned, %d unexpected; "+
		"%s{node=%q} rose by %v from a baseline of %v", replica, len(burst.admitted), wave, burst.capacity, len(burst.unaccountable),
		len(burst.unpinned), len(burst.unexpected), raLiveMetric, node, booked, baseline)

	if len(burst.unaccountable) > 0 {
		skipNoConfig(at, "inconclusive: %d forged review(s) were denied as unaccountable (%v) — the replica could not reach a reservation "+
			"authority at all, so nothing was booked and there is no ledger to watch drain", len(burst.unaccountable), burst.unaccountable)
	}
	if len(burst.admitted) == 0 {
		skipNoConfig(at, "inconclusive: the forged burst booked nothing on %s, so the attack never landed. The node has no free per-NUMA room "+
			"for a %d-core span-1 pod — check for leftover pinned pods or outstanding reservations from an earlier test (they expire one "+
			"webhook.reservationTTL after admission)", node, perPod)
	}
	if burst.capacity == 0 {
		skipNoConfig(at, "inconclusive: all %d forged reviews were answered with a pin and NONE was refused, so the ledger was never filled and "+
			"neither the denial nor the recovery below means anything. The node has more room than %d x %d cores — raise the wave or drain the node",
			wave, wave, perPod)
	}

	if len(burst.unexpected) > 0 {
		at.Errorf("%d forged review(s) got a verdict that is neither a pin nor a capacity/unaccountable denial: %v", len(burst.unexpected), burst.unexpected)
	}
	// No apiserver here, so failurePolicy cannot be why an allow carries no balloon.
	if len(burst.unpinned) > 0 {
		at.Errorf("%d forged review(s) were allowed without a balloon annotation (%v) — those reviews reserved nothing, so the counts here are "+
			"not the room the attack actually took", len(burst.unpinned), burst.unpinned)
	}
	if booked < float64(len(burst.admitted)) {
		at.Errorf("%s{node=%q} rose by %v but %d forged review(s) were answered with a pin — a reserve that reached no store is room handed out "+
			"with nothing accounting for it, and nothing for the TTL to reclaim", raLiveMetric, node, booked, len(burst.admitted))
	}

	fabAssertNoPods(at, c, fabAttackerNS, "the forged reviews were answered")
	fabAssertNoPods(at, c, fabVictimNS, "the forged reviews were answered")

	// Audit against every pod submitted, not one name.
	answers := append(append([]raxDecision{}, forged...), victim, outside)

	// Each verdict records an Event against the decoded pod with no existence check; the
	// preconditions guarantee at least one capacity denial to look for.
	var deniedPod string
	for _, d := range forged {
		if d.deniedOnCapacity() {
			deniedPod = d.pod
			break
		}
	}
	if !waitForWarningEvent(at, c, fabAttackerNS, deniedPod, ccReasonDenied, "", fabEventWait, nil) {
		at.Errorf("no %s Event for the forged pod %s appeared in %s within %s — the burst's Events are the one durable trace a forged review "+
			"leaves, and the threat model says so; if the webhook has stopped recording them the claim needs revisiting rather than the test "+
			"quietly passing", ccReasonDenied, deniedPod, fabAttackerNS, fabEventWait)
	}
	// Events must land only in the named pod's namespace. Unfiltered by type, since a
	// pinned forgery records a Normal Pinned event.
	fabAssertNoEventsFor(at, c, fabVictimNS, fabOtherNames(victim, answers),
		"an Event must be recorded in the namespace of the pod it refers to, or a forger picks which tenant's namespace it writes into")
	// An untargeted namespace never reaches a recorder, so even the probe's own pod is forbidden.
	fabAssertNoEventsFor(at, c, fabOutsideNS, append(fabOtherNames(outside, answers), outside.pod),
		"an unmatched pod is answered without a verdict, so a forger must not be able to leave a trace in a namespace outside the policies' reach")

	switch {
	case victim.admitted():
		at.Logf("the other tenant's pod in %s was admitted into %s while the attacker's ledger was full — the node still had room to spare",
			fabVictimNS, victim.balloon)
	case victim.deniedOnCapacity():
		at.Logf("the other tenant's pod in %s was refused for want of capacity on %s while the ledger was full — the shared node's capacity is "+
			"the blast radius, which the drain below bounds to one TTL", fabVictimNS, node)
	case victim.err != nil:
		at.Errorf("no verdict for the other tenant's pod in %s during the burst: %v", fabVictimNS, victim.err)
	default:
		at.Errorf("the other tenant's pod in %s was answered %q (allowed=%v balloon=%q) during the burst — a pod in another namespace must be "+
			"decided on its own merits: pinned, or refused for want of capacity on the node it shares, and nothing else",
			fabVictimNS, victim.message, victim.allowed, victim.balloon)
	}

	// An allow with a mutation would mean the forgery widened the match set.
	outsidePatch := fabPatch(at, outside)
	switch {
	case outside.err != nil:
		at.Errorf("no verdict for the %s-namespace review: %v — the namespace-scoping probe needs one", fabOutsideNS, outside.err)
	case !outside.allowed:
		at.Errorf("the review for a pod in %s, which no policy targets, was DENIED (%q) — an untargeted namespace must be allowed untouched, "+
			"never blocked by a decision meant for another tenant", fabOutsideNS, outside.message)
	case len(outsidePatch) > 0:
		at.Errorf("the review for a pod in %s, which no policy targets, came back with a patch (%s) — a forged review must not widen the set of "+
			"namespaces the webhook mutates", fabOutsideNS, raxTruncate(string(outsidePatch)))
	default:
		at.Logf("a review for a pod in %s, which no policy targets, was allowed with no patch: matching stayed namespace-scoped", fabOutsideNS)
	}

	// Each answer is audited against every other pod, probes included, so a forged answer
	// carrying the other tenant's pod name is caught.
	for _, d := range forged {
		fabAuditAnswer(at, d, fabAttackerNS, fabOtherNames(d, answers))
	}
	fabAuditAnswer(at, victim, fabVictimNS, fabOtherNames(victim, answers))
	fabAuditAnswer(at, outside, fabOutsideNS, fabOtherNames(outside, answers))

	// Nothing releases a reserve early: one TTL from now all of the burst's have expired.
	raxDrainLedger(at, cs, opNS, node)

	// A real pod, so recovery means the runtime pins it; its admission also republishes the gauge.
	recovered := raxRealWave(t, c, fabVictimNS, hostname, node, "fabrec", 1, perPod, containerName)
	if len(recovered.admitted) == 0 {
		at.Errorf("no pod was admitted in %s a full reservation TTL (%s) after the forged burst (%d refused on capacity, %d unaccountable, "+
			"%d allowed unpinned, %d unexpected) — the burst's reserves outlived their TTL, which turns a bounded denial into a permanent one",
			fabVictimNS, ttl, recovered.capacity, len(recovered.unaccountable), len(recovered.unpinned), len(recovered.unexpected))
		return
	}

	// Scrape before the ground-truth wait, which can outlast a TTL.
	final := scrapeMetricsPerReplicaOrSkip(at, "inconclusive: the manager /metrics endpoints stopped serving before the return-to-baseline scrape")
	// Equality, not an upper bound: both ends hold one fresh reservation, so below the
	// baseline is a defect too.
	if live := raLive(final[replica], node); live != baseline {
		at.Errorf("%s{node=%q} is %v one reservation TTL (%s) after the forged burst, against a baseline of %v. Above the baseline the ledger did "+
			"not give the booked room back and the attack is not bounded by the TTL after all (the burst booked %d reserve(s), so a value that far "+
			"above it is the attack's; one above it is another admission on this node during the wait, and the run should be repeated on a quiet "+
			"node). Below it the recovery pod was admitted without a reservation reaching the authority's store, which is room handed out with "+
			"nothing accounting for it", raLiveMetric, node, live, ttl, baseline, len(burst.admitted))
	} else {
		at.Logf("contained: %s{node=%q} is back at its baseline of %v and pod %v in %s was admitted — the forged burst cost one reservation TTL "+
			"(%s) of denial, not a permanent one", raLiveMetric, node, live, recovered.admitted, fabVictimNS, ttl)
	}

	// Ground truth last, since it can outrun the TTL.
	raxAssertPinned(t, c, node, fabVictimNS, containerName, recovered.admitted, perPod)
}

// fabManager returns a clientset and the operator namespace; any replica count works.
func fabManager(t testing.TB) (kubernetes.Interface, string) {
	t.Helper()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		skipNoCapability(t, "no kubeconfig available: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build clientset: %v", err)
	}
	return cs, env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system")
}

// fabPod is a pre-scheduled span-1 pod; spec.nodeName makes the gate read live NRT free
// room for that node.
func fabPod(name, ns, node string, cores int) *corev1.Pod {
	p := vllmPod("e2e-fab-"+name, ns, strconv.Itoa(cores), strconv.Itoa(cores), 1 /*TP => span 1*/)
	p.Spec.NodeName = node
	return p
}

// fabProvision creates the namespaces and one enforce-mode policy covering all of them,
// then waits until the webhook pins in each.
func fabProvision(t testing.TB, c client.Client, containerName string, namespaces ...string) {
	t.Helper()
	ctx := context.Background()
	const policy = "e2e-forged-admission"

	targets := make([]interface{}, 0, len(namespaces))
	for _, ns := range namespaces {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		_ = c.Delete(ctx, nsObj)
		if err := createEventuallyGone(c, nsObj); err != nil {
			t.Fatalf("create namespace %s: %v", ns, err)
		}
		t.Cleanup(func() {
			e2eDelete(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		targets = append(targets, ns)
	}

	// enforce makes the full ledger deny, which is how the attack is observed.
	bpp := newUnstructured(bppGVK)
	bpp.SetName(policy)
	bpp.Object["spec"] = map[string]interface{}{
		"match":         map[string]interface{}{"byContainerName": []interface{}{containerName}},
		"namespaces":    targets,
		"intent":        map[string]interface{}{"span": map[string]interface{}{"default": int64(1)}},
		"capacityCheck": map[string]interface{}{"mode": "enforce"},
	}
	_ = c.Delete(ctx, bpp)
	if err := createEventuallyGone(c, bpp); err != nil {
		t.Fatalf("create BalloonPinningPolicy %s: %v", policy, err)
	}
	t.Cleanup(func() {
		p := newUnstructured(bppGVK)
		p.SetName(policy)
		e2eDelete(t, c, p)
	})

	for _, ns := range namespaces {
		if !waitForWebhookNamespace(t, c, ns, containerName) {
			skipNoConfig(t, "webhook never began pinning pods in %q (policy reconcile / selector patch did not take effect), so a tenant that "+
				"cannot pin in the first place would look identical to one the attack denied", ns)
		}
	}
}

// fabAssertNoPods reports any live pod in ns.
func fabAssertNoPods(t testing.TB, c client.Client, ns, when string) {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(ns)); err != nil {
		t.Errorf("list pods in %s after %s: %v — whether the forged reviews created anything cannot be checked", ns, when, err)
		return
	}
	// Skip terminating pods, such as the provisioning canary.
	names := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			names = append(names, pods.Items[i].Name)
		}
	}
	if len(names) == 0 {
		return
	}
	t.Errorf("namespace %s holds %d pod(s) after %s (%v) — a forged AdmissionReview is answered to its caller and must create nothing",
		ns, len(names), when, strings.Join(names, ", "))
}

// fabAssertNoEventsFor reports every Event in ns recorded against any of pods. One read
// is enough: the caller has already waited for the burst's Events to flush.
func fabAssertNoEventsFor(t testing.TB, c client.Client, ns string, pods []string, why string) {
	t.Helper()
	var events corev1.EventList
	if err := c.List(context.Background(), &events, client.InNamespace(ns)); err != nil {
		t.Errorf("list events in %s: %v — whether a forged review left a trace there cannot be checked", ns, err)
		return
	}
	forbidden := make(map[string]bool, len(pods))
	for _, p := range pods {
		forbidden[p] = true
	}
	for i := range events.Items {
		e := &events.Items[i]
		if e.InvolvedObject.Kind != "Pod" || !forbidden[e.InvolvedObject.Name] {
			continue
		}
		t.Errorf("namespace %s holds a %s Event %q against %s, a pod submitted elsewhere in this run (%q) — %s",
			ns, e.Type, e.Reason, e.InvolvedObject.Name, e.Message, why)
	}
}

// fabPatch returns the answer's JSONPatch, or nil; an undecodable body is reported.
func fabPatch(t testing.TB, d raxDecision) []byte {
	t.Helper()
	if d.err != nil || d.raw == "" {
		return nil
	}
	var answer admissionv1.AdmissionReview
	if err := json.Unmarshal([]byte(d.raw), &answer); err != nil || answer.Response == nil {
		t.Errorf("the answer for %s does not decode as an AdmissionReview (%v): %s", d.pod, err, raxTruncate(d.raw))
		return nil
	}
	return answer.Response.Patch
}

// fabOtherNames returns every submitted pod name except d's own.
func fabOtherNames(d raxDecision, all []raxDecision) []string {
	out := make([]string, 0, len(all))
	for _, other := range all {
		if other.pod != d.pod {
			out = append(out, other.pod)
		}
	}
	return out
}

// fabAuditAnswer asserts an answer only patches allowed paths and names no other pod,
// tenant namespace or CPU id.
func fabAuditAnswer(t testing.TB, d raxDecision, ownNS string, otherPods []string) {
	t.Helper()
	if d.err != nil {
		return // no answer to audit; the verdict assertions already report the exchange
	}
	// "default" is too generic a word to forbid.
	forbidden := append([]string{}, otherPods...)
	for _, ns := range []string{fabAttackerNS, fabVictimNS} {
		if ns != ownNS {
			forbidden = append(forbidden, ns)
		}
	}
	for _, name := range forbidden {
		if strings.Contains(d.raw, name) {
			t.Errorf("the answer for %s names %q, which belongs to another pod or tenant — an answer carries a patch for the pod submitted and "+
				"nothing about anyone else: %s", d.pod, name, raxTruncate(d.raw))
		}
	}
	lower := strings.ToLower(d.raw)
	for _, token := range fabCPUIDTokens {
		if strings.Contains(lower, token) {
			t.Errorf("the answer for %s contains %q — the webhook predicts per-NUMA CPU ids to size a reservation and must never answer with "+
				"them; the plugin picks the real ones later: %s", d.pod, token, raxTruncate(d.raw))
		}
	}

	patch := fabPatch(t, d)
	if len(patch) == 0 {
		return
	}
	var ops []struct {
		Op   string `json:"op"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(patch, &ops); err != nil {
		t.Errorf("the patch in the answer for %s does not decode as a JSONPatch (%v): %s", d.pod, err, raxTruncate(string(patch)))
		return
	}
	for _, op := range ops {
		if !fabPathAllowed(op.Path) {
			t.Errorf("the answer for %s patches %q, which is outside the pinning mutations this leak check has reviewed (%v) — either the answer "+
				"carries more than it should, or a new mutation needs reviewing and adding to fabPatchPrefixes",
				d.pod, op.Path, fabPatchPrefixes)
		}
	}
}

// fabPathAllowed matches whole pointer segments, so "/metadata/annotations_evil" fails.
func fabPathAllowed(path string) bool {
	for _, prefix := range fabPatchPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
