// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// File names controller-runtime's webhook server loads (Options.CertName / KeyName).
const (
	defaultCertName = "tls.crt"
	defaultKeyName  = "tls.key"
)

// CertPresenceChecker returns a readiness check that passes once the serving cert and
// key exist. Without it the pod can go ready before cert-manager writes the cert, and
// with failurePolicy: Ignore pods are admitted unpinned. Presence only, not validity.
func CertPresenceChecker(certDir string) healthz.Checker {
	return func(_ *http.Request) error {
		for _, f := range []struct{ kind, name string }{
			{"serving cert", defaultCertName},
			{"private key", defaultKeyName},
		} {
			path := filepath.Join(certDir, f.name)
			if _, err := os.Stat(path); err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("webhook %s %q not present yet", f.kind, path)
				}
				return fmt.Errorf("stat webhook %s %q: %w", f.kind, path, err)
			}
		}
		return nil
	}
}
