// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"

// labelPrefix aliases cpopv1alpha1.LabelPrefix for operator-managed labels/annotations.
const labelPrefix = cpopv1alpha1.LabelPrefix

// Drift annotation keys; the hash comes from match.PinnedConfigHash.
const (
	// PinnedHashAnnotation records a hash of the pinning-relevant config.
	PinnedHashAnnotation = labelPrefix + "pinned-hash"
	// PinnedByAnnotation marks a pod the webhook pinned.
	PinnedByAnnotation = labelPrefix + "pinned-by"
	// PinnedByValue is the PinnedByAnnotation value the webhook stamps. internal/controller
	// re-declares this literal; changing it breaks drift detection and status.boundPods.
	PinnedByValue = "cpu-pinning-operator"
)
