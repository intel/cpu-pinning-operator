// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/intel/cpu-pinning-operator/internal/readiness"
	"github.com/intel/cpu-pinning-operator/internal/topology"
	"github.com/intel/cpu-pinning-operator/internal/webhook"
)

const (
	podSecurityGate = "E2E_POD_SECURITY"
	podSecurityNS   = "e2e-pod-security"
)

// TestOperatorPodsAdmittedUnderRestrictedPSA proves the API server's restricted
// Pod Security admission accepts every operator pod shape, and checks the hardening
// fields directly so a relaxed admission level cannot hide a regression.
//
// The bundled nri-resource-policy-balloons DaemonSet is deliberately not here: it is
// not an operator pod, needs hostPath, and fails restricted by design. The hardened
// install runs it in its own namespace (docs/secure-configuration.md).
func TestOperatorPodsAdmittedUnderRestrictedPSA(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(podSecurityGate) == "" {
		skipNotSelected(at, "set %s=1 to dry-run the operator pod shapes under restricted Pod Security admission", podSecurityGate)
	}
	c := newClient(at)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	opNS := env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system")
	name := env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment)
	var dep appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: opNS, Name: name}, &dep); err != nil {
		skipNoConfig(at, "could not read manager Deployment %s/%s (operator may not be installed): %v", opNS, name, err)
	}

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: podSecurityNS, Labels: map[string]string{
		"pod-security.kubernetes.io/enforce":         "restricted",
		"pod-security.kubernetes.io/enforce-version": "latest",
	}}}
	_ = c.Delete(ctx, nsObj) // best-effort clean of a leftover run
	if err := createEventuallyGone(c, nsObj); err != nil {
		at.Fatalf("create namespace %s: %v", podSecurityNS, err)
	}
	at.Cleanup(func() {
		e2eDelete(at, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: podSecurityNS}})
	})
	// Pod admission looks the ServiceAccount up, and the namespace's default one appears asynchronously.
	if err := waitFor(ctx, func() bool {
		return c.Get(ctx, client.ObjectKey{Namespace: podSecurityNS, Name: "default"}, &corev1.ServiceAccount{}) == nil
	}); err != nil {
		at.Fatalf("default ServiceAccount never appeared in %s: %v", podSecurityNS, err)
	}

	pods := operatorPodShapes(at, &dep)
	// Positive control: an admission that admits a privileged pod is not enforcing restricted.
	priv := pods[0].DeepCopy()
	priv.Name = "privileged-control"
	priv.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
	if err := e2eCreate(ctx, c, priv, client.DryRunAll); !apierrors.IsForbidden(err) {
		at.Fatalf("namespace %s admitted a privileged pod (err=%v): restricted Pod Security admission is not enforcing, so admitting the operator pods would prove nothing",
			podSecurityNS, err)
	}

	for _, pod := range pods {
		for _, gap := range restrictedGaps(&pod.Spec) {
			at.Errorf("pod %s: %s", pod.Name, gap)
		}
		if err := e2eCreate(ctx, c, pod, client.DryRunAll); err != nil {
			at.Errorf("restricted Pod Security admission refused the %s pod shape: %v", pod.Name, err)
			continue
		}
		at.Logf("restricted Pod Security admission admitted the %s pod shape", pod.Name)
	}
}

// operatorPodShapes is every pod the operator runs, re-homed to podSecurityNS.
func operatorPodShapes(t testing.TB, dep *appsv1.Deployment) []*corev1.Pod {
	t.Helper()
	image := ""
	for _, ctr := range dep.Spec.Template.Spec.Containers {
		if ctr.Name == "manager" {
			image = ctr.Image
		}
	}
	if image == "" {
		t.Fatalf("manager Deployment %s/%s has no container named manager", dep.Namespace, dep.Name)
	}

	manager := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "manager", Namespace: podSecurityNS},
		Spec: *dep.Spec.Template.Spec.DeepCopy()}
	// The release's ServiceAccount lives in the operator namespace; the SA does not affect the profile.
	manager.Spec.ServiceAccountName = ""
	manager.Spec.DeprecatedServiceAccount = ""

	probe := topology.ProbePodForE2E(podSecurityNS, "topology-probe", "", image)

	// The CA mount is part of the injected shape only when this is set.
	t.Setenv(readiness.CAConfigMapEnv, "readiness-ca")
	endpoint := "https://balloons-ready.example.svc:8082"
	initPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "wait-for-balloons", Namespace: podSecurityNS},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{webhook.InitContainerForE2E(endpoint, image)},
			// A restricted stand-in workload, so only the injected container is under test.
			Containers: []corev1.Container{{
				Name: "workload", Image: "registry.k8s.io/pause:3.9",
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot: ptr.To(true), AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
					Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			}},
		},
	}
	webhook.InjectReadinessCAVolumeForE2E(initPod, endpoint)
	return []*corev1.Pod{manager, probe, initPod}
}

// restrictedGaps lists every container missing a restricted-PSA hardening field; pod-level
// runAsNonRoot and seccomp count, as they do for admission.
func restrictedGaps(spec *corev1.PodSpec) []string {
	var out []string
	psc := spec.SecurityContext
	if psc == nil {
		psc = &corev1.PodSecurityContext{}
	}
	for _, ctr := range slices.Concat(spec.InitContainers, spec.Containers) {
		sc := ctr.SecurityContext
		if sc == nil {
			sc = &corev1.SecurityContext{}
		}
		gap := func(f string, a ...any) {
			out = append(out, fmt.Sprintf("container %s: ", ctr.Name)+fmt.Sprintf(f, a...))
		}
		if !ptr.Deref(sc.RunAsNonRoot, ptr.Deref(psc.RunAsNonRoot, false)) {
			gap("runAsNonRoot is not true")
		}
		if ptr.Deref(sc.RunAsUser, ptr.Deref(psc.RunAsUser, -1)) == 0 {
			gap("runAsUser is 0")
		}
		if ptr.Deref(sc.Privileged, false) {
			gap("privileged")
		}
		if ptr.Deref(sc.AllowPrivilegeEscalation, true) {
			gap("allowPrivilegeEscalation is not false")
		}
		if sc.Capabilities == nil || !slices.Contains(sc.Capabilities.Drop, "ALL") {
			gap("capabilities do not drop ALL")
		} else if len(sc.Capabilities.Add) > 0 {
			gap("capabilities add %v", sc.Capabilities.Add)
		}
		seccomp := sc.SeccompProfile
		if seccomp == nil {
			seccomp = psc.SeccompProfile
		}
		if seccomp == nil || seccomp.Type != corev1.SeccompProfileTypeRuntimeDefault {
			gap("seccomp profile is not RuntimeDefault")
		}
		if !ptr.Deref(sc.ReadOnlyRootFilesystem, false) {
			gap("readOnlyRootFilesystem is not true")
		}
	}
	return out
}

// waitFor polls cond every second until it holds or ctx ends.
func waitFor(ctx context.Context, cond func() bool) error {
	for !cond() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil
}

// TestRestrictedGapsJudgesShapes runs the field check on the real probe and init
// container builders and on a stripped container. No cluster.
func TestRestrictedGapsJudgesShapes(t *testing.T) {
	t.Setenv(readiness.CAConfigMapEnv, "readiness-ca")
	probe := topology.ProbePodForE2E("ns", "probe", "", "img")
	init := corev1.PodSpec{InitContainers: []corev1.Container{webhook.InitContainerForE2E("https://x.svc:8082", "img")}}
	for name, spec := range map[string]*corev1.PodSpec{"topology probe": &probe.Spec, "wait-for-balloons": &init} {
		if gaps := restrictedGaps(spec); len(gaps) > 0 {
			t.Errorf("%s builder: %q", name, gaps)
		}
	}

	bare := &corev1.PodSpec{Containers: []corev1.Container{{Name: "bare", SecurityContext: &corev1.SecurityContext{
		RunAsUser: ptr.To(int64(0)), Privileged: ptr.To(true),
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"NET_ADMIN"}},
	}}}}
	if got := restrictedGaps(bare); len(got) != 7 {
		t.Errorf("stripped container: %d gap(s) %q, want 7 (runAsNonRoot, runAsUser, privileged, escalation, capabilities, seccomp, read-only root)", len(got), got)
	}
}
