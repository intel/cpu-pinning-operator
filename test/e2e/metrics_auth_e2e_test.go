// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

// TestMetricsRejectsUnauthenticatedScrape proves /metrics refuses a client with no credential.
// The apiserver pod proxy strips Authorization and presents no client cert, so it is that client.
func TestMetricsRejectsUnauthenticatedScrape(t *testing.T) {
	at := allureCase(t)
	if os.Getenv("E2E_METRICS") == "" {
		skipNotSelected(at, "set E2E_METRICS=1 to exercise the operator metrics endpoint via the apiserver proxy")
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		skipNoCapability(at, "no kubeconfig available: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		at.Fatalf("build clientset: %v", err)
	}
	opNS := env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system")
	name := env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dep, err := cs.AppsV1().Deployments(opNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		skipNoConfig(at, "could not read manager Deployment %s/%s (operator may not be installed): %v", opNS, name, err)
	}

	// Either opt-out leaves the endpoint open by design, so there is no refusal to assert, even under E2E_STRICT.
	for _, optOut := range []string{"--metrics-require-auth=false", "--metrics-secure=false"} {
		if raHasFlag(dep, optOut) {
			skipNotSelected(at, "manager Deployment %s/%s runs with %s, so the metrics endpoint does not check its clients and has no rejection to prove",
				opNS, name, optOut)
		}
	}

	body, target, err := scrapeLeaderMetrics(at)
	if err == nil {
		// Report the size, not the body.
		at.Fatalf("unauthenticated scrape of %s returned %d bytes of Prometheus exposition; the endpoint must reject a client that presents no credential. "+
			"Check that --metrics-require-auth reaches the manager and that metricsServerOptions still installs the authentication filter", target, len(body))
	}
	if !metricsAuthRejected(err) {
		// A failure that is not a refusal proves nothing about the refusal.
		skipNoConfig(at, "unauthenticated scrape of %s failed without a 401: %v — cannot tell a rejected client from an unreachable endpoint", target, err)
	}
	at.Logf("metrics endpoint %s rejected the unauthenticated scrape: %v", target, err)
}
