// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The two kubelet CPU Manager policies the operator recognises. Under static the
// kubelet and the plugin both allocate exclusive cpusets; none leaves them to the
// plugin. Anything else is unknown, not compatible.
const (
	CPUManagerPolicyStatic = "static"
	CPUManagerPolicyNone   = "none"
)

// CPUManagerVerdict is who allocates cpusets on a node. Unknown is published as
// absence (no label, series or event), never as compatible.
type CPUManagerVerdict string

const (
	// CPUManagerUnknown covers a policy that could not be read AND one that was
	// read but is not a policy this operator recognises.
	CPUManagerUnknown CPUManagerVerdict = ""
	// CPUManagerConflict: the kubelet and the plugin both allocate cpusets.
	CPUManagerConflict CPUManagerVerdict = "conflict"
	// CPUManagerCompatible: the kubelet was read and leaves cpusets to the plugin.
	CPUManagerCompatible CPUManagerVerdict = "compatible"
)

// ClassifyCPUManagerPolicy is the single place a policy string becomes a verdict.
// Only an exact "static" or "none" is recognised; anything else is unknown, never a guess.
func ClassifyCPUManagerPolicy(policy string) CPUManagerVerdict {
	switch policy {
	case CPUManagerPolicyStatic:
		return CPUManagerConflict
	case CPUManagerPolicyNone:
		return CPUManagerCompatible
	default:
		return CPUManagerUnknown
	}
}

// kubeletConfigMap is kubeadm's cluster-wide KubeletConfiguration, readable with
// the existing configmaps grant. Per-kubelet reads would need node-proxy RBAC or a
// hostPath probe, so a hand-reconfigured kubelet is not detected.
var kubeletConfigMap = client.ObjectKey{Namespace: "kube-system", Name: "kubelet-config"}

// kubeletConfigDataKey is the KubeletConfiguration YAML key; only the unversioned ConfigMap is read.
const kubeletConfigDataKey = "kubelet"

// kubeletConfigurationKind gates defaulting an omitted cpuManagerPolicy.
const kubeletConfigurationKind = "KubeletConfiguration"

// ParseCPUManagerPolicy returns the cpuManagerPolicy a kubelet configured from doc
// runs, defaulting an omitted field only for a KubeletConfiguration; "" is unknown.
// Decoded as a plain map for an exact key match: struct decoding is case-insensitive,
// unlike the kubelet's strict decoder.
func ParseCPUManagerPolicy(data []byte) string {
	var kubeletConfig map[string]any
	if err := yaml.Unmarshal(data, &kubeletConfig); err != nil {
		return ""
	}
	raw, present := kubeletConfig["cpuManagerPolicy"]
	if !present {
		// The API default is "none", and kubeadm omits the key unless set; only a
		// KubeletConfiguration document gets that default.
		if kind, _ := kubeletConfig["kind"].(string); kind == kubeletConfigurationKind {
			return CPUManagerPolicyNone
		}
		return ""
	}
	// Present but not a string is unknown, not the omitted default.
	policy, _ := raw.(string)
	return policy
}

// CPUManagerPolicy returns the cluster kubelets' CPU Manager policy, or "" when
// unknown (including any non-kubeadm cluster or a nil reader). The error is for logging only.
func CPUManagerPolicy(ctx context.Context, reader client.Reader) (string, error) {
	if reader == nil {
		return "", nil
	}
	cm := &corev1.ConfigMap{}
	if err := reader.Get(ctx, kubeletConfigMap, cm); err != nil {
		return "", fmt.Errorf("failed to read ConfigMap %s: %w", kubeletConfigMap, err)
	}
	data, ok := cm.Data[kubeletConfigDataKey]
	if !ok {
		return "", fmt.Errorf("ConfigMap %s carries no %q key", kubeletConfigMap, kubeletConfigDataKey)
	}
	policy := ParseCPUManagerPolicy([]byte(data))
	if policy == "" {
		return "", fmt.Errorf("ConfigMap %s carries no readable cpuManagerPolicy: its %q document does not parse, "+
			"its policy is not a string, or it omits the key without declaring kind: %s",
			kubeletConfigMap, kubeletConfigDataKey, kubeletConfigurationKind)
	}
	return policy, nil
}
