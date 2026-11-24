// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package match

import (
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
)

// vLLM's tensor- and pipeline-parallel intent sources, as in the shipped sample policy.
var vllmTP = cpopv1alpha1.IntentSourceSpec{
	Annotation: "cpu-pinning-operator.intel.com/tp",
	ArgRegex:   `(?:^|\s)(?:--tensor-parallel-size|-tp)(?:=|\s+)(\d+)`,
	EnvVar:     "VLLM_TENSOR_PARALLEL_SIZE",
	Default:    1,
}
var vllmMultiplier = cpopv1alpha1.IntentSourceSpec{
	Annotation: "cpu-pinning-operator.intel.com/multiplier",
	ArgRegex:   `(?:^|\s)(?:--pipeline-parallel-size|-pp)(?:=|\s+)(\d+)`,
	EnvVar:     "VLLM_PIPELINE_PARALLEL_SIZE",
	Default:    1,
}

func TestSnap(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 1},
		{1, 1},
		{2, 2},
		{3, 4},
		{4, 4},
		{5, 8},
		{7, 8},
		{8, 8},
		{9, 8},
		{16, 8},
	}
	for _, tc := range cases {
		if got := Snap(tc.in); got != tc.want {
			t.Errorf("Snap(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestSpanFromBalloonName proves every generated name round-trips and nothing else is accepted.
func TestSpanFromBalloonName(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"balloon-span1", 1, true},
		{"balloon-span2", 2, true},
		{"balloon-span4", 4, true},
		{"balloon-span8", 8, true},
		{"balloon-span2-colocate", 2, true},
		{"balloon-span4-spread", 4, true},
		{"balloon-device", 1, true},
		{"balloon-span3", 0, false},  // not a generated width
		{"balloon-span0", 0, false},  // would divide the request by zero spans
		{"balloon-span16", 0, false}, // above the generated ceiling
		{"balloon-span2-elsewhere", 0, false},
		{"balloon-span", 0, false},
		{"balloon-span+2", 0, false}, // Atoi tolerates a sign; fmt never writes one
		{"balloon-span02", 0, false},
		{"reserved", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := SpanFromBalloonName(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("SpanFromBalloonName(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestBalloonNameRoundTripsItsSpan proves every name BalloonNameForPod emits reads its span back.
func TestBalloonNameRoundTripsItsSpan(t *testing.T) {
	for _, span := range []int{1, 2, 4, 8} {
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm",
			Args: []string{"--tensor-parallel-size", strconv.Itoa(span)},
		}}}}
		for _, coLocate := range []string{"", "package", "numa"} {
			rules, err := CompileIntent(cpopv1alpha1.IntentSpec{Span: vllmTP, CoLocate: coLocate})
			if err != nil {
				t.Fatalf("compile(coLocate=%q): %v", coLocate, err)
			}
			name := BalloonNameForPod(pod, &pod.Spec.Containers[0], rules, 8)
			got, ok := SpanFromBalloonName(name)
			if !ok || got != span {
				t.Errorf("SpanFromBalloonName(%q) = (%d, %v), want (%d, true)", name, got, ok, span)
			}
		}
	}

	// Device pods route to the fixed device balloon, whose span is not in its name.
	rules, err := CompileIntent(cpopv1alpha1.IntentSpec{Span: vllmTP})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	dev := devicePod("nvidia.com/gpu", "requests", "--tensor-parallel-size", "4")
	name := BalloonNameForPod(dev, &dev.Spec.Containers[0], rules, 8)
	got, ok := SpanFromBalloonName(name)
	if !ok || got != DesiredSpan(dev, &dev.Spec.Containers[0], rules) {
		t.Errorf("SpanFromBalloonName(%q) = (%d, %v), want (%d, true)", name, got, ok, DesiredSpan(dev, &dev.Spec.Containers[0], rules))
	}
}

// compileTP compiles the vLLM TP source into an IntentRule.
func compileTP(t *testing.T) IntentRule {
	t.Helper()
	rules, err := CompileIntent(cpopv1alpha1.IntentSpec{Span: vllmTP})
	if err != nil {
		t.Fatalf("compile vLLM TP: %v", err)
	}
	return rules.Span
}

// compileMultiplier compiles the vLLM PP source via the Multiplier branch of CompileIntent.
func compileMultiplier(t *testing.T) IntentRule {
	t.Helper()
	rules, err := CompileIntent(cpopv1alpha1.IntentSpec{Span: vllmTP, Multiplier: &vllmMultiplier})
	if err != nil {
		t.Fatalf("compile vLLM Multiplier: %v", err)
	}
	if rules.Multiplier == nil {
		t.Fatal("CompileIntent did not populate Multiplier")
	}
	return *rules.Multiplier
}

func TestExtractIntentTP(t *testing.T) {
	tp := compileTP(t)
	cases := []struct {
		name string
		pod  *corev1.Pod
		c    *corev1.Container
		want int
	}{
		{
			name: "long flag with space",
			c:    &corev1.Container{Args: []string{"--tensor-parallel-size", "4"}},
			want: 4,
		},
		{
			name: "long flag with equals",
			c:    &corev1.Container{Args: []string{"--tensor-parallel-size=2"}},
			want: 2,
		},
		{
			name: "short flag with space",
			c:    &corev1.Container{Args: []string{"-tp", "8"}},
			want: 8,
		},
		{
			name: "short flag with equals",
			c:    &corev1.Container{Args: []string{"-tp=2"}},
			want: 2,
		},
		{
			name: "env fallback",
			c:    &corev1.Container{Env: []corev1.EnvVar{{Name: "VLLM_TENSOR_PARALLEL_SIZE", Value: "4"}}},
			want: 4,
		},
		{
			name: "annotation override wins over args",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				"cpu-pinning-operator.intel.com/tp": "8",
			}}},
			c:    &corev1.Container{Args: []string{"--tensor-parallel-size", "2"}},
			want: 8,
		},
		{
			name: "default when nothing set",
			c:    &corev1.Container{},
			want: 1,
		},
		{
			name: "nil container",
			c:    nil,
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractIntent(tc.pod, tc.c, tp); got != tc.want {
				t.Errorf("extractIntent(vLLM TP) = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestExtractIntentPodScoped proves a pod-scoped match takes the largest intent across containers.
func TestExtractIntentPodScoped(t *testing.T) {
	tp := compileTP(t)
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "sidecar"}, // no TP flag
				{Name: "server", Args: []string{"--tensor-parallel-size", "4"}},
			},
		},
	}
	if got := extractIntent(pod, nil, tp); got != 4 {
		t.Errorf("pod-scoped extractIntent = %d, want 4", got)
	}

	// Annotation override still wins over per-container scanning.
	pod.Annotations = map[string]string{"cpu-pinning-operator.intel.com/tp": "2"}
	if got := extractIntent(pod, nil, tp); got != 2 {
		t.Errorf("pod-scoped extractIntent with annotation = %d, want 2", got)
	}

	// No flags anywhere → default 1.
	plain := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}, {Name: "b"}}}}
	if got := extractIntent(plain, nil, tp); got != 1 {
		t.Errorf("pod-scoped extractIntent with no flags = %d, want 1", got)
	}
}

func TestExtractIntentMultiplier(t *testing.T) {
	pp := compileMultiplier(t)
	cases := []struct {
		name string
		pod  *corev1.Pod
		c    *corev1.Container
		want int
	}{
		{
			name: "long flag with space",
			c:    &corev1.Container{Args: []string{"--pipeline-parallel-size", "2"}},
			want: 2,
		},
		{
			name: "long flag with equals",
			c:    &corev1.Container{Args: []string{"--pipeline-parallel-size=4"}},
			want: 4,
		},
		{
			name: "short flag",
			c:    &corev1.Container{Args: []string{"-pp", "2"}},
			want: 2,
		},
		{
			name: "env fallback",
			c:    &corev1.Container{Env: []corev1.EnvVar{{Name: "VLLM_PIPELINE_PARALLEL_SIZE", Value: "2"}}},
			want: 2,
		},
		{
			name: "annotation override",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				"cpu-pinning-operator.intel.com/multiplier": "4",
			}}},
			c:    &corev1.Container{},
			want: 4,
		},
		{
			name: "default",
			c:    &corev1.Container{},
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractIntent(tc.pod, tc.c, pp); got != tc.want {
				t.Errorf("extractIntent(vLLM Multiplier) = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCompileIntentBadRegex proves a bad argRegex is a compile error.
func TestCompileIntentBadRegex(t *testing.T) {
	_, err := CompileIntent(cpopv1alpha1.IntentSpec{
		Span: cpopv1alpha1.IntentSourceSpec{ArgRegex: "["}, // unclosed class
	})
	if err == nil {
		t.Fatal("expected compile error on invalid argRegex")
	}
}

// TestCompileIntentGenericExtractor proves an arbitrary runtime's flag works with no Go changes.
func TestCompileIntentGenericExtractor(t *testing.T) {
	rules, err := CompileIntent(cpopv1alpha1.IntentSpec{
		Span: cpopv1alpha1.IntentSourceSpec{
			Annotation: "acme.io/parallel",
			ArgRegex:   `--acme-parallelism(?:=|\s+)(\d+)`,
			EnvVar:     "ACME_PARALLELISM",
			Default:    1,
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	c := &corev1.Container{Args: []string{"--acme-parallelism", "4"}}
	if got := extractIntent(nil, c, rules.Span); got != 4 {
		t.Errorf("acme extractor = %d, want 4", got)
	}
	env := &corev1.Container{Env: []corev1.EnvVar{{Name: "ACME_PARALLELISM", Value: "2"}}}
	if got := extractIntent(nil, env, rules.Span); got != 2 {
		t.Errorf("acme env = %d, want 2", got)
	}
}

// TestCompileIntentMultiplierBranch proves spec.Multiplier compiles to a Multiplier
// rule reading its own sources, distinct from Span's.
func TestCompileIntentMultiplierBranch(t *testing.T) {
	rules, err := CompileIntent(cpopv1alpha1.IntentSpec{
		Span:       vllmTP,
		Multiplier: &vllmMultiplier,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if rules.Multiplier == nil {
		t.Fatal("Multiplier rules missing after CompileIntent with spec.Multiplier != nil")
	}
	// Only the PP flag set: it must extract via Multiplier, not Span.
	c := &corev1.Container{Args: []string{"--pipeline-parallel-size", "4"}}
	if got := extractIntent(nil, c, *rules.Multiplier); got != 4 {
		t.Errorf("Multiplier branch extractor = %d, want 4", got)
	}
	if got := extractIntent(nil, c, rules.Span); got != 1 {
		t.Errorf("Span rule should NOT match a Multiplier-only container (got %d, want default 1)", got)
	}

	// An invalid Multiplier.ArgRegex must be a compile error.
	_, err = CompileIntent(cpopv1alpha1.IntentSpec{
		Span:       vllmTP,
		Multiplier: &cpopv1alpha1.IntentSourceSpec{ArgRegex: "["},
	})
	if err == nil {
		t.Fatal("expected compile error on invalid Multiplier.argRegex")
	}
}

// TestSpanForPodMultipliesMultiplier proves SpanForPod snaps span × multiplier to {1,2,4,8}.
func TestSpanForPodMultipliesMultiplier(t *testing.T) {
	rules, err := CompileIntent(cpopv1alpha1.IntentSpec{Span: vllmTP, Multiplier: &vllmMultiplier})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// span=2, multiplier=2 → 4 → snap(4) = 4.
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "vllm",
		Args: []string{"--tensor-parallel-size", "2", "--pipeline-parallel-size", "2"},
	}}}}
	if got := SpanForPod(pod, &pod.Spec.Containers[0], rules, 8); got != 4 {
		t.Errorf("SpanForPod(span=2, multiplier=2) = %d, want 4", got)
	}
	// span=2, multiplier=3 → 6 → snap(6) = 8.
	pod.Spec.Containers[0].Args = []string{"--tensor-parallel-size", "2", "--pipeline-parallel-size", "3"}
	if got := SpanForPod(pod, &pod.Spec.Containers[0], rules, 8); got != 8 {
		t.Errorf("SpanForPod(span=2, multiplier=3) = %d, want 8 (6 snaps up)", got)
	}
	// span=4, multiplier=1 (implicit) → 4, capped by nodeMaxSpan=2 → 2.
	pod.Spec.Containers[0].Args = []string{"--tensor-parallel-size", "4"}
	if got := SpanForPod(pod, &pod.Spec.Containers[0], rules, 2); got != 2 {
		t.Errorf("SpanForPod(span=4, nodeMaxSpan=2) = %d, want 2 (capped)", got)
	}
}

// TestDesiredSpanIgnoresNodeCap proves DesiredSpan, unlike SpanForPod, is not clamped to a node max.
func TestDesiredSpanIgnoresNodeCap(t *testing.T) {
	rules, err := CompileIntent(cpopv1alpha1.IntentSpec{Span: vllmTP, Multiplier: &vllmMultiplier})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// span=4 → DesiredSpan = 4, regardless of any node's max span.
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "vllm",
		Args: []string{"--tensor-parallel-size", "4"},
	}}}}
	if got := DesiredSpan(pod, &pod.Spec.Containers[0], rules); got != 4 {
		t.Errorf("DesiredSpan(span=4) = %d, want 4 (un-capped)", got)
	}
	// span=2 × multiplier=3 = 6 → snap(6) = 8, still un-capped.
	pod.Spec.Containers[0].Args = []string{"--tensor-parallel-size", "2", "--pipeline-parallel-size", "3"}
	if got := DesiredSpan(pod, &pod.Spec.Containers[0], rules); got != 8 {
		t.Errorf("DesiredSpan(span=2, multiplier=3) = %d, want 8", got)
	}
	// SpanForPod caps the same intent to 2 on a 2-span node.
	if got := SpanForPod(pod, &pod.Spec.Containers[0], rules, 2); got != 2 {
		t.Errorf("SpanForPod(desired 8, nodeMaxSpan 2) = %d, want 2", got)
	}
}

// TestBalloonNameForPodCoLocateSuffix pins the coLocate → balloon-name suffix mapping the generator relies on.
func TestBalloonNameForPodCoLocateSuffix(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm",
			Args: []string{"--tensor-parallel-size", "2"},
		}}},
	}
	base := cpopv1alpha1.IntentSpec{Span: vllmTP}
	cases := []struct {
		coLocate string
		want     string
	}{
		{"", "balloon-span2"},
		{"package", "balloon-span2-colocate"},
		{"numa", "balloon-span2-spread"},
	}
	for _, tc := range cases {
		spec := base
		spec.CoLocate = tc.coLocate
		rules, err := CompileIntent(spec)
		if err != nil {
			t.Fatalf("compile(coLocate=%q): %v", tc.coLocate, err)
		}
		got := BalloonNameForPod(pod, &pod.Spec.Containers[0], rules, 8)
		if got != tc.want {
			t.Errorf("coLocate=%q → %q, want %q", tc.coLocate, got, tc.want)
		}
	}
}

// devicePod builds a one-container pod requesting one unit of resource on `where` ("requests" or "limits").
func devicePod(resourceName, where string, args ...string) *corev1.Pod {
	c := corev1.Container{Name: "workload", Args: args}
	rl := corev1.ResourceList{corev1.ResourceName(resourceName): resource.MustParse("1")}
	if where == "limits" {
		c.Resources.Limits = rl
	} else {
		c.Resources.Requests = rl
	}
	return &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{c}}}
}

// computePod builds a one-container pod requesting only standard compute resources.
func computePod(rl corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name:      "workload",
		Resources: corev1.ResourceRequirements{Requests: rl},
	}}}}
}

// TestPodRequestsDevice proves any non-standard resource is a device, while standard
// compute resources and zero quantities are not.
func TestPodRequestsDevice(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		// GPUs remain the motivating case.
		{"nvidia on requests", devicePod("nvidia.com/gpu", "requests"), true},
		{"nvidia on limits only", devicePod("nvidia.com/gpu", "limits"), true},
		{"intel i915", devicePod("gpu.intel.com/i915", "requests"), true},
		{"intel xe", devicePod("gpu.intel.com/xe", "requests"), true},
		// Generic detection: any non-standard resource is a device, GPU or not.
		{"non-gpu extended resource (fpga)", devicePod("example.com/fpga", "requests"), true},
		{"non-gpu extended resource (nic) on limits", devicePod("example.com/nic", "limits"), true},
		{"device on init container", &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "workload"}},
			InitContainers: []corev1.Container{{
				Name:      "device-init",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")}},
			}},
		}}, true},
		// Standard compute resources must NOT be treated as devices.
		{"cpu only", computePod(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}), false},
		{"cpu + memory", computePod(corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
		}), false},
		{"ephemeral-storage", computePod(corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}), false},
		{"hugepages-2Mi is not a device", computePod(corev1.ResourceList{corev1.ResourceName("hugepages-2Mi"): resource.MustParse("128Mi")}), false},
		{"hugepages-1Gi is not a device", computePod(corev1.ResourceList{corev1.ResourceName("hugepages-1Gi"): resource.MustParse("2Gi")}), false},
		{"zero device quantity is not a device pod", &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "workload",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("0")}},
		}}}}, false},
		{"nil pod", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PodRequestsDevice(tc.pod); got != tc.want {
				t.Errorf("PodRequestsDevice = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDevicePodIgnoresIntentFlags proves a device pod goes to the device balloon
// with its intent flags ignored by DesiredSpan, SpanForPod and BalloonNameForPod.
func TestDevicePodIgnoresIntentFlags(t *testing.T) {
	rules, err := CompileIntent(cpopv1alpha1.IntentSpec{Span: vllmTP, Multiplier: &vllmMultiplier, CoLocate: "numa"})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// TP 4 × PP 2 = 8 for a non-device pod; a device pod ignores both.
	pod := devicePod("nvidia.com/gpu", "requests", "--tensor-parallel-size", "4", "--pipeline-parallel-size", "2")
	c := &pod.Spec.Containers[0]

	if got := DesiredSpan(pod, c, rules); got != 1 {
		t.Errorf("DesiredSpan(device pod, tp=4 pp=2) = %d, want 1 (intent ignored)", got)
	}
	if got := SpanForPod(pod, c, rules, 8); got != 1 {
		t.Errorf("SpanForPod(device pod) = %d, want 1", got)
	}
	if got := BalloonNameForPod(pod, c, rules, 8); got != deviceBalloonName {
		t.Errorf("BalloonNameForPod(device pod, coLocate=numa) = %q, want %q", got, deviceBalloonName)
	}
}

// TestNonGPUDevicePodRoutesBalanced proves a non-GPU device (an FPGA) also routes to the device balloon.
func TestNonGPUDevicePodRoutesBalanced(t *testing.T) {
	rules, err := CompileIntent(cpopv1alpha1.IntentSpec{Span: vllmTP, CoLocate: "numa"})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	pod := devicePod("example.com/fpga", "limits", "--tensor-parallel-size", "4")
	c := &pod.Spec.Containers[0]
	if got := DesiredSpan(pod, c, rules); got != 1 {
		t.Errorf("DesiredSpan(fpga pod, tp=4) = %d, want 1 (intent ignored)", got)
	}
	if got := BalloonNameForPod(pod, c, rules, 8); got != deviceBalloonName {
		t.Errorf("BalloonNameForPod(fpga pod, coLocate=numa) = %q, want %q", got, deviceBalloonName)
	}
}

// TestDeviceDetectionSecondaryContainer proves a device request on any container flips the whole pod.
func TestDeviceDetectionSecondaryContainer(t *testing.T) {
	rules, err := CompileIntent(cpopv1alpha1.IntentSpec{Span: vllmTP})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "main", Args: []string{"--tensor-parallel-size", "4"}},
		{Name: "device-sidecar", Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceName("gpu.intel.com/i915"): resource.MustParse("1")},
		}},
	}}}
	if !PodRequestsDevice(pod) {
		t.Fatal("PodRequestsDevice = false, want true (device on secondary container)")
	}
	// Pod-scoped (container nil): still the device balloon, intent on the main
	// container ignored.
	if got := BalloonNameForPod(pod, nil, rules, 8); got != deviceBalloonName {
		t.Errorf("BalloonNameForPod(device sidecar pod) = %q, want %q", got, deviceBalloonName)
	}
}

// TestDeviceDetectionInitContainer proves a device request on an init container flips the whole pod.
func TestDeviceDetectionInitContainer(t *testing.T) {
	rules, err := CompileIntent(cpopv1alpha1.IntentSpec{Span: vllmTP})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "main", Args: []string{"--tensor-parallel-size", "4"}},
		},
		InitContainers: []corev1.Container{
			{Name: "device-init", Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")},
			}},
		},
	}}
	if !PodRequestsDevice(pod) {
		t.Fatal("PodRequestsDevice = false, want true (device on init container)")
	}
	// Pod-scoped (container nil): still the device balloon, intent on the main
	// container ignored.
	if got := BalloonNameForPod(pod, nil, rules, 8); got != deviceBalloonName {
		t.Errorf("BalloonNameForPod(device init pod) = %q, want %q", got, deviceBalloonName)
	}
}

// TestNonDevicePodKeepsIntentBehavior proves a cpu + hugepages pod still derives span from intent.
func TestNonDevicePodKeepsIntentBehavior(t *testing.T) {
	rules, err := CompileIntent(cpopv1alpha1.IntentSpec{Span: vllmTP, CoLocate: "numa"})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "vllm",
		Args: []string{"--tensor-parallel-size", "4"},
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:                   resource.MustParse("8"),
			corev1.ResourceName("hugepages-2Mi"): resource.MustParse("128Mi"),
		}},
	}}}}
	c := &pod.Spec.Containers[0]
	if got := DesiredSpan(pod, c, rules); got != 4 {
		t.Errorf("DesiredSpan(non-device, tp=4) = %d, want 4", got)
	}
	if got := BalloonNameForPod(pod, c, rules, 8); got != "balloon-span4-spread" {
		t.Errorf("BalloonNameForPod(non-device, tp=4, coLocate=numa) = %q, want %q", got, "balloon-span4-spread")
	}
}

func TestSnapOfProduct(t *testing.T) {
	cases := []struct {
		tp, pp int
		want   int
	}{
		{1, 1, 1},
		{2, 1, 2},
		{2, 2, 4},
		{4, 2, 8},
		{8, 1, 8},
		{4, 4, 8}, // 16 capped/snapped to 8
	}
	for _, tc := range cases {
		if got := Snap(tc.tp * tc.pp); got != tc.want {
			t.Errorf("Snap(%d*%d) = %d, want %d", tc.tp, tc.pp, got, tc.want)
		}
	}
}

func TestIndexMatch(t *testing.T) {
	idx := NewIndex()
	idx.Update([]PolicyRule{
		{Namespaces: []string{"inference"}, ByContainerName: []string{"vllm"}},
	})

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "inference"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "sidecar"},
				{Name: "vllm", Args: []string{"--tensor-parallel-size", "2"}},
			},
		},
	}

	res := idx.Match(pod)
	if res.Rule == nil || res.Container == nil {
		t.Fatalf("expected match, got %+v", res)
	}
	if res.Container.Name != "vllm" {
		t.Errorf("matched container = %s, want vllm", res.Container.Name)
	}

	unmatched := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "inference"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "other"}}},
	}
	if res := idx.Match(unmatched); res.Rule != nil || res.Container != nil {
		t.Errorf("expected no match, got %+v", res)
	}
}

// TestIndexMatchNamespaceIsolation proves a rule never matches pods in another policy's namespace.
func TestIndexMatchNamespaceIsolation(t *testing.T) {
	idx := NewIndex()
	idx.Update([]PolicyRule{
		{Namespaces: []string{"ns-a"}, ByContainerName: []string{"vllm"}, CapacityCheckMode: "warn"},
		{Namespaces: []string{"ns-b"}, ByContainerName: []string{"sglang"}, CapacityCheckMode: "off"},
	})

	// A "vllm" pod in ns-b must NOT match the ns-a rule (nor the ns-b rule,
	// whose container is "sglang").
	podB := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-b"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm"}}},
	}
	if res := idx.Match(podB); res.Rule != nil || res.Container != nil {
		t.Errorf("vllm pod in ns-b should not match ns-a rule, got %+v", res)
	}

	// A "vllm" pod in ns-a matches the ns-a rule.
	podA := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm"}}},
	}
	res := idx.Match(podA)
	if res.Rule == nil || res.Container == nil {
		t.Fatalf("vllm pod in ns-a should match ns-a rule, got %+v", res)
	}
	if res.Rule.CapacityCheckMode != "warn" {
		t.Errorf("matched wrong rule: CapacityCheckMode=%s, want warn", res.Rule.CapacityCheckMode)
	}
}

// TestIndexMatchOverlapNameOrderWins: on an overlap the rule whose name sorts
// first wins whatever order Update got, and a pod only one rule matches keeps it.
func TestIndexMatchOverlapNameOrderWins(t *testing.T) {
	early := PolicyRule{Name: "00-specific", Namespaces: []string{"ns-a"}, ByContainerName: []string{"vllm"}}
	late := PolicyRule{Name: "01-broad", Namespaces: []string{"ns-*"}, ByContainerName: []string{"vllm"}}
	overlap := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm"}}},
	}
	onlyLate := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-b"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm"}}},
	}
	for _, order := range [][]PolicyRule{{early, late}, {late, early}} {
		idx := NewIndex()
		idx.Update(order)
		if res := idx.Match(overlap); res.Rule == nil || res.Rule.Name != early.Name {
			t.Errorf("Update(%s, %s): overlap matched %+v, want %s", order[0].Name, order[1].Name, res.Rule, early.Name)
		}
		if res := idx.Match(onlyLate); res.Rule == nil || res.Rule.Name != late.Name {
			t.Errorf("Update(%s, %s): ns-b pod matched %+v, want %s", order[0].Name, order[1].Name, res.Rule, late.Name)
		}
	}
}

// mustSelector compiles a MatchLabels selector or fails the test.
func mustSelector(t *testing.T, matchLabels map[string]string) labels.Selector {
	t.Helper()
	sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: matchLabels})
	if err != nil {
		t.Fatalf("compile selector %v: %v", matchLabels, err)
	}
	return sel
}

// TestResolveNodeConfig proves the first matching override layers per field over the cluster default.
func TestResolveNodeConfig(t *testing.T) {
	// A distinct pointer per call so identity checks can tell default from override.
	clusterCpus := 12
	clusterReserved := &cpopv1alpha1.ReservedSizingSpec{Cpus: &clusterCpus}

	t.Run("no overrides returns cluster default", func(t *testing.T) {
		idx := NewIndex()
		idx.SetClusterConfig(ClusterConfig{CPUAllocation: "physical-cores", Reserved: clusterReserved})
		alloc, reserved := idx.ResolveNodeConfig(map[string]string{"any": "label"})
		if alloc != "physical-cores" {
			t.Errorf("cpuAllocation = %q, want physical-cores", alloc)
		}
		if reserved != clusterReserved {
			t.Errorf("reserved = %+v, want cluster default pointer", reserved)
		}
	})

	t.Run("nil overrides slice returns cluster default", func(t *testing.T) {
		idx := NewIndex()
		idx.SetClusterConfig(ClusterConfig{CPUAllocation: "hyperthreads", Reserved: clusterReserved, NodeOverrides: nil})
		alloc, reserved := idx.ResolveNodeConfig(map[string]string{"node-tier": "perf"})
		if alloc != "hyperthreads" || reserved != clusterReserved {
			t.Errorf("resolved (%q,%+v), want cluster default (hyperthreads, %+v)", alloc, reserved, clusterReserved)
		}
	})

	t.Run("node matching no override returns cluster default", func(t *testing.T) {
		idx := NewIndex()
		ovCpus := 20
		idx.SetClusterConfig(ClusterConfig{
			CPUAllocation: "physical-cores",
			Reserved:      clusterReserved,
			NodeOverrides: []NodeConfigOverride{{
				Selector:      mustSelector(t, map[string]string{"node-tier": "perf"}),
				CPUAllocation: "hyperthreads",
				Reserved:      &cpopv1alpha1.ReservedSizingSpec{Cpus: &ovCpus},
			}},
		})
		alloc, reserved := idx.ResolveNodeConfig(map[string]string{"node-tier": "standard"})
		if alloc != "physical-cores" {
			t.Errorf("non-matching node cpuAllocation = %q, want cluster default physical-cores", alloc)
		}
		if reserved != clusterReserved {
			t.Errorf("non-matching node reserved = %+v, want cluster default", reserved)
		}
	})

	t.Run("first matching override wins", func(t *testing.T) {
		idx := NewIndex()
		firstCpus, secondCpus := 6, 20
		idx.SetClusterConfig(ClusterConfig{
			CPUAllocation: "physical-cores",
			Reserved:      clusterReserved,
			NodeOverrides: []NodeConfigOverride{
				{
					Selector:      mustSelector(t, map[string]string{"a": "1"}),
					CPUAllocation: "hyperthreads",
					Reserved:      &cpopv1alpha1.ReservedSizingSpec{Cpus: &firstCpus},
				},
				{
					Selector:      mustSelector(t, map[string]string{"b": "2"}),
					CPUAllocation: "physical-cores",
					Reserved:      &cpopv1alpha1.ReservedSizingSpec{Cpus: &secondCpus},
				},
			},
		})
		// Node matches BOTH selectors; the FIRST entry must win.
		alloc, reserved := idx.ResolveNodeConfig(map[string]string{"a": "1", "b": "2"})
		if alloc != "hyperthreads" {
			t.Errorf("cpuAllocation = %q, want hyperthreads (first override)", alloc)
		}
		if reserved == nil || reserved.Cpus == nil || *reserved.Cpus != firstCpus {
			t.Errorf("reserved = %+v, want Cpus=%d (first override)", reserved, firstCpus)
		}
	})

	t.Run("cpuAllocation-only override keeps cluster default reserved", func(t *testing.T) {
		idx := NewIndex()
		idx.SetClusterConfig(ClusterConfig{
			CPUAllocation: "physical-cores",
			Reserved:      clusterReserved,
			NodeOverrides: []NodeConfigOverride{{
				Selector:      mustSelector(t, map[string]string{"node-tier": "perf"}),
				CPUAllocation: "hyperthreads",
				Reserved:      nil, // inherit cluster default reserved
			}},
		})
		alloc, reserved := idx.ResolveNodeConfig(map[string]string{"node-tier": "perf"})
		if alloc != "hyperthreads" {
			t.Errorf("cpuAllocation = %q, want hyperthreads (override)", alloc)
		}
		if reserved != clusterReserved {
			t.Errorf("reserved = %+v, want cluster default (override left Reserved nil)", reserved)
		}
	})

	t.Run("reserved-only override keeps cluster default cpuAllocation", func(t *testing.T) {
		idx := NewIndex()
		ovCpus := 20
		ovReserved := &cpopv1alpha1.ReservedSizingSpec{Cpus: &ovCpus}
		// A reserved-only override must not change cpuAllocation.
		idx.SetClusterConfig(ClusterConfig{
			CPUAllocation: "hyperthreads",
			Reserved:      clusterReserved,
			NodeOverrides: []NodeConfigOverride{{
				Selector:      mustSelector(t, map[string]string{"node-tier": "perf"}),
				CPUAllocation: "", // inherit cluster default cpuAllocation
				Reserved:      ovReserved,
			}},
		})
		alloc, reserved := idx.ResolveNodeConfig(map[string]string{"node-tier": "perf"})
		if alloc != "hyperthreads" {
			t.Errorf("cpuAllocation = %q, want hyperthreads (reserved-only override must not flip it)", alloc)
		}
		if reserved != ovReserved {
			t.Errorf("reserved = %+v, want override Cpus=%d", reserved, ovCpus)
		}
	})

	t.Run("measured-reserved override applies", func(t *testing.T) {
		idx := NewIndex()
		headroom := 40
		measured := &cpopv1alpha1.ReservedSizingSpec{
			Measured: &cpopv1alpha1.MeasuredSizingSpec{HeadroomPercent: &headroom},
		}
		idx.SetClusterConfig(ClusterConfig{
			CPUAllocation: "physical-cores",
			Reserved:      clusterReserved,
			NodeOverrides: []NodeConfigOverride{{
				Selector: mustSelector(t, map[string]string{"node-tier": "perf"}),
				Reserved: measured,
			}},
		})
		alloc, reserved := idx.ResolveNodeConfig(map[string]string{"node-tier": "perf"})
		if alloc != "physical-cores" {
			t.Errorf("cpuAllocation = %q, want cluster default physical-cores", alloc)
		}
		if reserved != measured {
			t.Fatalf("reserved = %+v, want the measured override spec", reserved)
		}
		if reserved.Measured == nil || reserved.Measured.HeadroomPercent == nil || *reserved.Measured.HeadroomPercent != headroom {
			t.Errorf("resolved measured spec = %+v, want HeadroomPercent=%d", reserved.Measured, headroom)
		}
	})

	t.Run("override with nil selector is skipped", func(t *testing.T) {
		idx := NewIndex()
		ovCpus := 20
		idx.SetClusterConfig(ClusterConfig{
			CPUAllocation: "physical-cores",
			Reserved:      clusterReserved,
			NodeOverrides: []NodeConfigOverride{
				{
					// nil selector: must be skipped, not treated as match-all.
					Selector:      nil,
					CPUAllocation: "hyperthreads",
				},
				{
					Selector:      mustSelector(t, map[string]string{"node-tier": "perf"}),
					CPUAllocation: "physical-cores",
					Reserved:      &cpopv1alpha1.ReservedSizingSpec{Cpus: &ovCpus},
				},
			},
		})
		// The nil-selector entry must not swallow the match.
		alloc, reserved := idx.ResolveNodeConfig(map[string]string{"node-tier": "perf"})
		if alloc != "physical-cores" {
			t.Errorf("cpuAllocation = %q, want physical-cores (second override)", alloc)
		}
		if reserved == nil || reserved.Cpus == nil || *reserved.Cpus != ovCpus {
			t.Errorf("reserved = %+v, want Cpus=%d", reserved, ovCpus)
		}
		// Matching neither override keeps the cluster default.
		alloc, reserved = idx.ResolveNodeConfig(map[string]string{"node-tier": "standard"})
		if alloc != "physical-cores" || reserved != clusterReserved {
			t.Errorf("unmatched node resolved (%q,%+v), want cluster default", alloc, reserved)
		}
	})
}

// TestResolveNodeConfigForClass proves cluster default → class profile → override layering.
func TestResolveNodeConfigForClass(t *testing.T) {
	clusterCpus := 12
	clusterReserved := &cpopv1alpha1.ReservedSizingSpec{Cpus: &clusterCpus}
	classCpus := 16
	classReserved := &cpopv1alpha1.ReservedSizingSpec{Cpus: &classCpus}
	ovCpus := 20
	ovReserved := &cpopv1alpha1.ReservedSizingSpec{Cpus: &ovCpus}

	baseConfig := func() ClusterConfig {
		return ClusterConfig{
			CPUAllocation: "physical-cores",
			Reserved:      clusterReserved,
			ClassProfiles: map[string]ClassProfile{
				"cpu-amx":  {CPUAllocation: "hyperthreads", Reserved: classReserved},
				"gpu-only": {Reserved: classReserved}, // cpuAllocation unset → inherit
			},
		}
	}

	t.Run("empty class falls back to cluster default (== ResolveNodeConfig)", func(t *testing.T) {
		idx := NewIndex()
		idx.SetClusterConfig(baseConfig())
		alloc, reserved := idx.ResolveNodeConfigForClass(map[string]string{"any": "x"}, "")
		if alloc != "physical-cores" || reserved != clusterReserved {
			t.Errorf("resolved (%q,%+v), want cluster default", alloc, reserved)
		}
	})

	t.Run("class with no profile keeps cluster default base", func(t *testing.T) {
		idx := NewIndex()
		idx.SetClusterConfig(baseConfig())
		alloc, reserved := idx.ResolveNodeConfigForClass(map[string]string{"any": "x"}, "cpu-only")
		if alloc != "physical-cores" || reserved != clusterReserved {
			t.Errorf("resolved (%q,%+v), want cluster default", alloc, reserved)
		}
	})

	t.Run("class profile layers per-field over cluster default", func(t *testing.T) {
		idx := NewIndex()
		idx.SetClusterConfig(baseConfig())
		alloc, reserved := idx.ResolveNodeConfigForClass(map[string]string{"any": "x"}, "cpu-amx")
		if alloc != "hyperthreads" {
			t.Errorf("cpuAllocation = %q, want hyperthreads (class profile)", alloc)
		}
		if reserved != classReserved {
			t.Errorf("reserved = %+v, want class profile reserved", reserved)
		}
	})

	t.Run("class profile partial: unset field inherits cluster default", func(t *testing.T) {
		idx := NewIndex()
		idx.SetClusterConfig(baseConfig())
		alloc, reserved := idx.ResolveNodeConfigForClass(map[string]string{"any": "x"}, "gpu-only")
		if alloc != "physical-cores" {
			t.Errorf("cpuAllocation = %q, want cluster default physical-cores (profile unset)", alloc)
		}
		if reserved != classReserved {
			t.Errorf("reserved = %+v, want class profile reserved", reserved)
		}
	})

	t.Run("nodeConfigOverride wins over class profile per-field", func(t *testing.T) {
		idx := NewIndex()
		cfg := baseConfig()
		// Override sets only reserved; cpuAllocation should stay the class profile's.
		cfg.NodeOverrides = []NodeConfigOverride{{
			Selector: mustSelector(t, map[string]string{"node-tier": "perf"}),
			Reserved: ovReserved,
		}}
		idx.SetClusterConfig(cfg)
		alloc, reserved := idx.ResolveNodeConfigForClass(map[string]string{"node-tier": "perf"}, "cpu-amx")
		if alloc != "hyperthreads" {
			t.Errorf("cpuAllocation = %q, want class profile hyperthreads (override didn't set it)", alloc)
		}
		if reserved != ovReserved {
			t.Errorf("reserved = %+v, want override reserved", reserved)
		}
	})
}

func TestNodeExcluded(t *testing.T) {
	idx := NewIndex()
	// No selector configured → nothing excluded.
	if idx.NodeExcluded(map[string]string{"any": "label"}) {
		t.Error("no selector should exclude nothing")
	}

	sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"cpu-pinning-operator.intel.com/disabled": "true"},
	})
	if err != nil {
		t.Fatalf("compile selector: %v", err)
	}
	idx.SetClusterConfig(ClusterConfig{ExcludeNode: sel})

	if !idx.NodeExcluded(map[string]string{"cpu-pinning-operator.intel.com/disabled": "true"}) {
		t.Error("node with matching label should be excluded")
	}
	if idx.NodeExcluded(map[string]string{"other": "label"}) {
		t.Error("node without matching label should not be excluded")
	}

	// Clearing the selector re-includes everything.
	idx.SetClusterConfig(ClusterConfig{})
	if idx.NodeExcluded(map[string]string{"cpu-pinning-operator.intel.com/disabled": "true"}) {
		t.Error("cleared selector should exclude nothing")
	}
}

// TestHTSiblingPolicy proves the getter is "" until set and an empty config clears it.
func TestHTSiblingPolicy(t *testing.T) {
	idx := NewIndex()
	if got := idx.HTSiblingPolicy(); got != "" {
		t.Errorf("HTSiblingPolicy with no config = %q, want empty", got)
	}
	idx.SetClusterConfig(ClusterConfig{HTSiblingPolicy: "reserved"})
	if got := idx.HTSiblingPolicy(); got != "reserved" {
		t.Errorf("HTSiblingPolicy = %q, want %q", got, "reserved")
	}
	idx.SetClusterConfig(ClusterConfig{})
	if got := idx.HTSiblingPolicy(); got != "" {
		t.Errorf("HTSiblingPolicy after clear = %q, want empty", got)
	}
}

func TestNamespaceMatches(t *testing.T) {
	cases := []struct {
		pattern, ns string
		want        bool
	}{
		{"inference", "inference", true},
		{"inference", "inference-a", false},
		{"inference-*", "inference-a", true},
		{"inference-*", "inference-team-1", true},
		{"inference-*", "inference", false}, // '*' needs at least the dash+something
		{"inference-*", "prod-inference", false},
		{"*", "anything", true},
		{"team-?", "team-a", true},
		{"team-?", "team-ab", false},
	}
	for _, tc := range cases {
		if got := NamespaceMatches(tc.pattern, tc.ns); got != tc.want {
			t.Errorf("NamespaceMatches(%q,%q) = %v, want %v", tc.pattern, tc.ns, got, tc.want)
		}
	}
	if !IsGlob("a-*") || !IsGlob("a?") || IsGlob("exact") {
		t.Error("IsGlob misclassified a pattern")
	}
}

func TestMatchNamespaceGlobIsolation(t *testing.T) {
	idx := NewIndex()
	idx.Update([]PolicyRule{
		{Namespaces: []string{"inference-*"}, ByContainerName: []string{"vllm"}},
	})
	// vllm pod in a glob-matching namespace → matches.
	podIn := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "inference-team-a"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm"}}},
	}
	if res := idx.Match(podIn); res.Rule == nil {
		t.Error("vllm pod in inference-team-a should match inference-* rule")
	}
	// vllm pod in a non-matching namespace → no match.
	podOut := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm"}}},
	}
	if res := idx.Match(podOut); res.Rule != nil {
		t.Error("vllm pod in prod should NOT match inference-* rule")
	}
}

// cpuContainer builds a container with a CPU request in whole cores.
func cpuContainer(name string, cores int) corev1.Container {
	return corev1.Container{
		Name: name,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: *resource.NewQuantity(int64(cores), resource.DecimalSI),
			},
		},
	}
}

// A byContainerName glob matching exactly one container pins it.
func TestMatchGlobSingleHit(t *testing.T) {
	idx := NewIndex()
	idx.Update([]PolicyRule{{
		Namespaces: []string{"inference"}, ByContainerName: []string{"vllm-*"},
	}})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "inference"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "sidecar"}, {Name: "vllm-server"},
		}},
	}
	res := idx.Match(pod)
	if res.Rule == nil || res.Container == nil || res.Ambiguous {
		t.Fatalf("glob single-hit should be container-scoped, got %+v", res)
	}
	if res.Container.Name != "vllm-server" {
		t.Errorf("matched %s, want vllm-server", res.Container.Name)
	}
}

// A glob matching multiple containers under wholePod is ambiguous.
func TestMatchGlobMultiHitWholePodAmbiguous(t *testing.T) {
	idx := NewIndex()
	idx.Update([]PolicyRule{{
		Namespaces: []string{"inference"}, ByContainerName: []string{"vllm-*"},
		ContainerSelection: containerSelectionWholePod,
	}})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "inference"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "vllm-a"}, {Name: "vllm-b"},
		}},
	}
	res := idx.Match(pod)
	if res.Rule == nil || !res.Ambiguous {
		t.Fatalf("glob multi-hit under wholePod should be ambiguous, got %+v", res)
	}
	if res.Container != nil {
		t.Errorf("ambiguous result should carry no container, got %s", res.Container.Name)
	}
}

// A glob matching multiple containers under largestContainer pins the highest-CPU one.
func TestMatchGlobMultiHitLargest(t *testing.T) {
	idx := NewIndex()
	idx.Update([]PolicyRule{{
		Namespaces: []string{"inference"}, ByContainerName: []string{"vllm-*"},
		ContainerSelection: containerSelectionLargest,
	}})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "inference"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			cpuContainer("vllm-a", 4),
			cpuContainer("vllm-b", 8),
		}},
	}
	res := idx.Match(pod)
	if res.Rule == nil || res.Container == nil || res.Ambiguous {
		t.Fatalf("largestContainer should pick a container, got %+v", res)
	}
	if res.Container.Name != "vllm-b" {
		t.Errorf("largestContainer picked %s, want vllm-b (8 cores)", res.Container.Name)
	}
}

// A byPodLabels match defaults to pod-scoped (wholePod).
func TestMatchLabelWholePod(t *testing.T) {
	idx := NewIndex()
	idx.Update([]PolicyRule{{
		Namespaces: []string{"inference"}, ByPodLabels: map[string]string{"app": "vllm"},
		ContainerSelection: containerSelectionWholePod,
	}})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "inference", Labels: map[string]string{"app": "vllm"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			cpuContainer("main", 4), cpuContainer("side", 2),
		}},
	}
	res := idx.Match(pod)
	if res.Rule == nil || !res.PodScoped || res.Container != nil {
		t.Fatalf("label match under wholePod should be pod-scoped, got %+v", res)
	}
}

// A byPodLabels match under largestContainer pins the biggest container.
func TestMatchLabelLargest(t *testing.T) {
	idx := NewIndex()
	idx.Update([]PolicyRule{{
		Namespaces: []string{"inference"}, ByPodLabels: map[string]string{"app": "vllm"},
		ContainerSelection: containerSelectionLargest,
	}})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "inference", Labels: map[string]string{"app": "vllm"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			cpuContainer("side", 2), cpuContainer("main", 16),
		}},
	}
	res := idx.Match(pod)
	if res.Rule == nil || res.Container == nil || res.PodScoped {
		t.Fatalf("label match under largestContainer should be container-scoped, got %+v", res)
	}
	if res.Container.Name != "main" {
		t.Errorf("picked %s, want main (16 cores)", res.Container.Name)
	}
}
