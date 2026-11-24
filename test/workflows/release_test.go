// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package workflows checks every release job runs only in the public repository (keyless
// signing writes its name to the public Rekor log) and behind the release environment.
package workflows_test

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReleaseJobs(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			If          string `yaml:"if"`
			Uses        string `yaml:"uses"`
			Environment string `yaml:"environment"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	if w.Permissions == nil || len(w.Permissions) != 0 {
		t.Errorf("workflow-level permissions must be {}, got %v", w.Permissions)
	}
	if len(w.Jobs) == 0 {
		t.Fatal("release.yml has no jobs")
	}
	const publicRepo = "github.repository == 'intel/cpu-pinning-operator'"
	for name, j := range w.Jobs {
		if j.If != publicRepo {
			t.Errorf("job %s: if must be %q, got %q", name, publicRepo, j.If)
		}
		// A job calling a reusable workflow cannot take an environment.
		if j.Uses == "" && j.Environment != "release" {
			t.Errorf("job %s: must run in the release environment, got %q", name, j.Environment)
		}
	}
}
