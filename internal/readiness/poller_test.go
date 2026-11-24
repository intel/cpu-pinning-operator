// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestPollSucceedsAfter503 proves the poller waits through a 503 and returns nil on 200.
func TestPollSucceedsAfter503(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("node") != "node-1" {
			t.Errorf("node query = %q, want node-1", r.URL.Query().Get("node"))
		}
		if atomic.AddInt32(&calls, 1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := PollConfig{Endpoint: srv.URL, Node: "node-1", Interval: time.Millisecond, Timeout: 2 * time.Second}
	if err := Poll(context.Background(), srv.Client(), cfg, io.Discard); err != nil {
		t.Fatalf("Poll returned %v, want nil after the endpoint went ready", err)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Errorf("endpoint hit %d times, want >= 2 (should have retried past the 503)", got)
	}
}

// TestPollTimesOut proves a persistent 503 ends in a timeout error.
func TestPollTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := PollConfig{Endpoint: srv.URL, Node: "node-1", Interval: time.Millisecond, Timeout: 50 * time.Millisecond}
	err := Poll(context.Background(), srv.Client(), cfg, io.Discard)
	if err == nil {
		t.Fatal("Poll returned nil, want a timeout error for a never-ready endpoint")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want a timeout", err)
	}
}

// TestPollFailsFastOn400 confirms a 400 (misconfiguration) is not retried.
func TestPollFailsFastOn400(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	cfg := PollConfig{Endpoint: srv.URL, Node: "node-1", Interval: time.Millisecond, Timeout: 2 * time.Second}
	err := Poll(context.Background(), srv.Client(), cfg, io.Discard)
	if err == nil {
		t.Fatal("Poll returned nil, want an error on a 400 response")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("endpoint hit %d times, want exactly 1 (400 must not be retried)", got)
	}
}

func TestEndpointURL(t *testing.T) {
	cases := []struct {
		name, endpoint, node, want string
	}{
		{"base host only", "http://mgr:8082", "node-1", "http://mgr:8082/balloons/ready?node=node-1"},
		{"trailing slash", "http://mgr:8082/", "node-1", "http://mgr:8082/balloons/ready?node=node-1"},
		{"node needs escaping", "http://mgr:8082", "node/x", "http://mgr:8082/balloons/ready?node=node%2Fx"},
		{"full path", "http://mgr:8082/balloons/ready", "node-1", "http://mgr:8082/balloons/ready?node=node-1"},
		{"full path trailing slash", "http://mgr:8082/balloons/ready/", "node-1", "http://mgr:8082/balloons/ready?node=node-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := endpointURL(tc.endpoint, tc.node); got != tc.want {
				t.Errorf("endpointURL(%q,%q) = %q, want %q", tc.endpoint, tc.node, got, tc.want)
			}
		})
	}
}

// TestRunPollerRequiresEndpointAndNode confirms the subcommand flag validation.
func TestRunPollerRequiresEndpointAndNode(t *testing.T) {
	if err := RunPoller([]string{"--node=node-1"}, io.Discard); err == nil {
		t.Error("expected an error when --endpoint is missing")
	}
	if err := RunPoller([]string{"--endpoint=http://mgr:8082"}, io.Discard); err == nil {
		t.Error("expected an error when --node is missing")
	}
}

// TestProbeNamesAnUnreadableBody proves an unreadable non-200 body keeps its verdict and is named.
func TestProbeNamesAnUnreadableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Promise more body than is sent, so the client's read ends early.
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()

	ready, retriable, detail := probe(context.Background(), srv.Client(), srv.URL)
	if ready || !retriable {
		t.Fatalf("ready=%v retriable=%v, want a retriable not-ready verdict", ready, retriable)
	}
	if !strings.Contains(detail, "HTTP 503") || !strings.Contains(detail, "body unreadable") {
		t.Fatalf("detail = %q, want the status and the read failure", detail)
	}
}
