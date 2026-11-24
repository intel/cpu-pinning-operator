// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterConfigName is the required name of the BalloonsClusterConfig
// singleton; CEL validation rejects any other name.
const ClusterConfigName = "cluster"

// BalloonsClusterConfigSpec holds the node-global pinning settings: how balloon
// types are generated on every node and how the mutating webhook fails.
type BalloonsClusterConfigSpec struct {
	// Balloon holds the cluster-default balloon-generation settings (hyperthread
	// handling and reserved-pool sizing), applied to every node that matches no
	// entry in NodeConfigOverrides.
	Balloon BalloonSpec `json:"balloon,omitempty"`

	// NodeConfigOverrides gives label-selected node sets a different balloon
	// block than the cluster default, for mixed-CPU fleets. The list is ordered:
	// the first override whose nodeSelector matches a node wins, and its set
	// fields (cpuAllocation, reserved) are layered per-field onto spec.balloon.
	// webhook.failurePolicy and excludeNodeSelector are cluster-global and
	// cannot be overridden per node.
	//
	// Capped at 32 entries to bound the CEL validation cost of the reused
	// BalloonSpec across the array.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	NodeConfigOverrides []NodeConfigOverride `json:"nodeConfigOverrides,omitempty"`

	// Classification tunes automatic node classification. The Node Reconciler
	// classifies every provisioned node as mixed-cpu-amx-gpu, cpu-amx, gpu-only
	// or cpu-only from its GPU presence, AMX support and logical-CPU count,
	// publishes the result as the node-class label, and uses the matching
	// ClassProfiles entry, if any, as the node's balloon base. Classification
	// always runs; an empty block uses the built-in defaults.
	Classification ClassificationSpec `json:"classification,omitempty"`

	// ClassProfiles maps a node class (mixed-cpu-amx-gpu, cpu-amx, gpu-only,
	// cpu-only) to the balloon block used as the base for nodes of that class.
	// The effective per-node config is layered per-field in this order: cluster
	// default (spec.balloon), matching class profile, matching
	// NodeConfigOverride. A class with no entry keeps the cluster default. Only
	// cpuAllocation and reserved are honored per node; packing, htSiblingPolicy
	// and reservedNamespaces are read from spec.balloon only.
	//
	// Capped at 16 entries to bound the CEL validation cost of the reused
	// BalloonSpec across the map.
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	ClassProfiles map[string]BalloonSpec `json:"classProfiles,omitempty"`

	// Webhook holds the MutatingWebhookConfiguration settings (failurePolicy).
	Webhook WebhookSpec `json:"webhook,omitempty"`

	// ExcludeNodeSelector marks nodes that are not pinning targets. The Node
	// Reconciler does not provision a BalloonsPolicy for them or label them
	// balloons-enabled, and prunes any existing CR and labels. A nil or empty
	// selector excludes nothing.
	ExcludeNodeSelector *metav1.LabelSelector `json:"excludeNodeSelector,omitempty"`
}

// NodeConfigOverride binds a label-selected node set to a balloon block that
// overrides the cluster default per-field (an unset field falls back to
// spec.balloon). Overrides are matched first-match-wins in list order, so an
// override must set at least one of cpuAllocation or reserved: an empty one
// would shadow later overrides while changing nothing.
// +kubebuilder:validation:XValidation:rule="has(self.nodeSelector) && ((has(self.nodeSelector.matchLabels) && size(self.nodeSelector.matchLabels) > 0) || (has(self.nodeSelector.matchExpressions) && size(self.nodeSelector.matchExpressions) > 0))",message="nodeConfigOverrides[].nodeSelector must be non-empty (set a non-empty matchLabels or matchExpressions); an empty selector matches every node and would shadow the cluster default and all later overrides"
// +kubebuilder:validation:XValidation:rule="has(self.balloon) && (has(self.balloon.cpuAllocation) || has(self.balloon.reserved))",message="nodeConfigOverrides[].balloon must override at least one of cpuAllocation or reserved; an override that changes nothing is not allowed"
type NodeConfigOverride struct {
	// NodeSelector selects the nodes this override applies to. Required and
	// non-empty: an empty selector would match every node and is rejected.
	NodeSelector *metav1.LabelSelector `json:"nodeSelector"`

	// Balloon holds the settings applied to matching nodes, layered per-field
	// over spec.balloon: set cpuAllocation and/or reserved; unset fields are
	// inherited. reserved supports the same cpus, percent and measured modes as
	// the cluster default.
	Balloon BalloonSpec `json:"balloon,omitempty"`
}

// ClassificationSpec tunes automatic node classification. Every field is
// optional; an empty block uses the "cpu-pinning-operator.intel.com/node-class-override"
// override label, a 64-logical-CPU cpu-amx threshold, and no AMX/GPU selectors.
//
// Rules apply in priority order: a valid override label wins; a GPU node is
// gpu-only if it matches GPUOnlyNodeSelector, else mixed-cpu-amx-gpu if it has
// AMX, else gpu-only; a non-GPU AMX node is cpu-amx if it matches
// AMXNodeSelector or has at least CPUAMXMinLogicalCPUs logical CPUs; everything
// else is cpu-only.
type ClassificationSpec struct {
	// OverrideLabel is the node label that manually forces a node's class; its
	// value must be a known class name. An AMX class requested on a node without
	// AMX is rejected with a Warning event and the node keeps its automatic
	// class. Empty means "cpu-pinning-operator.intel.com/node-class-override".
	// +optional
	OverrideLabel string `json:"overrideLabel,omitempty"`

	// AMXNodeSelector marks AMX nodes intended for CPU inference: a matching
	// non-GPU AMX node is cpu-amx regardless of CPU count. nil means AMX nodes
	// reach cpu-amx only via the CPUAMXMinLogicalCPUs threshold.
	// +optional
	AMXNodeSelector *metav1.LabelSelector `json:"amxNodeSelector,omitempty"`

	// GPUOnlyNodeSelector forces matching GPU nodes into gpu-only even when they
	// support AMX. nil leaves GPU+AMX nodes as mixed-cpu-amx-gpu.
	// +optional
	GPUOnlyNodeSelector *metav1.LabelSelector `json:"gpuOnlyNodeSelector,omitempty"`

	// CPUAMXMinLogicalCPUs is the logical-CPU count at or above which a non-GPU
	// AMX node is cpu-amx without an AMXNodeSelector match. Zero or unset means
	// DefaultCPUAMXMinLogicalCPUs (64).
	// +optional
	// +kubebuilder:validation:Minimum=0
	CPUAMXMinLogicalCPUs int `json:"cpuAMXMinLogicalCPUs,omitempty"`
}

// BalloonsClusterConfigStatus reports the cluster-wide pinning pool: the
// balloons-enabled nodes the operator has provisioned.
type BalloonsClusterConfigStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ProvisionedNodes counts balloons-enabled nodes in the pinning pool.
	ProvisionedNodes int `json:"provisionedNodes,omitempty"`
	// PerNode reports provisioning per balloons-enabled node.
	// +listType=map
	// +listMapKey=name
	PerNode []NodeStatus `json:"perNode,omitempty"`
	// Conditions follows the standard Kubernetes conditions convention
	// (e.g. Ready) for the pinning pool.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BalloonsClusterConfig is the cluster-scoped singleton holding the node-global
// pinning configuration. It must be named "cluster".
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=bcc
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="BalloonsClusterConfig is a singleton and must be named 'cluster'"
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.provisionedNodes`
// +kubebuilder:printcolumn:name="CPUAllocation",type=string,JSONPath=`.spec.balloon.cpuAllocation`
type BalloonsClusterConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BalloonsClusterConfigSpec   `json:"spec,omitempty"`
	Status BalloonsClusterConfigStatus `json:"status,omitempty"`
}

// BalloonsClusterConfigList contains a list of BalloonsClusterConfig.
// +kubebuilder:object:root=true
type BalloonsClusterConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BalloonsClusterConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BalloonsClusterConfig{}, &BalloonsClusterConfigList{})
}
