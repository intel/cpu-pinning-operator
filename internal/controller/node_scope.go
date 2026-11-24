// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ProvisionedNodeLabels is the label set of a node the operator has provisioned.
// Each call returns a fresh map.
func ProvisionedNodeLabels() map[string]string {
	return map[string]string{LabelBalloonsEnabled: "true"}
}

// provisionedNodesOnly narrows a Node list to provisioned nodes. The cache still
// walks every Node, but skips the deep copy and per-node work for the rest. Lists
// that must see unlabelled nodes (enqueueAllNodes) go without it.
func provisionedNodesOnly() client.ListOption {
	return client.MatchingLabels(ProvisionedNodeLabels())
}
