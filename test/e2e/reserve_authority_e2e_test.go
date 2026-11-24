// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// raGate opts in. raNegativeGate inverts the precondition: the run expects --reserve-authority ABSENT and the ledger assertions to go red.
	raGate         = "E2E_RESERVE_AUTHORITY"
	raNegativeGate = "E2E_RESERVE_AUTHORITY_NEGATIVE"

	// raAuthorityFlag is rendered only for an HA install (reserveAuthority.enabled, replicaCount > 1, leaderElection.enabled).
	raAuthorityFlag = "--reserve-authority"

	// raLiveMetric is the per-replica gauge of unexpired reservations in THAT replica's store; raRecordedMetric counts booked reserves.
	raLiveMetric     = "cpu_pinning_webhook_reservations_live"
	raRecordedMetric = "cpu_pinning_webhook_reservations_recorded_total"

	// raUnaccountableSubstr is the enforce denial when the authority is unreachable (denyUnaccountable), distinct from ccDenialSubstr.
	raUnaccountableSubstr = "cannot account for this pod"

	// raWatch bounds the CreateContainerError watch, which runs after the scrape so it cannot eat the reservation TTL.
	raWatch = 60 * time.Second
)

// TestReserveAuthorityBurstSingleLedger proves a concurrent over-capacity burst across two replicas is booked only into the lease holder's ledger.
// Every skip precedes every assertion: a Skip after an Errorf is reported FAILED. The negative control
// (reserveAuthority.enabled=false plus E2E_RESERVE_AUTHORITY_NEGATIVE=1) must go red.
func TestReserveAuthorityBurstSingleLedger(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(raGate) == "" {
		skipNotSelected(at, "set %s=1 to run the leader-authoritative reserve burst "+
			"(needs a real NRI cluster; a default install already renders the authority, since "+
			"reserveAuthority.enabled, replicaCount=2 and leaderElection.enabled are all defaults)", raGate)
	}
	c := newClient(at)                 // skips cleanly when no kubeconfig
	node := balloonsEnabledNode(at, c) // skips cleanly when no node is provisioned

	leader, cs, opNS := raRequireAuthority(at)
	perNUMA, numa := snPerNUMACapacity(at, c, node)
	nodeCores := perNUMA * numa
	hostname := snNodeHostname(at, c, node)

	// A quarter NUMA per pod keeps several slots per NUMA, so the reserved pool and the readiness canary cannot strand
	// every slot and a second ledger overshoots.
	perPod := max(1, perNUMA/4)
	total := 2*(nodeCores/perPod) + 2
	at.Logf("node %s: %d cores across %d NUMA nodes (per-NUMA span-1 capacity %d); bursting %d pods x %d cores = %d cores, leader=%s",
		node, nodeCores, numa, perNUMA, total, perPod, total*perPod, leader)

	// The ledger assertions need metrics, so check reachability before the burst.
	serving := scrapeMetricsPerReplicaOrSkip(at, "the manager /metrics endpoints are not reachable via the apiserver pod proxy, so the reserve "+
		"cannot be traced to the lease holder. Check metrics.enabled and E2E_METRICS_PORT/E2E_METRICS_SCHEME (see docs/internal/e2e-internals.md)")
	replicas := make([]string, 0, len(serving))
	for pod := range serving {
		replicas = append(replicas, pod)
	}
	sort.Strings(replicas)

	ns, containerName := "cpu-pinning-e2e-reserve-authority", "vllm"
	raProvision(at, c, ns, containerName)

	// Drain old reservations, then make every replica republish its gauge: the gauge is published on change and never
	// recomputed at scrape time, so without both the delta is not comparable to the admission count.
	raxDrainLedger(at, cs, opNS, node)
	if !raxWarmGauges(context.Background(), at, cs, opNS, ns, node, replicas...) {
		skipNoConfig(at, "inconclusive: could not get every manager replica (%v) to pin a 1-core warm-up pod, so %s has no dependable baseline",
			replicas, raLiveMetric)
	}

	// Baseline after raProvision, which books a reservation of its own.
	before := scrapeMetricsPerReplicaOrSkip(at, "the manager /metrics endpoints stopped serving between the precondition scrape and the burst; "+
		"the ledger proof needs a baseline taken next to the burst")

	// Concurrent: the apiserver reuses one webhook connection for sequential CREATEs. Cleanups are registered up front
	// so no goroutine touches the testing handle.
	balloonKey := balloonAnnotationBase + "/container." + containerName
	pods := make([]*corev1.Pod, total)
	for i := range pods {
		p := vllmPod(fmt.Sprintf("e2e-ra-%02d", i), ns, strconv.Itoa(perPod), strconv.Itoa(perPod), 1 /*TP => span 1*/)
		p.Spec.NodeSelector = map[string]string{snHostnameLabel: hostname}
		pods[i] = p
		t.Cleanup(func() { e2eDelete(t, c, p, client.GracePeriodSeconds(0)) })
	}

	errs := make([]error, total)
	var wg sync.WaitGroup
	burstStart := time.Now()
	for i, p := range pods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = e2eCreate(context.Background(), c, p)
		}()
	}
	wg.Wait()
	// The burst must land well inside reservationTTL or early reserves expire before the scrape.
	at.Logf("burst of %d CREATEs completed in %s (reservationTTL default is 90s)", total, time.Since(burstStart).Round(time.Millisecond))

	// Create returns the object with the webhook's output. Unexpected errors are collected: every Skip must precede every Errorf.
	var admitted, unaccountable, unexpected []string
	denied := 0
	for i, p := range pods {
		switch {
		case errs[i] != nil && strings.Contains(errs[i].Error(), ccDenialSubstr):
			denied++
		case errs[i] != nil && strings.Contains(errs[i].Error(), raUnaccountableSubstr):
			unaccountable = append(unaccountable, p.Name)
		case errs[i] != nil:
			unexpected = append(unexpected, fmt.Sprintf("%s: %v", p.Name, errs[i]))
		case p.Annotations[balloonKey] != "" && p.Annotations[pinnedByAnnotation] == "cpu-pinning-operator":
			admitted = append(admitted, p.Name)
		default:
			at.Logf("pod %s admitted but carries no balloon annotation (%q) — not counted as accounted", p.Name, p.Annotations[balloonKey])
		}
	}
	admittedCores := len(admitted) * perPod
	at.Logf("burst: %d/%d admitted+pinned (%d cores), %d denied on capacity, %d denied as unaccountable, %d other errors",
		len(admitted), total, admittedCores, denied, len(unaccountable), len(unexpected))

	if len(admitted) == 0 {
		skipNoConfig(at, "inconclusive: nothing was admitted, so no reserve was accounted — check the policy reconcile and the node's free room")
	}
	if len(unaccountable) > 0 {
		skipNoConfig(at, "inconclusive: %d pod(s) were denied because the authority was unreachable (%v) — that is the deferred unreachable-authority "+
			"scenario, not the accounting under test; re-run once the reserve endpoint is reachable between replicas", len(unaccountable), unaccountable)
	}
	if denied == 0 {
		skipNoConfig(at, "inconclusive: %d pods admitted and NONE denied, so the burst never reached the node's capacity and nothing contended. "+
			"The node has more free room than the burst demanded (%d cores) — raise the burst or drain the node", len(admitted), total*perPod)
	}

	after := scrapeMetricsPerReplicaOrSkip(at, "inconclusive: the manager /metrics endpoints stopped serving mid-run; the ledger proof needs both scrapes")

	// A lease move mid-burst would fail the holder comparison for an unrelated reason.
	if now := leaderPodNameAfterHandover(at, cs, opNS); now != leader {
		skipNoConfig(at, "inconclusive: the leader-election lease moved from %s to %s during the burst, so the reserves are split across two "+
			"authorities by design rather than by a routing bug", leader, now)
	}

	// Fewer than two serving replicas means the cross-replica window never opened.
	var served, rose []string
	for pod, body := range after {
		beforeBody, known := before[pod]
		if known && counterValue(body, raRecordedMetric, `node="`+node+`"`) > counterValue(beforeBody, raRecordedMetric, `node="`+node+`"`) {
			served = append(served, pod)
		}
		// The gauge can still carry a stale positive, so only a RISE attributes a reservation to this burst.
		if raLive(body, node) > raLive(beforeBody, node) {
			rose = append(rose, pod)
		}
	}
	sort.Strings(served)
	sort.Strings(rose)
	at.Logf("replicas that booked a reserve for %s: %v; replicas whose live-reservation gauge rose: %v", node, served, rose)

	if len(served) < 2 {
		raReportUnspreadBurst(at, cs, served, node)
	}

	if len(unexpected) > 0 {
		at.Errorf("%d pod(s) failed to create for a reason that is neither a capacity nor an unaccountable denial: %v", len(unexpected), unexpected)
	}

	// On one NUMA node the bound equals the whole node, so a second ledger cannot overshoot it.
	if numa < 2 {
		at.Logf("single-NUMA node: skipping the over-admission bound (it cannot be exceeded by a second ledger here)")
	} else if admittedCores > nodeCores {
		at.Errorf("OVER-ADMISSION: %d cores admitted onto %s (%d pods x %d) but it pins at most %d — more headroom was handed out than exists, "+
			"which is what a single reservation ledger prevents", admittedCores, node, len(admitted), perPod, nodeCores)
	}

	switch {
	case len(rose) == 0:
		at.Errorf("no replica's %s{node=%q} rose during a burst that admitted %d pods — the reserves were not booked into any store this test "+
			"can see (a reservation TTL shorter than the burst, or the gauge is not being published)", raLiveMetric, node, len(admitted))
	case len(rose) > 1 || rose[0] != leader:
		at.Errorf("%s{node=%q} rose on %v, want only the lease holder %q — a follower accumulating its own live reservations means the reserve "+
			"was booked locally instead of being forwarded to the authority", raLiveMetric, node, rose, leader)
	}

	// Every admitted reserve must land in the leader's store; this holds on any topology.
	if got := raLive(after[leader], node) - raLive(before[leader], node); got < float64(len(admitted)) {
		at.Errorf("lease holder %s gained %v live reservation(s) for %s but the burst admitted %d pods — reserves the followers admitted never "+
			"reached the authority's store", leader, got, node, len(admitted))
	}

	// By-product only: an over-admitted pod waits in Init and never reaches CreateContainer.
	if stuck := raCreateContainerErrors(at, c, ns, admitted); len(stuck) > 0 {
		at.Errorf("pods %v hit CreateContainerError within %s — an admitted pod's cpuset could not be applied", stuck, raWatch)
	}
}

// raLive returns the replica's live-reservation gauge for node, 0 when absent.
func raLive(body, node string) float64 {
	v, _ := metricValueForNode(body, raLiveMetric, node)
	return v
}

// raRequireAuthority skips unless the manager has two Ready replicas and the authority wired as the run asked,
// and returns the lease holder, clientset and operator namespace.
func raRequireAuthority(t testing.TB) (leader string, cs kubernetes.Interface, opNS string) {
	t.Helper()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		skipNoCapability(t, "no kubeconfig available: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build clientset: %v", err)
	}
	opNS = env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system")
	name := env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dep, err := clientset.AppsV1().Deployments(opNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		skipNoConfig(t, "operator Deployment %s/%s not found (set E2E_OPERATOR_NAMESPACE/E2E_OPERATOR_DEPLOYMENT): %v", opNS, name, err)
	}
	if msg := raAuthorityModeSkip(opNS, name, raHasFlag(dep, raAuthorityFlag), os.Getenv(raNegativeGate) != ""); msg != "" {
		skipNoConfig(t, "%s", msg)
	}
	if dep.Status.ReadyReplicas < 2 {
		skipNoConfig(t, "manager Deployment %s/%s has %d Ready replica(s); the cross-replica reserve needs at least 2 (--set replicaCount=2)",
			opNS, name, dep.Status.ReadyReplicas)
	}
	return leaderPodNameAfterHandover(t, clientset, opNS), clientset, opNS // skips when no leader is resolvable
}

// raAuthorityModeSkip returns why the install cannot serve the requested mode, or "". Both directions are
// configuration gaps: the positive proof needs the flag, the negative control needs it off.
func raAuthorityModeSkip(opNS, name string, hasFlag, negative bool) string {
	switch {
	case !hasFlag && !negative:
		return fmt.Sprintf("manager Deployment %s/%s does not carry %s, so every replica keeps its own reservation store and there is no authority to prove. "+
			"The chart renders the flag by default, so something turned it off: check the release for "+
			"--set webhook.reserveAuthority.enabled=false, replicaCount=1, or leaderElection.enabled=false. "+
			"To run this deliberately as the negative control and watch the ledger assertions fail, set %s=1",
			opNS, name, raAuthorityFlag, raNegativeGate)
	case hasFlag && negative:
		return fmt.Sprintf("%s asks for the negative control, which needs an install with webhook.reserveAuthority.enabled=false, but manager Deployment "+
			"%s/%s still carries %s, so the replicas share one ledger and the waves cannot over-admit. The chart renders the flag by default: reinstall "+
			"with --set webhook.reserveAuthority.enabled=false (keeping replicaCount=2) to run the control, or unset %s to run the positive proof "+
			"against this release",
			raNegativeGate, opNS, name, raAuthorityFlag, raNegativeGate)
	}
	return ""
}

// raHasFlag reports whether a container in the pod template passes flag, bare or as --flag=value.
func raHasFlag(dep *appsv1.Deployment, flag string) bool {
	for _, ctr := range dep.Spec.Template.Spec.Containers {
		for _, arg := range ctr.Args {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return true
			}
		}
	}
	return false
}

// raProvision creates a throwaway namespace and an enforce-mode policy for it, and waits until the webhook pins there.
func raProvision(t testing.TB, c client.Client, ns, containerName string) {
	t.Helper()
	ctx := context.Background()
	const policy = "e2e-reserve-authority"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	_ = c.Delete(ctx, nsObj)
	if err := createEventuallyGone(c, nsObj); err != nil {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Cleanup(func() {
		e2eDelete(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	// enforce makes the capacity gate DENY once the ledger is full, which is what makes accounting observable.
	bpp := newUnstructured(bppGVK)
	bpp.SetName(policy)
	bpp.Object["spec"] = map[string]interface{}{
		"match":         map[string]interface{}{"byContainerName": []interface{}{containerName}},
		"namespaces":    []interface{}{ns},
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

	if !waitForWebhookNamespace(t, c, ns, containerName) {
		skipNoConfig(t, "webhook never began pinning pods in %q (policy reconcile / selector patch did not take effect)", ns)
	}
}

// raCreateContainerErrors returns the admitted pods whose workload container hit CreateContainerError within raWatch.
func raCreateContainerErrors(t testing.TB, c client.Client, ns string, names []string) []string {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.Now().Add(raWatch)
	for {
		for _, name := range names {
			p := &corev1.Pod{}
			if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, p); err != nil {
				continue // transient read error — re-checked next tick
			}
			for _, st := range p.Status.ContainerStatuses {
				if st.State.Waiting != nil && st.State.Waiting.Reason == "CreateContainerError" {
					seen[name] = true
				}
			}
		}
		if len(seen) == len(names) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// raReportUnspreadBurst skips with the reason a burst landed on one replica. sessionAffinity: ClientIP is configuration.
// One apiserver is a capability gap: it keeps one HTTP/2 connection to the Service, so one replica serves every
// admission (TestReserveAuthorityCrossReplicaLedger covers that case). Anything else is a routing problem.
func raReportUnspreadBurst(t testing.TB, cs kubernetes.Interface, served []string, node string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opNS := env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system")

	svcName := env("E2E_WEBHOOK_SERVICE", operatorDeployment+"-webhook")
	if svc, err := cs.CoreV1().Services(opNS).Get(ctx, svcName, metav1.GetOptions{}); err == nil &&
		svc.Spec.SessionAffinity == corev1.ServiceAffinityClientIP {
		skipNoConfig(t, "inconclusive: only %v booked a reserve for %s because Service %s/%s has sessionAffinity: ClientIP, which pins the "+
			"apiserver to one replica by design. Set webhook.service.sessionAffinity to \"\" or None to spread the burst", served, node, opNS, svcName)
	}

	apiservers := 0
	if ep, err := cs.CoreV1().Endpoints("default").Get(ctx, "kubernetes", metav1.GetOptions{}); err == nil {
		for _, subset := range ep.Subsets {
			apiservers += len(subset.Addresses)
		}
	}
	if apiservers == 1 {
		skipNoCapability(t, "only %v booked a reserve for %s, and this control plane has ONE apiserver: it holds a single HTTP/2 connection to "+
			"the webhook Service, kube-proxy chose that connection's backend once, and every admission in the burst multiplexed over it. No burst "+
			"size opens the cross-replica window here — several apiservers or connection churn would. "+
			"TestReserveAuthorityCrossReplicaLedger proves the same accounting on this cluster by addressing each replica directly",
			served, node)
	}
	skipNoConfig(t, "inconclusive: only %v booked a reserve for %s even though this control plane has %d apiserver(s) and the webhook Service is "+
		"not pinning a client — the burst was not spread across replicas, so the assertions would prove nothing. Check the Service's EndpointSlice "+
		"for both replica IPs before raising the burst", served, node, apiservers)
}
