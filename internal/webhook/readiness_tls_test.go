// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/intel/cpu-pinning-operator/internal/readiness"
)

// tlsReadinessServer is an https readiness endpoint plus a cert dir whose ca.crt trusts it.
func tlsReadinessServer(t *testing.T, status int) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ready":` + map[bool]string{true: "true", false: "false"}[status == http.StatusOK] + `}`))
	}))
	t.Cleanup(srv.Close)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, caCertName), caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, dir
}

// TestPreflightOverTLSTrustsTheMountedCA: https is verified against CertDir/ca.crt.
func TestPreflightOverTLSTrustsTheMountedCA(t *testing.T) {
	srv, dir := tlsReadinessServer(t, http.StatusServiceUnavailable)
	t.Setenv("BALLOONS_READY_ENDPOINT", srv.URL)

	m, _ := newHandleMutator(t, "enforce", labeledNode("n1", true, "2"))
	m.CertDir = dir
	if resp := admitScheduledPod(t, m); resp.Allowed {
		t.Fatal("enforce + verified not-ready verdict over TLS should deny, got allowed")
	}
}

// TestPreflightOverTLSUntrustedIsInconclusive: an untrusted server gives no verdict.
func TestPreflightOverTLSUntrustedIsInconclusive(t *testing.T) {
	srv, _ := tlsReadinessServer(t, http.StatusServiceUnavailable)
	// All httptest TLS servers share one cert, so mint a foreign CA.
	otherDir := t.TempDir()
	otherCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: mintReserveCA(t).der})
	if err := os.WriteFile(filepath.Join(otherDir, caCertName), otherCA, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BALLOONS_READY_ENDPOINT", srv.URL)

	m, _ := newHandleMutator(t, "enforce", labeledNode("n1", true, "2"))
	m.CertDir = otherDir
	if resp := admitScheduledPod(t, m); !resp.Allowed {
		t.Fatalf("an unverifiable readiness answer must be inconclusive, got denied: %q", resp.Result.Message)
	}
}

// TestHandleMountsReadinessCA: https endpoints mount the CA ConfigMap and pass --ca-file.
func TestHandleMountsReadinessCA(t *testing.T) {
	srv, dir := tlsReadinessServer(t, http.StatusOK)
	t.Setenv("BALLOONS_READY_ENDPOINT", srv.URL)
	t.Setenv("INIT_CONTAINER_IMAGE", testInitImage)
	t.Setenv(readiness.CAConfigMapEnv, "op-readiness-ca")

	m, _ := newHandleMutator(t, "warn", labeledNode("n1", true, "2"))
	m.CertDir = dir
	resp := admitPod(t, m, "vllm-ok", "2", "1")
	if !patchesTouchInitContainers(resp) {
		t.Fatalf("expected the init container to be injected, got patches %+v", resp.Patches)
	}
	patched, err := json.Marshal(resp.Patches)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--endpoint=" + srv.URL,
		"--ca-file=" + readinessCAMountPath + "/" + readiness.CAKey,
		`"configMap":{"name":"op-readiness-ca","optional":true}`,
		`"mountPath":"` + readinessCAMountPath + `"`,
	} {
		if !strings.Contains(string(patched), want) {
			t.Errorf("patch lacks %s: %s", want, patched)
		}
	}
}

// TestCreateInitContainerCAMount: plaintext gets no CA mount; https a read-only directory mount.
func TestCreateInitContainerCAMount(t *testing.T) {
	t.Setenv(readiness.CAConfigMapEnv, "op-readiness-ca")
	ic := createInitContainer("https://mgr.op.svc:8082", testInitImage)
	if len(ic.VolumeMounts) != 1 || !ic.VolumeMounts[0].ReadOnly || ic.VolumeMounts[0].SubPath != "" {
		t.Errorf("https mounts = %+v, want one read-only mount without subPath", ic.VolumeMounts)
	}
	if !strings.Contains(strings.Join(ic.Args, " "), "--ca-file=") {
		t.Errorf("https args %v lack --ca-file", ic.Args)
	}

	plain := createInitContainer("http://mgr.op.svc:8082", testInitImage)
	if len(plain.VolumeMounts) != 0 || strings.Contains(strings.Join(plain.Args, " "), "--ca-file") {
		t.Errorf("plaintext init container carries CA wiring: %+v %v", plain.VolumeMounts, plain.Args)
	}
	pod := &corev1.Pod{}
	injectReadinessCAVolume(pod, "http://mgr.op.svc:8082")
	if len(pod.Spec.Volumes) != 0 {
		t.Errorf("plaintext endpoint added volumes %+v", pod.Spec.Volumes)
	}
}
