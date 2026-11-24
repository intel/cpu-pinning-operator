// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// reserveCA holds a self-signed CA and key so several leaves can share one trust root.
type reserveCA struct {
	cert *x509.Certificate
	der  []byte
	key  *ecdsa.PrivateKey
}

// mintReserveCA creates a self-signed CA usable to sign reserve leaf certs.
func mintReserveCA(t *testing.T) reserveCA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "reserve-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	return reserveCA{cert: caCert, der: caDER, key: caKey}
}

// reserveTestDNSName is the serving-cert SAN every replica presents and verifies,
// standing in for the chart's `<fullname>-webhook-service.<ns>.svc`.
const reserveTestDNSName = "cpu-pinning-operator-webhook-service.test.svc"

// writeReserveCertDir mints a leaf signed by ca with the given EKUs and SANs and
// writes tls.crt/tls.key/ca.crt into a temp dir shaped like the cert-manager secret.
func writeReserveCertDir(t *testing.T, ca reserveCA, ekus []x509.ExtKeyUsage, dnsNames ...string) string {
	t.Helper()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "reserve-test-replica"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  ekus,
		DNSNames:     dnsNames,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca.cert, &leafKey.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	writePEM := func(name, typ string, der []byte) {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(defaultCertName, "CERTIFICATE", leafDER)
	writePEM(defaultKeyName, "EC PRIVATE KEY", leafKeyDER)
	writePEM(caCertName, "CERTIFICATE", ca.der)
	return dir
}

// writeReserveCerts mints a CA and one leaf used as both server and client identity,
// as in production, and writes them like writeReserveCertDir.
func writeReserveCerts(t *testing.T) string {
	t.Helper()
	return writeReserveCertDir(t, mintReserveCA(t),
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, reserveTestDNSName)
}

// freeLoopbackAddr returns a loopback address from an ephemeral port it closes
// again. The kernel tears the listener down asynchronously, so a TCP connect may
// still succeed for a while; use authorityServing to tell if an authority serves it.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// authorityProbeTimeout bounds one handshake probe, far above a loopback handshake
// so a failure means "not serving".
const authorityProbeTimeout = time.Second

// authorityProbe builds the TLS config a legitimate follower uses for certDir's
// identity, with ServerName pinned to the serving-cert SAN as reserve_transport.go does.
func authorityProbe(t *testing.T, certDir string) *tls.Config {
	t.Helper()
	cert, pool, err := loadReserveTLS(certDir)
	if err != nil {
		t.Fatal(err)
	}
	names, err := servingCertNames(cert, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   names[0],
	}
}

// authorityServing reports whether the reserve authority answers a verified
// follower handshake at addr. A bare TCP dial cannot prove the negative that
// TestReserveAuthorityWarmsBeforeItServes needs (see freeLoopbackAddr).
func authorityServing(probe *tls.Config, addr string) bool {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: authorityProbeTimeout}, "tcp", addr, probe)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// startAuthority runs a ReserveAuthority on an ephemeral port and returns its
// address, stopping it via t.Cleanup.
func startAuthority(t *testing.T, certDir string, store *ReservationStore) string {
	t.Helper()
	addr := freeLoopbackAddr(t)
	a := &ReserveAuthority{Addr: addr, CertDir: certDir, Store: store, Log: logr.Discard()}
	runAuthority(t, a)
	return awaitAuthorityTLS(t, certDir, addr)
}

// runAuthority starts an authority in the background and stops it via t.Cleanup.
func runAuthority(t *testing.T, a *ReserveAuthority) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Start(ctx); err != nil {
			t.Logf("authority stopped: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// awaitAuthorityTLS blocks until the authority at addr answers a TLS handshake, and
// returns addr.
func awaitAuthorityTLS(t *testing.T, certDir, addr string) string {
	t.Helper()
	// A TCP dial succeeds against the listen backlog before ServeTLS accepts; only a
	// real handshake avoids the first request racing startup.
	probe := authorityProbe(t, certDir)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if authorityServing(probe, addr) {
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("authority did not start serving TLS in time")
	return ""
}

// TestReserveRoundTripMTLS proves a follower forwards over mTLS to a real authority:
// the first reserve is accepted, the second for the same headroom rejected.
func TestReserveRoundTripMTLS(t *testing.T) {
	certDir := writeReserveCerts(t)
	store := NewReservationStore(0)
	addr := startAuthority(t, certDir, store)

	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	host, _, _ := net.SplitHostPort(addr)
	port := atoiOrFatal(t, portStr)

	follower := NewReservationStore(0)
	c, err := newLeaderReserveClient(certDir, port, follower,
		func(context.Context) (string, bool, error) { return host, false, nil }, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}

	req := ReserveRequest{Node: "n1", PerNUMACores: 2, Span: 2, RawFree: map[int]int{0: 2, 1: 2}}
	node, _, ok, err := c.reserveBest(context.Background(), "", []ReserveRequest{req})
	if err != nil {
		t.Fatalf("first reserve RPC failed: %v", err)
	}
	if !ok || node != "n1" {
		t.Fatalf("first reserve must be accepted over the wire on n1, got (%q, %v)", node, ok)
	}
	node, _, ok, err = c.reserveBest(context.Background(), "", []ReserveRequest{req})
	if err != nil {
		t.Fatalf("second reserve RPC failed: %v", err)
	}
	if ok || node != "" {
		t.Fatalf("second reserve for the same headroom must be REJECTED by the authority, got (%q, %v)", node, ok)
	}
	// The reservation lives on the authority, not the follower.
	if r := reservedCores(store, "n1"); r[0] != 2 || r[1] != 2 {
		t.Fatalf("authority store = %v, want 2 on each NUMA", r)
	}
	if r := reservedCores(follower, "n1"); len(r) != 0 {
		t.Fatalf("follower store must be untouched (reserve was forwarded), got %v", r)
	}
}

// TestReserveClientCurvePreferences asserts the follower's mTLS client pins the
// curve order P-384, P-521, X25519, P-256.
func TestReserveClientCurvePreferences(t *testing.T) {
	certDir := writeReserveCerts(t)
	c, err := newLeaderReserveClient(certDir, 9444, NewReservationStore(0),
		func(context.Context) (string, bool, error) { return "127.0.0.1", false, nil }, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := c.tlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.CurvePreferences
	want := []tls.CurveID{tls.CurveP384, tls.CurveP521, tls.X25519, tls.CurveP256}
	if len(got) != len(want) {
		t.Fatalf("CurvePreferences = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CurvePreferences = %v, want %v", got, want)
		}
	}
}

// TestReserveRoundTripUntrustedClientRejected proves a client cert from a different
// CA fails the handshake (local fallback), not reserving.
func TestReserveRoundTripUntrustedClientRejected(t *testing.T) {
	serverCerts := writeReserveCerts(t)
	store := NewReservationStore(0)
	addr := startAuthority(t, serverCerts, store)
	host, portStr, _ := net.SplitHostPort(addr)
	port := atoiOrFatal(t, portStr)

	otherCerts := writeReserveCerts(t)
	c, err := newLeaderReserveClient(otherCerts, port, NewReservationStore(0),
		func(context.Context) (string, bool, error) { return host, false, nil }, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	_, _, ok, err := c.reserveBest(context.Background(), "", []ReserveRequest{{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}})
	if err == nil {
		t.Fatal("an untrusted client must fail the mTLS handshake (transport error), got nil")
	}
	if ok {
		t.Fatal("an untrusted client must not get an accepted reservation")
	}
	if r := reservedCores(store, "n1"); len(r) != 0 {
		t.Fatalf("untrusted call must not touch the authority store, got %v", r)
	}
}

// TestReserveRoundTripClientCertNoServerAuthAccepted proves a ClientAuth-only
// client cert is accepted. An empty KeyUsages would make x509.Verify require
// ServerAuth; the transport sets ExtKeyUsageAny.
func TestReserveRoundTripClientCertNoServerAuthAccepted(t *testing.T) {
	ca := mintReserveCA(t)
	serverCerts := writeReserveCertDir(t, ca,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, reserveTestDNSName)
	// Client leaf with ClientAuth only, no ServerAuth.
	clientCerts := writeReserveCertDir(t, ca, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, reserveTestDNSName)

	store := NewReservationStore(0)
	addr := startAuthority(t, serverCerts, store)
	host, portStr, _ := net.SplitHostPort(addr)
	port := atoiOrFatal(t, portStr)

	c, err := newLeaderReserveClient(clientCerts, port, NewReservationStore(0),
		func(context.Context) (string, bool, error) { return host, false, nil }, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	req := ReserveRequest{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}
	node, _, ok, err := c.reserveBest(context.Background(), "", []ReserveRequest{req})
	if err != nil {
		t.Fatalf("a client cert chaining to the CA (ClientAuth-only EKU) must be accepted, got error: %v", err)
	}
	if !ok || node != "n1" {
		t.Fatalf("reserve over the wire must succeed for a CA-chained client, got (%q, %v)", node, ok)
	}
	if r := reservedCores(store, "n1"); r[0] != 1 {
		t.Fatalf("authority store = %v, want 1 core reserved on NUMA 0", r)
	}
}

// TestReserveAuthorityRejectsWrongSANClient proves the authority rejects a client
// cert from the shared CA with a different Service SAN (a rogue pod from the same
// Issuer). The attacker verifies the server correctly, so only the authority's SAN
// check can stop it.
func TestReserveAuthorityRejectsWrongSANClient(t *testing.T) {
	ca := mintReserveCA(t)
	serverCerts := writeReserveCertDir(t, ca,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, reserveTestDNSName)
	store := NewReservationStore(0)
	addr := startAuthority(t, serverCerts, store)

	rogueDir := writeReserveCertDir(t, ca,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, "rogue-workload.other.svc")
	rogueCert, caPool, err := loadReserveTLS(rogueDir)
	if err != nil {
		t.Fatal(err)
	}
	// The attacker trusts the real authority.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{rogueCert},
		RootCAs:      caPool,
		ServerName:   reserveTestDNSName,
		MinVersion:   tls.VersionTLS13,
	}}}
	body, err := json.Marshal([]ReserveRequest{{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Post("https://"+addr+ReservePath, "application/json", bytes.NewReader(body))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a same-CA client with a mismatched SAN must fail the mTLS handshake, got status %d", resp.StatusCode)
	}
	if r := reservedCores(store, "n1"); len(r) != 0 {
		t.Fatalf("impersonating client must not touch the authority store, got %v", r)
	}
}

// TestFollowerRejectsWrongSANLeader proves a follower rejects a leader whose cert
// chains to the shared CA but has a different SAN, and falls back locally. The rogue
// leader accepts any client cert and always says "accepted", so the follower's
// ServerName pin is the only gate.
func TestFollowerRejectsWrongSANLeader(t *testing.T) {
	ca := mintReserveCA(t)
	// Rogue leader: same CA, foreign SAN.
	rogueServer := writeReserveCertDir(t, ca,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, "rogue-workload.other.svc")
	rogueCert, _, err := loadReserveTLS(rogueServer)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reserveResponse{Node: "n1", Accepted: true})
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{rogueCert},
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.RequireAnyClientCert, // accept any client cert — a malicious leader does no SAN check
		NextProtos:   []string{"http/1.1"},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port := atoiOrFatal(t, portStr)

	legit := writeReserveCertDir(t, ca,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, reserveTestDNSName)
	c, err := newLeaderReserveClient(legit, port, NewReservationStore(0),
		func(context.Context) (string, bool, error) { return host, false, nil }, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	_, _, ok, err := c.reserveBest(context.Background(), "", []ReserveRequest{{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}})
	if err == nil {
		t.Fatal("a rogue leader with a mismatched SAN must fail the mTLS handshake (transport error → local fallback), got nil")
	}
	if ok {
		t.Fatal("a rogue leader must not yield an accepted reservation")
	}
}

// TestFollowerRejectsCommitOutsideCandidates proves a committed node the follower
// never offered surfaces as a failed RPC, not an accepted commit: the pod would be
// pinned where it cannot run.
func TestFollowerRejectsCommitOutsideCandidates(t *testing.T) {
	for _, answer := range []string{"", "not-a-candidate"} {
		t.Run("answer="+answer, func(t *testing.T) {
			ca := mintReserveCA(t)
			certDir := writeReserveCertDir(t, ca,
				[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, reserveTestDNSName)
			cert, _, err := loadReserveTLS(certDir)
			if err != nil {
				t.Fatal(err)
			}
			// A legitimate leader answering with a node outside the candidates.
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(reserveResponse{Node: answer, Accepted: true})
			}))
			srv.TLS = &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS13,
				ClientAuth:   tls.RequireAnyClientCert,
				NextProtos:   []string{"http/1.1"},
			}
			srv.StartTLS()
			host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
			c, err := newLeaderReserveClient(certDir, atoiOrFatal(t, portStr), NewReservationStore(0),
				func(context.Context) (string, bool, error) { return host, false, nil }, logr.Discard())
			if err != nil {
				srv.Close()
				t.Fatal(err)
			}
			node, _, ok, err := c.reserveBest(context.Background(), "",
				[]ReserveRequest{{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}})
			srv.Close()
			if err == nil {
				t.Fatalf("leader answered node %q (not a candidate) — want a transport error so the caller falls back locally, got nil", answer)
			}
			if !strings.Contains(err.Error(), "not one of the candidates sent") {
				t.Errorf("error = %v, want the candidate-list rejection (a TLS/decode failure would pass this test for the wrong reason)", err)
			}
			if ok || node != "" {
				t.Errorf("leader answering node %q yielded (%q, %v), want no commit", answer, node, ok)
			}
		})
	}
}

func atoiOrFatal(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("non-numeric port %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
