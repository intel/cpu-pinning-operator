// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPackagedChartShipsLicenseAndNotice proves the packaged chart carries LICENSE and
// NOTICE at its root.
func TestPackagedChartShipsLicenseAndNotice(t *testing.T) {
	helm := helmBin(t)
	out := t.TempDir()
	cmd := exec.Command(helm, "package", filepath.Join("..", "..", "deploy", "cpu-pinning-operator"), "-d", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helm package: %v\n%s", err, b)
	}
	archives, err := filepath.Glob(filepath.Join(out, "cpu-pinning-operator-*.tgz"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("want one packaged chart in %s, got %v (%v)", out, archives, err)
	}
	f, err := os.Open(archives[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found[h.Name] = h.Size > 0
	}
	for _, name := range []string{"cpu-pinning-operator/LICENSE", "cpu-pinning-operator/NOTICE"} {
		if !found[name] {
			t.Errorf("packaged chart %s lacks a non-empty %s", filepath.Base(archives[0]), name)
		}
	}
}
