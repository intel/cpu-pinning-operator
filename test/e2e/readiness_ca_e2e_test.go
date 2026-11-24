// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	rcGate = "E2E_READINESS_TLS"
	// rcHold is how long the pod must stay in Init after its first verification failure.
	rcHold = 30 * time.Second
	// rcQuietFor is how long the copy must stay unrepaired before a pod is started on it.
	rcQuietFor = 10 * time.Second
	// rcAttempts bounds the retries after a repair reached the pod.
	rcAttempts = 3
)

// TestReadinessInitRefusesUnverifiedServer proves wait-for-balloons holds a matched pod
// in Init over https only while its CA copy names a foreign CA, and starts once restored.
func TestReadinessInitRefusesUnverifiedServer(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(rcGate) == "" {
		skipNotSelected(at, "set %s=1 to corrupt the target namespace's readiness CA copy and assert wait-for-balloons refuses "+
			"the server; the copy is restored afterwards", rcGate)
	}
	c := newClient(at)
	cfg, err := operatorConfig()
	if err != nil {
		skipNoConfig(at, "cannot read operator config %s/%s: %v", operatorNamespace(), operatorConfigName(), err)
	}
	endpoint, caCM := cfg["BALLOONS_READY_ENDPOINT"], cfg["READINESS_CA_CONFIGMAP"]
	if endpoint == "" || cfg["INIT_CONTAINER_IMAGE"] == "" {
		skipNotSelected(at, "the readiness gate is not configured, so no wait-for-balloons init container is injected")
	}
	if !strings.HasPrefix(endpoint, "https://") || caCM == "" {
		skipNotSelected(at, "the readiness endpoint %q is plaintext by configuration (balloonsReady TLS off); this test covers the https mode", endpoint)
	}
	ns := useTargetNamespace(at, c)
	node := balloonsEnabledNode(at, c)
	cs, opNS := fabManager(at)
	ctx := context.Background()
	key := client.ObjectKey{Namespace: ns, Name: caCM}

	orig := rcWaitCopy(at, c, key)
	_, _, foreign := wcaTestCert(at, "e2e-foreign-ca", nil, nil, nil)

	// The operator repairs a tampered copy on its resync, so a keeper re-corrupts it
	// until the hold is proven; cleanup always puts the operator's CA back.
	keepCtx, stopKeeper := context.WithCancel(ctx)
	var keeper sync.WaitGroup
	var repairs atomic.Int64
	restore := func() {
		stopKeeper()
		keeper.Wait()
		if err := rcSetCA(c, key, orig); err != nil {
			at.Errorf("restore %s ca.crt: %v — the operator's resync repairs it within a minute", key, err)
		}
	}
	t.Cleanup(restore)
	if err := rcSetCA(c, key, string(foreign)); err != nil {
		at.Fatalf("write a foreign CA into %s: %v", key, err)
	}
	keeper.Add(1)
	go func() {
		defer keeper.Done()
		for {
			select {
			case <-keepCtx.Done():
				return
			case <-time.After(time.Second):
			}
			var cm corev1.ConfigMap
			if c.Get(keepCtx, key, &cm) == nil && cm.Data["ca.crt"] != string(foreign) {
				repairs.Add(1)
				_ = rcSetCA(c, key, string(foreign))
			}
		}
	}()

	// A repair can land just as the kubelet syncs the pod's ConfigMap volume, handing the
	// pod the operator's CA until the next sync. A pod that leaves Init after a repair
	// during its own attempt is retried with a fresh pod; leaving Init with no repair fails.
	var (
		since   metav1.Time
		pod     *corev1.Pod
		log     string
		heldFor string
	)
	for attempt := 1; ; attempt++ {
		rcQuiet(at, &repairs)
		before := repairs.Load()
		since = metav1.Now()
		pod = vllmPod(fmt.Sprintf("e2e-readiness-ca-%d", attempt), ns, "1", "1", 1)
		pod.Spec.NodeName = node
		created := createAndCleanup(t, c, pod)
		if !hasInitContainer(created, waitForBalloonsInit) {
			at.Fatalf("pod %s/%s was not given the %s init container, though the readiness gate is configured", ns, pod.Name, waitForBalloonsInit)
		}
		var left string
		left, log = rcHoldInit(at, c, cs, ns, pod.Name)
		if left == "" {
			heldFor = fmt.Sprintf("attempt %d", attempt)
			break
		}
		if repairs.Load() == before || attempt == rcAttempts {
			at.Fatalf("pod %s/%s left Init while its CA copy named a foreign CA: %s (attempt %d, %d repair(s) during it)\ninit log:\n%s",
				ns, pod.Name, left, attempt, repairs.Load()-before, log)
		}
		at.Logf("attempt %d: the operator repaired the copy while pod %s was starting and the pod got its CA; retrying with a new pod", attempt, pod.Name)
		_ = c.Delete(ctx, pod)
	}
	_, problems := rcInitLogVerdict(log)
	for _, p := range problems {
		at.Error(p)
	}
	at.Logf("held in Init for %s past the first verification failure (%s); the operator repaired the copy %d time(s) meanwhile",
		rcHold, heldFor, repairs.Load())
	rcServerSideEvidence(at, cs, opNS, rcGetPod(at, c, ns, pod.Name).Status.PodIP, since)

	// Control: with the operator's CA back the same pod must start, so the hold was the CA.
	restore()
	deadline := time.Now().Add(4 * time.Minute)
	for rcLeftInit(at, c, ns, pod.Name) == "" {
		if time.Now().After(deadline) {
			at.Errorf("pod %s/%s is still in Init 4m after the CA copy was restored, so the hold cannot be pinned on the CA", ns, pod.Name)
			return
		}
		time.Sleep(3 * time.Second)
	}
}

// rcQuiet waits until the operator has gone rcQuietFor without repairing the copy, so a
// pod starts on a settled foreign CA. It gives up after two minutes.
func rcQuiet(t testing.TB, repairs *atomic.Int64) {
	t.Helper()
	last, settled := repairs.Load(), time.Now()
	for deadline := time.Now().Add(2 * time.Minute); time.Since(settled) < rcQuietFor; time.Sleep(time.Second) {
		if n := repairs.Load(); n != last {
			last, settled = n, time.Now()
		}
		if time.Now().After(deadline) {
			t.Logf("the operator kept repairing the copy for 2m; starting the pod anyway")
			return
		}
	}
}

// rcHoldInit waits for the first verification failure, then holds rcHold more. It returns
// why the pod left Init ("" when it was held) and the last init log.
func rcHoldInit(t testing.TB, c client.Client, cs kubernetes.Interface, ns, name string) (string, string) {
	t.Helper()
	var log string
	var failedAt time.Time
	for deadline := time.Now().Add(3 * time.Minute); ; {
		if left := rcLeftInit(t, c, ns, name); left != "" {
			return left, log
		}
		if l, err := podLogs(context.Background(), cs, ns, name, waitForBalloonsInit, nil); err == nil {
			log = l
		}
		if verifyFailed, _ := rcInitLogVerdict(log); verifyFailed && failedAt.IsZero() {
			failedAt = time.Now()
		}
		if !failedAt.IsZero() && time.Since(failedAt) >= rcHold {
			return "", log
		}
		if time.Now().After(deadline) {
			skipNoConfig(t, "the %s init container logged no TLS verification failure within 3m (%s)\ninit log:\n%s",
				waitForBalloonsInit, wiProbeStall(rcGetPod(t, c, ns, name)), log)
		}
		time.Sleep(3 * time.Second)
	}
}

// rcWaitCopy returns the copy's ca.crt once the operator has synced it (resync is 1m).
func rcWaitCopy(t testing.TB, c client.Client, key client.ObjectKey) string {
	t.Helper()
	var last error
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		var cm corev1.ConfigMap
		if last = c.Get(context.Background(), key, &cm); last == nil && len(wcaPEMCerts([]byte(cm.Data["ca.crt"]))) > 0 {
			return cm.Data["ca.crt"]
		}
	}
	t.Fatalf("the operator never synced readiness CA copy %s with a PEM ca.crt (last read: %v)", key, last)
	return ""
}

// rcSetCA writes ca into the copy's ca.crt, retrying conflicts with the operator's resync.
func rcSetCA(c client.Client, key client.ObjectKey, ca string) error {
	var err error
	for range 5 {
		var cm corev1.ConfigMap
		if err = c.Get(context.Background(), key, &cm); err != nil {
			continue
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["ca.crt"] = ca
		if err = c.Update(context.Background(), &cm); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return err
}

func rcGetPod(t testing.TB, c client.Client, ns, name string) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &pod); err != nil {
		t.Fatalf("get pod %s/%s: %v", ns, name, err)
	}
	return &pod
}

// rcLeftInit says how the pod got past Init, or "" while it is still there.
func rcLeftInit(t testing.TB, c client.Client, ns, name string) string {
	t.Helper()
	pod := rcGetPod(t, c, ns, name)
	for _, s := range pod.Status.InitContainerStatuses {
		if s.Name == waitForBalloonsInit && s.State.Terminated != nil && s.State.Terminated.ExitCode == 0 {
			return fmt.Sprintf("init container %s exited 0", s.Name)
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		if s.State.Running != nil || s.State.Terminated != nil {
			return fmt.Sprintf("container %s started", s.Name)
		}
	}
	return ""
}

// rcServerSideEvidence logs the manager's TLS handshake errors from the pod's IP: the
// client aborting the handshake. Go answers a plaintext request on a TLS port silently,
// so the absence of a plaintext attempt is not observable server-side.
func rcServerSideEvidence(t testing.TB, cs kubernetes.Interface, opNS, podIP string, since metav1.Time) {
	t.Helper()
	ctx := context.Background()
	pods, err := cs.CoreV1().Pods(opNS).List(ctx, metav1.ListOptions{LabelSelector: managerPodSelector(ctx, cs, opNS)})
	if err != nil || podIP == "" {
		t.Logf("server-side evidence not read (pod IP %q): %v", podIP, err)
		return
	}
	needle := "TLS handshake error from " + podIP + ":"
	for i := range pods.Items {
		p := &pods.Items[i]
		log, err := podLogs(ctx, cs, opNS, p.Name, p.Spec.Containers[0].Name, &since)
		if err != nil {
			t.Logf("manager pod %s log: %v", p.Name, err)
			continue
		}
		var hits []string
		for _, line := range strings.Split(log, "\n") {
			if strings.Contains(line, needle) {
				hits = append(hits, line)
			}
		}
		if len(hits) > 0 {
			t.Logf("manager pod %s logged %d handshake abort(s) from %s, e.g. %s", p.Name, len(hits), podIP, hits[0])
		}
	}
}

// rcInitLogVerdict reports whether the init log shows TLS verification failing, and
// any line that dialled plaintext or found the server ready.
func rcInitLogVerdict(log string) (verifyFailed bool, problems []string) {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "x509:") || strings.Contains(line, "tls: failed to verify certificate") {
			verifyFailed = true
		}
		if strings.Contains(line, "http://") {
			problems = append(problems, "the init container dialled plaintext: "+line)
		}
		if strings.Contains(line, "balloons plugin ready") {
			problems = append(problems, "the init container accepted the server: "+line)
		}
	}
	if !verifyFailed {
		problems = append(problems, "the init log shows no TLS verification failure")
	}
	return verifyFailed, problems
}

// TestReadinessInitLogVerdict proves rcInitLogVerdict over the poller's own log lines. No cluster.
func TestReadinessInitLogVerdict(t *testing.T) {
	const (
		waiting  = `waiting for balloons plugin to be ready on node "n1" via https://op-balloons-ready.op.svc:8082/balloons/ready?node=n1`
		refused  = `not ready yet (request failed: Get "https://op-balloons-ready.op.svc:8082/balloons/ready?node=n1": tls: failed to verify certificate: x509: certificate signed by unknown authority); retrying in 5s`
		noFile   = `not ready yet (request failed: Get "https://op-balloons-ready.op.svc:8082/balloons/ready?node=n1": reading readiness CA: open /etc/cpu-pinning-operator/readiness-ca/ca.crt: no such file or directory); retrying in 5s`
		plain    = `not ready yet (HTTP 503: {"node":"n1","ready":false}); retrying in 5s via http://op-balloons-ready.op.svc:8082`
		accepted = `balloons plugin ready on node "n1"`
	)
	for _, tc := range []struct {
		name         string
		log          string
		verifyFailed bool
		problems     int
	}{
		{"refused a foreign CA over https", waiting + "\n" + refused + "\n" + refused, true, 0},
		{"CA file missing: no verification happened yet", waiting + "\n" + noFile, false, 1},
		{"a plaintext attempt", waiting + "\n" + refused + "\n" + plain, true, 1},
		{"accepted the server", waiting + "\n" + refused + "\n" + accepted, true, 1},
		{"empty log", "", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifyFailed, problems := rcInitLogVerdict(tc.log)
			if verifyFailed != tc.verifyFailed || len(problems) != tc.problems {
				t.Errorf("rcInitLogVerdict() = %v, %q; want %v and %d problem(s)", verifyFailed, problems, tc.verifyFailed, tc.problems)
			}
		})
	}
}
