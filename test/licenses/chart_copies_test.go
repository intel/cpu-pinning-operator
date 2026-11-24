// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package licenses

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestChartCopiesMatchRepoRoot proves the chart's LICENSE and NOTICE copies (helm packages
// only files inside the chart) match the repository root's.
func TestChartCopiesMatchRepoRoot(t *testing.T) {
	root := repoRoot(t)
	chart := filepath.Join(root, "deploy", "cpu-pinning-operator")
	for _, name := range []string{"LICENSE", "NOTICE"} {
		want, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(chart, name))
		if err != nil {
			t.Fatalf("the chart must ship %s at its root: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("deploy/cpu-pinning-operator/%s differs from the repository's %s; copy it again: cp %s deploy/cpu-pinning-operator/", name, name, name)
		}
	}
}
