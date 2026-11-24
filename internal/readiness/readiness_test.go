// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/go-logr/logr"
)

// countingReader counts Get and List calls on a client.Reader.
type countingReader struct {
	client.Reader
	reads int
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.reads++
	return c.Reader.Get(ctx, key, obj, opts...)
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.reads++
	return c.Reader.List(ctx, list, opts...)
}

const (
	testNamespace = "kube-system"
	testDaemonSet = "nri-resource-policy-balloons"
)

// balloonsDaemonSet returns a DaemonSet with the plugin's pod selector.
func balloonsDaemonSet() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: testDaemonSet, Namespace: testNamespace},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "balloons"}},
		},
	}
}

// balloonsPod builds a plugin pod with a ready condition and optional deletion timestamp.
func balloonsPod(node string, ready bool, terminating bool) *corev1.Pod {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "balloons-a",
			Namespace: testNamespace,
			Labels:    map[string]string{"app": "balloons"},
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
		},
	}
	if terminating {
		ts := metav1.NewTime(time.Now())
		pod.DeletionTimestamp = &ts
		pod.Finalizers = []string{"keep"} // fake client requires a finalizer to hold a deleting object
	}
	return pod
}

func newHandler(t *testing.T, objs ...runtime.Object) *Handler {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &Handler{Client: c, Namespace: testNamespace, DaemonSet: testDaemonSet, Log: logr.Discard()}
}

func do(t *testing.T, h *Handler, query string) (*httptest.ResponseRecorder, result) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, Path+query, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var res result
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	return rec, res
}

func TestReadyWhenPodReadyOnNode(t *testing.T) {
	h := newHandler(t, balloonsDaemonSet(), balloonsPod("node-1", true, false))
	rec, res := do(t, h, "?node=node-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !res.Ready || res.Node != "node-1" {
		t.Errorf("result = %+v, want ready node-1", res)
	}
}

func TestNotReadyWhenNoPodOnNode(t *testing.T) {
	// A ready pod exists, but on a DIFFERENT node — the target node has none yet.
	h := newHandler(t, balloonsDaemonSet(), balloonsPod("node-2", true, false))
	rec, res := do(t, h, "?node=node-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if res.Ready {
		t.Errorf("result = %+v, want not-ready", res)
	}
}

func TestNotReadyWhenPodNotReady(t *testing.T) {
	h := newHandler(t, balloonsDaemonSet(), balloonsPod("node-1", false, false))
	rec, _ := do(t, h, "?node=node-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a not-ready pod", rec.Code)
	}
}

func TestNotReadyWhenPodTerminating(t *testing.T) {
	// Ready condition True, but the pod is terminating — must not count as ready.
	h := newHandler(t, balloonsDaemonSet(), balloonsPod("node-1", true, true))
	rec, _ := do(t, h, "?node=node-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a terminating pod", rec.Code)
	}
}

func TestBadRequestWhenNodeMissing(t *testing.T) {
	h := newHandler(t, balloonsDaemonSet())
	rec, _ := do(t, h, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when node is missing", rec.Code)
	}
	rec, _ = do(t, h, "?node=")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when node is empty", rec.Code)
	}
}

func TestNotReadyWhenDaemonSetMissing(t *testing.T) {
	// No DaemonSet in the cluster yet → not ready (keep waiting), not an error.
	h := newHandler(t)
	rec, _ := do(t, h, "?node=node-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the DaemonSet is absent", rec.Code)
	}
}

// doFrom issues a request from an explicit source address.
func doFrom(t *testing.T, h *Handler, query, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, Path+query, nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRateLimitDeAmplifies proves a 429 causes zero API reads: reads == 2 per served request.
func TestRateLimitDeAmplifies(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	fakeC := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(balloonsDaemonSet(), balloonsPod("node-1", true, false)).Build()
	counter := &countingReader{Reader: fakeC}
	h := &Handler{Client: counter, Namespace: testNamespace, DaemonSet: testDaemonSet, Log: logr.Discard()}

	const flood = 50
	ok, limited := 0, 0
	for i := 0; i < flood; i++ {
		rec := doFrom(t, h, "?node=node-1", "10.0.0.1:5000")
		switch rec.Code {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("unexpected status %d", rec.Code)
		}
	}

	// nodeReady does one Get + one List per served request.
	if counter.reads != 2*ok {
		t.Errorf("reads = %d, want %d (2 per served request, 0 per 429)", counter.reads, 2*ok)
	}
	// Serving all 50 would need >20s of refill, so this cannot flake.
	if limited == 0 {
		t.Errorf("a flood of %d requests produced no 429s (ok=%d)", flood, ok)
	}
}

// TestSourceLimiterBurst proves, at a fixed timestamp, that perSourceBurst requests pass and the next is denied.
func TestSourceLimiterBurst(t *testing.T) {
	s := newSourceLimiter()
	now := time.Now()
	for i := 0; i < perSourceBurst; i++ {
		if !s.allow("10.0.0.1", now) {
			t.Fatalf("request %d within burst was denied", i+1)
		}
	}
	if s.allow("10.0.0.1", now) {
		t.Errorf("request past burst=%d was allowed at the same instant", perSourceBurst)
	}
}

// TestSourceLimiterPerSource proves one source exhausting its burst does not throttle another.
func TestSourceLimiterPerSource(t *testing.T) {
	s := newSourceLimiter()
	now := time.Now()
	for i := 0; i < perSourceBurst; i++ {
		s.allow("10.0.0.1", now)
	}
	if s.allow("10.0.0.1", now) {
		t.Fatalf("source A allowed past its burst at the same instant")
	}
	if !s.allow("10.0.0.2", now) {
		t.Errorf("source B denied though it has its own bucket")
	}
}

// TestSourceLimiterEvictsIdle proves an entry idle past the TTL is dropped on the next sweep.
func TestSourceLimiterEvictsIdle(t *testing.T) {
	s := newSourceLimiter()
	t0 := time.Now()
	s.allow("10.0.0.1", t0)
	if len(s.clients) != 1 {
		t.Fatalf("clients = %d, want 1 after first request", len(s.clients))
	}
	// A later request past the sweep interval and TTL evicts the idle entry.
	s.allow("10.0.0.2", t0.Add(limiterSweepEvery+limiterIdleTTL))
	if _, stillThere := s.clients["10.0.0.1"]; stillThere {
		t.Errorf("idle entry 10.0.0.1 was not evicted")
	}
	if _, ok := s.clients["10.0.0.2"]; !ok {
		t.Errorf("active entry 10.0.0.2 missing")
	}
}

// TestSourceIP parses the peer IP out of RemoteAddr and tolerates a bare address.
func TestSourceIP(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"10.0.0.1:5000", "10.0.0.1"},
		{"[fe80::1]:5000", "fe80::1"},
		{"10.0.0.1", "10.0.0.1"}, // no port — returned as-is
	} {
		req := httptest.NewRequest(http.MethodGet, Path, nil)
		req.RemoteAddr = tc.addr
		if got := sourceIP(req); got != tc.want {
			t.Errorf("sourceIP(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// secretText stands in for internal detail an API error can carry; it must never reach a client.
const secretText = "forbidden: serviceaccount system:serviceaccount:cpu-pinning-system:manager cannot list pods at cluster scope"

// TestNoAPIErrorLeakOnGet proves a DaemonSet Get error is not echoed and the status stays 503.
func TestNoAPIErrorLeakOnGet(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New(secretText)
		},
	}).Build()
	h := &Handler{Client: c, Namespace: testNamespace, DaemonSet: testDaemonSet, Log: logr.Discard()}

	rec, res := do(t, h, "?node=node-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on a read error", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secretText) || strings.Contains(res.Reason, secretText) || strings.Contains(res.Error, secretText) {
		t.Errorf("client-facing response leaked the raw API error: reason=%q error=%q", res.Reason, res.Error)
	}
	// A handler failure must set Error.
	if res.Error == "" {
		t.Errorf("result = %+v, want a non-empty Error for an API read failure", res)
	}
}

// badSelectorDaemonSet has a selector that passes structural checks but fails
// metav1.LabelSelectorAsSelector: an In requirement with no values.
func badSelectorDaemonSet() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: testDaemonSet, Namespace: testNamespace},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn},
				},
			},
		},
	}
}

// TestNoSelectorErrorLeakOnInvalidSelector proves a selector-conversion error is not echoed and stays 503.
func TestNoSelectorErrorLeakOnInvalidSelector(t *testing.T) {
	h := newHandler(t, badSelectorDaemonSet())
	rec, res := do(t, h, "?node=node-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on an invalid selector", rec.Code)
	}
	if !strings.Contains(res.Reason, "has an invalid pod selector") {
		t.Errorf("reason = %q, want the generic invalid-selector message", res.Reason)
	}
	// "Invalid value" appears only if the raw conversion error leaked.
	if strings.Contains(res.Reason, "Invalid value") {
		t.Errorf("client-facing reason leaked the raw selector error: %q", res.Reason)
	}
}

// TestNoAPIErrorLeakOnList asserts the same for a Pod List error — the twin path.
func TestNoAPIErrorLeakOnList(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(balloonsDaemonSet()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New(secretText)
			},
		}).Build()
	h := &Handler{Client: c, Namespace: testNamespace, DaemonSet: testDaemonSet, Log: logr.Discard()}

	rec, res := do(t, h, "?node=node-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on a list error", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secretText) || strings.Contains(res.Reason, secretText) || strings.Contains(res.Error, secretText) {
		t.Errorf("client-facing response leaked the raw API error: reason=%q error=%q", res.Reason, res.Error)
	}
	// A handler failure must set Error.
	if res.Error == "" {
		t.Errorf("result = %+v, want a non-empty Error for an API read failure", res)
	}
}

// TestNoErrorFieldOnDefinitiveNotReady proves a genuine not-ready leaves Error empty.
func TestNoErrorFieldOnDefinitiveNotReady(t *testing.T) {
	h := newHandler(t, balloonsDaemonSet(), balloonsPod("node-2", true, false))
	rec, res := do(t, h, "?node=node-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if res.Error != "" {
		t.Errorf("result = %+v, want empty Error for a definitive not-ready verdict", res)
	}
}

// TestNoErrorFieldWhenReady asserts a 200 never carries an Error field.
func TestNoErrorFieldWhenReady(t *testing.T) {
	h := newHandler(t, balloonsDaemonSet(), balloonsPod("node-1", true, false))
	rec, res := do(t, h, "?node=node-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if res.Error != "" {
		t.Errorf("result = %+v, want empty Error when ready", res)
	}
}
