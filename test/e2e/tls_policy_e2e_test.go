// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Dials manager pod IPs directly when this host routes them, else from a curl probe
// pod (E2E_WEBHOOK_PROBE_IMAGE).

package e2e

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const tlsGate = "E2E_TLS_POLICY"

// tlsPolicy is what one listener may negotiate; nil approved12 means TLS 1.2 is refused.
type tlsPolicy struct {
	minVersion uint16
	approved12 []uint16
}

var (
	tls13Only = tlsPolicy{minVersion: tls.VersionTLS13}
	// A copy of cmd/main.go approvedTLS12CipherSuites, so weakening that list fails here.
	tls12Hardened = tlsPolicy{minVersion: tls.VersionTLS12, approved12: []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	}}
)

// tlsAttempt is one handshake pinned to a version and, at TLS 1.2, optionally one suite.
type tlsAttempt struct {
	version, suite       uint16 // suite 0: the client's default list
	err                  error
	gotVersion, gotSuite uint16
	untested             bool // the probe could not offer it, so it says nothing about the server
}

func (a tlsAttempt) String() string {
	if a.suite == 0 {
		return tls.VersionName(a.version)
	}
	return tls.VersionName(a.version) + " " + tls.CipherSuiteName(a.suite)
}

// tlsAttemptPlan is every version alone, then every TLS 1.2 suite Go can offer, alone.
// SSLv3 is absent: crypto/tls cannot speak it.
func tlsAttemptPlan() []tlsAttempt {
	plan := []tlsAttempt{{version: tls.VersionTLS10}, {version: tls.VersionTLS11}, {version: tls.VersionTLS12}, {version: tls.VersionTLS13}}
	for _, s := range append(tls.CipherSuites(), tls.InsecureCipherSuites()...) {
		if slices.Contains(s.SupportedVersions, tls.VersionTLS12) {
			plan = append(plan, tlsAttempt{version: tls.VersionTLS12, suite: s.ID})
		}
	}
	return plan
}

// runTLSPlan runs the plan against addr. A TCP failure is returned apart from the
// attempts: it says nothing about the listener's TLS policy.
func runTLSPlan(ctx context.Context, addr string) ([]tlsAttempt, error) {
	return runTLSAttempts(ctx, addr, tlsAttemptPlan())
}

// runTLSAttempts handshakes each of plan against addr with crypto/tls, filling it in place.
func runTLSAttempts(ctx context.Context, addr string, plan []tlsAttempt) ([]tlsAttempt, error) {
	for i := range plan {
		a := &plan[i]
		d := net.Dialer{Timeout: 5 * time.Second}
		raw, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		var certReq uint16
		cfg := &tls.Config{
			MinVersion: a.version, MaxVersion: a.version,
			// Only the negotiated protocol is under test, not the serving cert.
			InsecureSkipVerify: true, //nolint:gosec // protocol probe
			// An mTLS port refuses the missing cert after it has settled the version and
			// suite; reaching its CertificateRequest is what shows the version was accepted.
			GetClientCertificate: func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
				certReq = cri.Version
				return &tls.Certificate{}, nil
			},
		}
		if a.suite != 0 {
			cfg.CipherSuites = []uint16{a.suite}
		}
		conn := tls.Client(raw, cfg)
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		a.err = conn.HandshakeContext(ctx)
		// A pinned suite is the only one offered, so it is the one the request came under.
		if a.err != nil && certReq != 0 {
			a.err, a.gotVersion, a.gotSuite = nil, certReq, a.suite
		} else if a.err == nil {
			st := conn.ConnectionState()
			a.gotVersion, a.gotSuite = st.Version, st.CipherSuite
		}
		_ = conn.Close()
	}
	return plan, nil
}

// tlsViolations judges the attempts against p; empty means compliant.
func tlsViolations(p tlsPolicy, attempts []tlsAttempt) []string {
	var out []string
	approvedSeen, approvedUntested := false, false
	for _, a := range attempts {
		if a.untested {
			approvedUntested = approvedUntested || slices.Contains(p.approved12, a.suite)
			continue
		}
		ok := a.err == nil
		switch {
		case ok && a.gotVersion < p.minVersion:
			out = append(out, fmt.Sprintf("%s accepted (negotiated %s), below the %s floor",
				a, tls.VersionName(a.gotVersion), tls.VersionName(p.minVersion)))
		case ok && a.gotVersion == tls.VersionTLS12 && a.gotSuite != 0 && !slices.Contains(p.approved12, a.gotSuite):
			out = append(out, fmt.Sprintf("%s negotiated %s, not an approved suite", a, tls.CipherSuiteName(a.gotSuite)))
		case !ok && a.suite == 0 && a.version >= p.minVersion:
			out = append(out, fmt.Sprintf("%s refused, but the policy allows it: %v", a, a.err))
		}
		if ok && a.suite != 0 && slices.Contains(p.approved12, a.gotSuite) {
			approvedSeen = true
		}
	}
	// Positive control: refusing every suite would otherwise pass as "only approved ones".
	if len(p.approved12) > 0 && !approvedSeen && !approvedUntested {
		out = append(out, "no approved TLS 1.2 suite was accepted when offered alone")
	}
	return out
}

// tlsRetestUntested re-runs, with crypto/tls against addr, the attempts the curl probe
// could not offer (OpenSSL 3 has no RC4 or 3DES), so they are judged, not skipped. On a
// TCP failure they stay untested.
func tlsRetestUntested(ctx context.Context, addr string, attempts []tlsAttempt) error {
	var idx []int
	var plan []tlsAttempt
	for i, a := range attempts {
		if a.untested {
			idx = append(idx, i)
			plan = append(plan, tlsAttempt{version: a.version, suite: a.suite})
		}
	}
	if len(plan) == 0 {
		return nil
	}
	got, err := runTLSAttempts(ctx, addr, plan)
	if err != nil {
		return err
	}
	for j, i := range idx {
		attempts[i] = got[j]
	}
	return nil
}

// portForwardPort reads the local port from a "kubectl port-forward" stdout line.
func portForwardPort(line string) (int, bool) {
	var port, remote int
	n, _ := fmt.Sscanf(line, "Forwarding from 127.0.0.1:%d -> %d", &port, &remote)
	return port, n == 2 && port > 0
}

// tlsPortForward forwards a local port to pod:port with a kubectl subprocess (E2E_KUBECTL_BIN)
// and returns the local address; cleanup kills the subprocess.
func tlsPortForward(t *testing.T, ns, pod string, port int) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, env("E2E_KUBECTL_BIN", "kubectl"), "port-forward", "-n", ns, "--address", "127.0.0.1",
		"pod/"+pod, ":"+strconv.Itoa(port))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return "", err
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	found := make(chan int, 1)
	go func() {
		defer close(found)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if p, ok := portForwardPort(sc.Text()); ok {
				found <- p
				break
			}
		}
		_, _ = io.Copy(io.Discard, stdout) // keep draining so kubectl never blocks on a full pipe
	}()
	select {
	case p, ok := <-found:
		if !ok {
			cancel()
			_ = cmd.Wait()
			return "", fmt.Errorf("kubectl port-forward exited without a local port: %s", strings.TrimSpace(stderr.String()))
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(p)), nil
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("kubectl port-forward printed no local port in 30s")
	}
}

// tlsListener is one manager port and the policy it must hold.
type tlsListener struct {
	name       string
	port       int
	policy     tlsPolicy
	portName   string
	leaderOnly bool
	off        string // why this install serves no TLS here, or ""
}

// TestTLSListenersNegotiateApprovedProtocols proves every manager TLS listener
// accepts only the approved protocol versions and TLS 1.2 cipher suites.
func TestTLSListenersNegotiateApprovedProtocols(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(tlsGate) == "" {
		skipNotSelected(at, "set %s=1 to handshake-test the manager's TLS listeners", tlsGate)
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		skipNoCapability(at, "no kubeconfig available: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		at.Fatalf("build clientset: %v", err)
	}
	c := newClient(at)
	opNS := env("E2E_OPERATOR_NAMESPACE", "cpu-pinning-operator-system")
	name := env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	dep, err := cs.AppsV1().Deployments(opNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		skipNoConfig(at, "could not read manager Deployment %s/%s (operator may not be installed): %v", opNS, name, err)
	}
	pods, err := cs.CoreV1().Pods(opNS).List(ctx, metav1.ListOptions{LabelSelector: managerPodSelector(ctx, cs, opNS)})
	if err != nil {
		at.Fatalf("list manager pods in %s: %v", opNS, err)
	}

	for _, l := range tlsListenersOf(dep) {
		t.Run(l.name, func(t *testing.T) {
			if l.off != "" {
				skipNoConfig(t, "%s/%s: %s", opNS, name, l.off)
			}
			var targets []corev1.Pod
			if l.leaderOnly {
				// Followers do not serve it; only the lease holder starts the authority.
				leader := leaderPodNameAfterHandover(t, cs, opNS)
				for _, p := range pods.Items {
					if p.Name == leader {
						targets = append(targets, p)
					}
				}
			} else {
				for _, p := range pods.Items {
					if podReady(&p) {
						targets = append(targets, p)
					}
				}
			}
			if len(targets) == 0 {
				skipNoConfig(t, "no Ready manager pod in %s serves %s", opNS, l.name)
			}
			results := map[string][]tlsAttempt{}
			var viaPod, untested []string
			for _, p := range targets {
				addr := net.JoinHostPort(p.Status.PodIP, strconv.Itoa(l.port))
				// Direct when this host routes the pod network; else from a probe pod.
				// A host whose own network reuses the pod CIDR could answer here;
				// pin the serving-cert SAN if that ever bites.
				if attempts, err := runTLSPlan(ctx, addr); err == nil {
					results[addr] = attempts
				} else {
					viaPod = append(viaPod, addr)
				}
			}
			if len(viaPod) > 0 {
				var probeNS string
				var peer map[string]string
				if l.leaderOnly {
					// The reserve port admits only manager-labelled peers in the operator namespace.
					probeNS, peer = opNS, dep.Spec.Selector.MatchLabels
				} else {
					probeNS = tlsProbeNamespace(t, c)
				}
				maps.Copy(results, tlsProbeViaPod(t, c, cs, probeNS, "tls-probe-"+l.name, peer, viaPod))
			}
			for _, p := range targets {
				addr := net.JoinHostPort(p.Status.PodIP, strconv.Itoa(l.port))
				attempts := results[addr]
				if attempts == nil {
					if wiIngressRestricted(t, c, opNS, l.portName) {
						skipNotSelected(t, "pod %s (%s) is unreachable and the NetworkPolicy opens %s only to named peers; "+
							"run with that from list empty to cover this port", p.Name, addr, l.portName)
					}
					skipNoConfig(t, "pod %s (%s) unreachable directly and from a probe pod", p.Name, addr)
				}
				if slices.ContainsFunc(attempts, func(a tlsAttempt) bool { return a.untested }) {
					// curl cannot offer them, but the listener may still accept them: put them through Go.
					fwd, err := tlsPortForward(t, opNS, p.Name, l.port)
					if err == nil {
						err = tlsRetestUntested(ctx, fwd, attempts)
					}
					if err != nil {
						t.Logf("pod %s: could not re-test the attempts curl cannot offer over a port-forward: %v", p.Name, err)
					}
				}
				for _, v := range tlsViolations(l.policy, attempts) {
					t.Errorf("pod %s %s: %s", p.Name, addr, v)
				}
				for _, a := range attempts {
					if a.untested {
						untested = append(untested, fmt.Sprintf("pod %s %s", p.Name, a))
					}
				}
				t.Logf("pod %s %s: %d pinned handshakes judged", p.Name, addr, len(attempts))
			}
			// Not compliance: these were never put to the server. Errors above still fail the test.
			if len(untested) > 0 {
				skipNoConfig(t, "incomplete coverage, neither the probe pod nor a kubectl port-forward could offer %d attempt(s): %s; "+
					"put kubectl on PATH (E2E_KUBECTL_BIN), or set E2E_WEBHOOK_PROBE_IMAGE to a curl whose TLS library still has them",
					len(untested), strings.Join(untested, ", "))
			}
		})
	}
}

// opensslSuiteNames maps each TLS 1.2 suite in tlsAttemptPlan to curl's OpenSSL name.
var opensslSuiteNames = map[uint16]string{
	tls.TLS_RSA_WITH_RC4_128_SHA:                      "RC4-SHA",
	tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA:                 "DES-CBC3-SHA",
	tls.TLS_RSA_WITH_AES_128_CBC_SHA:                  "AES128-SHA",
	tls.TLS_RSA_WITH_AES_256_CBC_SHA:                  "AES256-SHA",
	tls.TLS_RSA_WITH_AES_128_CBC_SHA256:               "AES128-SHA256",
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256:               "AES128-GCM-SHA256",
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384:               "AES256-GCM-SHA384",
	tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA:              "ECDHE-ECDSA-RC4-SHA",
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA:          "ECDHE-ECDSA-AES128-SHA",
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA:          "ECDHE-ECDSA-AES256-SHA",
	tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA:                "ECDHE-RSA-RC4-SHA",
	tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA:           "ECDHE-RSA-DES-CBC3-SHA",
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA:            "ECDHE-RSA-AES128-SHA",
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA:            "ECDHE-RSA-AES256-SHA",
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256:       "ECDHE-ECDSA-AES128-SHA256",
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256:         "ECDHE-RSA-AES128-SHA256",
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256:         "ECDHE-RSA-AES128-GCM-SHA256",
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256:       "ECDHE-ECDSA-AES128-GCM-SHA256",
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384:         "ECDHE-RSA-AES256-GCM-SHA384",
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384:       "ECDHE-ECDSA-AES256-GCM-SHA384",
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256:   "ECDHE-RSA-CHACHA20-POLY1305",
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256: "ECDHE-ECDSA-CHACHA20-POLY1305",
}

// curlArgs pins one attempt. SECLEVEL=0 lets OpenSSL 3 offer TLS 1.0/1.1 and SHA-1
// suites at all; without it a weak server would look like it refused them.
func curlArgs(a tlsAttempt) string {
	v := map[uint16]string{tls.VersionTLS10: "1.0", tls.VersionTLS11: "1.1", tls.VersionTLS12: "1.2", tls.VersionTLS13: "1.3"}[a.version]
	args := "--tlsv" + v + " --tls-max " + v
	switch {
	case a.suite != 0:
		args += " --ciphers " + opensslSuiteNames[a.suite] + ":@SECLEVEL=0"
	case a.version < tls.VersionTLS12:
		args += " --ciphers DEFAULT:@SECLEVEL=0"
	}
	return args
}

// curlProbeScript prints "TLSPROBE <addr#> <attempt#> <curl exit> <cert requested>" per
// attempt, and stops an address at its first TCP-level failure. "Request CERT" in curl's
// verbose trace is the server's CertificateRequest, sent only once it accepted the version.
func curlProbeScript(addrs []string) string {
	var b strings.Builder
	b.WriteString(`p() { t=$1 a=$2; shift 2; [ "${skip:-}" = "$t" ] && return
  v=$(curl -skv -o /dev/null --connect-timeout 3 -m 10 "$@" 2>&1); c=$?; r=0
  case $v in *"Request CERT"*) r=1 ;; esac; echo "TLSPROBE $t $a $c $r"
  case $c in 7|28) skip=$t ;; esac; }
`)
	for ti, addr := range addrs {
		for ai, a := range tlsAttemptPlan() {
			fmt.Fprintf(&b, "p %d %d %s https://%s/\n", ti, ai, curlArgs(a), addr)
		}
	}
	return b.String()
}

// parseCurlProbe maps the script output back onto the plan, per address; an address
// that hit a TCP-level failure, or reported nothing, gets nil.
func parseCurlProbe(out string, addrs []string) map[string][]tlsAttempt {
	plan := tlsAttemptPlan()
	res := map[string][]tlsAttempt{}
	seen := map[string]int{}
	for _, addr := range addrs {
		res[addr] = slices.Clone(plan)
	}
	for _, line := range strings.Split(out, "\n") {
		var ti, ai, code, certReq int
		if n, _ := fmt.Sscanf(line, "TLSPROBE %d %d %d %d", &ti, &ai, &code, &certReq); n != 4 || ti < 0 || ti >= len(addrs) || ai < 0 || ai >= len(plan) {
			continue
		}
		addr := addrs[ti]
		if res[addr] == nil {
			continue
		}
		seen[addr]++
		a := &res[addr][ai]
		switch {
		case code == 7 || code == 28:
			res[addr] = nil
		// 59: curl could not set the cipher, so it was never offered.
		case code == 59:
			a.untested = true
		// 56: the TLS 1.3 reserve port refused the missing client cert after the handshake.
		// certReq: the server sent its CertificateRequest, so it took this version and suite.
		case code == 0 || certReq == 1 || (code == 56 && a.version == tls.VersionTLS13):
			a.gotVersion, a.gotSuite = a.version, a.suite
		default:
			a.err = fmt.Errorf("curl exit %d", code)
		}
	}
	// A truncated log must not read as a run of refusals.
	for addr := range res {
		if seen[addr] != len(plan) {
			res[addr] = nil
		}
	}
	return res
}

// tlsProbeNamespace creates the namespace the unlabelled probe pods run in. A previous
// subtest's cleanup may still be deleting it, so wait that out as wiRunProbe does.
func tlsProbeNamespace(t *testing.T, c client.Client) string {
	const ns = "cpu-pinning-e2e-tls-probe"
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	_ = c.Delete(context.Background(), nsObj) // best-effort clean of a leftover run
	if err := createEventuallyGone(c, nsObj); err != nil {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Cleanup(func() { e2eDelete(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
	return ns
}

// tlsProbeViaPod runs the plan against addrs from a curl pod in ns. Non-nil peer gives
// it the manager's selector labels, test-only, so the reserve NetworkPolicy admits it.
func tlsProbeViaPod(t *testing.T, c client.Client, cs kubernetes.Interface, ns, name string, peer map[string]string, addrs []string) map[string][]tlsAttempt {
	ctx := context.Background()
	var owner []metav1.OwnerReference
	if peer != nil {
		// A controller owner keeps the manager ReplicaSet from adopting the labelled pod.
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		_ = c.Delete(ctx, cm)
		if err := createEventuallyGone(c, cm); err != nil {
			t.Fatalf("create probe owner ConfigMap %s/%s: %v", ns, name, err)
		}
		t.Cleanup(func() {
			e2eDelete(t, c, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		})
		owner = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: name, UID: cm.UID, Controller: ptr.To(true)}}
	}
	mutate := func(p *corev1.Pod) {
		p.Labels, p.OwnerReferences = peer, owner
		if peer != nil {
			// Never Ready, so the manager Services never route to it.
			p.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: "e2e.cpu-pinning-operator.intel.com/never"}}
		}
	}
	_ = c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
	waitForPodGone(ctx, c, ns, name, time.Minute)
	pod, err := wiCreateProbe(ctx, c, name, ns, curlProbeScript(addrs), mutate)
	if err != nil {
		t.Fatalf("create TLS probe pod %s/%s: %v", ns, name, err)
	}
	t.Cleanup(func() {
		e2eDelete(t, c, pod)
		// Later tests list manager pods by these labels.
		waitForPodGone(context.Background(), c, ns, name, time.Minute)
	})
	if code := wiAwaitProbe(t, c, ns, name); code != 0 {
		t.Fatalf("TLS probe pod %s/%s exited %d", ns, name, code)
	}
	raw, err := cs.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil {
		t.Fatalf("read TLS probe log %s/%s: %v", ns, name, err)
	}
	return parseCurlProbe(string(raw), addrs)
}

// tlsListenersOf reads the real ports and TLS switches from the manager's pod template.
func tlsListenersOf(dep *appsv1.Deployment) []tlsListener {
	port := func(name string, def int) int {
		for _, c := range dep.Spec.Template.Spec.Containers {
			for _, p := range c.Ports {
				if p.Name == name {
					return int(p.ContainerPort)
				}
			}
		}
		return def
	}
	metricsOff := ""
	switch {
	case raHasFlag(dep, "--metrics-bind-address=0"):
		metricsOff = "metrics are disabled (--metrics-bind-address=0)"
	case raHasFlag(dep, "--metrics-secure=false"):
		metricsOff = "metrics are served in plaintext (--metrics-secure=false); install with metrics.secure=true"
	}
	readyOff := ""
	if raHasFlag(dep, "--balloons-ready-secure=false") {
		readyOff = "balloons-ready is served in plaintext (--balloons-ready-secure=false)"
	}
	reserveOff := ""
	if !raHasFlag(dep, raAuthorityFlag) {
		reserveOff = "no " + raAuthorityFlag + ", so no reserve listener; install with replicaCount=2 and leader election on"
	}
	return []tlsListener{
		{name: "webhook", portName: "webhook-server", port: port("webhook-server", 9443), policy: tls12Hardened},
		{name: "metrics", portName: "metrics", port: port("metrics", 8080), policy: tls12Hardened, off: metricsOff},
		{name: "balloons-ready", portName: "balloons-ready", port: port("balloons-ready", 8082), policy: tls13Only, off: readyOff},
		{name: "reserve", portName: "reserve", port: port("reserve", 9444), policy: tls13Only, leaderOnly: true, off: reserveOff},
	}
}

// TestTLSProbeJudgesListenerPolicy runs the probe against local listeners with known
// configs, so a probe that stops detecting a weak listener fails here. No cluster.
func TestTLSProbeJudgesListenerPolicy(t *testing.T) {
	cert := selfSignedCert(t)
	for _, tc := range []struct {
		name   string
		server *tls.Config
		policy tlsPolicy
		want   string // a substring of the expected violation; "" = compliant
	}{
		{"hardened TLS 1.2 floor", &tls.Config{MinVersion: tls.VersionTLS12, CipherSuites: tls12Hardened.approved12}, tls12Hardened, ""},
		{"TLS 1.3 only", &tls.Config{MinVersion: tls.VersionTLS13}, tls13Only, ""},
		{"TLS 1.3 mTLS, no client cert offered", &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAnyClientCert}, tls13Only, ""},
		{"TLS 1.2 against a 1.3-only policy", &tls.Config{MinVersion: tls.VersionTLS12}, tls13Only, "below the TLS 1.3 floor"},
		// The reserve port weakened to 1.2: client auth still fails, but 1.2 was negotiated.
		{"TLS 1.2 mTLS, no client cert offered, against a 1.3-only policy", &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAnyClientCert}, tls13Only, "TLS 1.2 accepted"},
		{"TLS 1.0 accepted", &tls.Config{MinVersion: tls.VersionTLS10}, tls12Hardened, "TLS 1.0 accepted"},
		{"TLS 1.1 accepted", &tls.Config{MinVersion: tls.VersionTLS11}, tls12Hardened, "TLS 1.1 accepted"},
		{"CBC suite allowed", &tls.Config{MinVersion: tls.VersionTLS12, CipherSuites: append([]uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA}, tls12Hardened.approved12...)}, tls12Hardened, "AES_128_CBC_SHA, not an approved suite"},
		{"ChaCha20 allowed", &tls.Config{MinVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256}}, tls12Hardened, "CHACHA20_POLY1305_SHA256, not an approved suite"},
		{"TLS 1.3 refused on a 1.2 policy", &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12, CipherSuites: tls12Hardened.approved12}, tls12Hardened, "TLS 1.3 refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts, err := runTLSPlan(context.Background(), localTLSListener(t, cert, tc.server))
			if err != nil {
				t.Fatalf("dial local listener: %v", err)
			}
			got := tlsViolations(tc.policy, attempts)
			joined := strings.Join(got, "; ")
			if (tc.want == "") != (len(got) == 0) || !strings.Contains(joined, tc.want) {
				t.Errorf("violations %q, want one containing %q", got, tc.want)
			}
		})
	}
}

// localTLSListener serves handshakes under cfg until the test ends and returns its address.
func localTLSListener(t *testing.T, cert tls.Certificate, cfg *tls.Config) string {
	t.Helper()
	srv := cfg.Clone()
	srv.Certificates = []tls.Certificate{cert}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).Handshake()
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

// selfSignedCert is a throwaway ECDSA serving cert; the probe skips verification.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestCurlProbeFeedsTheSameJudge checks the probe-pod path: every planned suite has a
// curl name, and parsed exit codes reach tlsViolations as the direct path would. No cluster.
func TestCurlProbeFeedsTheSameJudge(t *testing.T) {
	plan := tlsAttemptPlan()
	for _, a := range plan {
		if a.suite != 0 && opensslSuiteNames[a.suite] == "" {
			t.Errorf("no OpenSSL name for %s, so the probe pod cannot offer it", a)
		}
	}
	if s := curlProbeScript([]string{"10.0.0.1:9443"}); !strings.Contains(s, "--tlsv1.0 --tls-max 1.0 --ciphers DEFAULT:@SECLEVEL=0 https://10.0.0.1:9443/") {
		t.Errorf("script does not pin TLS 1.0 with SECLEVEL=0:\n%s", s)
	}
	// out renders a server that answers each attempt with accept's curl exit code.
	out := func(accept func(tlsAttempt) string) string {
		var b strings.Builder
		for i, a := range plan {
			fmt.Fprintf(&b, "TLSPROBE 0 %d %s\n", i, accept(a))
		}
		return b.String()
	}
	hardened := func(a tlsAttempt) string {
		if a.version >= tls.VersionTLS12 && (a.suite == 0 || slices.Contains(tls12Hardened.approved12, a.suite)) {
			return "0 0"
		}
		return "35 0"
	}
	for _, tc := range []struct {
		name   string
		policy tlsPolicy
		accept func(tlsAttempt) string // "<curl exit> <cert requested>"
		want   string                  // "" = compliant; "nil" = unreachable
	}{
		{"hardened", tls12Hardened, hardened, ""},
		{"TLS 1.0 accepted", tls12Hardened, func(a tlsAttempt) string {
			if a.version == tls.VersionTLS10 {
				return "0 0"
			}
			return hardened(a)
		}, "TLS 1.0 accepted"},
		{"CBC suite accepted", tls12Hardened, func(a tlsAttempt) string {
			if a.suite == tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA {
				return "0 0"
			}
			return hardened(a)
		}, "AES_128_CBC_SHA, not an approved suite"},
		{"mTLS 1.3 refuses the missing cert after the handshake", tls13Only, func(a tlsAttempt) string {
			if a.version == tls.VersionTLS13 {
				return "56 1"
			}
			return "35 0"
		}, ""},
		// The reserve port weakened to 1.2: 1.2 handshakes fail on the cert, after the request.
		{"mTLS weakened to 1.2 still refuses the missing cert", tls13Only, func(a tlsAttempt) string {
			if a.version >= tls.VersionTLS12 {
				return "35 1"
			}
			return "35 0"
		}, "TLS 1.2 accepted"},
		{"unreachable", tls12Hardened, func(tlsAttempt) string { return "7 0" }, "nil"},
	} {
		got := parseCurlProbe(out(tc.accept), []string{"a"})["a"]
		if tc.want == "nil" {
			if got != nil {
				t.Errorf("%s: parsed %d attempts, want nil (unreachable)", tc.name, len(got))
			}
			continue
		}
		v := tlsViolations(tc.policy, got)
		if (tc.want == "") != (len(v) == 0) || !strings.Contains(strings.Join(v, "; "), tc.want) {
			t.Errorf("%s: violations %q, want one containing %q", tc.name, v, tc.want)
		}
	}
	// Exit 59: curl could not set the cipher. Untested, never a refusal the judge passes,
	// and an approved suite it could not offer does not trip the positive control.
	noCipher := func(a tlsAttempt) string {
		if a.suite == tls.TLS_RSA_WITH_RC4_128_SHA || a.suite == tls12Hardened.approved12[0] {
			return "59 0"
		}
		return hardened(a)
	}
	parsed := parseCurlProbe(out(noCipher), []string{"a"})["a"]
	var untested []string
	for _, a := range parsed {
		if a.untested {
			untested = append(untested, a.String())
		}
	}
	if len(untested) != 2 {
		t.Errorf("exit 59 marked %q untested, want RC4-SHA and %s", untested, tls.CipherSuiteName(tls12Hardened.approved12[0]))
	}
	if v := tlsViolations(tls12Hardened, parsed); len(v) > 0 {
		t.Errorf("exit 59: violations %q, want none (untested, not judged)", v)
	}
	// The Go fallback re-tests what curl could not offer: an accepted RC4 suite is a
	// violation, a refused one passes, and with no fallback it stays a coverage gap.
	cert := selfSignedCert(t)
	noRC4 := func(a tlsAttempt) string {
		if a.suite == tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA {
			return "59 0"
		}
		return hardened(a)
	}
	for _, tc := range []struct {
		name   string
		server *tls.Config // nil: the fallback cannot reach the listener
		want   string      // "" = compliant; "gap" = still untested
	}{
		{"fallback finds RC4 accepted", &tls.Config{MinVersion: tls.VersionTLS12,
			CipherSuites: append([]uint16{tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA}, tls12Hardened.approved12...)}, "RC4_128_SHA, not an approved suite"},
		{"fallback finds RC4 refused", &tls.Config{MinVersion: tls.VersionTLS12, CipherSuites: tls12Hardened.approved12}, ""},
		{"no fallback", nil, "gap"},
	} {
		parsed := parseCurlProbe(out(noRC4), []string{"a"})["a"]
		addr := "127.0.0.1:1" // nothing listens on tcpmux
		if tc.server != nil {
			addr = localTLSListener(t, cert, tc.server)
		}
		err := tlsRetestUntested(context.Background(), addr, parsed)
		gap := slices.ContainsFunc(parsed, func(a tlsAttempt) bool { return a.untested })
		v := tlsViolations(tls12Hardened, parsed)
		switch {
		case tc.want == "gap":
			if err == nil || !gap || len(v) > 0 {
				t.Errorf("%s: err %v, gap %v, violations %q; want a dial error and the attempt left untested", tc.name, err, gap, v)
			}
		case err != nil || gap:
			t.Errorf("%s: err %v, gap %v; want every attempt judged", tc.name, err, gap)
		case (tc.want == "") != (len(v) == 0) || !strings.Contains(strings.Join(v, "; "), tc.want):
			t.Errorf("%s: violations %q, want one containing %q", tc.name, v, tc.want)
		}
	}
	// A listener that refuses everything still fails, whatever the probe could not offer.
	parsed = parseCurlProbe(out(func(a tlsAttempt) string {
		if a.suite == tls.TLS_RSA_WITH_RC4_128_SHA {
			return "59 0"
		}
		return "35 0"
	}), []string{"a"})["a"]
	if v := strings.Join(tlsViolations(tls12Hardened, parsed), "; "); !strings.Contains(v, "no approved TLS 1.2 suite") {
		t.Errorf("refuse-all with RC4 untested: violations %q, want the positive control", v)
	}
	if got := parseCurlProbe("TLSPROBE 0 0 0 0\n", []string{"a"})["a"]; got != nil {
		t.Errorf("a truncated log parsed as %d attempts, want nil", len(got))
	}
}

// TestTLSProbeReadsPortForwardPort parses kubectl port-forward's stdout. No cluster.
func TestTLSProbeReadsPortForwardPort(t *testing.T) {
	for line, want := range map[string]int{
		"Forwarding from 127.0.0.1:41235 -> 9443": 41235,
		"Forwarding from [::1]:41235 -> 9443":     0,
		"Handling connection for 41235":           0,
		"":                                        0,
	} {
		if got, ok := portForwardPort(line); ok != (want != 0) || ok && got != want {
			t.Errorf("portForwardPort(%q) = %d, %v; want %d", line, got, ok, want)
		}
	}
}
