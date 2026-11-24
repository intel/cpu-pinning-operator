// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains API Schema definitions for the cpu-pinning-operator.intel.com
// v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=cpu-pinning-operator.intel.com

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupName is the API group and the domain prefix shared by every
// operator-managed label and annotation key.
const GroupName = "cpu-pinning-operator.intel.com"

// LabelPrefix is the domain prefix every operator-managed label and annotation
// key is composed from (e.g. LabelPrefix+"pinned-by").
const LabelPrefix = GroupName + "/"

var (
	// GroupVersion is group version used to register these objects
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

	// SchemeGroupVersion is an alias for GroupVersion for parity with other API packages.
	SchemeGroupVersion = GroupVersion

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
