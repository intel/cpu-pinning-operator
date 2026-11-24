// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package topology

// ProbePodForE2E lets the e2e suite admission-test the real probe pod shape; the
// build tag keeps it out of the manager binary.
var ProbePodForE2E = buildProbePod
