// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// staticKubeletConfig is a realistic kubeadm KubeletConfiguration with a static policy.
const staticKubeletConfig = `apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cpuManagerPolicy: static
cpuManagerReconcilePeriod: 10s
kubeReserved:
  cpu: 100m
syncFrequency: 1m0s
topologyManagerPolicy: none
`

// policylessKubeletConfig omits cpuManagerPolicy, so the kubelet runs the default "none".
const policylessKubeletConfig = `apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cpuManagerReconcilePeriod: 10s
kubeReserved:
  cpu: 100m
syncFrequency: 1m0s
topologyManagerPolicy: none
`

// kubeletConfigReader serves the kubeadm ConfigMap from a fake client; no data means no ConfigMap.
func kubeletConfigReader(t *testing.T, data map[string]string) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	if data != nil {
		b = b.WithObjects(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      kubeletConfigMap.Name,
				Namespace: kubeletConfigMap.Namespace,
			},
			Data: data,
		})
	}
	return b
}

// TestParseCPUManagerPolicy proves unreadable documents give "" and only a
// KubeletConfiguration omitting the field defaults to "none".
func TestParseCPUManagerPolicy(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"static policy in a KubeletConfiguration document", staticKubeletConfig, "static"},
		{"none policy", "cpuManagerPolicy: none\n", "none"},
		// Stock kubeadm: the field is absent, so the default applies.
		{"KubeletConfiguration omitting the optional field is the default none", policylessKubeletConfig, "none"},
		{"minimal KubeletConfiguration omitting the field is none", "kind: KubeletConfiguration\n", "none"},
		// Not a KubeletConfiguration: no default.
		{"policy-less document of no declared kind is unknown", "topologyManagerPolicy: none\n", ""},
		{"policy-less document of another kind is unknown", "kind: ConfigMap\ndata:\n  a: b\n", ""},
		// The kind match is exact.
		{"mis-cased kind is unknown", "kind: kubeletconfiguration\n", ""},
		{"non-string kind is unknown", "kind: 7\n", ""},
		// JSON is valid YAML, so a kubelet configured from a JSON document parses too.
		{"json document", `{"cpuManagerPolicy":"static"}`, "static"},
		{"document that is not YAML is unknown", "\tnot: [a, document", ""},
		{"empty document is unknown", "", ""},
		// A nested policy (e.g. a /configz envelope) is not read.
		{"key nested under another is unknown", "kubeletconfig:\n  cpuManagerPolicy: static\n", ""},
		// The key match is exact, unlike encoding/json struct decoding.
		{"differently-cased key is unknown", "CpuMAnAgerPoliCY: static\n", ""},
		{"lowercased key is unknown", "cpumanagerpolicy: static\n", ""},
		// A non-string policy is unknown.
		{"non-string policy is unknown", "cpuManagerPolicy: true\n", ""},
		{"null policy is unknown", "cpuManagerPolicy: null\n", ""},
		{"mapping under the key is unknown", "cpuManagerPolicy:\n  static: true\n", ""},
		// A named policy is never defaulted; an unrecognised one comes back verbatim.
		{"non-string policy in a KubeletConfiguration is unknown, not the default", "kind: KubeletConfiguration\ncpuManagerPolicy: true\n", ""},
		{"null policy in a KubeletConfiguration is unknown, not the default", "kind: KubeletConfiguration\ncpuManagerPolicy: null\n", ""},
		{"mapping under the key in a KubeletConfiguration is unknown, not the default", "kind: KubeletConfiguration\ncpuManagerPolicy:\n  static: true\n", ""},
		{"empty policy in a KubeletConfiguration is unknown, not the default", "kind: KubeletConfiguration\ncpuManagerPolicy: \"\"\n", ""},
		{"unrecognised policy in a KubeletConfiguration is read verbatim", "kind: KubeletConfiguration\ncpuManagerPolicy: Static\n", "Static"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseCPUManagerPolicy([]byte(tc.doc)); got != tc.want {
				t.Errorf("ParseCPUManagerPolicy(%q) = %q, want %q", tc.doc, got, tc.want)
			}
		})
	}
}

// TestClassifyCPUManagerPolicy proves only an exact "static" or "none" yields a verdict.
func TestClassifyCPUManagerPolicy(t *testing.T) {
	cases := []struct {
		policy string
		want   CPUManagerVerdict
	}{
		{"static", CPUManagerConflict},
		{"none", CPUManagerCompatible},
		{"", CPUManagerUnknown},
		// Not recognised: unknown, never guessed.
		{"Static", CPUManagerUnknown},
		{"STATIC", CPUManagerUnknown},
		{"None", CPUManagerUnknown},
		{"future-policy", CPUManagerUnknown},
		{"static-options", CPUManagerUnknown},
		{" static", CPUManagerUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.policy, func(t *testing.T) {
			if got := ClassifyCPUManagerPolicy(tc.policy); got != tc.want {
				t.Errorf("ClassifyCPUManagerPolicy(%q) = %q, want %q", tc.policy, got, tc.want)
			}
		})
	}
}

// TestCPUManagerPolicy proves a policy is returned only when actually read; every unknown is "".
func TestCPUManagerPolicy(t *testing.T) {
	// Nil reader: unknown and no error.
	policy, err := CPUManagerPolicy(context.Background(), nil)
	if policy != "" || err != nil {
		t.Errorf("nil reader = (%q, %v), want (\"\", nil)", policy, err)
	}

	// No ConfigMap (any non-kubeadm cluster): unknown.
	policy, err = CPUManagerPolicy(context.Background(), kubeletConfigReader(t, nil).Build())
	if policy != "" {
		t.Errorf("absent ConfigMap policy = %q, want empty (an unread configuration must not flag a node)", policy)
	}
	if err == nil {
		t.Error("absent ConfigMap err = nil, want the read failure surfaced for the log")
	}

	// The ConfigMap exists but carries some other key.
	policy, err = CPUManagerPolicy(context.Background(),
		kubeletConfigReader(t, map[string]string{"kubelet.conf": staticKubeletConfig}).Build())
	if policy != "" {
		t.Errorf("wrong-key ConfigMap policy = %q, want empty", policy)
	}
	if err == nil {
		t.Error("wrong-key ConfigMap err = nil, want an error (unknown, not a successful read)")
	}

	// Successful read.
	policy, err = CPUManagerPolicy(context.Background(),
		kubeletConfigReader(t, map[string]string{kubeletConfigDataKey: staticKubeletConfig}).Build())
	if policy != CPUManagerPolicyStatic || err != nil {
		t.Errorf("static document = (%q, %v), want (%q, nil)", policy, err, CPUManagerPolicyStatic)
	}

	// An omitted field in a KubeletConfiguration is a successful read of "none".
	policy, err = CPUManagerPolicy(context.Background(),
		kubeletConfigReader(t, map[string]string{kubeletConfigDataKey: policylessKubeletConfig}).Build())
	if policy != CPUManagerPolicyNone || err != nil {
		t.Errorf("KubeletConfiguration omitting cpuManagerPolicy = (%q, %v), want (%q, nil) — the API default is a reading, not a guess",
			policy, err, CPUManagerPolicyNone)
	}

	// No policy and no kind: unknown, with an error.
	policy, err = CPUManagerPolicy(context.Background(),
		kubeletConfigReader(t, map[string]string{kubeletConfigDataKey: "syncFrequency: 1m0s\n"}).Build())
	if policy != "" {
		t.Errorf("policy-less document of no declared kind policy = %q, want empty", policy)
	}
	if err == nil {
		t.Error("policy-less document of no declared kind err = nil, want an error (unknown, not a successful read)")
	}
}
