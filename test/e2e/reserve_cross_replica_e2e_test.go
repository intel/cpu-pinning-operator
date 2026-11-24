// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// raxMutatePath is the pod mutator path cmd/main.go registers; addressing a replica directly means naming it here.
	raxMutatePath = "mutate-v1-pod"

	// raxAdmissionsMetric counts every admission a replica decided, so a denied wave still shows as served.
	raxAdmissionsMetric = "cpu_pinning_webhook_admission_seconds_count"

	// raxBalloonPrefix prefixes every node balloon; an allow whose patch names none was not pinned.
	raxBalloonPrefix = "balloon-"

	// raxRPC bounds one direct admission call, well above the manager's 2x2s reserve RPC budget.
	raxRPC = 30 * time.Second
)

// raxWebhookPort is the manager's admission port in the pod; the default webhook pod port is 9443.
func raxWebhookPort() string { return env("E2E_WEBHOOK_POD_PORT", "9443") }

// raxDecision is one replica's verdict on one AdmissionReview; err is set only when the exchange failed.
type raxDecision struct {
	pod     string
	replica string
	allowed bool
	message string
	balloon string
	// raw is the body exactly as the replica sent it, for callers auditing what an answer carries.
	raw string
	err error
}

// admitted reports whether the replica allowed AND pinned the pod; an allow without a balloon reserves nothing.
func (d raxDecision) admitted() bool { return d.err == nil && d.allowed && d.balloon != "" }

// deniedOnCapacity reports the enforce-mode capacity denial.
func (d raxDecision) deniedOnCapacity() bool {
	return d.err == nil && !d.allowed && strings.Contains(d.message, ccDenialSubstr)
}

// deniedUnaccountable reports the enforce-mode denial for an unreachable authority.
func (d raxDecision) deniedUnaccountable() bool {
	return d.err == nil && !d.allowed && strings.Contains(d.message, raUnaccountableSubstr)
}

// raxAdmit POSTs one CREATE AdmissionReview for pod to the NAMED replica via the apiserver pod proxy and returns its verdict.
// The proxy opens a fresh connection per request, bypassing the Service's single multiplexed apiserver connection that keeps
// a burst on one replica. A fresh UID per call matters: reserveKey derives the idempotency key from it.
func raxAdmit(ctx context.Context, cs kubernetes.Interface, opNS, replica string, pod *corev1.Pod) raxDecision {
	out := raxDecision{pod: pod.Name, replica: replica}

	// The decoder needs apiVersion/kind; a bare pod body decodes into nothing.
	body := pod.DeepCopy()
	body.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	raw, err := json.Marshal(body)
	if err != nil {
		out.err = fmt.Errorf("marshal pod %s: %w", pod.Name, err)
		return out
	}

	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       uuid.NewUUID(),
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
			Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Operation: admissionv1.Create,
			Namespace: pod.Namespace,
			Name:      pod.Name,
			UserInfo:  authenticationv1.UserInfo{Username: "system:serviceaccount:" + pod.Namespace + ":default"},
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
	reqBody, err := json.Marshal(review)
	if err != nil {
		out.err = fmt.Errorf("marshal AdmissionReview for %s: %w", pod.Name, err)
		return out
	}

	callCtx, cancel := context.WithTimeout(ctx, raxRPC)
	defer cancel()
	respBody, err := cs.CoreV1().RESTClient().Post().
		Namespace(opNS).
		Resource("pods").
		Name(fmt.Sprintf("https:%s:%s", replica, raxWebhookPort())).
		SubResource("proxy").
		Suffix(raxMutatePath).
		SetHeader("Content-Type", "application/json").
		Body(reqBody).
		DoRaw(callCtx)
	if err != nil {
		out.err = fmt.Errorf("POST %s to replica %s: %w", raxMutatePath, replica, err)
		return out
	}

	out.raw = string(respBody)
	var answer admissionv1.AdmissionReview
	if err := json.Unmarshal(respBody, &answer); err != nil {
		out.err = fmt.Errorf("decode the AdmissionReview replica %s answered for %s: %w", replica, pod.Name, err)
		return out
	}
	if answer.Response == nil {
		out.err = fmt.Errorf("replica %s answered %s with no response section: %s", replica, pod.Name, raxTruncate(string(respBody)))
		return out
	}
	out.allowed = answer.Response.Allowed
	if answer.Response.Result != nil {
		out.message = answer.Response.Result.Message
	}
	out.balloon = raxPatchedBalloon(answer.Response.Patch)
	return out
}

// raxTruncate bounds an unexpected body in an error message.
func raxTruncate(s string) string {
	const max = 300
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// raxPatchedBalloon returns the balloon the admission patch stamps, or "". It handles both patch shapes:
// one add per annotation key, or one add of the whole annotations object.
func raxPatchedBalloon(patch []byte) string {
	if len(patch) == 0 {
		return ""
	}
	var ops []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(patch, &ops); err != nil {
		return ""
	}
	for _, op := range ops {
		if !strings.HasPrefix(op.Path, "/metadata/annotations") {
			continue
		}
		var one string
		if err := json.Unmarshal(op.Value, &one); err == nil {
			if strings.HasPrefix(one, raxBalloonPrefix) {
				return one
			}
			continue
		}
		var many map[string]string
		if err := json.Unmarshal(op.Value, &many); err != nil {
			continue
		}
		keys := make([]string, 0, len(many))
		for k := range many {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic pick when a pod is stamped for several containers
		for _, k := range keys {
			if strings.HasPrefix(many[k], raxBalloonPrefix) {
				return many[k]
			}
		}
	}
	return ""
}

// raxWave sends count concurrent CREATE admissions to one replica so the reserves contend in the authority's store.
// Pods are pre-scheduled so the capacity gate reads the node's live NRT free room. No goroutine touches the testing handle.
func raxWave(ctx context.Context, cs kubernetes.Interface, opNS, replica, ns, node, tag string, count, perPod int) []raxDecision {
	out := make([]raxDecision, count)
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := vllmPod(fmt.Sprintf("e2e-rax-%s-%02d", tag, i), ns,
				strconv.Itoa(perPod), strconv.Itoa(perPod), 1 /*TP => span 1*/)
			p.Spec.NodeName = node
			out[i] = raxAdmit(ctx, cs, opNS, replica, p)
		}()
	}
	wg.Wait()
	return out
}

// raxTally summarises a wave's verdicts. unpinned is kept apart from unexpected: through the apiserver,
// failurePolicy: Ignore can legitimately admit a pod unmutated.
type raxTally struct {
	admitted      []string
	balloons      []string
	capacity      int
	unaccountable []string
	unpinned      []string
	unexpected    []string
}

func raxSummarise(ds []raxDecision) raxTally {
	var t raxTally
	for _, d := range ds {
		switch {
		case d.admitted():
			t.admitted = append(t.admitted, d.pod)
			t.balloons = append(t.balloons, d.balloon)
		case d.deniedOnCapacity():
			t.capacity++
		case d.deniedUnaccountable():
			t.unaccountable = append(t.unaccountable, d.pod)
		case d.err != nil:
			t.unexpected = append(t.unexpected, fmt.Sprintf("%s: %v", d.pod, d.err))
		case d.allowed:
			t.unpinned = append(t.unpinned, d.pod)
		default:
			t.unexpected = append(t.unexpected, fmt.Sprintf("%s: denied with %q", d.pod, d.message))
		}
	}
	sort.Strings(t.admitted)
	sort.Strings(t.unpinned)
	return t
}

// raxServed returns the admissions a replica decided; an absent series reads as 0.
func raxServed(body string) float64 { return counterSum(body, raxAdmissionsMetric) }

// TestReserveAuthorityCrossReplicaLedger proves a follower wave is refused once a leader wave filled the single ledger.
// Waves are sent to named replicas via the pod proxy, since one apiserver keeps every admission on one replica.
// With E2E_RESERVE_AUTHORITY_NEGATIVE (reserveAuthority.enabled=false) it proves the opposite: the waves over-admit.
func TestReserveAuthorityCrossReplicaLedger(t *testing.T) {
	at := allureCase(t)
	negative := os.Getenv(raNegativeGate) != ""
	if os.Getenv(raGate) == "" {
		skipNotSelected(at, "set %s=1 to run the cross-replica reserve ledger proof (needs a real NRI cluster "+
			"and at least two Ready manager replicas); add %s=1 to run it as the negative control against an "+
			"install with webhook.reserveAuthority.enabled=false", raGate, raNegativeGate)
	}
	c := newClient(at)                 // skips cleanly when no kubeconfig
	node := balloonsEnabledNode(at, c) // skips cleanly when no node is provisioned

	leader, cs, opNS := raRequireAuthority(at)
	follower := raxFollower(at, cs, opNS, leader)
	perNUMA, numa := snPerNUMACapacity(at, c, node)
	// An upper bound: the reserved pool narrows it further.
	nodeCores := perNUMA * numa

	// A quarter NUMA per pod keeps several slots per NUMA, so the reserved pool cannot strand them all.
	perPod := max(1, perNUMA/4)
	// Enough pods to fill every span-1 slot plus two, so one ledger is saturated.
	wave := (perNUMA/perPod)*numa + 2
	at.Logf("node %s: %d NUMA node(s) x %d span-1 cores; two waves of %d pods x %d cores, leader=%s follower=%s (negative control: %v)",
		node, numa, perNUMA, wave, perPod, leader, follower, negative)

	scrapeMetricsPerReplicaOrSkip(at, "the manager /metrics endpoints did not serve this scrape via the apiserver pod proxy, so a reserve cannot be "+
		"attributed to a replica. Check metrics.enabled and E2E_METRICS_PORT/E2E_METRICS_SCHEME, and the logged per-replica reason "+
		"(see docs/internal/e2e-internals.md)")

	ns, containerName := "cpu-pinning-e2e-reserve-xreplica", "vllm"
	raProvision(at, c, ns, containerName)
	ctx := context.Background()
	raxDrainLedger(at, cs, opNS, node)
	nrtBefore := raxNRTVersion(at, c, node)

	// Republish both live gauges before the baseline (see raxWarmGauges).
	if !raxWarmGauges(ctx, at, cs, opNS, ns, node, leader, follower) {
		skipNoConfig(at, "inconclusive: could not get both replicas to pin a 1-core warm-up pod, so neither a fresh baseline for %s nor a "+
			"reachability check for the direct admissions can be established on %s", raLiveMetric, node)
	}

	// Baseline right before the waves, keeping the delta window as narrow as the burst.
	mark := scrapeMetricsPerReplicaOrSkip(at, "the manager /metrics endpoints stopped serving between the precondition scrape and the waves; "+
		"the ledger proof needs a baseline taken next to the burst")

	waveA := raxSummarise(raxWave(ctx, cs, opNS, leader, ns, node, "a", wave, perPod))
	waveB := raxSummarise(raxWave(ctx, cs, opNS, follower, ns, node, "b", wave, perPod))
	nrtAfter := raxNRTVersion(at, c, node)
	at.Logf("wave A -> leader   %s: %d/%d admitted+pinned, %d denied on capacity, %d unaccountable, %d allowed unpinned, %d unexpected",
		leader, len(waveA.admitted), wave, waveA.capacity, len(waveA.unaccountable), len(waveA.unpinned), len(waveA.unexpected))
	at.Logf("wave B -> follower %s: %d/%d admitted+pinned, %d denied on capacity, %d unaccountable, %d allowed unpinned, %d unexpected",
		follower, len(waveB.admitted), wave, waveB.capacity, len(waveB.unaccountable), len(waveB.unpinned), len(waveB.unexpected))

	after := scrapeMetricsPerReplicaOrSkip(at, "inconclusive: the manager /metrics endpoints stopped serving mid-run; the ledger proof needs both scrapes")
	servedA := raxServed(after[leader]) - raxServed(mark[leader])
	servedB := raxServed(after[follower]) - raxServed(mark[follower])
	roseLeader := raLive(after[leader], node) - raLive(mark[leader], node)
	roseFollower := raLive(after[follower], node) - raLive(mark[follower], node)
	at.Logf("admissions decided during the waves: leader %v, follower %v; %s{node=%q} rose by leader %v, follower %v",
		servedA, servedB, raLiveMetric, node, roseLeader, roseFollower)

	if len(waveA.unaccountable) > 0 {
		skipNoConfig(at, "inconclusive: wave A was denied as unaccountable (%v) — the lease holder could not reserve against its OWN store, "+
			"which is a broken install rather than the accounting under test", waveA.unaccountable)
	}
	if len(waveA.admitted) == 0 {
		skipNoConfig(at, "inconclusive: wave A admitted nothing on %s, so no reserve was taken and wave B has no ledger to collide with. "+
			"The node has no free per-NUMA room for a %d-core span-1 pod — check for leftover pinned pods or outstanding reservations "+
			"from an earlier test (they expire one webhook.reservationTTL after admission)", node, perPod)
	}
	if waveA.capacity == 0 {
		skipNoConfig(at, "inconclusive: wave A admitted all %d pods and NONE was denied, so the ledger was never filled and wave B being "+
			"refused would prove nothing about the authority. The node has more room than %d x %d cores — raise the wave or drain the node",
			len(waveA.admitted), wave, perPod)
	}
	// Each replica must have decided a full wave, or the proxy put requests elsewhere.
	if servedA < float64(wave) || servedB < float64(wave) {
		skipNoConfig(at, "inconclusive: %s decided %v admissions and %s decided %v, but each was sent %d — the direct pod-proxy admissions "+
			"did not all reach the replica they were addressed to, so nothing can be attributed", leader, servedA, follower, servedB, wave)
	}
	if now := leaderPodNameAfterHandover(at, cs, opNS); now != leader {
		skipNoConfig(at, "inconclusive: the leader-election lease moved from %s to %s during the waves, so the two waves were served against "+
			"two different authorities by design rather than by a routing bug", leader, now)
	}

	if len(waveA.unexpected) > 0 {
		at.Errorf("wave A: %d verdict(s) that are neither an admission nor a capacity/unaccountable denial: %v", len(waveA.unexpected), waveA.unexpected)
	}
	if len(waveB.unexpected) > 0 {
		at.Errorf("wave B: %d verdict(s) that are neither an admission nor a capacity/unaccountable denial: %v", len(waveB.unexpected), waveB.unexpected)
	}
	// No apiserver here, so failurePolicy cannot admit unmutated: an allow without a balloon is a defect.
	if unpinned := append(append([]string{}, waveA.unpinned...), waveB.unpinned...); len(unpinned) > 0 {
		at.Errorf("%d pod(s) were allowed without a balloon annotation (%v) — a replica answering a matched pod without pinning it reserves "+
			"nothing, so the wave counts below are not the room that was handed out", len(unpinned), unpinned)
	}
	// Every stamped balloon must exist in the node's policy.
	raxAssertBalloonsExist(at, c, node, append(append([]string{}, waveA.balloons...), waveB.balloons...))

	if negative {
		// The control must over-admit, or these waves cannot tell one ledger from two and the positive run proves nothing.
		handedOut := (len(waveA.admitted) + len(waveB.admitted)) * perPod
		switch {
		case len(waveB.admitted) == 0:
			at.Errorf("negative control did NOT over-admit: wave A took %d pod(s) x %d cores on %s and wave B, served by the second replica "+
				"%s, was refused all %d — with webhook.reserveAuthority.enabled=false the replicas keep separate stores and the second "+
				"one should have handed the same per-NUMA room out again. Either the authority is still wired (check the manager args for %s) "+
				"or these waves cannot distinguish one ledger from two, which invalidates the positive run as well",
				len(waveA.admitted), perPod, node, follower, wave, raAuthorityFlag)
		case handedOut <= nodeCores:
			// The two ledgers together must exceed what the node can pin.
			at.Errorf("negative control admitted through both replicas (%d + %d pod(s) x %d cores) but %d cores still fits inside the %d "+
				"cores %s pins, so no over-admission was demonstrated — raise the wave or drain the node before trusting this control",
				len(waveA.admitted), len(waveB.admitted), perPod, handedOut, nodeCores, node)
		default:
			at.Logf("negative control CONFIRMED: %d pod(s) x %d cores admitted through %s and a further %d x %d through %s — %d cores handed "+
				"out on a node that pins at most %d, because each replica accounted against its own store",
				len(waveA.admitted), perPod, leader, len(waveB.admitted), perPod, follower, handedOut, nodeCores)
		}
		if roseFollower <= 0 {
			at.Errorf("negative control: %s{node=%q} did not rise on %s even though it admitted %d pod(s) — a second store that books "+
				"nothing is not the second ledger this control is meant to demonstrate",
				raLiveMetric, node, follower, len(waveB.admitted))
		}
		return
	}

	// With the authority on, wave B must be refused outright. Only an NRT change could legitimately free room.
	if len(waveB.admitted) > 0 {
		detail := "the node's NodeResourceTopology did not change between the waves, so no room was freed"
		if nrtAfter != nrtBefore {
			detail = fmt.Sprintf("NOTE the node's NodeResourceTopology changed during the run (resourceVersion %s -> %s), so some room may "+
				"genuinely have been freed; re-run on a quiet node before treating this as the over-admission itself", nrtBefore, nrtAfter)
		}
		at.Errorf("OVER-ADMISSION: wave A took %d pod(s) x %d cores through %s and wave B took a further %d through the follower %s, "+
			"%d cores in total on a node that pins at most %d — the follower's reserve was not accounted against the lease holder's store. %s",
			len(waveA.admitted), perPod, leader, len(waveB.admitted), follower,
			(len(waveA.admitted)+len(waveB.admitted))*perPod, nodeCores, detail)
	}
	if waveB.capacity == 0 {
		at.Errorf("the follower %s refused none of wave B's %d pods on capacity (%d unaccountable, %d unexpected) — under "+
			"capacityCheck.mode: enforce a reserve the authority cannot commit must come back as a capacity denial",
			follower, wave, len(waveB.unaccountable), len(waveB.unexpected))
	}
	if roseFollower > 0 {
		at.Errorf("%s{node=%q} rose by %v on the follower %s — it booked reserves into its OWN store instead of forwarding them to the "+
			"lease holder, which is the second ledger the authority exists to remove", raLiveMetric, node, roseFollower, follower)
	}
	// Sound only because the drain and warm-up leave nothing in the window to expire or read stale.
	if roseLeader < float64(len(waveA.admitted)) {
		at.Errorf("%s{node=%q} rose by %v on the lease holder %s but %d pod(s) were admitted against it — a reserve that reached no store is "+
			"room handed out with nothing accounting for it",
			raLiveMetric, node, roseLeader, leader, len(waveA.admitted))
	}
}

// raxReservationTTL returns the manager's --reservation-ttl, defaulting to 90s.
func raxReservationTTL(t testing.TB, cs kubernetes.Interface, opNS string) time.Duration {
	t.Helper()
	const managerDefault = 90 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dep, err := cs.AppsV1().Deployments(opNS).Get(ctx, env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment), metav1.GetOptions{})
	if err != nil {
		return managerDefault
	}
	const flag = "--reservation-ttl="
	for _, ctr := range dep.Spec.Template.Spec.Containers {
		for _, arg := range ctr.Args {
			if !strings.HasPrefix(arg, flag) {
				continue
			}
			if d, err := time.ParseDuration(strings.TrimPrefix(arg, flag)); err == nil && d > 0 {
				return d
			}
		}
	}
	return managerDefault
}

// raxDrainLedger waits out any reservation the node's ledger still holds; nothing releases a reserve early.
// It waits one TTL by the clock rather than probing, since a probe admission consumes a slot. The live gauge
// is only ever stale positive, so a zero is safe to trust. This file is named to sort after
// reserve_authority_e2e_test.go so the cheaper apiserver-path burst does not pay the wait.
func raxDrainLedger(t testing.TB, cs kubernetes.Interface, opNS, node string) {
	t.Helper()
	ttl := raxReservationTTL(t, cs, opNS) + 10*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	leader, why := leaderPodOnce(ctx, cs, opNS, env("E2E_LEADER_LEASE", operatorDeployment))
	cancel()
	// A refusal and an unreachable endpoint both mean: wait the full TTL.
	byReplica, _ := scrapeMetricsPerReplicaOutcome(t) // scrape outcome: intentional
	if why = raxLedgerHeld(byReplica, leader, node, why); why == "" {
		return
	}
	t.Logf("%s; waiting %s, one full reservation TTL, since nothing releases a reserve early and these waves need the node's whole ledger",
		why, ttl)
	time.Sleep(ttl)
}

// raxLedgerHeld is "" only when the lease holder's, the authority's, exposition was scraped
// and every scraped replica shows nothing live for node; else why the ledger may still hold
// room. Followers' zeros alone prove nothing: their stores are not the fleet ledger.
func raxLedgerHeld(byReplica map[string]string, leader, node, noLeader string) string {
	if noLeader != "" {
		return "no lease holder to read the authority ledger from (" + noLeader + ")"
	}
	if _, ok := byReplica[leader]; !ok {
		return fmt.Sprintf("the lease holder %s served no %s, so %s's ledger is unknown", leader, raLiveMetric, node)
	}
	held := 0.0
	for _, body := range byReplica {
		held += raLive(body, node)
	}
	if held > 0 {
		return fmt.Sprintf("the fleet still holds %v reservation(s) for %s", held, node)
	}
	return ""
}

// raxWarmGauges pins one 1-core pod on EACH replica so every live-reservation gauge is republished before the baseline,
// and reports whether all did. The gauge is published on change and never recomputed at scrape time, so an untouched
// store keeps serving an expired count.
func raxWarmGauges(ctx context.Context, t testing.TB, cs kubernetes.Interface, opNS, ns, node string, replicas ...string) bool {
	t.Helper()
	for i, replica := range replicas {
		p := vllmPod(fmt.Sprintf("e2e-rax-warm-%02d", i), ns, "1", "1", 1 /*TP => span 1*/)
		p.Spec.NodeName = node
		if d := raxAdmit(ctx, cs, opNS, replica, p); !d.admitted() {
			t.Logf("replica %s did not pin a 1-core warm-up pod (allowed=%v balloon=%q message=%q err=%v), so its live-reservation series "+
				"cannot be refreshed before the baseline", replica, d.allowed, d.balloon, d.message, d.err)
			return false
		}
	}
	return true
}

// raxFollower returns a Ready non-leader manager replica, skipping when there is none.
func raxFollower(t testing.TB, cs kubernetes.Interface, opNS, leader string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	selector := managerPodSelector(ctx, cs, opNS)
	pods, err := cs.CoreV1().Pods(opNS).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		skipNoConfig(t, "manager pods not listable (ns %s selector %q): %v", opNS, selector, err)
	}
	var ready []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Name != leader && pod.DeletionTimestamp == nil && podReady(pod) {
			ready = append(ready, pod.Name)
		}
	}
	if len(ready) == 0 {
		skipNoConfig(t, "no Ready manager replica other than the lease holder %s in ns %s (selector %q); the cross-replica proof needs a "+
			"follower to forward from (--set replicaCount=2)", leader, opNS, selector)
	}
	sort.Strings(ready)
	return ready[0]
}

// TestReserveAuthorityFailoverAcrossBurst proves a lease successor does not re-issue room the deposed leader gave away, with real pods.
// The handover is between two waves: a burst finishes in milliseconds, re-election takes seconds.
func TestReserveAuthorityFailoverAcrossBurst(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(raGate) == "" {
		skipNotSelected(at, "set %s=1 to run the leader-failover burst (deletes the lease-holding manager replica; needs a real NRI "+
			"cluster and at least two Ready manager replicas)", raGate)
	}
	c := newClient(at)
	node := balloonsEnabledNode(at, c)

	leader, cs, opNS := raRequireAuthority(at)
	perNUMA, numa := snPerNUMACapacity(at, c, node)
	nodeCores := perNUMA * numa
	hostname := snNodeHostname(at, c, node)
	perPod := max(1, perNUMA/4)
	// Half the slots per wave: the first leaves room to over-admit into, together they exceed the node.
	wave := max(2, (perNUMA/perPod)*numa/2+1)
	at.Logf("node %s: %d NUMA node(s) x %d span-1 cores; two waves of %d real pods x %d cores with a lease handover in between, leader=%s",
		node, numa, perNUMA, wave, perPod, leader)

	ns, containerName := "cpu-pinning-e2e-reserve-failover", "vllm"
	raProvision(at, c, ns, containerName)
	raxDrainLedger(at, cs, opNS, node)

	first := raxRealWave(t, c, ns, hostname, node, "f1", wave, perPod, containerName)
	at.Logf("wave 1 (leader %s): %d/%d admitted+pinned, %d denied on capacity, %d unaccountable, %d allowed unpinned, %d unexpected",
		leader, len(first.admitted), wave, first.capacity, len(first.unaccountable), len(first.unpinned), len(first.unexpected))

	successor := raxForceFailover(at, cs, opNS, leader)

	second := raxRealWave(t, c, ns, hostname, node, "f2", wave, perPod, containerName)
	at.Logf("wave 2 (successor %s): %d/%d admitted+pinned, %d denied on capacity, %d unaccountable, %d allowed unpinned, %d unexpected",
		successor, len(second.admitted), wave, second.capacity, len(second.unaccountable), len(second.unpinned), len(second.unexpected))
	// failurePolicy: Ignore can admit unmutated during a handover; such pods hold no balloon, so only report them.
	if unpinned := append(append([]string{}, first.unpinned...), second.unpinned...); len(unpinned) > 0 {
		at.Logf("%d pod(s) were admitted unmutated across the handover (%v) — the mutating webhook's failurePolicy: Ignore doing its job; "+
			"they hold no balloon and are excluded from the core count below", len(unpinned), unpinned)
	}

	admitted := append(append([]string{}, first.admitted...), second.admitted...)
	admittedCores := len(admitted) * perPod

	if successor == leader {
		skipNoConfig(at, "inconclusive: the leader-election lease came back to %s, so no handover was taken and wave 2 was served by the same "+
			"authority as wave 1", leader)
	}
	if len(first.admitted) == 0 {
		skipNoConfig(at, "inconclusive: wave 1 admitted nothing, so the successor inherited no reservation to respect. The node has no room for a "+
			"%d-core span-1 pod — check for leftover pinned pods or reservations from an earlier test", perPod)
	}
	if first.capacity+second.capacity == 0 {
		skipNoConfig(at, "inconclusive: all %d pods across both waves were admitted and none was denied, so the node was never contended and a "+
			"successor that over-admits would look identical to one that does not. Raise the waves or drain the node", 2*wave)
	}

	for _, u := range append(append([]string{}, first.unexpected...), second.unexpected...) {
		at.Errorf("verdict that is neither an admission nor a capacity/unaccountable denial: %s", u)
	}
	if len(second.unaccountable) > 0 {
		at.Errorf("%d pod(s) in wave 2 were denied because the authority was unreachable (%v) — after a handover the successor must BE the "+
			"authority, so an unaccountable denial means the fleet had no reachable one at all", len(second.unaccountable), second.unaccountable)
	}
	if admittedCores > nodeCores {
		at.Errorf("OVER-ADMISSION ACROSS THE HANDOVER: %d pod(s) x %d cores = %d admitted onto %s, which pins at most %d — the successor gave "+
			"out room wave 1 had already taken (%d pods before the handover, %d after)",
			len(admitted), perPod, admittedCores, node, nodeCores, len(first.admitted), len(second.admitted))
	}
	// Admitted pods must be pinned to their requested width; pods still in Init are not counted.
	raxAssertPinned(t, c, node, ns, containerName, admitted, perPod)
	if stuck := raCreateContainerErrors(at, c, ns, admitted); len(stuck) > 0 {
		at.Errorf("pods %v hit CreateContainerError within %s — an admitted pod's cpuset could not be applied, which is the failure a single "+
			"reservation ledger exists to prevent", stuck, raWatch)
	}
}

// TestReserveAuthorityUnreachableDenies proves a follower under enforce denies when the reserve port is severed, and admits again once restored.
// It removes only the NetworkPolicy rule for the reserve port; the before/after admits make the middle denial attributable.
func TestReserveAuthorityUnreachableDenies(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(raGate) == "" {
		skipNotSelected(at, "set %s=1 to run the unreachable-authority denial (removes and restores one ingress rule of the operator's "+
			"NetworkPolicy; needs a real NRI cluster, at least two Ready manager replicas and a CNI that enforces NetworkPolicy)", raGate)
	}
	c := newClient(at)
	node := balloonsEnabledNode(at, c)

	leader, cs, opNS := raRequireAuthority(at)
	follower := raxFollower(at, cs, opNS, leader)
	ns, containerName := "cpu-pinning-e2e-reserve-unreachable", "vllm"
	raProvision(at, c, ns, containerName)

	ctx := context.Background()
	// One core: a capacity denial then means the node is genuinely full. A fresh name per attempt is its own reserve.
	probe := func(tag string) raxDecision {
		p := vllmPod("e2e-rax-unreach-"+tag, ns, "1", "1", 1 /*TP => span 1*/)
		p.Spec.NodeName = node
		return raxAdmit(ctx, cs, opNS, follower, p)
	}

	baseline := probe("before")
	if baseline.err != nil {
		skipNoConfig(at, "could not get a verdict out of the follower %s before severing anything: %v", follower, baseline.err)
	}
	if !baseline.admitted() {
		skipNoConfig(at, "inconclusive: the follower %s did not admit a 1-core probe pod before the reserve port was severed (allowed=%v "+
			"balloon=%q message=%q) — with no admitted baseline, a denial after severing proves nothing. A capacity denial here means the "+
			"node is full: wait out webhook.reservationTTL after an earlier burst, or drain it",
			follower, baseline.allowed, baseline.balloon, baseline.message)
	}
	at.Logf("baseline: follower %s admitted the probe into %s", follower, baseline.balloon)

	// The follower's gauge rising means it booked locally (the defect); the leader's means the rule was never enforced.
	beforeSever := scrapeMetricsPerReplicaOrSkip(at, "the manager /metrics endpoints did not serve this scrape via the apiserver pod proxy, so an "+
		"admission while the authority is severed cannot be attributed to a store. Check metrics.enabled and "+
		"E2E_METRICS_PORT/E2E_METRICS_SCHEME")

	raxSeverReserve(at, c, opNS)

	// Polled: the CNI needs a moment to program the rule change.
	var severed raxDecision
	deadline := time.Now().Add(90 * time.Second)
	for attempt := 0; ; attempt++ {
		severed = probe(fmt.Sprintf("severed-%d", attempt))
		if severed.deniedUnaccountable() || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	afterSever := scrapeMetricsPerReplicaOrSkip(at, "inconclusive: the manager /metrics endpoints stopped serving while the authority was severed; "+
		"attributing the verdict needs both scrapes")
	bookedFollower := raLive(afterSever[follower], node) - raLive(beforeSever[follower], node)
	bookedLeader := raLive(afterSever[leader], node) - raLive(beforeSever[leader], node)

	switch {
	case severed.deniedUnaccountable():
		at.Logf("severed: follower %s denied the probe — %q", follower, severed.message)
	case severed.err != nil:
		skipNoConfig(at, "inconclusive: no verdict from the follower %s while the reserve port was severed: %v", follower, severed.err)
	case severed.admitted() && bookedFollower <= 0 && bookedLeader > 0:
		// The reserve still reached the authority, so the CNI is not enforcing the NetworkPolicy.
		skipNoCapability(at, "inconclusive: with the %q ingress rule removed the follower %s still admitted the probe AND the lease holder's "+
			"%s{node=%q} rose by %v — the reserve reached the authority anyway, so this cluster is not enforcing the NetworkPolicy and the "+
			"port cannot be severed this way", raxReservePortName, follower, raLiveMetric, node, bookedLeader)
	case severed.admitted():
		// Answered from its own store: the second ledger.
		at.Errorf("the follower %s ADMITTED a pod into %s while the authority was unreachable (its own %s{node=%q} rose by %v, the lease "+
			"holder's by %v) — under capacityCheck.mode: enforce it must deny (%q) rather than fall back to its own reservation store, or two "+
			"replicas account the same room again",
			follower, severed.balloon, raLiveMetric, node, bookedFollower, bookedLeader, raUnaccountableSubstr)
	default:
		at.Errorf("with the authority unreachable the follower %s answered %q (allowed=%v) instead of either admitting or the unaccountable "+
			"denial — enforce mode has exactly two defensible answers here", follower, severed.message, severed.allowed)
	}

	raxRestoreReserve(at, c, opNS)

	// Self-healing once the authority is back is the argument for denying above.
	var healed raxDecision
	deadline = time.Now().Add(90 * time.Second)
	for attempt := 0; ; attempt++ {
		healed = probe(fmt.Sprintf("healed-%d", attempt))
		if healed.admitted() || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !healed.admitted() {
		at.Errorf("the follower %s did not admit the probe again within 90s of restoring the reserve port's ingress rule (allowed=%v "+
			"balloon=%q message=%q err=%v) — a denial that needs a human to clear is the outcome enforce mode is meant to avoid",
			follower, healed.allowed, healed.balloon, healed.message, healed.err)
	} else {
		at.Logf("healed: follower %s admitted the probe into %s again with no operator intervention", follower, healed.balloon)
	}
}

// raxReservePortName is the chart's name for the reserve container port, used by its NetworkPolicy rule.
const raxReservePortName = "reserve"

// raxSeverReserve removes the reserve port's ingress rule from the operator NetworkPolicy, restoring it in t.Cleanup too.
func raxSeverReserve(t testing.TB, c client.Client, opNS string) {
	t.Helper()
	ctx := context.Background()
	key := client.ObjectKey{Name: env("E2E_NETWORK_POLICY", operatorDeployment), Namespace: opNS}

	var np networkingv1.NetworkPolicy
	if err := c.Get(ctx, key, &np); err != nil {
		skipNoConfig(t, "NetworkPolicy %s/%s not readable (%v) — the reserve port cannot be severed without it; install with "+
			"--set networkPolicy.enabled=true or point E2E_NETWORK_POLICY at the policy that guards the manager", opNS, key.Name, err)
	}
	original := make([]networkingv1.NetworkPolicyIngressRule, len(np.Spec.Ingress))
	copy(original, np.Spec.Ingress)
	kept := make([]networkingv1.NetworkPolicyIngressRule, 0, len(original))
	for _, rule := range np.Spec.Ingress {
		if !raxRuleCoversReserve(rule) {
			kept = append(kept, rule)
		}
	}
	if len(kept) == len(original) {
		skipNoConfig(t, "NetworkPolicy %s/%s has no ingress rule naming the %q port, so removing one would sever nothing — the reserve "+
			"endpoint is reachable by some other path and this scenario cannot be set up", opNS, key.Name, raxReservePortName)
	}
	t.Cleanup(func() { raxPutIngress(t, c, key, original) })

	np.Spec.Ingress = kept
	if err := c.Update(ctx, &np); err != nil {
		t.Fatalf("remove the %q ingress rule from NetworkPolicy %s/%s: %v", raxReservePortName, opNS, key.Name, err)
	}
	raxSaved.Store(key.String(), original)
	t.Logf("severed the authority: removed the ingress rule for the %q port from NetworkPolicy %s/%s (%d rules left)",
		raxReservePortName, opNS, key.Name, len(kept))
}

// raxRestoreReserve puts the reserve port's ingress rule back; the later t.Cleanup repeat is a no-op.
func raxRestoreReserve(t testing.TB, c client.Client, opNS string) {
	t.Helper()
	key := client.ObjectKey{Name: env("E2E_NETWORK_POLICY", operatorDeployment), Namespace: opNS}
	saved, ok := raxSaved.Load(key.String())
	if !ok {
		t.Errorf("no saved ingress rules for NetworkPolicy %s — the reserve port may still be severed; check the policy before trusting "+
			"any later test on this cluster", key)
		return
	}
	raxPutIngress(t, c, key, saved.([]networkingv1.NetworkPolicyIngressRule))
	t.Logf("restored the %q ingress rule on NetworkPolicy %s", raxReservePortName, key)
}

// raxSaved holds the ingress rules to restore, keyed by policy, so cleanup and the mid-test restore agree.
var raxSaved sync.Map

// raxPutIngress writes the ingress rules, retrying conflicts. Failure is an Errorf even from cleanup:
// a severed reserve port breaks every later test on the cluster.
func raxPutIngress(t testing.TB, c client.Client, key client.ObjectKey, rules []networkingv1.NetworkPolicyIngressRule) {
	deadline := time.Now().Add(1 * time.Minute)
	for {
		var np networkingv1.NetworkPolicy
		err := c.Get(context.Background(), key, &np)
		if err == nil {
			np.Spec.Ingress = rules
			err = c.Update(context.Background(), &np)
			if err == nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Errorf("could not restore the ingress rules of NetworkPolicy %s: %v — the reserve port may still be severed, so check the policy "+
				"before trusting any later reserve on this cluster", key, err)
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// raxRuleCoversReserve reports whether rule opens the reserve port by name or number; a rule without ports opens all.
func raxRuleCoversReserve(rule networkingv1.NetworkPolicyIngressRule) bool {
	if len(rule.Ports) == 0 {
		return true
	}
	for _, p := range rule.Ports {
		if p.Port == nil {
			return true
		}
		if p.Port.Type == intstr.String && p.Port.StrVal == raxReservePortName {
			return true
		}
		if p.Port.Type == intstr.Int && strconv.Itoa(int(p.Port.IntVal)) == raxReservePort() {
			return true
		}
	}
	return false
}

// raxReservePort is the manager's reserve port, the chart's webhook.reserveAuthority.port default.
func raxReservePort() string { return env("E2E_RESERVE_PORT", "9444") }

// raxRealWave creates count real pods concurrently and reads each verdict off the object Create returns.
func raxRealWave(t *testing.T, c client.Client, ns, hostname, node, tag string, count, perPod int, containerName string) raxTally {
	t.Helper()
	balloonKey := balloonAnnotationBase + "/container." + containerName
	pods := make([]*corev1.Pod, count)
	for i := range pods {
		p := vllmPod(fmt.Sprintf("e2e-rax-%s-%02d", tag, i), ns,
			strconv.Itoa(perPod), strconv.Itoa(perPod), 1 /*TP => span 1*/)
		p.Spec.NodeSelector = map[string]string{snHostnameLabel: hostname}
		pods[i] = p
		t.Cleanup(func() { e2eDelete(t, c, p, client.GracePeriodSeconds(0)) })
	}

	errs := make([]error, count)
	var wg sync.WaitGroup
	for i, p := range pods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = e2eCreate(context.Background(), c, p)
		}()
	}
	wg.Wait()

	decisions := make([]raxDecision, count)
	for i, p := range pods {
		d := raxDecision{pod: p.Name}
		switch {
		case errs[i] != nil:
			d.message = errs[i].Error()
			if !strings.Contains(d.message, ccDenialSubstr) && !strings.Contains(d.message, raUnaccountableSubstr) {
				d.err = errs[i]
			}
		default:
			d.allowed = true
			if p.Annotations[pinnedByAnnotation] == "cpu-pinning-operator" {
				d.balloon = p.Annotations[balloonKey]
			}
		}
		decisions[i] = d
	}
	return raxSummarise(decisions)
}

// raxForceFailover deletes the leader and returns the next Ready lease holder, or the original leader if the lease did not move.
func raxForceFailover(t testing.TB, cs kubernetes.Interface, opNS, leader string) string {
	t.Helper()
	ctx := context.Background()
	if err := cs.CoreV1().Pods(opNS).Delete(ctx, leader, metav1.DeleteOptions{}); err != nil {
		skipNoConfig(t, "could not delete the lease-holding replica %s/%s to force a handover: %v", opNS, leader, err)
	}
	leaseName := env("E2E_LEADER_LEASE", operatorDeployment)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		lease, err := cs.CoordinationV1().Leases(opNS).Get(ctx, leaseName, metav1.GetOptions{})
		if err != nil || lease.Spec.HolderIdentity == nil {
			continue
		}
		holder := *lease.Spec.HolderIdentity
		if i := strings.LastIndex(holder, "_"); i >= 0 {
			holder = holder[:i]
		}
		if holder == "" || holder == leader {
			continue
		}
		pod, err := cs.CoreV1().Pods(opNS).Get(ctx, holder, metav1.GetOptions{})
		if err != nil || !podReady(pod) {
			continue
		}
		t.Logf("leader-election lease moved from %s to %s after deleting the holder", leader, holder)
		return holder
	}
	t.Logf("the lease did not move to a Ready replica other than %s within 3m", leader)
	return leader
}

// raxAssertPinned checks admitted pods' NRT cpusets match the request width; it fails if none was ever pinned.
func raxAssertPinned(t *testing.T, c client.Client, node, ns, containerName string, pods []string, perPod int) {
	t.Helper()
	checked, wrong := 0, 0
	deadline := time.Now().Add(2 * time.Minute)
	pending := append([]string{}, pods...)
	for len(pending) > 0 && time.Now().Before(deadline) {
		var still []string
		for _, name := range pending {
			cpus := containerCPUSet(t, c, node, ns, name, containerName)
			if cpus == "" {
				still = append(still, name)
				continue
			}
			checked++
			if got := len(parseCPUSet(cpus)); got != perPod {
				wrong++
				t.Errorf("pod %s is pinned to %d CPU(s) (%q) but requested %d — the pin is not the width the admission promised",
					name, got, cpus, perPod)
			}
		}
		pending = still
		if len(pending) > 0 {
			time.Sleep(10 * time.Second)
		}
	}
	if checked == 0 {
		t.Errorf("none of the %d admitted pod(s) had a cpuset in %s's NodeResourceTopology within 2m, so the pins this test admitted were "+
			"never verified against ground truth (still without one: %v)", len(pods), node, pending)
		return
	}
	t.Logf("NodeResourceTopology ground truth: %d/%d admitted pod(s) pinned to exactly %d CPU(s), %d wrong; %d still held without a cpuset (%v)",
		checked-wrong, len(pods), perPod, wrong, len(pending), pending)
}

// raxNRTVersion returns the node's NRT resourceVersion, or "", to detect occupancy republished between waves.
func raxNRTVersion(t testing.TB, c client.Client, node string) string {
	t.Helper()
	nrt := newUnstructured(nrtGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, nrt); err != nil {
		t.Logf("NodeResourceTopology for %s not readable (%v) — a change in free room between the waves cannot be ruled out", node, err)
		return ""
	}
	return nrt.GetResourceVersion()
}

// raxAssertBalloonsExist checks every stamped balloon is defined in the node's BalloonsPolicy; an empty list fails too.
func raxAssertBalloonsExist(t testing.TB, c client.Client, node string, stamped []string) {
	t.Helper()
	if len(stamped) == 0 {
		t.Errorf("no admitted pod carried a balloon annotation, so nothing was pinned and the reserves below account for nothing")
		return
	}
	bp := newUnstructured(bpGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "node." + node, Namespace: balloonsNS()}, bp); err != nil {
		t.Errorf("get BalloonsPolicy node.%s in %s: %v — the stamped balloons cannot be checked against the node's policy", node, balloonsNS(), err)
		return
	}
	defined, _, _ := unstructured.NestedSlice(bp.Object, "spec", "balloonTypes")
	missing := map[string]bool{}
	for _, name := range stamped {
		if !hasBalloonType(defined, name) {
			missing[name] = true
		}
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		sort.Strings(names)
		t.Errorf("admitted pods were stamped balloon(s) %v that BalloonsPolicy node.%s does not define — the pin names a balloon the "+
			"node cannot honour, so the admission handed out room nothing will apply", names, node)
	}
}
