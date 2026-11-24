// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Package e2e holds end-to-end tests against a real NRI-enabled, multi-NUMA cluster
// with the operator installed; envtest and kind cannot pin CPUs.
//
//	make test-e2e   # ambient kubeconfig; E2E_RUN_PINNING=1 and E2E_DASHBOARDS=1 add optional checks
package e2e

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	balloonAnnotationBase = "balloon.balloons.resource-policy.nri.io"
	pinnedByAnnotation    = "cpu-pinning-operator.intel.com/pinned-by"
	pinnedHashAnnotation  = "cpu-pinning-operator.intel.com/pinned-hash"
	labelBalloonsEnabled  = "cpu-pinning-operator.intel.com/balloons-enabled"
	labelMaxSpan          = "cpu-pinning-operator.intel.com/max-span"
	labelNumaNodes        = "cpu-pinning-operator.intel.com/numa-nodes"
	labelPhysicalCPUs     = "cpu-pinning-operator.intel.com/physical-cpus"
	labelThreadsPerCore   = "cpu-pinning-operator.intel.com/threads-per-core"
	// labelReservedCPUs counts the node's reserved CPUs, which workloads cannot pin.
	labelReservedCPUs   = "cpu-pinning-operator.intel.com/reserved-cpus"
	waitForBalloonsInit = "wait-for-balloons"

	// operatorDeployment is the manager Deployment name.
	operatorDeployment = "cpu-pinning-operator"
)

var (
	// targetNamespace resolves a namespace the webhook mutates, or why none was found.
	targetNamespace = onceOnSuccess(resolveTargetNamespace)

	// operatorConfig reads the ConfigMap the manager loads via envFrom.
	operatorConfig = onceOnSuccess(resolveOperatorConfig)

	// discoveredBalloonsNamespace is the live plugin DaemonSet's namespace, the fallback.
	discoveredBalloonsNamespace = onceOnSuccess(resolveDiscoveredBalloonsNamespace)

	// sharedClient is the resolvers' lazy client. Tests use newClient: a shared
	// RESTMapper would go stale for a test that removes CRDs.
	sharedClient = sync.OnceValues(clusterClient)

	nrtGVK = schema.GroupVersionKind{Group: "topology.node.k8s.io", Version: "v1alpha2", Kind: "NodeResourceTopology"}
	bpGVK  = schema.GroupVersionKind{Group: "config.nri", Version: "v1alpha1", Kind: "BalloonsPolicy"}
	bccGVK = schema.GroupVersionKind{Group: "cpu-pinning-operator.intel.com", Version: "v1alpha1", Kind: "BalloonsClusterConfig"}
	bppGVK = schema.GroupVersionKind{Group: "cpu-pinning-operator.intel.com", Version: "v1alpha1", Kind: "BalloonPinningPolicy"}
)

// env returns the env var or a default.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// metricsProxyScheme returns the metrics proxy scheme (E2E_METRICS_SCHEME, default
// https). An invalid value fails fast: it would otherwise turn into a silent skip.
func metricsProxyScheme(t testing.TB) string {
	t.Helper()
	rawScheme := env("E2E_METRICS_SCHEME", "https")
	scheme := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(rawScheme), ":"))
	if scheme != "http" && scheme != "https" {
		t.Fatalf("invalid E2E_METRICS_SCHEME %q: want \"http\" or \"https\" (no trailing colon)", rawScheme)
	}
	return scheme
}

// onceOnSuccess memoizes only a successful resolution, so one transient read failure
// cannot decide the rest of the run.
func onceOnSuccess[T any](resolve func() (T, error)) func() (T, error) {
	var (
		mu     sync.Mutex
		cached T
		done   bool
	)
	return func() (T, error) {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return cached, nil
		}
		v, err := resolve()
		if err != nil {
			var zero T
			return zero, err
		}
		cached, done = v, true
		return v, nil
	}
}

// errNoPolicy means no BalloonPinningPolicy is installed: a missing capability.
var errNoPolicy = errors.New("no BalloonPinningPolicy is installed")

// nsProvisionFailure holds why creating the target namespace failed, or "".
var nsProvisionFailure string

// e2eOwnerLabel marks a namespace this suite created; teardown deletes only those.
const e2eOwnerLabel = "app.kubernetes.io/managed-by"

const e2eOwnerValue = "cpu-pinning-e2e"

// targetNamespaceTeardown is non-nil only when ensureTargetNamespace created a namespace.
var targetNamespaceTeardown func()

// ensureTargetNamespace creates the target namespace once per package, on first use:
// a namespace deleted per test stays Terminating and rejects the next test's pods.
var ensureTargetNamespace = sync.OnceFunc(func() { targetNamespaceTeardown = provisionTargetNamespace() })

// TestMain sweeps after the run, on SIGTERM/SIGINT, and shortly before -timeout
// would panic past every cleanup. The pre-run sweep happens in armSweep, on the
// first newClient, so a cluster-free run never reaches a cluster.
func TestMain(m *testing.M) {
	flag.Parse()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigs
		fmt.Printf("e2e: %v received, sweeping before exit\n", sig)
		sweepAndReport(sweepTimeout, true)
		os.Exit(128 + int(sig.(syscall.Signal)))
	}()
	var deadline *time.Timer
	if timeout := flag.Lookup("test.timeout").Value.(flag.Getter).Get().(time.Duration); timeout > 0 {
		// The sweep gets 90% of the reserve, so it ends before Go's own panic.
		reserve := min(sweepTimeout, timeout/4)
		deadline = time.AfterFunc(timeout-reserve, func() {
			if !sweepArmed() {
				return // nothing to sweep: let -timeout panic as usual
			}
			fmt.Printf("e2e: -timeout %s is %s away, dumping goroutines and sweeping before exit\n", timeout, reserve)
			_ = pprof.Lookup("goroutine").WriteTo(os.Stdout, 2)
			sweepAndReport(reserve*9/10, true)
			os.Exit(1)
		})
	}
	code := m.Run()
	if deadline != nil {
		deadline.Stop()
	}
	reportSkipTally()
	if targetNamespaceTeardown != nil {
		targetNamespaceTeardown()
	}
	if !sweepAndReport(sweepTimeout, false) && code == 0 {
		code = 1 // a failed cleanup fails the run
	}
	os.Exit(code)
}

// sweepAndReport runs e2eSweep and prints its error; false when it failed.
func sweepAndReport(budget time.Duration, keepSnapshot bool) bool {
	if err := e2eSweep(budget, keepSnapshot); err != nil {
		fmt.Printf("e2e sweep FAILED (re-run `make test-e2e-sweep`): %v\n", err)
		return false
	}
	return true
}

// reportOnce prints the resolution on first use, not from TestMain, so the
// cluster-free guards stay cluster-free.
var reportOnce = sync.OnceFunc(reportResolution)

// reportResolution prints the resolved namespaces and DaemonSet.
func reportResolution() {
	target, err := targetNamespace()
	desc := fmt.Sprintf("%q", target)
	if err != nil {
		desc = fmt.Sprintf("unresolved (%v)", err)
	}
	ns, source := balloonsNamespace()
	fmt.Printf("e2e: target namespace %s; balloons namespace %q (from %s); balloons DaemonSet %q\n",
		desc, ns, source, balloonsDaemonSet())
}

// balloonsNS is the namespace half of balloonsNamespace.
func balloonsNS() string {
	reportOnce()
	ns, _ := balloonsNamespace()
	return ns
}

// provisionTargetNamespace creates the target namespace when absent and returns its
// teardown. No settle wait: the webhook selector lists exact names whether or not
// the namespace exists.
func provisionTargetNamespace() func() {
	noop := func() {}
	ns, nsErr := targetNamespace()
	if nsErr != nil {
		return noop
	}
	c, err := sharedClient()
	if err != nil {
		return noop
	}
	ctx := context.Background()
	var existing corev1.Namespace
	err = c.Get(ctx, client.ObjectKey{Name: ns}, &existing)
	switch {
	case err == nil && existing.Status.Phase != corev1.NamespaceTerminating:
		if existing.Labels[e2eOwnerLabel] == e2eOwnerValue {
			// Left by an earlier run that died before teardown: adopt it.
			fmt.Printf("e2e: adopting namespace %q left labelled %s=%s by an earlier run; it will be deleted at the end\n",
				ns, e2eOwnerLabel, e2eOwnerValue)
			return func() { deleteIfOwned(c, ns) }
		}
		return noop // not ours to delete
	case err != nil && !apierrors.IsNotFound(err):
		// Each test reads it again; don't record a transient failure for the whole run.
		return noop
	}
	// Absent, or still terminating from an aborted run; createEventuallyGone waits that out.
	created := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   ns,
		Labels: map[string]string{e2eOwnerLabel: e2eOwnerValue},
	}}
	if err := createEventuallyGone(c, created); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Not ours to own; useTargetNamespace reports why, per test.
			return noop
		}
		nsProvisionFailure = fmt.Sprintf("creating namespace %q failed: %v", ns, err)
		return noop
	}
	fmt.Printf("e2e: namespace %q was absent — created it for this run (deleted again at the end)\n", ns)
	return func() { deleteIfOwned(c, ns) }
}

// deleteIfOwned removes the namespace only while it still carries the owner label.
func deleteIfOwned(c client.Client, ns string) {
	ctx := context.Background()
	var cur corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: ns}, &cur); err != nil {
		if !apierrors.IsNotFound(err) {
			fmt.Printf("e2e: could not read namespace %q at teardown, leaving it: %v\n", ns, err)
		}
		return
	}
	if cur.Labels[e2eOwnerLabel] != e2eOwnerValue {
		return
	}
	// The UID precondition spares a namespace recreated since the read.
	if err := c.Delete(ctx, &cur, client.Preconditions{UID: &cur.UID}); err != nil {
		fmt.Printf("e2e: could not delete namespace %q at teardown: %v\n", ns, err)
	}
}

func resolveOperatorConfig() (map[string]string, error) {
	c, err := sharedClient()
	if err != nil {
		return nil, err
	}
	var cm corev1.ConfigMap
	key := client.ObjectKey{Name: operatorConfigName(), Namespace: operatorNamespace()}
	if err := c.Get(context.Background(), key, &cm); err != nil {
		return nil, err
	}
	return cm.Data, nil
}

func operatorNamespace() string { return env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system") }

// operatorConfigName follows E2E_OPERATOR_DEPLOYMENT: the chart names the ConfigMap after the release.
func operatorConfigName() string {
	return env("E2E_OPERATOR_CONFIGMAP", env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment)+"-config")
}

// webhookConfigName follows E2E_OPERATOR_DEPLOYMENT: the chart names the
// MutatingWebhookConfiguration <fullname>-mutating-webhook, and the Deployment <fullname>.
func webhookConfigName() string {
	return env("E2E_WEBHOOK_CONFIG_NAME", env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment)+"-mutating-webhook")
}

// balloonsDaemonSet is the NRI balloons plugin DaemonSet name.
func balloonsDaemonSet() string {
	if v := os.Getenv("E2E_BALLOONS_DAEMONSET"); v != "" {
		return v
	}
	if cfg, err := operatorConfig(); err == nil {
		if v := strings.TrimSpace(cfg["BALLOONS_DAEMONSET"]); v != "" {
			return v
		}
	}
	return "nri-resource-policy-balloons"
}

// clusterClient builds a client without a testing handle, for the resolvers.
func clusterClient() (client.Client, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}
	// On the config: RESTMapper discovery takes no context.
	cfg.Timeout = 30 * time.Second
	return client.New(cfg, client.Options{})
}

// operatorBalloonsDefault is the operator's own BALLOONS_NAMESPACE default.
const operatorBalloonsDefault = "kube-system"

// balloonsNamespace resolves the per-node BalloonsPolicy namespace and its source,
// preferring the operator's BALLOONS_NAMESPACE. Not memoized, so a failed read can recover.
func balloonsNamespace() (namespace, source string) {
	if v := os.Getenv("E2E_BALLOONS_NAMESPACE"); v != "" {
		return v, "E2E_BALLOONS_NAMESPACE"
	}
	if cfg, err := operatorConfig(); err == nil {
		// Present-but-empty means the operator's default.
		if v, ok := cfg["BALLOONS_NAMESPACE"]; ok {
			if v = strings.TrimSpace(v); v != "" {
				return v, "the operator's BALLOONS_NAMESPACE"
			}
			return operatorBalloonsDefault, "the operator's default for an empty BALLOONS_NAMESPACE"
		}
	}
	if ns, err := discoveredBalloonsNamespace(); err == nil {
		return ns, "the live plugin DaemonSet"
	}
	return operatorBalloonsDefault, "the operator's default, nothing else resolved"
}

// resolveDiscoveredBalloonsNamespace finds the plugin DaemonSet by name cluster-wide.
func resolveDiscoveredBalloonsNamespace() (string, error) {
	name := balloonsDaemonSet()
	c, err := sharedClient()
	if err != nil {
		return "", err
	}
	var daemonSets appsv1.DaemonSetList
	if err := c.List(context.Background(), &daemonSets); err != nil {
		return "", err
	}
	for i := range daemonSets.Items {
		if daemonSets.Items[i].Name == name {
			return daemonSets.Items[i].Namespace, nil
		}
	}
	return "", fmt.Errorf("no DaemonSet named %q in any namespace", name)
}

// resolveTargetNamespace reads an exact namespace name off an installed policy. Globs
// are skipped: they expand only against existing namespaces.
func resolveTargetNamespace() (string, error) {
	if v := os.Getenv("E2E_NAMESPACE"); v != "" {
		return v, nil
	}
	c, err := sharedClient()
	if err != nil {
		return "", fmt.Errorf("no usable kubeconfig: %w", err)
	}
	policies, err := installedPolicies(context.Background(), c)
	if err != nil {
		return "", err
	}
	for _, policy := range policies {
		namespaces, _, err := unstructured.NestedStringSlice(policy.Object, "spec", "namespaces")
		if err != nil {
			return "", fmt.Errorf("BalloonPinningPolicy %q has an unreadable spec.namespaces: %w", policy.GetName(), err)
		}
		for _, ns := range namespaces {
			if ns != "" && !isGlob(ns) {
				return ns, nil
			}
		}
	}
	return "", fmt.Errorf("no installed BalloonPinningPolicy names an exact namespace (only globs, or a namespaceSelector); "+
		"set E2E_NAMESPACE to a namespace a policy targets, or E2E_POLICY_NAME to select a policy other than %q", policyName())
}

// isGlob mirrors internal/match.IsGlob; e2e imports nothing from internal/.
func isGlob(pattern string) bool { return strings.ContainsAny(pattern, "*?[") }

// policyTargetsNamespace reports whether any installed policy targets ns by exact name,
// glob or namespaceSelector. Unknown answers true: a wrong false would report a scoping
// regression, a wrong true only skips.
func policyTargetsNamespace(ns string) bool {
	c, err := sharedClient()
	if err != nil {
		return true
	}
	policies, err := installedPolicies(context.Background(), c)
	if err != nil {
		return true
	}
	for _, policy := range policies {
		patterns, _, _ := unstructured.NestedStringSlice(policy.Object, "spec", "namespaces")
		for _, pattern := range patterns {
			if pattern == ns {
				return true
			}
			if isGlob(pattern) {
				if ok, err := path.Match(pattern, ns); err == nil && ok {
					return true
				}
			}
		}
		if targeted, ok := selectorTargetsNamespace(c, policy, ns); targeted || !ok {
			return true
		}
	}
	return false
}

// selectorTargetsNamespace resolves one policy's spec.namespaceSelector against ns's
// labels, and whether that could be determined at all.
func selectorTargetsNamespace(c client.Client, policy *unstructured.Unstructured, ns string) (targeted, determined bool) {
	raw, found, err := unstructured.NestedMap(policy.Object, "spec", "namespaceSelector")
	if err != nil {
		return false, false
	}
	if !found {
		return false, true
	}
	var ls metav1.LabelSelector
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &ls); err != nil {
		return false, false
	}
	// An empty selector {} matches every namespace, hence the presence test above.
	sel, err := metav1.LabelSelectorAsSelector(&ls)
	if err != nil {
		return false, false
	}
	var nsObj corev1.Namespace
	if err := c.Get(context.Background(), client.ObjectKey{Name: ns}, &nsObj); err != nil {
		return false, false
	}
	return sel.Matches(labels.Set(nsObj.Labels)), true
}

// installedPolicies returns the E2E_POLICY_NAME policy first, then every other one.
// No policy or no CRD is errNoPolicy.
func installedPolicies(ctx context.Context, c client.Client) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	named := newUnstructured(bppGVK)
	err := c.Get(ctx, client.ObjectKey{Name: policyName()}, named)
	switch {
	case err == nil:
		out = append(out, named)
	case apimeta.IsNoMatchError(err):
		return nil, fmt.Errorf("%w (its CRD is not installed)", errNoPolicy)
	case !apierrors.IsNotFound(err):
		// Don't fall through: another policy would be mutated by mistake.
		return nil, fmt.Errorf("reading BalloonPinningPolicy %q: %w", policyName(), err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(bppGVK)
	if err := c.List(ctx, list); err != nil {
		if apimeta.IsNoMatchError(err) {
			return nil, fmt.Errorf("%w (its CRD is not installed)", errNoPolicy)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("listing BalloonPinningPolicy: %w", err)
		}
		// RBAC scoped to one named resource is ordinary; keep the policy already read.
		fmt.Printf("e2e: listing BalloonPinningPolicy failed, continuing with %q alone: %v\n", policyName(), err)
		return out, nil
	}
	for i := range list.Items {
		if list.Items[i].GetName() != policyName() {
			out = append(out, &list.Items[i])
		}
	}
	if len(out) == 0 {
		return nil, errNoPolicy
	}
	return out, nil
}

// installedPolicyName is the policy the mutating cases edit: E2E_POLICY_NAME's, else
// the only one. It fails when several exist and none matches.
func installedPolicyName(t testing.TB, c client.Client) string {
	t.Helper()
	policies, err := installedPolicies(context.Background(), c)
	switch {
	case errors.Is(err, errNoPolicy):
		skipNoCapability(t, "%v, so the cluster cannot exercise pinning", err)
	case err != nil:
		t.Fatalf("resolving the BalloonPinningPolicy to edit: %v", err)
	}
	if name := policies[0].GetName(); name == policyName() || len(policies) == 1 {
		return name
	}
	t.Fatalf("%d BalloonPinningPolicies are installed and none is named %q; set E2E_POLICY_NAME to the one the tests should edit",
		len(policies), policyName())
	return ""
}

// newClient builds a client, skipping when no kubeconfig is available.
func newClient(t testing.TB) client.Client {
	t.Helper()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		skipNoCapability(t, "no kubeconfig available, skipping e2e: %v", err)
	}
	armSweep()
	if err := sweepInitErr(); err != nil {
		t.Fatalf("e2e sweep: not ready (leftovers or no shared-object snapshot), so no test may change the cluster: %v", err)
	}
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatalf("failed to build client: %v", err)
	}
	return c
}

func newUnstructured(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}

// balloonsEnabledNode returns a node the operator provisioned, or skips.
func balloonsEnabledNode(t testing.TB, c client.Client) string {
	t.Helper()
	var nodes corev1.NodeList
	if err := c.List(context.Background(), &nodes); err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	for i := range nodes.Items {
		if nodes.Items[i].Labels[labelBalloonsEnabled] == "true" {
			return nodes.Items[i].Name
		}
	}
	skipNoCapability(t, "no node carries balloons-enabled=true; operator not provisioning any node")
	return ""
}

// clusterMaxSpan mirrors the webhook's cap for an unbound pod: the widest node's max-span.
func clusterMaxSpan(t testing.TB, c client.Client) int {
	_, span := widestSpanNode(t, c)
	return span
}

// widestSpanNode returns the balloons-enabled node with the largest max-span (missing
// or invalid = 1); binding there makes the node cap equal the cluster cap.
func widestSpanNode(t testing.TB, c client.Client) (string, int) {
	t.Helper()
	balloonsEnabledNode(t, c)
	var nodes corev1.NodeList
	if err := c.List(context.Background(), &nodes); err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	name, maxSpan := "", 0
	for i := range nodes.Items {
		labels := nodes.Items[i].Labels
		if labels[labelBalloonsEnabled] != "true" {
			continue
		}
		span := 1
		if v, err := strconv.Atoi(labels[labelMaxSpan]); err == nil && v > 0 {
			span = v
		}
		if span > maxSpan {
			name, maxSpan = nodes.Items[i].Name, span
		}
	}
	return name, maxSpan
}

func clusterCPUAllocation(t testing.TB, c client.Client) string {
	t.Helper()
	bcc := newUnstructured(bccGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "cluster"}, bcc); err != nil {
		if apierrors.IsNotFound(err) {
			return "physical-cores" // operator default when the singleton is absent
		}
		t.Fatalf("get BalloonsClusterConfig/cluster: %v", err)
	}
	alloc, _, _ := unstructured.NestedString(bcc.Object, "spec", "balloon", "cpuAllocation")
	if alloc == "" {
		return "physical-cores"
	}
	return alloc
}

// TestNodeProvisioning proves a provisioned node has topology labels and a BalloonsPolicy.
func TestNodeProvisioning(t *testing.T) {
	at := allureCase(t)
	c := newClient(at)
	node := balloonsEnabledNode(at, c)

	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		at.Fatalf("get node %s: %v", node, err)
	}
	for _, label := range []string{labelMaxSpan, labelNumaNodes, labelPhysicalCPUs, labelThreadsPerCore} {
		if _, ok := n.Labels[label]; !ok {
			at.Errorf("node %s missing topology label %s", node, label)
		}
	}

	bp := newUnstructured(bpGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "node." + node, Namespace: balloonsNS()}, bp); err != nil {
		at.Fatalf("get BalloonsPolicy node.%s: %v", node, err)
	}
	types, _, _ := unstructured.NestedSlice(bp.Object, "spec", "balloonTypes")
	if len(types) == 0 {
		at.Fatal("BalloonsPolicy has no balloonTypes")
	}
	if !hasBalloonType(types, "reserved") {
		at.Error("BalloonsPolicy missing the reserved balloon type")
	}
	if !hasBalloonType(types, "balloon-span1") {
		at.Error("BalloonsPolicy missing balloon-span1")
	}
}

func hasBalloonType(types []interface{}, name string) bool {
	for _, tt := range types {
		if m, ok := tt.(map[string]interface{}); ok {
			if n, _, _ := unstructured.NestedString(m, "name"); n == name {
				return true
			}
		}
	}
	return false
}

// TestPhysicalCoresExcludeSiblings proves physical-cores mode excludes HT siblings via exclude-cpuset.
func TestPhysicalCoresExcludeSiblings(t *testing.T) {
	at := allureCase(t)
	c := newClient(at)
	if alloc := clusterCPUAllocation(t, c); alloc != "physical-cores" {
		skipNoConfig(at, "cluster cpuAllocation is %q, not physical-cores; skipping the sibling-exclusion check", alloc)
	}
	node := balloonsEnabledNode(at, c)

	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		at.Fatalf("get node: %v", err)
	}
	threads, _ := strconv.Atoi(n.Labels[labelThreadsPerCore])
	if threads < 2 {
		skipNoCapability(at, "node %s has threads-per-core=%d (no hyperthreads); nothing to exclude", node, threads)
	}

	bp := newUnstructured(bpGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "node." + node, Namespace: balloonsNS()}, bp); err != nil {
		at.Fatalf("get BalloonsPolicy: %v", err)
	}

	cpu, _, _ := unstructured.NestedString(bp.Object, "spec", "availableResources", "cpu")
	if !strings.HasPrefix(cpu, "exclude-cpuset:") {
		at.Fatalf("availableResources.cpu = %q, want an exclude-cpuset for physical-cores", cpu)
	}
	if strings.TrimPrefix(cpu, "exclude-cpuset:") == "" {
		at.Fatal("exclude-cpuset is empty")
	}

	// Leaf balloons must not carry hideHyperthreads.
	types, _, _ := unstructured.NestedSlice(bp.Object, "spec", "balloonTypes")
	for _, tt := range types {
		m, ok := tt.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "name")
		if _, found, _ := unstructured.NestedBool(m, "hideHyperthreads"); found {
			at.Errorf("balloon %s carries hideHyperthreads in physical-cores mode (should be dropped)", name)
		}
	}
	at.Logf("node %s physical-cores exclude-cpuset = %s", node, cpu)
}

// TestWebhookStampsWholeCoreVLLM proves the webhook stamps a matched whole-core vLLM
// pod (balloon, pinned-by/hash, init container); actual pinning is checked elsewhere.
func TestWebhookStampsWholeCoreVLLM(t *testing.T) {
	at := allureCase(t)
	c := newClient(at)
	ns := useTargetNamespace(at, c)

	// The pod is unbound, so the webhook caps its span by the cluster-wide maximum.
	maxTP := clusterMaxSpan(at, c)

	// Span comes from the policy's intent, not the pod arg alone; set it. Self-restored.
	applyVLLMSpanIntent(t, c)

	wantSpan := snap(2)
	if maxTP > 0 && wantSpan > maxTP {
		wantSpan = largestSpanAtMost(maxTP)
	}

	balloonKey := balloonAnnotationBase + "/container.vllm"
	want := fmt.Sprintf("balloon-span%d", wantSpan)
	// Recreate until the intent edit is live on every webhook replica.
	created := admitVLLMUntil(t, c, ns, "e2e-pin-tp2", 2 /*tp*/, func(p *corev1.Pod) bool {
		return p.Annotations[balloonKey] == want
	})

	if got := created.Annotations[balloonKey]; got != want {
		at.Errorf("annotation %s = %q, want %s (TP=2, cluster max-span=%d)", balloonKey, got, want, maxTP)
	}
	if created.Annotations[pinnedByAnnotation] != "cpu-pinning-operator" {
		at.Errorf("pinned-by = %q, want cpu-pinning-operator", created.Annotations[pinnedByAnnotation])
	}
	if created.Annotations[pinnedHashAnnotation] == "" {
		at.Error("pinned-hash annotation missing")
	}

	// The init container is injected only when the readiness gate is configured.
	gateConfigured, known := readinessGateConfigured(t)
	ic := initContainer(created, waitForBalloonsInit)
	switch {
	case !known:
		at.Log("operator config unreadable — cannot determine whether the readiness gate is configured; skipping wait-for-balloons injection assertion")
	case gateConfigured:
		if ic == nil {
			at.Fatal("the readiness gate is configured but the wait-for-balloons init container was not injected")
		}
		// It must poll via the operator subcommand, not kubectl or a shell.
		cmdline := strings.Join(append(append([]string{}, ic.Command...), ic.Args...), " ")
		for _, banned := range []string{"kubectl", "/bin/sh", "/bin/bash"} {
			if strings.Contains(cmdline, banned) {
				at.Errorf("init container still references %q (%q) — the readiness refactor removed shell/kubectl", banned, cmdline)
			}
		}
		if !strings.Contains(cmdline, "wait-for-balloons") {
			at.Errorf("init container does not invoke the wait-for-balloons subcommand: %q", cmdline)
		}
		// An https endpoint needs the synced CA ConfigMap mounted.
		if cfg, err := operatorConfig(); err == nil && strings.HasPrefix(cfg["BALLOONS_READY_ENDPOINT"], "https://") {
			caCM := cfg["READINESS_CA_CONFIGMAP"]
			mounted := false
			for _, v := range created.Spec.Volumes {
				if v.ConfigMap != nil && v.ConfigMap.Name == caCM {
					mounted = true
				}
			}
			if !mounted || !strings.Contains(cmdline, "--ca-file=") {
				at.Errorf("the endpoint is https but the init container does not mount CA ConfigMap %q with --ca-file: %q", caCM, cmdline)
			}
			cm := &corev1.ConfigMap{}
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: caCM}, cm); err != nil {
				at.Errorf("CA ConfigMap %s/%s: %v", ns, caCM, err)
			} else if !x509.NewCertPool().AppendCertsFromPEM([]byte(cm.Data["ca.crt"])) {
				at.Errorf("CA ConfigMap %s/%s holds no PEM ca.crt", ns, caCM)
			}
		}
	default: // known && !gateConfigured: fail-open profile, no init container expected.
		if ic != nil {
			at.Error("the readiness gate is not configured but the wait-for-balloons init container WAS injected (the webhook should fail open and skip it)")
		}
	}
}

// vllmSpanArgRegex mirrors the chart's default balloonPinningPolicy.intent.span argRegex.
const vllmSpanArgRegex = `(?:^|\s)(?:--tensor-parallel-size|-tp)(?:=|\s+)(\d+)`

// editPolicyOnConflict re-reads the policy and applies edit until an Update lands
// without a Conflict; the operator's status writes move resourceVersion between a read
// and the write. Any other error is returned at once.
func editPolicyOnConflict(ctx context.Context, c client.Client, key client.ObjectKey, edit func(*unstructured.Unstructured) error) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cur := newUnstructured(bppGVK)
		if err := c.Get(ctx, key, cur); err != nil {
			return err
		}
		if err := edit(cur); err != nil {
			return err
		}
		return c.Update(ctx, cur)
	})
}

// restoreIntent puts back the original spec.intent, or removes it when there was none.
func restoreIntent(orig map[string]interface{}, had bool) func(*unstructured.Unstructured) error {
	return func(p *unstructured.Unstructured) error {
		if had {
			return unstructured.SetNestedMap(p.Object, orig, "spec", "intent")
		}
		unstructured.RemoveNestedField(p.Object, "spec", "intent")
		return nil
	}
}

// applyVLLMSpanIntent sets the policy's span intent to vllmSpanArgRegex, restores it
// on cleanup, and waits until the edit is live in the webhook.
func applyVLLMSpanIntent(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	key := client.ObjectKey{Name: installedPolicyName(t, c)}

	bpp := newUnstructured(bppGVK)
	if err := c.Get(ctx, key, bpp); err != nil {
		t.Fatalf("get BalloonPinningPolicy %q: %v", key.Name, err)
	}

	// NestedMap returns a deep copy, safe to stash for restore.
	origIntent, hadIntent, _ := unstructured.NestedMap(bpp.Object, "spec", "intent")
	t.Cleanup(func() {
		err := editPolicyOnConflict(context.Background(), c, key, restoreIntent(origIntent, hadIntent))
		if err != nil {
			t.Errorf("cleanup: restore BalloonPinningPolicy %q spec.intent: %v", key.Name, err)
			return
		}
		// Wait until the restored intent is live, so later tests start from baseline.
		if !waitForPolicySpecSettled(c, key, e2eSettleTimeout) {
			t.Logf("cleanup: policy %q observedGeneration did not catch up after restoring spec.intent; a later test may briefly see stale intent", key.Name)
		}
	})

	err := editPolicyOnConflict(ctx, c, key, func(p *unstructured.Unstructured) error {
		if err := unstructured.SetNestedField(p.Object, vllmSpanArgRegex, "spec", "intent", "span", "argRegex"); err != nil {
			return err
		}
		return unstructured.SetNestedField(p.Object, int64(1), "spec", "intent", "span", "default")
	})
	if err != nil {
		t.Fatalf("update BalloonPinningPolicy %q intent (a guardActivePods policy holding bound pods can reject a narrowing edit): %v", key.Name, err)
	}

	// Unlike the cleanup barrier, this one is a hard precondition.
	if !waitForPolicySpecSettled(c, key, 90*time.Second) {
		t.Fatalf("policy %q observedGeneration never caught up to metadata.generation; reconciler did not pick up the intent edit", key.Name)
	}
}

// admitVLLMUntil creates vLLM probe pods until one satisfies check or 45s pass, and
// returns the last; this absorbs the lag before every webhook replica sees an edit.
func admitVLLMUntil(t *testing.T, c client.Client, ns, base string, tp int, check func(*corev1.Pod) bool) *corev1.Pod {
	t.Helper()
	req := strconv.Itoa(tp)
	deadline := time.Now().Add(45 * time.Second)
	var last *corev1.Pod
	for i := 0; ; i++ {
		last = createAndCleanup(t, c, vllmPod(fmt.Sprintf("%s-%d", base, i), ns, req, req, tp))
		if check(last) || time.Now().After(deadline) {
			return last
		}
		// Delete now so retries don't pile up pods on a small cluster.
		_ = c.Delete(context.Background(), last, client.GracePeriodSeconds(0))
		time.Sleep(3 * time.Second)
	}
}

// readinessGateConfigured reports whether BALLOONS_READY_ENDPOINT and INIT_CONTAINER_IMAGE
// are both set, the condition for injecting wait-for-balloons; known is false when unreadable.
func readinessGateConfigured(t *testing.T) (configured bool, known bool) {
	t.Helper()
	cfg, err := operatorConfig()
	if err != nil {
		// Unknown, not guessed; log why.
		t.Logf("readinessGateConfigured: cannot read operator config %s/%s: %v (treating injection gate as unknown)",
			operatorNamespace(), operatorConfigName(), err)
		return false, false
	}
	return strings.TrimSpace(cfg["BALLOONS_READY_ENDPOINT"]) != "" &&
		strings.TrimSpace(cfg["INIT_CONTAINER_IMAGE"]) != "", true
}

// TestWebhookSpanSnapsUpCappedByMaxSpan proves TP=3 snaps up to span 4, capped by the
// cluster max-span.
func TestWebhookSpanSnapsUpCappedByMaxSpan(t *testing.T) {
	at := allureCase(t)
	c := newClient(at)
	ns := useTargetNamespace(at, c)

	maxTP := clusterMaxSpan(at, c)
	wantSpan := snap(3)
	if wantSpan > maxTP {
		wantSpan = largestSpanAtMost(maxTP)
	}

	pod := vllmPod("e2e-pin-tp3", ns, "4", "4", 3 /*tp*/)
	created := createAndCleanup(t, c, pod)

	balloonKey := balloonAnnotationBase + "/container.vllm"
	want := fmt.Sprintf("balloon-span%d", wantSpan)
	if got := created.Annotations[balloonKey]; got != want {
		at.Errorf("annotation %s = %q, want %s (TP=3 snaps to %d, capped by cluster max-span=%d)",
			balloonKey, got, want, snap(3), maxTP)
	}
}

// TestWebhookStampsKServePredictor proves a KServe predictor pod (kserve-container,
// TP=2) is pinned by the default policy.
func TestWebhookStampsKServePredictor(t *testing.T) {
	at := allureCase(t)
	c := newClient(at)
	ns := useTargetNamespace(at, c)

	// Resolved, so a renamed policy is still exercised.
	name := installedPolicyName(at, c)
	policy := newUnstructured(bppGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, policy); err != nil {
		at.Fatalf("get BalloonPinningPolicy %q: %v", name, err)
	}
	names, _, _ := unstructured.NestedStringSlice(policy.Object, "spec", "match", "byContainerName")
	if !slices.Contains(names, kserveContainer) {
		skipNoConfig(at, "BalloonPinningPolicy %q matches %v, not %s; install with "+
			"--set 'balloonPinningPolicy.match.byContainerName={vllm,%s}'", name, names, kserveContainer, kserveContainer)
	}

	// The pod is unbound, so the webhook caps its span by the cluster-wide maximum.
	maxTP := clusterMaxSpan(at, c)
	wantSpan := snap(2)
	if maxTP > 0 && wantSpan > maxTP {
		wantSpan = largestSpanAtMost(maxTP)
	}

	pod := vllmPod("e2e-kserve-tp2", ns, "2", "2", 2)
	pod.Spec.Containers[0].Name = kserveContainer
	pod.Spec.Containers[0].Args = []string{"--model_name=e2e-kserve", "--tensor-parallel-size=2"}
	created := createAndCleanup(t, c, pod)

	balloonKey := balloonAnnotationBase + "/container." + kserveContainer
	want := fmt.Sprintf("balloon-span%d", wantSpan)
	if got := created.Annotations[balloonKey]; got != want {
		at.Errorf("annotation %s = %q, want %s (TP=2, cluster max-span=%d)", balloonKey, got, want, maxTP)
	}
	if created.Annotations[pinnedByAnnotation] != "cpu-pinning-operator" {
		at.Errorf("pinned-by = %q, want cpu-pinning-operator", created.Annotations[pinnedByAnnotation])
	}
}

// kserveContainer is KServe's predictor model-server container name.
const kserveContainer = "kserve-container"

// snap mirrors match.Snap: round up to the nearest of {1,2,4,8}.
func snap(n int) int {
	for _, s := range []int{1, 2, 4, 8} {
		if n <= s {
			return s
		}
	}
	return 8
}

// largestSpanAtMost mirrors the cap: the largest supported span <= limit.
func largestSpanAtMost(limit int) int {
	best := 1
	for _, s := range []int{1, 2, 4, 8} {
		if s <= limit {
			best = s
		}
	}
	return best
}

// TestWebhookSkipsFractionalCPU proves a fractional-CPU pod is not pinned, against a
// whole-core control pod that must be.
func TestWebhookSkipsFractionalCPU(t *testing.T) {
	at := allureCase(t)
	c := newClient(at)
	ns := useTargetNamespace(at, c)
	// With no provisioned node, enforcePinning would deny the control pod.
	balloonsEnabledNode(at, c)

	control := createAndCleanup(t, c, vllmPod("e2e-whole-control", ns, "1", "1", 1))
	if control.Annotations[pinnedByAnnotation] != "cpu-pinning-operator" {
		at.Fatalf("the whole-core control pod in %q was not pinned, so this test cannot tell the whole-core gate "+
			"from a webhook that is not intercepting %q at all", ns, ns)
	}

	// 500m is not whole cores.
	pod := vllmPod("e2e-frac", ns, "500m", "500m", 1)
	created := createAndCleanup(t, c, pod)

	for k := range created.Annotations {
		if strings.HasPrefix(k, balloonAnnotationBase) {
			at.Errorf("fractional-CPU pod got balloon annotation %s (should be skipped)", k)
		}
	}
	if created.Annotations[pinnedByAnnotation] != "" {
		at.Error("fractional-CPU pod was marked pinned-by")
	}
}

// TestWebhookIgnoresUnmatchedNamespace proves a pod in an untargeted namespace is not mutated.
func TestWebhookIgnoresUnmatchedNamespace(t *testing.T) {
	at := allureCase(t)
	c := newClient(at)
	// "default" is normally in no policy's namespaces.
	ns := "default"
	// The premise needs a provably unmatched namespace.
	if policyTargetsNamespace(ns) {
		skipNoConfig(at, "%q is not a provably unmatched namespace on this cluster: a policy targets it by name, glob or namespaceSelector, or "+
			"the policies and its labels could not be read to tell", ns)
	}

	pod := vllmPod("e2e-unmatched", ns, "2", "2", 2)
	created := createAndCleanup(t, c, pod)

	for k := range created.Annotations {
		if strings.HasPrefix(k, balloonAnnotationBase) {
			at.Errorf("pod in unmatched namespace %q got balloon annotation %s", ns, k)
		}
	}
	if hasInitContainer(created, waitForBalloonsInit) {
		at.Errorf("pod in unmatched namespace %q got wait-for-balloons init injected", ns)
	}
}

// -- namespace label-selector targeting ---------------------------------------

// TestWebhookTargetsNamespaceByLabel proves a namespace a policy targets only through
// spec.namespaceSelector is mutated (E2E_MUTATING). It creates a uniquely labelled
// namespace and a policy selecting that label, and deletes both. E2E_LABEL_NAMESPACE
// instead names a namespace an installed policy already targets by label.
func TestWebhookTargetsNamespaceByLabel(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(e2eMutatingGate) == "" {
		skipNotSelected(at, "set %s=1 to run label-selector targeting (it creates a namespace and a BalloonPinningPolicy)", e2eMutatingGate)
	}
	c := newClient(at)
	ctx := context.Background()
	ns := os.Getenv("E2E_LABEL_NAMESPACE")
	if ns != "" {
		requireNamespace(at, c, ns)
	} else {
		suffix := utilrand.String(6)
		ns = "cpu-pinning-e2e-label-" + suffix
		policyName := "e2e-label-" + suffix
		// The label value is the namespace's own name, so a leftover from another run never matches.
		const labelKey = "cpu-pinning-e2e/label-target"
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{labelKey: ns}}}
		if err := e2eCreate(ctx, c, nsObj); err != nil {
			at.Fatalf("create namespace %s: %v", ns, err)
		}
		t.Cleanup(func() {
			e2eDelete(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		policy := newUnstructured(bppGVK)
		policy.SetName(policyName)
		policy.Object["spec"] = map[string]interface{}{
			"match": map[string]interface{}{"byContainerName": []interface{}{"vllm"}},
			"namespaceSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{labelKey: ns},
			},
		}
		if err := e2eCreate(ctx, c, policy); err != nil {
			at.Fatalf("create BalloonPinningPolicy %s: %v", policyName, err)
		}
		t.Cleanup(func() {
			p := newUnstructured(bppGVK)
			p.SetName(policyName)
			e2eDelete(t, c, p)
		})

		// The policy reconciler adds ns to the webhook selector asynchronously.
		whName := webhookConfigName()
		nsLabels := labels.Set{labelKey: ns, "kubernetes.io/metadata.name": ns}
		deadline := time.Now().Add(2 * time.Minute)
		for !liveWebhookSelector(at.T, c, whName).Matches(nsLabels) {
			if time.Now().After(deadline) {
				at.Fatalf("webhook %s namespaceSelector never included %q after creating policy %s", whName, ns, policyName)
			}
			time.Sleep(3 * time.Second)
		}
	}

	// A selector that includes ns is not every webhook replica serving the policy yet,
	// so fresh probe pods are created until one is stamped or the deadline passes.
	stamped := func(p *corev1.Pod) bool {
		for k := range p.Annotations {
			if strings.HasPrefix(k, balloonAnnotationBase) {
				return true
			}
		}
		return false
	}
	var created *corev1.Pod
	for i, deadline := 0, time.Now().Add(45*time.Second); ; i++ {
		created = createAndCleanup(t, c, vllmPod(fmt.Sprintf("e2e-label-selected-%d", i), ns, "2", "2", 2))
		if stamped(created) || time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !stamped(created) {
		at.Errorf("pod in label-selected namespace %q got no balloon annotation — label-selector targeting did not apply", ns)
	}
	if !hasInitContainer(created, waitForBalloonsInit) {
		at.Errorf("pod in label-selected namespace %q got no wait-for-balloons init injected", ns)
	}
}

// TestNodeResourceTopologyPublished proves the node's NodeResourceTopology has zones.
func TestNodeResourceTopologyPublished(t *testing.T) {
	at := allureCase(t)
	c := newClient(at)
	node := balloonsEnabledNode(at, c)

	nrt := newUnstructured(nrtGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, nrt); err != nil {
		if apierrors.IsNotFound(err) {
			skipNoConfig(at, "no NodeResourceTopology for %s yet (NRI agent.nodeResourceTopology may be off)", node)
		}
		at.Fatalf("get NRT %s: %v", node, err)
	}
	zones, _, _ := unstructured.NestedSlice(nrt.Object, "zones")
	if len(zones) == 0 {
		at.Fatal("NodeResourceTopology has no zones; cpu_assignment metric would be empty")
	}
}

// -- reserved-pool sizing + pressure metrics ---------------------------------

// TestReservedPoolMetricsExported proves the reserved-pool size and utilization gauges
// are exported for a provisioned node (E2E_METRICS).
func TestReservedPoolMetricsExported(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_METRICS") == "" {
		skipNotSelected(at, "set E2E_METRICS=1 to scrape the operator metrics endpoint via the apiserver proxy")
	}
	c := newClient(at)
	node := balloonsEnabledNode(at, c)

	// A fresh manager publishes both only after its first node reconcile.
	body := scrapeOperatorMetricsForNode(t, node,
		"cpu_pinning_node_reserved_cpus", "cpu_pinning_reserved_pool_utilization_ratio")

	// The applied reserved-pool size.
	if !metricPresentForNode(body, "cpu_pinning_node_reserved_cpus", node) {
		at.Errorf("metrics missing cpu_pinning_node_reserved_cpus for node %q", node)
	}
	// The pressure gauge.
	if !metricPresentForNode(body, "cpu_pinning_reserved_pool_utilization_ratio", node) {
		at.Errorf("metrics missing cpu_pinning_reserved_pool_utilization_ratio for node %q", node)
	}
	// demand/size, so ≥ 0.
	if v, ok := metricValueForNode(body, "cpu_pinning_reserved_pool_utilization_ratio", node); !ok {
		at.Errorf("could not parse cpu_pinning_reserved_pool_utilization_ratio sample for node %q", node)
	} else if v < 0 {
		at.Errorf("reserved_pool_utilization_ratio = %v, want ≥ 0", v)
	}
}

// scrapeOperatorMetrics scrapes the leader pod via the apiserver proxy, or skips.
// Only the leader sets the per-node gauges, so the Service would round-robin to nothing.
func scrapeOperatorMetrics(t testing.TB) string {
	t.Helper()
	body, target, err := scrapeLeaderMetrics(t)
	if err == nil {
		return body
	}
	// A refusal is a deployment mode; anything else is a config gap.
	skipNoMetrics(t, metricsAuthRejected(err), "could not scrape leader metrics via apiserver pod proxy (%s): %v", target, err)
	return ""
}

// scrapeLeaderMetrics returns the leader's exposition, the target and the raw error.
// The pod proxy carries no credential, so metrics client auth answers it 401.
func scrapeLeaderMetrics(t testing.TB) (body, target string, err error) {
	t.Helper()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		skipNoCapability(t, "no kubeconfig available: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build clientset: %v", err)
	}
	opNS := env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system")
	port := env("E2E_METRICS_PORT", "8080")
	// The apiserver terminates backend TLS, so no serving-cert CA is needed.
	scheme := metricsProxyScheme(t)
	pod := leaderPodNameAfterHandover(t, cs, opNS)
	target = fmt.Sprintf("%s/%s:%s", opNS, pod, port)
	// Started after discovery, so a slow handover does not eat the scrape's budget.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := cs.CoreV1().RESTClient().Get().
		Namespace(opNS).
		Resource("pods").
		Name(fmt.Sprintf("%s:%s:%s", scheme, pod, port)).
		SubResource("proxy").
		Suffix("metrics").
		DoRaw(ctx)
	return string(raw), target, err
}

// leaderPodName resolves the leader pod from the Lease holderIdentity ("<podName>_<uuid>"),
// skipping as a config gap when there is no live leader. A rollout ends before a new
// replica takes the Lease, so an unreadable, empty or stale one is retried until ctx
// ends; a caller that must ride out a handover passes a leaderLeaseWait context.
func leaderPodName(ctx context.Context, t testing.TB, cs kubernetes.Interface, opNS string) string {
	t.Helper()
	// The chart names the Lease after the release, not cmd/main.go's default.
	leaseName := env("E2E_LEADER_LEASE", operatorDeployment)
	start := time.Now()
	for {
		pod, why := leaderPodOnce(ctx, cs, opNS, leaseName)
		if why == "" {
			return pod
		}
		select {
		case <-ctx.Done():
			skipNoConfig(t, "%s (still so after %s: %v)", why, time.Since(start).Round(time.Second), ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

// leaderLeaseWait is the discovery budget that covers a handover, about one lease duration.
const leaderLeaseWait = 2 * time.Minute

// leaderPodNameAfterHandover is leaderPodName with its own leaderLeaseWait budget, apart
// from the caller's API-call context.
func leaderPodNameAfterHandover(t testing.TB, cs kubernetes.Interface, opNS string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), leaderLeaseWait)
	defer cancel()
	return leaderPodName(ctx, t, cs, opNS)
}

// leaderPodOnce reads the Lease once: the holder's pod name, or why there is no live leader.
func leaderPodOnce(ctx context.Context, cs kubernetes.Interface, opNS, leaseName string) (pod, why string) {
	lease, err := cs.CoordinationV1().Leases(opNS).Get(ctx, leaseName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Sprintf("could not read leader-election Lease %s/%s (operator may not be installed): %v", opNS, leaseName, err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return "", fmt.Sprintf("leader-election Lease %s/%s has no holderIdentity: no leader elected yet", opNS, leaseName)
	}
	holder := *lease.Spec.HolderIdentity
	// The pod name is the part before the first "_", as internal/webhook holderPodName reads it.
	pod, _, _ = strings.Cut(holder, "_")
	if pod == "" {
		return "", fmt.Sprintf("leader-election Lease %s/%s holderIdentity %q did not yield a pod name", opNS, leaseName, holder)
	}
	// A stale Lease may name a gone pod.
	if _, err := cs.CoreV1().Pods(opNS).Get(ctx, pod, metav1.GetOptions{}); err != nil {
		return "", fmt.Sprintf("leader pod %s/%s not found, Lease %s may be stale: %v", opNS, pod, leaseName, err)
	}
	return pod, ""
}

// -- per-node canvas dashboard ConfigMap (gated) -----------------------------

// TestPerNodeDashboardConfigMap proves a per-node canvas dashboard ConfigMap exists (E2E_DASHBOARDS).
func TestPerNodeDashboardConfigMap(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_DASHBOARDS") == "" {
		skipNotSelected(at, "set E2E_DASHBOARDS=1 to assert per-node dashboard ConfigMaps (grafanaDashboard.enabled)")
	}
	c := newClient(at)
	node := balloonsEnabledNode(at, c)
	dashNS := env("E2E_DASHBOARD_NAMESPACE", "cpu-pinning-operator-system")

	var cms corev1.ConfigMapList
	if err := c.List(context.Background(), &cms,
		client.InNamespace(dashNS),
		client.MatchingLabels{"cpu-pinning-operator.intel.com/dashboard": "node-canvas"}); err != nil {
		at.Fatalf("list dashboard configmaps: %v", err)
	}
	found := false
	for i := range cms.Items {
		if cms.Items[i].Labels["cpu-pinning-operator.intel.com/node"] == node {
			found = true
			if _, ok := cms.Items[i].Data["cpu-pinning-canvas.json"]; !ok {
				at.Errorf("dashboard ConfigMap %s has no canvas JSON", cms.Items[i].Name)
			}
		}
	}
	// None at all looks like grafanaDashboard.enabled=false (the default): a config gap.
	if !found {
		names := make([]string, 0, len(cms.Items))
		for i := range cms.Items {
			names = append(names, cms.Items[i].Name)
		}
		skipNoConfig(at, "no per-node dashboard ConfigMap for node %s in %s (%d dashboard ConfigMap(s) present there: %v) — "+
			"install with --set grafanaDashboard.enabled=true, and point E2E_DASHBOARD_NAMESPACE at grafanaDashboard.namespace if it "+
			"is not the release namespace", node, dashNS, len(cms.Items), names)
	}
}

// -- Actual NRI pinning, verified in NodeResourceTopology (gated) ------------

// TestActualPinningNoCollision proves, from NRT, that NRI pinned two pods to the
// requested CPU count with disjoint logical CPUs; in physical-cores mode it also
// checks they share no physical core (E2E_RUN_PINNING).
func TestActualPinningNoCollision(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_RUN_PINNING") == "" {
		skipNotSelected(at, "set E2E_RUN_PINNING=1 to run the live pinning test (needs the balloons DaemonSet)")
	}
	c := newClient(at)
	ns := useTargetNamespace(at, c)
	// Bind both pods to one node: collisions only make sense within a node.
	node, _ := widestSpanNode(at, c)

	const reqCPU = 4
	for _, name := range []string{"e2e-live-a", "e2e-live-b"} {
		p := vllmPod(name, ns, strconv.Itoa(reqCPU), strconv.Itoa(reqCPU), 2)
		p.Spec.NodeName = node
		createAndCleanup(t, c, p)
	}

	// Poll NRT until both containers have an assigned cpuset.
	deadline := time.Now().Add(6 * time.Minute)
	var setA, setB string
	for time.Now().Before(deadline) {
		setA = containerCPUSet(t, c, node, ns, "e2e-live-a", "vllm")
		setB = containerCPUSet(t, c, node, ns, "e2e-live-b", "vllm")
		if setA != "" && setB != "" {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if setA == "" || setB == "" {
		at.Fatalf("NRT shows no pinned cpuset for one/both pods (a=%q b=%q); "+
			"NRI did not pin on node %s — check the balloons DaemonSet and the wait-for-balloons init container's logs", setA, setB, node)
	}
	at.Logf("NRT cpusets on %s: e2e-live-a=%s  e2e-live-b=%s", node, setA, setB)

	a := parseCPUSet(setA)
	b := parseCPUSet(setB)

	if len(a) != reqCPU {
		at.Errorf("pod a pinned to %d CPUs (%s), want %d", len(a), setA, reqCPU)
	}
	if len(b) != reqCPU {
		at.Errorf("pod b pinned to %d CPUs (%s), want %d", len(b), setB, reqCPU)
	}

	for cpu := range a {
		if b[cpu] {
			at.Errorf("CPU %d assigned to BOTH pods (NRT overlap)", cpu)
		}
	}

	// In physical-cores mode, no physical core may be shared either.
	if clusterCPUAllocation(t, c) == "physical-cores" {
		cpuCore := cpuToCore(t, c, node)
		coresA := coresOf(a, cpuCore)
		coresB := coresOf(b, cpuCore)
		for core := range coresA {
			if coresB[core] {
				at.Errorf("physical-cores collision: core %d shared across both pods "+
					"(a=%s b=%s) — sibling exclusion failed", core, setA, setB)
			}
		}
	}
}

// cpuToCore maps logical CPU to physical core as cpu % physicalCPUs, the x86
// enumeration the operator's probe assumes; NRT carries no core id. Empty when unlabelled.
func cpuToCore(t *testing.T, c client.Client, node string) map[int]int {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		t.Fatalf("get node: %v", err)
	}
	phys, _ := strconv.Atoi(n.Labels[labelPhysicalCPUs])
	threads, _ := strconv.Atoi(n.Labels[labelThreadsPerCore])
	m := map[int]int{}
	if phys == 0 || threads == 0 {
		return m
	}
	for cpu := 0; cpu < phys*threads; cpu++ {
		m[cpu] = cpu % phys
	}
	return m
}

func coresOf(cpus map[int]bool, cpuCore map[int]int) map[int]bool {
	out := map[int]bool{}
	for cpu := range cpus {
		if core, ok := cpuCore[cpu]; ok {
			out[core] = true
		} else {
			out[cpu] = true // unknown mapping: treat the cpu id as its own core
		}
	}
	return out
}

// -- Cluster saturation: fill capacity, assert no collisions + bounded pool --

// TestClusterSaturationNoCollision proves an oversubscribed node never double-books a
// core and leaves the overflow unpinned (E2E_RUN_PINNING + E2E_SATURATE).
func TestClusterSaturationNoCollision(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_RUN_PINNING") == "" || os.Getenv("E2E_SATURATE") == "" {
		skipNotSelected(at, "set E2E_RUN_PINNING=1 and E2E_SATURATE=1 to run the cluster-saturation test")
	}
	c := newClient(at)
	ns := useTargetNamespace(at, c)
	node, _ := widestSpanNode(at, c) // the bound-node span cap then equals the unbound one

	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, &n); err != nil {
		at.Fatalf("get node: %v", err)
	}
	phys, _ := strconv.Atoi(n.Labels[labelPhysicalCPUs])
	if phys == 0 {
		skipNoConfig(at, "node has no physical-cpus label")
	}
	physicalCores := phys
	perPod := 4

	// Oversubscribe to exercise the overflow path.
	numPods := physicalCores/perPod + 3
	at.Logf("node %s: %d physical cores; launching %d pods x %d cores (oversubscribed)",
		node, physicalCores, numPods, perPod)

	// Bind every pod so one node saturates. A capacity denial counts as unpinned; any
	// other refusal (a quota) would let the test pass on a node that never filled.
	names := make([]string, 0, numPods)
	for i := 0; i < numPods; i++ {
		name := fmt.Sprintf("e2e-sat-%02d", i)
		names = append(names, name)
		p := vllmPod(name, ns, strconv.Itoa(perPod), strconv.Itoa(perPod), 2)
		p.Spec.NodeName = node
		_ = c.Delete(context.Background(), p)
		if err := e2eCreate(context.Background(), c, p); err != nil {
			if !satCapacityOverflow(err) {
				at.Fatalf("create pod %s on %s: %v (only the operator's capacity denial counts as overflow)", name, node, err)
			}
			at.Logf("pod %s denied by the capacity check on %s (counts as unpinned): %v", name, node, err)
			continue
		}
		t.Cleanup(func() {
			e2eDelete(t, c, p, client.GracePeriodSeconds(0))
		})
	}

	deadline := time.Now().Add(8 * time.Minute)
	cpuCore := cpuToCore(t, c, node)
	physicalCoresMode := clusterCPUAllocation(t, c) == "physical-cores"

	var pinnedCount int
	for {
		usedCores := map[int]string{} // core -> owning pod
		pinnedCount = 0
		collision := false
		for _, name := range names {
			set := containerCPUSet(t, c, node, ns, name, "vllm")
			if set == "" {
				continue
			}
			pinnedCount++
			cpus := parseCPUSet(set)
			var cores map[int]bool
			if physicalCoresMode {
				cores = coresOf(cpus, cpuCore)
			} else {
				cores = cpus // hyperthreads mode: compare at logical-CPU level
			}
			for core := range cores {
				if other, ok := usedCores[core]; ok && other != name {
					at.Errorf("collision under load: core %d used by both %s and %s", core, other, name)
					collision = true
				}
				usedCores[core] = name
			}
		}
		// Pinned physical cores must fit the node.
		if physicalCoresMode && len(usedCores) > physicalCores {
			at.Errorf("pinned cores %d exceed node physical cores %d", len(usedCores), physicalCores)
		}
		if collision || pinnedCount >= numPods || time.Now().After(deadline) {
			at.Logf("pinned %d/%d pods onto %d distinct cores", pinnedCount, numPods, len(usedCores))
			break
		}
		time.Sleep(15 * time.Second)
	}

	// Oversubscribed, so some pod must stay unpinned.
	maxPinnable := physicalCores / perPod
	if pinnedCount > maxPinnable {
		at.Errorf("pinned %d pods but node fits at most %d x %d-core pods without collision",
			pinnedCount, maxPinnable, perPod)
	}
	if pinnedCount == numPods {
		at.Errorf("all %d oversubscribed pods got pinned; expected some to stay Pending", numPods)
	}
	if pinnedCount == 0 {
		at.Errorf("no pod got pinned on %s; the collision and capacity checks looked at nothing", node)
	}
}

// satCapacityOverflow reports whether a pod Create error is the operator's capacity
// denial, the one refusal the saturation test reads as the node being full.
func satCapacityOverflow(err error) bool {
	return err != nil && strings.Contains(err.Error(), ccDenialSubstr)
}

// -- Operator restart resilience ---------------------------------------------

// TestOperatorPodDeletionRecovers proves the operator recovers from pod deletion with
// its BalloonsPolicy intact and the webhook still pinning (E2E_RESTART).
func TestOperatorPodDeletionRecovers(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_RESTART") == "" {
		skipNotSelected(at, "set E2E_RESTART=1 to run the operator-pod-deletion resilience test")
	}
	c := newClient(at)
	ctx := context.Background()
	opNS := env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system")
	node := balloonsEnabledNode(at, c)

	bpKey := client.ObjectKey{Name: "node." + node, Namespace: balloonsNS()}
	if err := c.Get(ctx, bpKey, newUnstructured(bpGVK)); err != nil {
		at.Fatalf("baseline: BalloonsPolicy node.%s not present: %v", node, err)
	}

	var dep appsv1.Deployment
	depKey := client.ObjectKey{Name: env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment), Namespace: opNS}
	if err := c.Get(ctx, depKey, &dep); err != nil {
		skipNoConfig(at, "operator Deployment %s/%s not found (set E2E_OPERATOR_NAMESPACE/E2E_OPERATOR_DEPLOYMENT): %v",
			depKey.Namespace, depKey.Name, err)
	}
	sel, err := metav1.LabelSelectorAsMap(dep.Spec.Selector)
	if err != nil {
		at.Fatalf("bad deployment selector: %v", err)
	}

	before := operatorPodNames(t, c, opNS, sel)
	if len(before) == 0 {
		at.Fatal("no operator pods found before deletion")
	}
	at.Logf("deleting operator pod(s): %v", before)
	for name := range before {
		_ = c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: opNS}})
	}

	// Wait for a new pod to be Ready and the Deployment Available.
	deadline := time.Now().Add(4 * time.Minute)
	for {
		if time.Now().After(deadline) {
			at.Fatal("operator did not recover to an Available state after pod deletion")
		}
		if operatorAvailable(t, c, depKey, before, sel) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	at.Log("operator recovered: a fresh pod is Ready and the Deployment is Available")

	if err := c.Get(ctx, bpKey, newUnstructured(bpGVK)); err != nil {
		at.Errorf("after restart: BalloonsPolicy node.%s missing: %v", node, err)
	}

	// Retry: with failurePolicy=Ignore a pod can slip through before readiness next succeeds.
	ns := useTargetNamespace(at, c)
	balloonKey := balloonAnnotationBase + "/container.vllm"

	admitDeadline := time.Now().Add(2 * time.Minute)
	var pinned bool
	for i := 0; time.Now().Before(admitDeadline); i++ {
		name := fmt.Sprintf("e2e-post-restart-%d", i)
		created := createAndCleanup(t, c, vllmPod(name, ns, "2", "2", 2))
		if created.Annotations[balloonKey] != "" &&
			created.Annotations[pinnedByAnnotation] == "cpu-pinning-operator" {
			pinned = true
			at.Logf("webhook pinning restored after restart (attempt %d, balloon=%s)",
				i+1, created.Annotations[balloonKey])
			break
		}
		_ = c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
			client.GracePeriodSeconds(0))
		time.Sleep(10 * time.Second)
	}
	if !pinned {
		at.Error("after restart: webhook never resumed pinning within the deadline (admission did not recover)")
	}
}

// operatorPodNames returns the current set of operator pod names.
func operatorPodNames(t *testing.T, c client.Client, ns string, sel map[string]string) map[string]bool {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(ns), client.MatchingLabels(sel)); err != nil {
		t.Fatalf("list operator pods: %v", err)
	}
	out := map[string]bool{}
	for i := range pods.Items {
		out[pods.Items[i].Name] = true
	}
	return out
}

// operatorAvailable reports whether the Deployment is fully Available with at least
// one Ready pod not in oldPods.
func operatorAvailable(t *testing.T, c client.Client, depKey client.ObjectKey, oldPods map[string]bool, sel map[string]string) bool {
	t.Helper()
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), depKey, &dep); err != nil {
		return false
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	if dep.Status.AvailableReplicas < want {
		return false
	}
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(depKey.Namespace), client.MatchingLabels(sel)); err != nil {
		return false
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if oldPods[p.Name] {
			continue
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

// -- helpers -----------------------------------------------------------------

// useTargetNamespace returns the namespace the webhook mutates, creating it on first
// use. It fails rather than skips: a silent skip looks like a pass.
func useTargetNamespace(t testing.TB, c client.Client) string {
	t.Helper()
	reportOnce()
	// Callers rely on the baseline match; report residue by name.
	if policyMatchResidue != "" {
		t.Fatalf("the default policy's spec.match is not at baseline: %s", policyMatchResidue)
	}
	ns, err := targetNamespace()
	if errors.Is(err, errNoPolicy) {
		skipNoCapability(t, "%v, so the cluster cannot exercise pinning", err)
	}
	if err != nil {
		t.Fatalf("no namespace to pin in: %v", err)
	}
	ensureTargetNamespace()
	if nsProvisionFailure != "" {
		t.Fatalf("namespace %q is not usable: %s", ns, nsProvisionFailure)
	}
	var got corev1.Namespace
	if err := c.Get(context.Background(), client.ObjectKey{Name: ns}, &got); err != nil {
		t.Fatalf("namespace %q is not usable: %v — the suite creates it on first use, so either the read "+
			"failed or something removed it mid-run", ns, err)
	}
	// A Terminating namespace GETs fine but refuses every create.
	if got.Status.Phase == corev1.NamespaceTerminating {
		t.Fatalf("namespace %q is Terminating; nothing can be created in it", ns)
	}
	return ns
}

// requireNamespace fails when the namespace is absent; a namespace created here would
// lack the labels a namespaceSelector matches.
func requireNamespace(t testing.TB, c client.Client, ns string) {
	t.Helper()
	var n corev1.Namespace
	if err := c.Get(context.Background(), client.ObjectKey{Name: ns}, &n); err != nil {
		t.Fatalf("namespace %q not present: %v", ns, err)
	}
}

// vllmPod builds a pause pod with a "vllm" container, --tensor-parallel-size tp, and
// the given CPU request/limit.
func vllmPod(name, ns, req, lim string, tp int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"app": "vllm-e2e"},
		},
		Spec: corev1.PodSpec{
			// tolerate everything so scheduling isn't the blocker in pinning tests
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    "vllm",
				Image:   "registry.k8s.io/pause:3.9",
				Command: []string{"/pause"},
				Args:    []string{"--tensor-parallel-size", strconv.Itoa(tp)},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(req)},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(lim)},
				},
			}},
		},
	}
}

// createAndCleanup creates the pod, registers its deletion, and returns the mutated object.
func createAndCleanup(t *testing.T, c client.Client, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	// Leftover from a previous run.
	_ = c.Delete(ctx, pod)
	if err := e2eCreate(ctx, c, pod); err != nil {
		t.Fatalf("create pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	t.Cleanup(func() {
		e2eDelete(t, c, pod, client.GracePeriodSeconds(0))
	})
	// Re-Get to observe the persisted, mutated object.
	got := &corev1.Pod{}
	if err := c.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, got); err != nil {
		t.Fatalf("get created pod: %v", err)
	}
	return got
}

func hasInitContainer(pod *corev1.Pod, name string) bool {
	return initContainer(pod, name) != nil
}

// initContainer returns the named init container, or nil if the pod has none.
func initContainer(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

// containerCPUSet reads the assigned cpuset for ns/pod/container from the node's
// NodeResourceTopology container subzone, or "" if not present yet.
func containerCPUSet(t *testing.T, c client.Client, node, ns, pod, container string) string {
	t.Helper()
	nrt := newUnstructured(nrtGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: node}, nrt); err != nil {
		return ""
	}
	// Exact match: a prefix would pick up the wait-for-balloons init container's subzone.
	want := fmt.Sprintf("%s/%s/%s", ns, pod, container)
	zones, _, _ := unstructured.NestedSlice(nrt.Object, "zones")
	for _, z := range zones {
		zone, ok := z.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(zone, "name")
		if name != want {
			continue
		}
		attrs, _, _ := unstructured.NestedSlice(zone, "attributes")
		for _, a := range attrs {
			attr, ok := a.(map[string]interface{})
			if !ok {
				continue
			}
			if an, _, _ := unstructured.NestedString(attr, "name"); an == "cpuset" {
				v, _, _ := unstructured.NestedString(attr, "value")
				return v
			}
		}
	}
	return ""
}

// parseCPUSet parses "0,10-12,64" into a set of ints.
func parseCPUSet(s string) map[int]bool {
	out := map[int]bool{}
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			l, _ := strconv.Atoi(strings.TrimSpace(lo))
			h, _ := strconv.Atoi(strings.TrimSpace(hi))
			for i := l; i <= h; i++ {
				out[i] = true
			}
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			out[n] = true
		}
	}
	return out
}
