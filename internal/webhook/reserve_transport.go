// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/intel/cpu-pinning-operator/internal/metrics"
)

// Followers forward only the reserve decision to the lease holder, so headroom is
// accounted against one store. Transport is a dedicated mTLS port using the serving
// cert as both identities. Followers dial by pod IP, so both ends pin the peer to the
// serving cert's DNS SAN instead of skipping name verification (which would accept
// anything the shared CA signed); do not relax it. Key pair and CA are re-read per
// handshake so cert-manager renewals apply without a restart.

// ReservePath is the internal reserve endpoint.
const ReservePath = "/internal/reserve"

// DefaultReservePort is the dedicated internal mTLS port.
const DefaultReservePort = 9444

const caCertName = "ca.crt"

const reserveRPCTimeout = 2 * time.Second

// warmupTimeout caps the leader-acquisition store warm-up. The reads are cache-backed,
// so this only bounds a pathologically slow apiserver cache.
const warmupTimeout = 10 * time.Second

const (
	reserveReadTimeout  = 3 * time.Second
	reserveWriteTimeout = 3 * time.Second
)

// maxReserveCandidates and maxReserveBodyBytes bound a served reserve request. The
// candidate limit bounds the list walked under the store-wide lock; the byte limit
// must stay above what the candidate limit admits, or a legitimate list would 400.
const (
	maxReserveCandidates = 8192
	maxReserveBodyBytes  = 16 << 20
)

// statusClientClosedRequest is nginx's "client closed request"; net/http has no
// constant. Answered when the follower gave up before the commit.
const statusClientClosedRequest = 499

// reserveIdempotencyHeader carries the per-admission key the authority replays a
// commit under. An over-long key is served without idempotency rather than refused.
const (
	reserveIdempotencyHeader = "Idempotency-Key"
	maxReserveKeyBytes       = 128
)

// reserveResponse names the node and the NUMAs the authority picked and recorded.
type reserveResponse struct {
	Node     string `json:"node"`
	NUMAs    []int  `json:"numas,omitempty"`
	Accepted bool   `json:"accepted"`
}

func loadReserveTLS(certDir string) (tls.Certificate, *x509.CertPool, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(certDir, defaultCertName), filepath.Join(certDir, defaultKeyName))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("loading reserve keypair from %s: %w", certDir, err)
	}
	caPEM, err := os.ReadFile(filepath.Join(certDir, caCertName)) // #nosec G304 -- certDir is a trusted operator-configured mount path; caCertName is a constant
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("reading reserve CA %s: %w", filepath.Join(certDir, caCertName), err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, fmt.Errorf("no CA certificates parsed from %s", filepath.Join(certDir, caCertName))
	}
	return cert, pool, nil
}

// servingCertNames returns the serving cert's DNS SANs. None is a fatal
// misconfiguration, not a reason to degrade to chain-only trust.
func servingCertNames(cert tls.Certificate, log logr.Logger) ([]string, error) {
	leaf := cert.Leaf
	if leaf == nil {
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			// Without a parseable leaf the peer cannot be SAN-pinned; log why setup aborts.
			log.Error(err, "reserve serving certificate leaf failed to parse")
			return nil, fmt.Errorf("parsing reserve serving-cert leaf: %w", err)
		}
		leaf = parsed
	}
	if len(leaf.DNSNames) == 0 {
		log.Error(nil, "reserve serving certificate carries no DNS SANs; peer identity cannot be verified")
		return nil, fmt.Errorf("reserve serving cert has no DNS SANs to verify the peer against")
	}
	return leaf.DNSNames, nil
}

func verifyPeerIdentity(pool *x509.CertPool, name string, log logr.Logger) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		// The TLS stack swallows this error, so log rejections (no cert contents).
		if len(rawCerts) == 0 {
			log.Info("rejecting reserve peer: no certificate presented")
			return fmt.Errorf("reserve peer presented no certificate")
		}
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			c, err := x509.ParseCertificate(raw)
			if err != nil {
				log.Error(err, "rejecting reserve peer: certificate failed to parse")
				return fmt.Errorf("parsing reserve peer certificate: %w", err)
			}
			certs = append(certs, c)
		}
		// DNSName enforces the SAN match on top of the chain. ExtKeyUsageAny: empty
		// KeyUsages means ServerAuth, which would reject a ClientAuth-only client cert.
		opts := x509.VerifyOptions{
			Roots:         pool,
			Intermediates: x509.NewCertPool(),
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			DNSName:       name,
		}
		for _, c := range certs[1:] {
			opts.Intermediates.AddCert(c)
		}
		if _, err := certs[0].Verify(opts); err != nil {
			log.Error(err, "rejecting reserve peer: certificate not trusted", "expectedName", name)
			return fmt.Errorf("reserve peer certificate not trusted for %q: %w", name, err)
		}
		return nil
	}
}

// ReserveAuthority serves the internal authoritative reserve endpoint.
type ReserveAuthority struct {
	Addr    string
	CertDir string
	Store   *ReservationStore
	Log     logr.Logger
	// Warm seeds the reservation set from cluster state before the first reserve.
	// Nil skips it. Start runs once per process, so this cannot seed twice.
	Warm func(ctx context.Context)
}

// NeedLeaderElection reports true (runs only on the lease holder).
func (a *ReserveAuthority) NeedLeaderElection() bool { return true }

// reserveServerTLS builds the authority's config for one handshake from the current
// files in certDir, pinning the peer to name.
func reserveServerTLS(certDir, name string, log logr.Logger) (*tls.Config, error) {
	cert, pool, err := loadReserveTLS(certDir)
	if err != nil {
		log.Error(err, "reserve handshake refused: serving certificate unreadable")
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		// P-384/P-521 first; X25519/P-256 kept for interop.
		CurvePreferences: []tls.CurveID{tls.CurveP384, tls.CurveP521, tls.X25519, tls.CurveP256},
		// Go does not name-check client certs; VerifyPeerCertificate does the chain
		// and SAN check, so without it anything the CA signed could reserve.
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: verifyPeerIdentity(pool, name, log),
		// Resumption would skip VerifyPeerCertificate (gosec G123).
		SessionTicketsDisabled: true,
		NextProtos:             []string{"http/1.1"},
	}, nil
}

// reserveClientTLS builds a follower's config for one dial from the current files in
// certDir, pinning the leader to name.
func reserveClientTLS(certDir, name string) (*tls.Config, error) {
	cert, pool, err := loadReserveTLS(certDir)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		RootCAs:      pool,
		// P-384/P-521 first; X25519/P-256 kept for interop.
		CurvePreferences: []tls.CurveID{tls.CurveP384, tls.CurveP521, tls.X25519, tls.CurveP256},
		// Dial is by pod IP; Go verifies against ServerName, not the dial address.
		ServerName: name,
		// Resumption would skip the authority's peer verification (gosec G123).
		SessionTicketsDisabled: true,
		ClientSessionCache:     nil,
	}, nil
}

// startupServingName loads certDir once to fail setup fast on a missing or unusable
// keypair, and returns the SAN both ends pin for the life of the process.
func startupServingName(certDir string, log logr.Logger) (string, error) {
	cert, _, err := loadReserveTLS(certDir)
	if err != nil {
		return "", err
	}
	names, err := servingCertNames(cert, log)
	if err != nil {
		return "", err
	}
	return names[0], nil
}

// Start serves the reserve endpoint over mTLS until the context is cancelled.
func (a *ReserveAuthority) Start(ctx context.Context) error {
	name, err := startupServingName(a.CertDir, a.Log)
	if err != nil {
		return err
	}
	// GetConfigForClient supplies the real config per handshake.
	tlsCfg := &tls.Config{
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
		NextProtos:             []string{"http/1.1"},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return reserveServerTLS(a.CertDir, name, a.Log)
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc(ReservePath, a.handleReserve)

	srv := &http.Server{
		Addr:         a.Addr,
		Handler:      mux,
		TLSConfig:    tlsCfg,
		ReadTimeout:  reserveReadTimeout,
		WriteTimeout: reserveWriteTimeout,
	}

	// Warm before the socket opens so no reserve is answered from an empty store.
	// Bounded: meanwhile follower commits fail as transport errors (warn/off reserve
	// locally, enforce refuses).
	if a.Warm != nil {
		warmCtx, cancel := context.WithTimeout(ctx, warmupTimeout)
		a.Warm(warmCtx)
		cancel()
	}

	ln, err := net.Listen("tcp", a.Addr)
	if err != nil {
		return fmt.Errorf("reserve authority listener on %s: %w", a.Addr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		a.Log.Info("serving leader-authoritative reserve endpoint", "addr", a.Addr, "path", ReservePath)
		errCh <- srv.ServeTLS(ln, "", "")
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), reserveWriteTimeout)
		defer cancel()
		a.Log.Info("reserve authority stopping (leadership lost or shutdown)")
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func decodeReserveRequests(w http.ResponseWriter, r *http.Request) ([]ReserveRequest, error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReserveBodyBytes))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('[') {
		return nil, fmt.Errorf("want a candidate array, got %v", tok)
	}
	var reqs []ReserveRequest
	for dec.More() {
		if len(reqs) == maxReserveCandidates {
			return nil, fmt.Errorf("more than %d candidates", maxReserveCandidates)
		}
		var req ReserveRequest
		if err := dec.Decode(&req); err != nil {
			return nil, err
		}
		reqs = append(reqs, req)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
		return nil, fmt.Errorf("unterminated candidate array")
	}
	return reqs, nil
}

func (a *ReserveAuthority) handleReserve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reqs, err := decodeReserveRequests(w, r)
	if err != nil {
		http.Error(w, fmt.Sprintf("decoding reserve request: %v", err), http.StatusBadRequest)
		return
	}
	// If the client already gave up, skip the commit (see statusClientClosedRequest).
	if err := r.Context().Err(); err != nil {
		http.Error(w, "client gone before commit", statusClientClosedRequest)
		return
	}
	key := r.Header.Get(reserveIdempotencyHeader)
	if len(key) > maxReserveKeyBytes {
		key = ""
	}
	node, numas, accepted := a.Store.authoritativeReserveBest(key, reqs)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(reserveResponse{Node: node, NUMAs: numas, Accepted: accepted}); err != nil {
		a.Log.V(1).Info("failed to encode reserve response", "error", err)
	}
}

// reserveClient commits a reservation for the best candidate that fits, either against
// the in-process store or by forwarding the candidates to the lease holder.
type reserveClient interface {
	// reserveBest returns the node and NUMAs the reservation was recorded against. err
	// is non-nil for anything that is not a verdict; accepted=false with err=nil is a
	// definite no-fit. Retrying with the same key is safe.
	reserveBest(ctx context.Context, key string, reqs []ReserveRequest) (node string, numas []int, accepted bool, err error)
}

type localReserveClient struct{ store *ReservationStore }

// reserveBest ignores the key: an in-process reserve cannot lose its own reply.
func (c localReserveClient) reserveBest(_ context.Context, _ string, reqs []ReserveRequest) (string, []int, bool, error) {
	node, numas, accepted := c.store.authoritativeReserveBest("", reqs)
	return node, numas, accepted, nil
}

// leaderReserveClient forwards reserves to the current lease holder.
type leaderReserveClient struct {
	discover func(ctx context.Context) (ip string, isSelf bool, err error)
	local    *ReservationStore
	port     int
	httpc    *http.Client
	// tlsConfig builds the mTLS config for one dial from the current cert dir.
	tlsConfig func() (*tls.Config, error)
	log       logr.Logger
}

// NewLeaderReserveClient constructs the HA leader-forwarding reserve client.
func NewLeaderReserveClient(certDir string, port int, local *ReservationStore, discover func(context.Context) (string, bool, error), log logr.Logger) (reserveClient, error) {
	return newLeaderReserveClient(certDir, port, local, discover, log)
}

func newLeaderReserveClient(certDir string, port int, local *ReservationStore, discover func(context.Context) (string, bool, error), log logr.Logger) (*leaderReserveClient, error) {
	name, err := startupServingName(certDir, log)
	if err != nil {
		return nil, err
	}
	tlsConfig := func() (*tls.Config, error) { return reserveClientTLS(certDir, name) }
	return &leaderReserveClient{
		discover: discover,
		local:    local,
		port:     port,
		httpc: &http.Client{
			Timeout: reserveRPCTimeout,
			Transport: &http.Transport{
				// DialTLSContext so each new connection uses the current cert files.
				DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					cfg, err := tlsConfig()
					if err != nil {
						return nil, err
					}
					return (&tls.Dialer{Config: cfg}).DialContext(ctx, network, addr)
				},
				TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
			},
		},
		tlsConfig: tlsConfig,
		log:       log,
	}, nil
}

func (c *leaderReserveClient) reserveBest(ctx context.Context, key string, reqs []ReserveRequest) (string, []int, bool, error) {
	ip, isSelf, err := c.discover(ctx)
	if err != nil {
		return "", nil, false, fmt.Errorf("discovering reserve leader: %w", err)
	}
	if isSelf {
		// No round trip, so no reply to lose: reserve without a ledger entry.
		node, numas, accepted := c.local.authoritativeReserveBest("", reqs)
		return node, numas, accepted, nil
	}
	body, err := json.Marshal(reqs)
	if err != nil {
		return "", nil, false, fmt.Errorf("marshaling reserve request: %w", err)
	}
	url := fmt.Sprintf("https://%s%s", net.JoinHostPort(ip, fmt.Sprintf("%d", c.port)), ReservePath)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", nil, false, fmt.Errorf("building reserve request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if key != "" {
		httpReq.Header.Set(reserveIdempotencyHeader, key)
	}
	resp, err := c.httpc.Do(httpReq)
	if err != nil {
		return "", nil, false, fmt.Errorf("reserve RPC to leader %s: %w", ip, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.log.V(1).Info("closing reserve response body", "leader", ip, "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return "", nil, false, fmt.Errorf("reserve RPC to leader %s: status %d", ip, resp.StatusCode)
	}
	var rr reserveResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return "", nil, false, fmt.Errorf("decoding reserve response from leader %s: %w", ip, err)
	}
	// A node outside the candidates would pin the pod where it cannot run; treat
	// it as a failed RPC.
	if rr.Accepted && !requestedNode(reqs, rr.Node) {
		return "", nil, false, fmt.Errorf("reserve RPC to leader %s: committed node %q is not one of the candidates sent", ip, rr.Node)
	}
	return rr.Node, rr.NUMAs, rr.Accepted, nil
}

// reservePolicy is what an admission needs to decide about committing.
type reservePolicy struct {
	dryRun bool   // skip the commit, per sideEffects: None
	key    string // idempotency key
	mode   string // capacityCheck.mode, used as the metric label
	// failClosed refuses a commit the authority cannot take instead of reserving locally.
	failClosed bool
}

// reserveKey identifies one admission attempt by admission UID (pod.UID is empty on
// CREATE). No admission UID means no key: a shared constant would replay one pod's node.
func reserveKey(pod *corev1.Pod, req admission.Request) string {
	if req.UID == "" {
		return ""
	}
	return string(pod.UID) + "/" + string(req.UID)
}

// commitReserve commits the candidates: accepted on a node and NUMA set, a definite
// no-fit, or unaccountable. A seam error retries on the same key so a lost reply returns
// the original commit; then failClosed refuses and warn/off reserve locally.
func (m *PodMutator) commitReserve(ctx context.Context, pod *corev1.Pod, rp reservePolicy, reqs []ReserveRequest, log logr.Logger) (node string, numas []int, accepted, unaccountable bool) {
	c := m.reserveClientOrLocal()
	node, numas, accepted, err := c.reserveBest(ctx, rp.key, reqs)
	if err == nil {
		return node, numas, accepted, false
	}
	first := err
	node, numas, accepted, err = c.reserveBest(ctx, rp.key, reqs)
	if err == nil {
		return node, numas, accepted, false
	}
	log.Info("reserve authority unavailable on both attempts",
		"error", err.Error(), "firstError", first.Error(), "failClosed", rp.failClosed)
	if rp.failClosed {
		return "", nil, false, true
	}
	metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, reasonUnaccountable, rp.mode).Inc()
	node, numas, accepted = m.Reservations.authoritativeReserveBest("", reqs)
	return node, numas, accepted, false
}

// requestedNode reports whether node is one of the candidates in reqs.
func requestedNode(reqs []ReserveRequest, node string) bool {
	for i := range reqs {
		if reqs[i].Node == node {
			return true
		}
	}
	return false
}
