// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	rlGate = "E2E_READINESS_RATE_LIMIT"
	// Mirror internal/readiness perSourceRate and perSourceBurst.
	rlRate  = 2
	rlBurst = 5
	// Enough to outrun the refill even on a slow replica.
	rlRequests = 30
)

// rlProbeScript sends rlRequests on one connection, waits the bucket full again and
// sends one more. -k: the limiter, not the server's identity, is under test.
const rlProbeScript = `
set -uf
url="$SCHEME://$TARGET/balloons/ready?node=e2e-rate-limit"
args=""
i=0
while [ "$i" -lt "$N" ]; do args="$args -o /dev/null $url"; i=$((i+1)); done
curl -sS -k -m 30 -w 'RL %{http_code} %{time_total} %header{retry-after}\n' $args
sleep 4
curl -sS -k -m 10 -o /dev/null -w 'AFTER %{http_code}\n' "$url"
`

// TestReadinessEndpointRateLimitsPerSource proves one source above 2 req/s past a
// burst of 5 gets 429 with Retry-After, and is served again once the bucket refills.
func TestReadinessEndpointRateLimitsPerSource(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(rlGate) == "" {
		skipNotSelected(at, "set %s=1 to burst the balloons-ready endpoint from a probe pod and assert its per-source rate limit", rlGate)
	}
	c := newClient(at)
	cs, opNS := fabManager(at)
	ctx := context.Background()

	// One replica, by pod IP: the limiter is per replica, so a Service would spread the burst.
	pods, err := cs.CoreV1().Pods(opNS).List(ctx, metav1.ListOptions{LabelSelector: managerPodSelector(ctx, cs, opNS)})
	if err != nil {
		at.Fatalf("list manager pods in %s: %v", opNS, err)
	}
	var target string
	for i := range pods.Items {
		if p := &pods.Items[i]; podReady(p) && p.Status.PodIP != "" {
			if port := rlReadyPort(p); port != 0 {
				target = net.JoinHostPort(p.Status.PodIP, strconv.Itoa(int(port)))
				at.Logf("bursting manager pod %s at %s", p.Name, target)
				break
			}
		}
	}
	if target == "" {
		skipNoConfig(at, "no Ready manager pod in %s exposes a balloons-ready container port", opNS)
	}
	scheme := "https"
	if cfg, err := operatorConfig(); err == nil && strings.HasPrefix(cfg["BALLOONS_READY_ENDPOINT"], "http://") {
		scheme = "http"
	}

	code, log := wiRunProbe(at, c, "cpu-pinning-e2e-ready-ratelimit", "ready-ratelimit-probe", rlProbeScript,
		corev1.EnvVar{Name: "SCHEME", Value: scheme},
		corev1.EnvVar{Name: "TARGET", Value: target},
		corev1.EnvVar{Name: "N", Value: strconv.Itoa(rlRequests)})
	if code != 0 {
		skipNoConfig(at, "the probe pod could not reach %s://%s (curl exit %d): %s", scheme, target, code, log)
	}
	samples, after := rlParse(log)
	problems, inconclusive := rlVerdict(samples, after)
	for _, p := range problems {
		at.Error(p)
	}
	if inconclusive != "" && len(problems) == 0 {
		skipNoConfig(at, "%s", inconclusive)
	}
	at.Logf("probe log:\n%s", log)
}

// rlReadyPort is the pod's balloons-ready container port, or 0.
func rlReadyPort(p *corev1.Pod) int32 {
	for _, ctr := range p.Spec.Containers {
		for _, port := range ctr.Ports {
			if port.Name == "balloons-ready" {
				return port.ContainerPort
			}
		}
	}
	return 0
}

// rlSample is one burst request as curl wrote it out.
type rlSample struct {
	code       int
	secs       float64
	retryAfter string
}

// rlParse reads the probe's RL lines and its AFTER code (0 when missing).
func rlParse(log string) (samples []rlSample, after int) {
	for _, line := range strings.Split(log, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) >= 3 && f[0] == "RL":
			code, _ := strconv.Atoi(f[1])
			secs, _ := strconv.ParseFloat(f[2], 64)
			s := rlSample{code: code, secs: secs}
			if len(f) > 3 {
				s.retryAfter = f[3]
			}
			samples = append(samples, s)
		case len(f) == 2 && f[0] == "AFTER":
			after, _ = strconv.Atoi(f[1])
		}
	}
	return samples, after
}

// rlVerdict judges a burst against rate and burst; inconclusive is set when the burst
// was too slow to exceed the rate, so no 429 was owed.
func rlVerdict(samples []rlSample, after int) (problems []string, inconclusive string) {
	if len(samples) == 0 {
		return []string{"the probe wrote out no burst responses"}, ""
	}
	var elapsed float64
	served, limited := 0, 0
	for i, s := range samples {
		elapsed += s.secs
		switch s.code {
		case 429:
			limited++
			if i < rlBurst {
				problems = append(problems, fmt.Sprintf("request %d of a fresh source was refused inside the burst of %d", i+1, rlBurst))
			}
			if s.retryAfter != "1" {
				problems = append(problems, fmt.Sprintf("request %d: 429 with Retry-After %q, want \"1\"", i+1, s.retryAfter))
			}
		case 200, 503:
			served++
		default:
			problems = append(problems, fmt.Sprintf("request %d answered HTTP %d, want 200, 503 or 429", i+1, s.code))
		}
	}
	// +1: a token can refill between the last two reads of the clock.
	allowed := rlBurst + int(rlRate*elapsed) + 1
	if served > allowed {
		problems = append(problems, fmt.Sprintf("%d of %d requests served in %.2fs; burst %d at %d/s allows at most %d",
			served, len(samples), elapsed, rlBurst, rlRate, allowed))
	}
	switch after {
	case 0:
		problems = append(problems, "the probe wrote out no request after the wait")
	case 429:
		problems = append(problems, "still 429 after waiting the bucket full again: the source stays locked out")
	}
	if limited == 0 && len(samples) <= allowed {
		inconclusive = fmt.Sprintf("%d requests took %.2fs, too slow to exceed burst %d at %d/s", len(samples), elapsed, rlBurst, rlRate)
	}
	return problems, inconclusive
}

// TestRateLimitVerdict proves rlVerdict over limited, unlimited, slow and malformed
// bursts. No cluster.
func TestRateLimitVerdict(t *testing.T) {
	burst := func(served, limited int, secs float64, retryAfter string) []rlSample {
		var out []rlSample
		for i := 0; i < served; i++ {
			out = append(out, rlSample{code: 503, secs: secs})
		}
		for i := 0; i < limited; i++ {
			out = append(out, rlSample{code: 429, secs: secs, retryAfter: retryAfter})
		}
		return out
	}
	for _, tc := range []struct {
		name         string
		samples      []rlSample
		after        int
		problems     int
		inconclusive bool
	}{
		{"burst of 5 then 429s, recovered", burst(5, 25, 0.005, "1"), 503, 0, false},
		{"one refill token inside the window", burst(6, 24, 0.01, "1"), 503, 0, false},
		{"no limit at all", burst(30, 0, 0.005, ""), 503, 1, false},
		{"limited inside the burst", append(burst(3, 0, 0.005, ""), burst(0, 27, 0.005, "1")...), 503, 2, false},
		{"429 without Retry-After", burst(5, 1, 0.005, ""), 503, 1, false},
		{"locked out after the wait", burst(5, 25, 0.005, "1"), 429, 1, false},
		{"an unexpected status", append(burst(5, 25, 0.005, "1"), rlSample{code: 400}), 503, 1, false},
		{"too slow to owe a 429", burst(10, 0, 0.5, ""), 503, 0, true},
		{"no output", nil, 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems, inconclusive := rlVerdict(tc.samples, tc.after)
			if len(problems) != tc.problems || (inconclusive != "") != tc.inconclusive {
				t.Errorf("rlVerdict() = %q, inconclusive %q; want %d problem(s), inconclusive %v",
					problems, inconclusive, tc.problems, tc.inconclusive)
			}
		})
	}
	// The write-out format the probe script uses.
	samples, after := rlParse("RL 503 0.012 \n{\"node\":\"x\"}\nRL 429 0.001 1\nAFTER 503\n")
	if len(samples) != 2 || samples[1] != (rlSample{429, 0.001, "1"}) || after != 503 {
		t.Errorf("rlParse() = %+v, after %d", samples, after)
	}
}
