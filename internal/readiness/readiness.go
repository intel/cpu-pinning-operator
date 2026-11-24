// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package readiness serves GET /balloons/ready?node=<name> and the init-container
// poller that blocks start until the node's NRI balloons plugin is Ready: NRI
// pins only at container creation. Unauthenticated by design, so rate-limited
// per source IP and served over TLS.
package readiness

import (
	"context"
	"crypto/tls"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Path is the endpoint the init container polls. ?node= is required.
const Path = "/balloons/ready"

// errDaemonSetRead and errPodListRead are generic; never surface the raw API error.
var (
	errDaemonSetRead = stderrors.New("error reading balloons DaemonSet")
	errPodListRead   = stderrors.New("error listing balloons pods")
)

const (
	perSourceRate  = rate.Limit(2)
	perSourceBurst = 5
	// Bound the per-source map so a client cycling IPs cannot grow it without limit.
	limiterIdleTTL    = 10 * time.Minute
	limiterSweepEvery = time.Minute
)

// sourceLimiter is one token bucket per source IP, with idle eviction.
type sourceLimiter struct {
	mu        sync.Mutex
	clients   map[string]*clientLimiter
	lastSweep time.Time
}

type clientLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newSourceLimiter() *sourceLimiter {
	return &sourceLimiter{clients: make(map[string]*clientLimiter)}
}

// allow consumes a token for ip at now. Evicts idle buckets inline.
func (s *sourceLimiter) allow(ip string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lastSweep.IsZero() {
		s.lastSweep = now
	} else if now.Sub(s.lastSweep) >= limiterSweepEvery {
		for k, e := range s.clients {
			if now.Sub(e.lastSeen) >= limiterIdleTTL {
				delete(s.clients, k)
			}
		}
		s.lastSweep = now
	}

	e := s.clients[ip]
	if e == nil {
		e = &clientLimiter{limiter: rate.NewLimiter(perSourceRate, perSourceBurst)}
		s.clients[ip] = e
	}
	e.lastSeen = now
	return e.limiter.AllowN(now, 1)
}

// sourceIP is the direct peer IP. It does not trust X-Forwarded-For: nothing
// sits in front of this endpoint, so that header would defeat the rate limit.
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

const (
	readTimeout  = 5 * time.Second
	writeTimeout = 10 * time.Second
)

// Handler answers whether the balloons plugin pod on a node is Ready. Client must
// be uncached (mgr.GetAPIReader): a stale false-ready leaves a pod unpinned forever.
type Handler struct {
	// Client reads the balloons DaemonSet and its pods. Uncached only.
	Client client.Reader
	// Namespace and DaemonSet name the NRI balloons plugin.
	Namespace string
	DaemonSet string
	Log       logr.Logger

	// Per replica, so the effective cap is perSourceRate × replicaCount.
	limiterOnce sync.Once
	limiter     *sourceLimiter
}

// allow reports whether ip may proceed to the live API read.
func (h *Handler) allow(ip string) bool {
	h.limiterOnce.Do(func() { h.limiter = newSourceLimiter() })
	return h.limiter.allow(ip, time.Now())
}

// result is the JSON body. Error is set only when the API read failed; both
// Reason and Error are fixed strings, never the raw API error.
type result struct {
	Node   string `json:"node"`
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ServeHTTP: 400 missing node, 429 rate-limited, 200 Ready, 503 otherwise.
// 503 is not 404 so the init container keeps polling.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	node := r.URL.Query().Get("node")
	if node == "" {
		h.write(w, http.StatusBadRequest, result{Ready: false, Reason: "missing node query parameter"})
		return
	}

	if !h.allow(sourceIP(r)) {
		w.Header().Set("Retry-After", "1")
		h.write(w, http.StatusTooManyRequests, result{Node: node, Ready: false, Reason: "rate limited"})
		return
	}

	ready, reason, apiErr := h.nodeReady(r.Context(), node)
	status := http.StatusServiceUnavailable
	res := result{Node: node, Ready: ready, Reason: reason}
	if ready {
		status = http.StatusOK
	} else if apiErr != nil {
		res.Error = apiErr.Error()
	}
	h.write(w, status, res)
}

// nodeReady returns a non-nil apiErr only when the API read failed, not for genuine not-ready.
func (h *Handler) nodeReady(ctx context.Context, node string) (ready bool, reason string, apiErr error) {
	ds := &appsv1.DaemonSet{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.Namespace, Name: h.DaemonSet}, ds); err != nil {
		if errors.IsNotFound(err) {
			return false, fmt.Sprintf("balloons DaemonSet %s/%s not found yet", h.Namespace, h.DaemonSet), nil
		}
		// Unauthenticated: never return the raw API error (it can name the SA).
		h.Log.Error(err, "reading balloons DaemonSet", "namespace", h.Namespace, "daemonset", h.DaemonSet)
		return false, fmt.Sprintf("error reading balloons DaemonSet %s/%s", h.Namespace, h.DaemonSet), errDaemonSetRead
	}

	sel, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil {
		h.Log.Error(err, "balloons DaemonSet has an invalid pod selector", "namespace", h.Namespace, "daemonset", h.DaemonSet)
		return false, fmt.Sprintf("balloons DaemonSet %s/%s has an invalid pod selector", h.Namespace, h.DaemonSet), nil
	}
	if sel.Empty() {
		// Empty selector matches every pod in the namespace — treat as unreadable.
		return false, fmt.Sprintf("balloons DaemonSet %s/%s has an empty pod selector", h.Namespace, h.DaemonSet), nil
	}

	pods := &corev1.PodList{}
	if err := h.Client.List(ctx, pods,
		client.InNamespace(h.Namespace),
		client.MatchingLabelsSelector{Selector: sel},
	); err != nil {
		h.Log.Error(err, "listing balloons pods", "namespace", h.Namespace)
		return false, fmt.Sprintf("error listing balloons pods in %q", h.Namespace), errPodListRead
	}

	for i := range pods.Items {
		if podReadyOnNode(&pods.Items[i], node) {
			return true, "", nil
		}
	}
	return false, fmt.Sprintf("no ready balloons pod on node %q yet", node), nil
}

// podReadyOnNode is true when the pod is bound to the node, not terminating,
// and PodReady=True.
func podReadyOnNode(pod *corev1.Pod, node string) bool {
	if pod.Spec.NodeName != node {
		return false
	}
	if pod.DeletionTimestamp != nil {
		return false // terminating — do not count it as ready
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// write emits JSON at status; an encode error is only logged, the status is the signal.
func (h *Handler) write(w http.ResponseWriter, status int, res result) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(res); err != nil {
		h.Log.V(1).Info("failed to encode readiness response", "error", err)
	}
}

// Server is a manager.Runnable serving Handler on its own listener; the healthz
// mux cannot take query parameters.
type Server struct {
	// Addr is the bind address (e.g. ":8082").
	Addr string
	// Secure serves TLS 1.3 with the CertDir keypair, reloaded on rotation; false serves plain HTTP.
	Secure  bool
	CertDir string
	TLSOpts []func(*tls.Config)
	Handler *Handler
	Log     logr.Logger
}

// NeedLeaderElection is false: an init container reaching any replica must get
// a verdict, not just the leader.
func (s *Server) NeedLeaderElection() bool { return false }

// Start serves until ctx is cancelled, then shuts the listener down.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle(Path, s.Handler)

	srv := &http.Server{
		Addr:         s.Addr,
		Handler:      mux,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
	}
	if s.Secure {
		cfg, err := s.tlsConfig(ctx)
		if err != nil {
			return err
		}
		srv.TLSConfig = cfg
	}

	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("balloons-ready listener on %s: %w", s.Addr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		s.Log.Info("serving per-node balloons readiness endpoint", "addr", s.Addr, "path", Path, "tls", s.Secure)
		if s.Secure {
			errCh <- srv.ServeTLS(ln, "", "")
			return
		}
		errCh <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		// Bound so manager teardown cannot hang on a slow in-flight request.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// tlsConfig watches the shared serving keypair so renewals apply without a restart.
func (s *Server) tlsConfig(ctx context.Context) (*tls.Config, error) {
	cw, err := certwatcher.New(filepath.Join(s.CertDir, "tls.crt"), filepath.Join(s.CertDir, "tls.key"))
	if err != nil {
		return nil, fmt.Errorf("balloons-ready serving cert in %s: %w", s.CertDir, err)
	}
	go func() {
		if err := cw.Start(ctx); err != nil {
			s.Log.Error(err, "balloons-ready certificate watcher stopped")
		}
	}()
	cfg := &tls.Config{GetCertificate: cw.GetCertificate, NextProtos: []string{"http/1.1"}}
	for _, opt := range s.TLSOpts {
		opt(cfg)
	}
	// TLS 1.3 only, set after TLSOpts so none can lower it: we control both ends.
	cfg.MinVersion = tls.VersionTLS13
	return cfg, nil
}
