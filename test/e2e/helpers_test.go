// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// podLogs returns one container's log, from since when it is set.
func podLogs(ctx context.Context, cs kubernetes.Interface, ns, pod, container string, since *metav1.Time) (string, error) {
	raw, err := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, SinceTime: since}).DoRaw(ctx)
	return string(raw), err
}

// -- policy name -------------------------------------------------------------

// policyName is the installed BalloonPinningPolicy; override with E2E_POLICY_NAME.
func policyName() string { return env("E2E_POLICY_NAME", "default") }

// -- metric scraping ---------------------------------------------------------

// scrapeMetricsAllReplicas returns every Ready manager pod's exposition concatenated,
// or "" when nothing served. Scrape pods, not the Service: an admission increments a
// counter on one replica only. Read it with counterSum; replica order is arbitrary.
func scrapeMetricsAllReplicas(t testing.TB) string {
	t.Helper()
	bodies, _ := scrapeMetricsPerReplicaOutcome(t)
	return joinReplicaBodies(bodies)
}

// scrapeMetricsAllReplicasOrSkip is scrapeMetricsAllReplicas that skips when nothing
// served; call it inside its own subtest.
func scrapeMetricsAllReplicasOrSkip(t testing.TB, format string, args ...any) string {
	t.Helper()
	return joinReplicaBodies(scrapeMetricsPerReplicaOrSkip(t, format, args...))
}

func joinReplicaBodies(byPod map[string]string) string {
	bodies := make([]string, 0, len(byPod))
	for _, body := range byPod {
		bodies = append(bodies, body)
	}
	return strings.Join(bodies, "\n")
}

// -- per-node series: the leader's publish window ----------------------------

const (
	// metricPublishTimeout bounds the wait for a freshly started leader to publish a
	// per-node gauge; each is set only after its controller's first reconcile pass.
	metricPublishTimeout = 90 * time.Second
	// metricPublishPoll is the interval between re-scrapes.
	metricPublishPoll = 3 * time.Second
)

// nodeMetricsMissing returns the named metrics with no node="<node>" sample, in order.
func nodeMetricsMissing(body, node string, metrics ...string) []string {
	var missing []string
	for _, metric := range metrics {
		if !metricPresentForNode(body, metric, node) {
			missing = append(missing, metric)
		}
	}
	return missing
}

// scrapeOperatorMetricsForNode re-scrapes until every named metric has a series for
// the node, or metricPublishTimeout passes. It waits only on absence, never on a
// wrong value. waitForNodeSettled does not cover this: a previous leader may have
// rendered the node.
func scrapeOperatorMetricsForNode(t testing.TB, node string, metrics ...string) string {
	t.Helper()
	body := scrapeOperatorMetrics(t)
	missing := nodeMetricsMissing(body, node, metrics...)
	if len(missing) == 0 {
		return body
	}
	start := time.Now()
	for time.Since(start) < metricPublishTimeout {
		time.Sleep(metricPublishPoll)
		body = scrapeOperatorMetrics(t)
		if missing = nodeMetricsMissing(body, node, metrics...); len(missing) == 0 {
			t.Logf("publish window: %v appeared for node %q after %s — the manager serving /metrics had not published them yet",
				metrics, node, time.Since(start).Round(time.Second))
			return body
		}
	}
	t.Logf("publish window: %v still absent for node %q after %s — treating it as a real absence", missing, node, metricPublishTimeout)
	return body
}

// scrapeMetricsPerReplicaOrSkip returns each replica's exposition keyed by pod name,
// for comparing per-replica state. Skips when nothing served.
func scrapeMetricsPerReplicaOrSkip(t testing.TB, format string, args ...any) map[string]string {
	t.Helper()
	bodies, authRefused := scrapeMetricsPerReplicaOutcome(t)
	if len(bodies) > 0 {
		return bodies
	}
	skipNoMetrics(t, authRefused, format, args...)
	return nil
}

// skipNoMetrics ends a test that got no exposition. A refused scrape (metrics auth
// on; the pod proxy has no credential) is not-selected; anything else is a config gap.
func skipNoMetrics(t testing.TB, authRefused bool, format string, args ...any) {
	t.Helper()
	if authRefused {
		skipNotSelected(t, "the manager /metrics endpoints refused this unauthenticated scrape, so the operator was not asked for anything it "+
			"failed to give: the endpoint authenticates its clients and the apiserver pod proxy cannot present a credential. "+
			"Deploy with --set metrics.requireAuth=false (TLS is kept) to run the metric assertions")
		return
	}
	skipNoConfig(t, format, args...)
}

// allScrapesRefused is true only when every attempted replica refused: a mixed
// outcome is a broken install, not a deployment mode.
func allScrapesRefused(attempted, refused int) bool { return attempted > 0 && refused == attempted }

// scrapeMetricsPerReplicaOutcome scrapes every Ready manager pod via the apiserver
// proxy. authRefused is true only when nothing served and every replica refused.
func scrapeMetricsPerReplicaOutcome(t testing.TB) (bodies map[string]string, authRefused bool) {
	t.Helper()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, false
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, false
	}
	opNS := env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system")
	port := env("E2E_METRICS_PORT", "8080")
	scheme := metricsProxyScheme(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Scope the scrape to this release's replicas.
	selector := managerPodSelector(ctx, cs, opNS)
	pods, err := cs.CoreV1().Pods(opNS).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Logf("manager pods not listable (ns %s selector %q): %v — skipping metric-delta assertions", opNS, selector, err)
		return nil, false
	}
	bodies = map[string]string{}
	attempted, refused := 0, 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !podReady(pod) {
			continue
		}
		attempted++
		raw, err := cs.CoreV1().RESTClient().Get().
			Namespace(opNS).
			Resource("pods").
			Name(fmt.Sprintf("%s:%s:%s", scheme, pod.Name, port)).
			SubResource("proxy").
			Suffix("metrics").
			DoRaw(ctx)
		if err != nil {
			// Keep the reachable bodies; log a refusal separately from an unreachable pod.
			if metricsAuthRejected(err) {
				refused++
				t.Logf("manager pod %s/%s rejected the unauthenticated pod-proxy scrape: %v — the endpoint requires a client credential the "+
					"proxy cannot present; deploy with --set metrics.requireAuth=false (TLS is kept) to run the metric-delta assertions",
					opNS, pod.Name, err)
				continue
			}
			t.Logf("manager pod %s/%s metrics not proxy-reachable: %v — excluding it from the metric sum", opNS, pod.Name, err)
			continue
		}
		bodies[pod.Name] = string(raw)
	}
	if len(bodies) == 0 {
		t.Logf("no manager replica served metrics (ns %s selector %q): %d of %d ready replica(s) refused the scrape — skipping metric-delta assertions",
			opNS, selector, refused, attempted)
		return nil, allScrapesRefused(attempted, refused)
	}
	return bodies, false
}

// metricsAuthRejected reports whether the metrics endpoint refused an anonymous
// scrape (401). A 403 here is the apiserver refusing the caller pods/proxy, a
// kubeconfig problem, so it is not counted.
func metricsAuthRejected(err error) bool { return apierrors.IsUnauthorized(err) }

// managerPodSelector returns E2E_MANAGER_SELECTOR, else the manager Deployment's own
// selector (release-scoped), else a chart-name fallback that may match sibling releases.
func managerPodSelector(ctx context.Context, cs kubernetes.Interface, ns string) string {
	const fallback = "control-plane=controller-manager,app.kubernetes.io/name=" + operatorDeployment
	if v := os.Getenv("E2E_MANAGER_SELECTOR"); v != "" {
		return v
	}
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, operatorDeployment, metav1.GetOptions{})
	if err != nil || dep.Spec.Selector == nil || len(dep.Spec.Selector.MatchLabels) == 0 {
		return fallback
	}
	return labels.SelectorFromSet(dep.Spec.Selector.MatchLabels).String()
}

// podReady reports whether a pod's Ready condition is True.
func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// -- Prometheus-text counter parsing -----------------------------------------

// sampleValue parses the trailing float value from a Prometheus exposition line.
func sampleValue(line string) (float64, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
	return v, err == nil
}

// lineHasAllLabels reports whether the line contains every substring matcher.
func lineHasAllLabels(line string, matchers []string) bool {
	for _, m := range matchers {
		if !strings.Contains(line, m) {
			return false
		}
	}
	return true
}

// counterValue returns the first sample matching all label matchers, or 0 when absent.
func counterValue(body, metric string, labelMatchers ...string) float64 {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric+"{") {
			continue
		}
		if !lineHasAllLabels(line, labelMatchers) {
			continue
		}
		if v, ok := sampleValue(line); ok {
			return v
		}
	}
	return 0
}

// counterSum sums every sample matching all label matchers, or 0 when absent.
func counterSum(body, metric string, labelMatchers ...string) float64 {
	total := 0.0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric+"{") {
			continue
		}
		if !lineHasAllLabels(line, labelMatchers) {
			continue
		}
		if v, ok := sampleValue(line); ok {
			total += v
		}
	}
	return total
}

// metricPresentForNode reports whether the exposition has a sample of metric
// carrying node="<node>".
func metricPresentForNode(body, metric, node string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, metric+"{") && strings.Contains(line, `node="`+node+`"`) {
			return true
		}
	}
	return false
}

// metricValueForNode returns the first sample of metric for the node.
func metricValueForNode(body, metric, node string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric+"{") || !strings.Contains(line, `node="`+node+`"`) {
			continue
		}
		return sampleValue(line)
	}
	return 0, false
}

// anyCounterValue returns the first sample of metric regardless of labels. present
// and parsed separate an absent series from an unparseable one.
func anyCounterValue(body, metric string) (value float64, present, parsed bool) {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, metric+"{") && !strings.HasPrefix(line, metric+" ") {
			continue
		}
		// A matching line means present, parsed or not.
		v, ok := sampleValue(line)
		return v, true, ok
	}
	return 0, false, false
}

// -- event polling -----------------------------------------------------------

// waitForWarningEvent reports whether a Warning Event with reason, on Pod ns/podName,
// whose message contains msgSubstr ("" matches any) appears within timeout. A nil
// reportErr retries over list errors.
func waitForWarningEvent(t testing.TB, c client.Client, ns, podName, reason, msgSubstr string, timeout time.Duration, reportErr func(error)) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var events corev1.EventList
		if err := c.List(context.Background(), &events, client.InNamespace(ns)); err != nil {
			if reportErr != nil {
				reportErr(err)
			}
		} else {
			for i := range events.Items {
				e := &events.Items[i]
				if e.Type == corev1.EventTypeWarning &&
					e.Reason == reason &&
					e.InvolvedObject.Kind == "Pod" &&
					e.InvolvedObject.Name == podName &&
					(msgSubstr == "" || strings.Contains(e.Message, msgSubstr)) {
					return true
				}
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(eventPollInterval)
	}
}

// eventPollInterval is the spacing between Event-list reads.
const eventPollInterval = 3 * time.Second

// -- pod / deployment lifecycle waiters --------------------------------------

// podIsReady reports whether the pod's PodReady condition is True.
func podIsReady(p *corev1.Pod) bool {
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// waitForRunningPinnedPod returns the first pinned, Running and Ready pod with the app
// label, or nil after 4 minutes. Ready, so an unready pod cannot be why a PDB blocks.
func waitForRunningPinnedPod(t testing.TB, c client.Client, ns, appLabel string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		var pods corev1.PodList
		if err := c.List(context.Background(), &pods,
			client.InNamespace(ns), client.MatchingLabels{"app": appLabel}); err == nil {
			for i := range pods.Items {
				p := &pods.Items[i]
				if p.DeletionTimestamp != nil {
					continue
				}
				if p.Annotations[pinnedByAnnotation] == "cpu-pinning-operator" &&
					p.Status.Phase == corev1.PodRunning && podIsReady(p) {
					return p.DeepCopy()
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
	return nil
}

// waitForPodEvicted reports whether the pod is gone within timeout; other errors are fatal.
func waitForPodEvicted(t testing.TB, c client.Client, ns, pod string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := c.Get(context.Background(), client.ObjectKey{Name: pod, Namespace: ns}, &corev1.Pod{})
		if apierrors.IsNotFound(err) {
			return true
		}
		if err != nil {
			t.Fatalf("get pod %s/%s while waiting for it to be gone: %v", ns, pod, err)
		}
		time.Sleep(eventPollInterval)
	}
	return false
}

// waitForPodGone waits until the pod is gone or timeout passes. Never fails; safe in cleanup.
func waitForPodGone(ctx context.Context, c client.Client, ns, name string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, &corev1.Pod{})
		if apierrors.IsNotFound(err) {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// -- node topology reads -----------------------------------------------------

// nodeTopology reads the node's physical-core and threads-per-core labels; threads is at least 1.
func nodeTopology(t testing.TB, c client.Client, node string) (physical, threads int) {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		t.Fatalf("get node %s: %v", node, err)
	}
	physical, _ = strconv.Atoi(n.Labels[labelPhysicalCPUs])
	threads, _ = strconv.Atoi(n.Labels[labelThreadsPerCore])
	if threads < 1 {
		threads = 1
	}
	return physical, threads
}

// -- BalloonsClusterConfig read / patch / update -----------------------------

const bccName = "cluster"

// getBCC reads the singleton; found is false on NotFound.
func getBCC(ctx context.Context, c client.Client) (*unstructured.Unstructured, bool, error) {
	bcc := newUnstructured(bccGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: bccName}, bcc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return bcc, true, nil
}

// updateBCC applies mutate to the singleton, retrying conflicts for a minute; errors
// are fatal. Cleanup callers use updateBCCReport.
func updateBCC(t testing.TB, c client.Client, mutate func(*unstructured.Unstructured) error) {
	t.Helper()
	updateBCCReport(t, c, mutate, func(verb string, err error) { t.Fatalf("%s: %v", verb, err) })
}

// updateBCCReport is updateBCC that reports the terminal error through reportErr.
func updateBCCReport(t testing.TB, c client.Client, mutate func(*unstructured.Unstructured) error, reportErr func(verb string, err error)) {
	t.Helper()
	deadline := time.Now().Add(1 * time.Minute)
	for {
		bcc := newUnstructured(bccGVK)
		if err := c.Get(context.Background(), client.ObjectKey{Name: bccName}, bcc); err != nil {
			reportErr("get BalloonsClusterConfig/cluster", err)
			return
		}
		if err := mutate(bcc); err != nil {
			reportErr("mutate BalloonsClusterConfig", err)
			return
		}
		err := c.Update(context.Background(), bcc)
		if err == nil {
			return
		}
		if apierrors.IsConflict(err) && time.Now().Before(deadline) {
			time.Sleep(2 * time.Second)
			continue
		}
		reportErr("update BalloonsClusterConfig/cluster", err)
		return
	}
}

// patchBCC merge-patches the singleton, reporting a failure through reportErr.
func patchBCC(ctx context.Context, c client.Client, body []byte, reportErr func(verb string, err error)) {
	cur := newUnstructured(bccGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: bccName}, cur); err != nil {
		reportErr("get BalloonsClusterConfig/cluster", err)
		return
	}
	if err := c.Patch(ctx, cur, client.RawPatch(types.MergePatchType, body)); err != nil {
		reportErr("patch BalloonsClusterConfig/cluster", err)
	}
}

// -- node BalloonsPolicy read ------------------------------------------------

// balloonsPolicyAvailableCPU returns the node BalloonsPolicy's spec.availableResources.cpu,
// or "" when absent or not rendered yet.
func balloonsPolicyAvailableCPU(t testing.TB, c client.Client, bpKey client.ObjectKey) string {
	t.Helper()
	bp := newUnstructured(bpGVK)
	if err := c.Get(context.Background(), bpKey, bp); err != nil {
		if apierrors.IsNotFound(err) {
			return ""
		}
		t.Fatalf("get BalloonsPolicy %s: %v", bpKey.Name, err)
	}
	cpu, _, _ := unstructured.NestedString(bp.Object, "spec", "availableResources", "cpu")
	return cpu
}
