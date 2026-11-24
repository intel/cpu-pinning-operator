// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package ctrlplane starts the envtest control plane shared by the
// real-apiserver guards in test/crdinstall and internal/controller.
package ctrlplane

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Start boots env and returns a client plus a stop func. Without the
// control-plane binaries this skips locally and fails under CI.
func Start(t *testing.T, env *envtest.Environment) (client.Client, func()) {
	t.Helper()
	assets, ok := binaries()
	if !ok {
		const msg = "envtest control-plane binaries not found; run `make test-crd`"
		if os.Getenv("CI") != "" {
			t.Fatalf("%s — CI must provide them, see .github/workflows/val-unit-tests.yml", msg)
		}
		t.Skip(msg)
	}

	t.Logf("envtest assets: %s", assets)
	// KUBEBUILDER_ASSETS outranks Environment.BinaryAssetsDirectory.
	t.Setenv("KUBEBUILDER_ASSETS", assets)
	// Loopback lets a host with no default route boot the apiserver; it requires the endpoint reconciler off.
	env.ControlPlane.GetAPIServer().Configure().
		Set("advertise-address", "127.0.0.1").
		Set("endpoint-reconciler-type", "none")

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest Start: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: env.Scheme})
	if err != nil {
		_ = env.Stop()
		t.Fatalf("build client: %v", err)
	}
	return c, func() { _ = env.Stop() }
}

// binaries locates a complete set of control-plane binaries. An explicit
// KUBEBUILDER_ASSETS decides alone, so no unrequested apiserver version runs.
func binaries() (string, bool) {
	if p := os.Getenv("KUBEBUILDER_ASSETS"); p != "" {
		return p, complete(p)
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// A CWD-relative probe would make the outcome depend on the working directory.
			return "", false
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	store := filepath.Join(dataHome, "kubebuilder-envtest", "k8s")
	entries, err := os.ReadDir(store)
	if err != nil {
		return "", false
	}
	// Store dirs are "<version>-<os>-<arch>"; the newest parsed version for this platform
	// wins (by name, 1.9.0 would outrank 1.37.0).
	suffix := "-" + runtime.GOOS + "-" + runtime.GOARCH
	var best string
	var bestVer *version.Version
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), suffix)
		if !ok {
			continue
		}
		ver, err := version.ParseGeneric(name)
		if err != nil {
			continue
		}
		dir := filepath.Join(store, e.Name())
		if complete(dir) && (bestVer == nil || ver.GreaterThan(bestVer)) {
			best, bestVer = dir, ver
		}
	}
	return best, best != ""
}

// complete reports whether dir holds both binaries envtest needs.
func complete(dir string) bool {
	for _, bin := range []string{"kube-apiserver", "etcd"} {
		if _, err := os.Stat(filepath.Join(dir, bin)); err != nil {
			return false
		}
	}
	return true
}
