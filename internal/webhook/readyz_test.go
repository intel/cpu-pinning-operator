// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCertPresenceChecker(t *testing.T) {
	writeFile := func(t *testing.T, dir, name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	t.Run("missing cert dir is not ready", func(t *testing.T) {
		// A missing path stands in for the cert directory before injection.
		check := CertPresenceChecker(filepath.Join(t.TempDir(), "does-not-exist"))
		if err := check(nil); err == nil {
			t.Fatal("expected not-ready error for missing cert dir, got nil")
		}
	})

	t.Run("empty cert dir is not ready", func(t *testing.T) {
		check := CertPresenceChecker(t.TempDir())
		if err := check(nil); err == nil {
			t.Fatal("expected not-ready error for empty cert dir, got nil")
		}
	})

	t.Run("only cert present blames the missing key", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, defaultCertName)
		check := CertPresenceChecker(dir)
		err := check(nil)
		if err == nil {
			t.Fatal("expected not-ready error when key is missing, got nil")
		}
		// The error must name the key path, not mislead with "serving cert".
		if !strings.Contains(err.Error(), defaultKeyName) {
			t.Errorf("error %q does not mention the missing key file %q", err, defaultKeyName)
		}
		if strings.Contains(err.Error(), "serving cert") {
			t.Errorf("error %q misattributes a missing key to the serving cert", err)
		}
	})

	t.Run("only key present blames the missing cert", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, defaultKeyName)
		check := CertPresenceChecker(dir)
		err := check(nil)
		if err == nil {
			t.Fatal("expected not-ready error when cert is missing, got nil")
		}
		if !strings.Contains(err.Error(), defaultCertName) {
			t.Errorf("error %q does not mention the missing cert file %q", err, defaultCertName)
		}
	})

	t.Run("both cert and key present is ready", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, defaultCertName)
		writeFile(t, dir, defaultKeyName)
		check := CertPresenceChecker(dir)
		if err := check(nil); err != nil {
			t.Fatalf("expected ready (nil) when cert and key present, got %v", err)
		}
	})
}
