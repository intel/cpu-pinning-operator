// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestLogValuesWiredToManager proves log.level and log.development exist in values.yaml
// and reach the manager as --zap-log-level / --zap-devel. Parses files directly so it
// needs no helm.
func TestLogValuesWiredToManager(t *testing.T) {
	chartDir := filepath.Join("..", "..", "deploy", "cpu-pinning-operator")

	valuesData, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(valuesData, &root); err != nil {
		t.Fatalf("parse values.yaml: %v", err)
	}
	logBlock := findChild(&root, "log")
	if logBlock == nil {
		t.Fatal("values.yaml missing top-level `log` block")
	}
	for _, key := range []string{"level", "development"} {
		if findChild(logBlock, key) == nil {
			t.Errorf("values.yaml `log` block missing `%s`", key)
		}
	}

	deployData, err := os.ReadFile(filepath.Join(chartDir, "templates", "deployment.yaml"))
	if err != nil {
		t.Fatalf("read deployment.yaml: %v", err)
	}
	deploy := string(deployData)

	wiring := map[string]string{
		"--zap-log-level": ".Values.log.level",
		"--zap-devel":     ".Values.log.development",
	}
	for flag, value := range wiring {
		if !strings.Contains(deploy, flag) {
			t.Errorf("deployment.yaml does not pass the %s flag to the manager", flag)
			continue
		}
		if !strings.Contains(deploy, value) {
			t.Errorf("deployment.yaml passes %s but not from %s (the chart value is not wired through)", flag, value)
		}
	}
}
