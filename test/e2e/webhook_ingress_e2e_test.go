// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Needs a CNI that enforces NetworkPolicy and an install with
// --set-json 'networkPolicy.webhook.from=[{"ipBlock":{"cidr":"<control-plane-cidr>"}}]';
// skips otherwise. E2E_WEBHOOK_PROBE_IMAGE overrides the curl image.

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// wiAdmissionReview is the forged request body; any HTTP answer means the port was reachable.
const wiAdmissionReview = `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview",` +
	`"request":{"uid":"e2e-forged","kind":{"group":"","version":"v1","kind":"Pod"},` +
	`"resource":{"group":"","version":"v1","resource":"pods"},"operation":"CREATE",` +
	`"userInfo":{"username":"e2e"},"object":{"apiVersion":"v1","kind":"Pod",` +
	`"metadata":{"name":"forged","namespace":"default"},"spec":{"containers":[` +
	`{"name":"c","image":"registry.k8s.io/pause:3.9"}]}}}}`

// wiProbeScript exits 0 only when the open control port answered and the webhook port
// failed with curl 7 (refused) or 28 (timeout); anything else counts as NOT blocked.
const wiProbeScript = `
set -u
if ! curl -sS -m 10 -o /dev/null "http://$READY_HOST/balloons/ready?node=e2e-probe"; then
  echo "CONTROL-FAILED: the balloons-ready port, open to any source, did not answer"
  exit 2
fi
echo "CONTROL-OK: this pod can reach the manager on an open port"
code=0
curl -sS -m 10 -k -o /dev/null -X POST -H "Content-Type: application/json" \
  --data "$REVIEW" "https://$WEBHOOK_HOST/mutate-v1-pod" || code=$?
case "$code" in
  7|28) echo "BLOCKED: AdmissionReview POST refused (curl exit $code)"; exit 0 ;;
  0)    echo "NOT-BLOCKED: the webhook ANSWERED an AdmissionReview posted from this pod"; exit 3 ;;
  *)    echo "NOT-BLOCKED: reached the webhook port (curl exit $code)"; exit 4 ;;
esac
`

// TestWebhookPortRejectsNonAllowedPod proves the CNI drops an AdmissionReview a plain
// pod posts at the webhook Service, with the open readiness port as the control.
func TestWebhookPortRejectsNonAllowedPod(t *testing.T) {
	at := allureCase(t)
	c := newClient(at)
	opNS := operatorNamespace()

	webhookSvc, readySvc := wiServiceHosts(at, c, opNS)
	if !wiIngressRestricted(at, c, opNS, "webhook-server") {
		skipNotSelected(at, "no NetworkPolicy in %s restricts the webhook ingress rule — reinstall with "+
			"networkPolicy.webhook.from set to your control-plane ipBlocks to run this test", opNS)
	}

	code, log := wiRunProbe(at, c, "cpu-pinning-e2e-webhook-ingress", "webhook-ingress-probe", wiProbeScript,
		corev1.EnvVar{Name: "WEBHOOK_HOST", Value: webhookSvc},
		corev1.EnvVar{Name: "READY_HOST", Value: readySvc},
		corev1.EnvVar{Name: "REVIEW", Value: wiAdmissionReview})
	at.Logf("probe log:\n%s", log)

	// Only exit codes 3 and 4 say the policy failed to hold.
	switch code {
	case wiExitBlocked:
		// The assertion held.
	case wiExitReachedAnswered, wiExitReachedOther:
		at.Errorf("a pod outside networkPolicy.webhook.from reached the webhook port (probe exit %d), so a "+
			"crafted AdmissionReview bypasses the API server — check that the CNI enforces NetworkPolicy", code)
	case wiExitControlFailed:
		skipNoConfig(at, "the probe pod could not reach the balloons-ready port either, which is open to any "+
			"source: nothing was proven about the webhook port")
	default:
		skipNoConfig(at, "probe pod exited %d without reaching either port", code)
	}
}

// Probe exit codes, mirrored in wiProbeScript.
const (
	wiExitBlocked         = 0
	wiExitControlFailed   = 2
	wiExitReachedAnswered = 3
	wiExitReachedOther    = 4
)

// wiServiceHosts returns host:port for the webhook and readiness Services, found by
// port name so any release name works.
func wiServiceHosts(t testing.TB, c client.Client, ns string) (webhook, ready string) {
	t.Helper()
	var services corev1.ServiceList
	if err := c.List(context.Background(), &services, client.InNamespace(ns)); err != nil {
		t.Fatalf("list services in %s: %v", ns, err)
	}
	for i := range services.Items {
		svc := &services.Items[i]
		for _, p := range svc.Spec.Ports {
			host := fmt.Sprintf("%s.%s.svc:%d", svc.Name, ns, p.Port)
			switch p.TargetPort.String() {
			case "webhook-server":
				webhook = host
			case "balloons-ready":
				ready = host
			}
		}
	}
	if webhook == "" || ready == "" {
		skipNoConfig(t, "namespace %s publishes no webhook Service (%q) and/or balloons-ready Service (%q); "+
			"the operator does not look installed there", ns, webhook, ready)
	}
	return webhook, ready
}

// wiIngressRestricted reports whether a NetworkPolicy opens the named port only to
// named peers.
func wiIngressRestricted(t testing.TB, c client.Client, ns, portName string) bool {
	t.Helper()
	var policies networkingv1.NetworkPolicyList
	if err := c.List(context.Background(), &policies, client.InNamespace(ns)); err != nil {
		t.Fatalf("list network policies in %s: %v", ns, err)
	}
	for _, np := range policies.Items {
		for _, rule := range np.Spec.Ingress {
			for _, port := range rule.Ports {
				if port.Port == nil || port.Port.String() != portName {
					continue
				}
				if len(rule.From) > 0 {
					return true
				}
			}
		}
	}
	return false
}

// wiProbePod builds a one-shot curl probe running script, hardened for the restricted
// Pod Security Standard.
func wiProbePod(name, ns, script string, vars []corev1.EnvVar) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   env("E2E_WEBHOOK_PROBE_IMAGE", "curlimages/curl:8.11.1"),
				Command: []string{"sh", "-c", script},
				Env:     vars,
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot: ptr.To(true),
					// curl_user in the default image. The image's USER is a name, which
					// the kubelet cannot verify as non-root, so a numeric uid is required.
					RunAsUser:                ptr.To(int64(100)),
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
			}},
		},
	}
}

// wiCreateProbe creates the probe pod, dropping the uid pin if refused: OpenShift's
// MustRunAsRange rejects an explicit uid but assigns a numeric one itself. A non-nil
// mutate customises each built pod before it is created.
func wiCreateProbe(ctx context.Context, c client.Client, name, ns, script string, mutate func(*corev1.Pod), env ...corev1.EnvVar) (*corev1.Pod, error) {
	build := func() *corev1.Pod {
		pod := wiProbePod(name, ns, script, env)
		if mutate != nil {
			mutate(pod)
		}
		return pod
	}
	pod := build()
	err := e2eCreate(ctx, c, pod)
	if !apierrors.IsForbidden(err) {
		return pod, err
	}
	pod = build()
	pod.Spec.Containers[0].SecurityContext.RunAsUser = nil
	if retryErr := e2eCreate(ctx, c, pod); retryErr != nil {
		// Report the first refusal too: which of the two rejected it matters.
		return pod, fmt.Errorf("%w (and with RunAsUser pinned: %v)", retryErr, err)
	}
	return pod, nil
}

// wiAwaitProbe returns the probe container's exit code; a pod that never finishes is a
// configuration gap and skips with the kubelet's reason.
func wiAwaitProbe(t testing.TB, c client.Client, ns, name string) int32 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var pod corev1.Pod
	for time.Now().Before(deadline) {
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &pod); err == nil {
			for _, cs := range pod.Status.ContainerStatuses {
				if term := cs.State.Terminated; term != nil {
					return term.ExitCode
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	skipNoConfig(t, "probe pod %s/%s never finished (%s), so nothing was probed — the image may "+
		"need a numeric USER or be unpullable here, in which case set E2E_WEBHOOK_PROBE_IMAGE",
		ns, name, wiProbeStall(&pod))
	return -1
}

// wiRunProbe runs script as a curl probe in a fresh namespace of its own, deleted at
// cleanup, and returns the probe's exit code and log.
func wiRunProbe(t testing.TB, c client.Client, ns, name, script string, env ...corev1.EnvVar) (int32, string) {
	t.Helper()
	ctx := context.Background()
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	_ = c.Delete(ctx, nsObj) // best-effort clean of a leftover run
	if err := createEventuallyGone(c, nsObj); err != nil {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Cleanup(func() {
		e2eDelete(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})
	pod, err := wiCreateProbe(ctx, c, name, ns, script, nil, env...)
	if err != nil {
		t.Fatalf("create probe pod: %v", err)
	}
	code := wiAwaitProbe(t, c, ns, pod.Name)
	cs, _ := fabManager(t)
	log, err := podLogs(ctx, cs, ns, pod.Name, "probe", nil)
	if err != nil {
		t.Fatalf("read probe pod %s/%s log: %v", ns, pod.Name, err)
	}
	return code, log
}

// wiProbeStall says why the probe pod is not running, from the waiting container's
// reason and message, falling back to the phase.
func wiProbeStall(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		detail := w.Reason
		if w.Message != "" {
			if detail != "" {
				detail += ": "
			}
			detail += w.Message
		}
		if detail != "" {
			return fmt.Sprintf("container %s waiting — %s", cs.Name, detail)
		}
	}
	return fmt.Sprintf("phase %q", pod.Status.Phase)
}

// TestProbeStallNamesTheKubeletsReason proves wiProbeStall reports the kubelet's own
// reason, not a guessed image-pull failure. No cluster.
func TestProbeStallNamesTheKubeletsReason(t *testing.T) {
	const refusal = "container has runAsNonRoot and image has non-numeric user (curl_user), cannot verify user is non-root"
	for _, tc := range []struct {
		name   string
		status corev1.PodStatus
		want   string
	}{
		{
			name: "a refused securityContext: reason and the kubelet's text",
			status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "probe",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: refusal}},
			}}},
			want: "container probe waiting — CreateContainerConfigError: " + refusal,
		},
		{
			name: "a pull that is still backing off: reason alone, no message",
			status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "probe",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}}},
			want: "container probe waiting — ImagePullBackOff",
		},
		{
			name:   "unschedulable: no container status at all, so the phase is all there is",
			status: corev1.PodStatus{Phase: corev1.PodPending},
			want:   `phase "Pending"`,
		},
		{
			name: "running but not yet finished: nothing is waiting",
			status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "probe",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}},
			want: `phase "Running"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wiProbeStall(&corev1.Pod{Status: tc.status}); got != tc.want {
				t.Errorf("wiProbeStall() = %q, want %q", got, tc.want)
			}
		})
	}
}
