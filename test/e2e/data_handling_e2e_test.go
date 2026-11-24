// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	dataHandlingGate = "E2E_DATA_HANDLING"
	dhNamespace      = "cpu-pinning-e2e-data-handling"
	dhPolicy         = "e2e-data-handling"
	dhContainer      = "data-handling"
	// The policy's span intent names this annotation, so the webhook reads it.
	dhSpanAnnotation = "e2e.example.com/span"
	dhMultiplierEnv  = "E2E_MULTIPLIER"
)

// Planted values. Each sits where the webhook parses an intent, or right next to
// one, and none is an integer, so the operator may only ever keep a parsed number.
const (
	dhMarkerEmail = "alice.e2emarker@example.com"
	dhMarkerEnv   = "e2emarker-env-7f3c9a1b"
	dhMarkerArg   = "e2emarker-arg-5d2e8c40"
	dhMarkerAnnot = "e2emarker-annot-91b6f2de"
)

var dhMarkers = []string{dhMarkerEmail, dhMarkerEnv, dhMarkerArg, dhMarkerAnnot}

// piiPatterns are generic shapes no operator output should carry.
var piiPatterns = map[string]*regexp.Regexp{
	"email":        regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`),
	"bearer token": regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	"JWT":          regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.`),
	"private key":  regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
}

// piiFindings names every planted marker and generic PII shape found in text. A
// generic match may be a real value from the cluster, so only its category is named.
func piiFindings(text string) []string {
	var out []string
	for _, m := range dhMarkers {
		if strings.Contains(text, m) {
			out = append(out, fmt.Sprintf("planted value %q", m))
		}
	}
	for name, re := range piiPatterns {
		if re.MatchString(text) {
			out = append(out, name+" (value withheld)")
		}
	}
	return out
}

// TestNoPersonalDataInOperatorOutputs plants personal-looking values where the
// webhook parses intents, then proves none of them, nor any generic PII shape, reaches
// what the operator logs, emits or writes.
func TestNoPersonalDataInOperatorOutputs(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(dataHandlingGate) == "" {
		skipNotSelected(at, "set %s=1 to plant marker values and scan the operator's logs, events, metrics and objects", dataHandlingGate)
	}
	c := newClient(at)
	cfg, err := ctrl.GetConfig()
	if err != nil {
		skipNoCapability(at, "no kubeconfig available: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		at.Fatalf("build clientset: %v", err)
	}
	ctx := context.Background()

	dhProvision(at, c)
	pod := dhMarkedPod()
	if err := e2eCreate(ctx, c, pod); err != nil {
		at.Fatalf("create marked pod: %v", err)
	}
	// Pinned means the webhook parsed every intent source on it.
	if waitForRunningPinnedPod(at.T, c, dhNamespace, dhContainer) == nil {
		skipNoConfig(at, "marked pod in %s was never pinned and Running (needs a balloons-enabled node with its plugin)", dhNamespace)
	}
	// Give the reconcilers, event recorder and metrics a moment to act on the pod.
	time.Sleep(15 * time.Second)

	opNS := env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system")
	sources := map[string]string{}
	dhCollectLogs(ctx, at, cs, opNS, managerPodSelector(ctx, cs, opNS), "manager", "manager", sources)
	// The plugin is not the operator, but it receives the same pod; scan it where reachable.
	if ds, err := cs.AppsV1().DaemonSets(balloonsNS()).Get(ctx, balloonsDaemonSet(), metav1.GetOptions{}); err == nil {
		dhCollectLogs(ctx, at, cs, ds.Namespace, metav1.FormatLabelSelector(ds.Spec.Selector), "", "plugin", sources)
	} else {
		at.Logf("balloons plugin DaemonSet not readable, its logs are not scanned: %v", err)
	}
	for _, ns := range []string{dhNamespace, opNS} {
		var events corev1.EventList
		if err := c.List(ctx, &events, client.InNamespace(ns)); err != nil {
			at.Fatalf("list events in %s: %v", ns, err)
		}
		sources["events/"+ns] = dhJSON(at, events.Items)
	}
	dhCollectObjects(ctx, at, c, sources)

	for src, text := range sources {
		for _, f := range piiFindings(text) {
			at.Errorf("%s carries %s", src, f)
		}
	}
	at.Logf("scanned %d source(s) for %d planted value(s) and %d PII pattern(s)", len(sources), len(dhMarkers), len(piiPatterns))

	t.Run("metrics", func(t *testing.T) {
		body := scrapeMetricsAllReplicasOrSkip(t, "no manager replica served /metrics; run with metrics.requireAuth=false to scan it")
		for _, f := range piiFindings(body) {
			t.Errorf("metrics exposition carries %s", f)
		}
	})
}

// dhProvision creates the namespace and a policy whose intents read the planted fields.
func dhProvision(at *allureT, c client.Client) {
	ctx := context.Background()
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: dhNamespace}}
	_ = c.Delete(ctx, nsObj) // best-effort clean of a leftover run
	if err := createEventuallyGone(c, nsObj); err != nil {
		at.Fatalf("create namespace %s: %v", dhNamespace, err)
	}
	at.Cleanup(func() {
		e2eDelete(at, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: dhNamespace}})
	})
	bpp := newUnstructured(bppGVK)
	bpp.SetName(dhPolicy)
	bpp.Object["spec"] = map[string]interface{}{
		"match":      map[string]interface{}{"byContainerName": []interface{}{dhContainer}},
		"namespaces": []interface{}{dhNamespace},
		"intent": map[string]interface{}{
			"span":       map[string]interface{}{"annotation": dhSpanAnnotation, "argRegex": `--tp-size(?:=|\s+)(\d+)`, "default": int64(1)},
			"multiplier": map[string]interface{}{"envVar": dhMultiplierEnv, "default": int64(1)},
		},
		"scheduling": map[string]interface{}{"enforcePinning": true},
	}
	_ = c.Delete(ctx, bpp)
	if err := createEventuallyGone(c, bpp); err != nil {
		at.Fatalf("create BalloonPinningPolicy %s: %v", dhPolicy, err)
	}
	at.Cleanup(func() {
		p := newUnstructured(bppGVK)
		p.SetName(dhPolicy)
		e2eDelete(at, c, p)
	})
	if !waitForWebhookNamespace(at.T, c, dhNamespace, dhContainer) {
		skipNoConfig(at, "webhook never began pinning pods in %q (policy reconcile / selector patch did not take effect)", dhNamespace)
	}
}

// dhMarkedPod carries a planted value in an intent annotation, an intent env var,
// a plain env var, a container arg and a plain annotation. Only --tp-size parses.
func dhMarkedPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "marked", Namespace: dhNamespace,
			Labels: map[string]string{"app": dhContainer},
			Annotations: map[string]string{
				dhSpanAnnotation:       dhMarkerEmail,
				"e2e.example.com/note": "owner " + dhMarkerAnnot,
			},
		},
		Spec: corev1.PodSpec{
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    dhContainer,
				Image:   "registry.k8s.io/pause:3.9",
				Command: []string{"/pause"},
				Args:    []string{"--tp-size=1", "--api-key=" + dhMarkerArg},
				Env: []corev1.EnvVar{
					{Name: dhMultiplierEnv, Value: "Bearer " + dhMarkerEnv},
					{Name: "E2E_API_TOKEN", Value: dhMarkerEnv},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				},
			}},
		},
	}
}

// dhCollectLogs adds each selected pod's log to sources; container "" means the only one.
func dhCollectLogs(ctx context.Context, at *allureT, cs kubernetes.Interface, ns, selector, container, label string, sources map[string]string) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil || len(pods.Items) == 0 {
		at.Fatalf("no %s pods in %s (selector %q): %v", label, ns, selector, err)
	}
	for _, p := range pods.Items {
		raw, err := cs.CoreV1().Pods(ns).GetLogs(p.Name, &corev1.PodLogOptions{Container: container}).DoRaw(ctx)
		if err != nil {
			at.Fatalf("read %s log %s/%s: %v", label, ns, p.Name, err)
		}
		sources[label+" log "+p.Name] = string(raw)
	}
}

// dhCollectObjects adds what the operator writes: BalloonsPolicies, its node labels and
// annotations, and the ConfigMaps in the namespaces it writes them to.
func dhCollectObjects(ctx context.Context, at *allureT, c client.Client, sources map[string]string) {
	bps := &unstructured.UnstructuredList{}
	bps.SetGroupVersionKind(bpGVK)
	if err := c.List(ctx, bps); err != nil {
		at.Fatalf("list BalloonsPolicies: %v", err)
	}
	sources["BalloonsPolicies"] = dhJSON(at, bps.Items)

	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		at.Fatalf("list nodes: %v", err)
	}
	// Only the operator's own keys: other controllers' node metadata is not under test.
	own := map[string]string{}
	for _, n := range nodes.Items {
		for _, m := range []map[string]string{n.Labels, n.Annotations} {
			for k, v := range m {
				if strings.HasPrefix(k, "cpu-pinning-operator.intel.com/") {
					own[n.Name+" "+k] = v
				}
			}
		}
	}
	sources["node labels and annotations"] = dhJSON(at, own)

	dashNS := env("E2E_DASHBOARD_NAMESPACE", operatorNamespace())
	for _, ns := range []string{dhNamespace, dashNS} {
		var cms corev1.ConfigMapList
		if err := c.List(ctx, &cms, client.InNamespace(ns)); err != nil {
			at.Fatalf("list ConfigMaps in %s: %v", ns, err)
		}
		sources["configmaps/"+ns] = dhJSON(at, cms.Items)
	}
}

func dhJSON(at *allureT, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		at.Fatalf("marshal for scan: %v", err)
	}
	return string(b)
}

// TestPIIMatcherFlagsPlantedAndGeneric proves the matcher catches every planted value
// and generic shape, and stays quiet on ordinary operator output. No cluster.
func TestPIIMatcherFlagsPlantedAndGeneric(t *testing.T) {
	for _, hit := range append(dhMarkers,
		"contact bob@corp.example.org",
		"Authorization: Bearer abcdEFGH1234.xyz",
		// Assembled at runtime so secret scanners do not flag this fake JWT fixture.
		"token="+strings.Join([]string{"eyJhbGciOiJIUzI1NiJ9", "eyJzdWIiOiIxMjM0NTY3ODkwIn0", "sig"}, "."),
		"-----BEGIN EC PRIVATE KEY-----",
	) {
		if len(piiFindings(hit)) == 0 {
			t.Errorf("matcher missed %q", hit)
		}
	}
	// A generic match must not echo the value it found into the report.
	if f := strings.Join(piiFindings("contact bob@corp.example.org"), " "); strings.Contains(f, "bob@") {
		t.Errorf("matcher reported the matched value: %q", f)
	}
	for _, quiet := range []string{
		`{"level":"info","msg":"pinned pod","namespace":"ns","pod":"marked","balloon":"span-1","cpus":"2-3"}`,
		`cpu_pinning_webhook_reservations_live{node="worker-1"} 1`,
		"image registry.k8s.io/pause@sha256:7031c1b283388d2c2e09b57badb803c05ebed362dc88d84b480cc47f72a21097",
		"cpu-pinning-operator.intel.com/physical-cpus=56",
		"go module sigs.k8s.io/controller-runtime@v0.21.0",
	} {
		if f := piiFindings(quiet); len(f) > 0 {
			t.Errorf("matcher flagged ordinary output %q: %q", quiet, f)
		}
	}
}
