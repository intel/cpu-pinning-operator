// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// mintCertDir writes a cert-manager-shaped dir with a CA and a localhost leaf; returns the dir and CA PEM.
func mintCertDir(t *testing.T) (string, []byte) {
	const dnsName = "localhost"
	t.Helper()
	newKey := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	caKey, leafKey := newKey(), newKey()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "readiness-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{dnsName},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	dir := t.TempDir()
	for name, data := range map[string][]byte{
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		"ca.crt":  caPEM,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, caPEM
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

func pollOnce(t *testing.T, c *http.Client, endpoint string, timeout time.Duration) error {
	t.Helper()
	cfg := PollConfig{Endpoint: endpoint, Node: "node-1", Interval: 20 * time.Millisecond, Timeout: timeout}
	return Poll(context.Background(), c, cfg, io.Discard)
}

// startServer runs srv until the test ends.
func startServer(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Start returned %v after cancel, want nil", err)
		}
	})
}

// writeCA writes pem as the client's CA file.
func writeCA(t *testing.T, path string, pem []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestServerTLSVerification proves the poller trusts only the right CA for the right server name.
func TestServerTLSVerification(t *testing.T) {
	dir, caPEM := mintCertDir(t)
	_, otherCA := mintCertDir(t)
	port := freePort(t)
	startServer(t, &Server{
		Addr:    "127.0.0.1:" + port,
		Secure:  true,
		CertDir: dir,
		// A TLS 1.2 floor the server must override, and a curve list proving TLSOpts apply.
		TLSOpts: []func(*tls.Config){func(c *tls.Config) {
			c.MinVersion = tls.VersionTLS12
			c.CurvePreferences = []tls.CurveID{tls.CurveP384}
		}},
		Handler: newHandler(t, balloonsDaemonSet(), balloonsPod("node-1", true, false)),
		Log:     logr.Discard(),
	})

	client := func(t *testing.T, endpoint string, ca []byte) *http.Client {
		t.Helper()
		caFile := filepath.Join(t.TempDir(), CAKey)
		writeCA(t, caFile, ca)
		c, err := Client(endpoint, caFile)
		if err != nil {
			t.Fatalf("Client(%s): %v", endpoint, err)
		}
		return c
	}
	good := "https://localhost:" + port

	// Runs first so the failures below are not "not listening yet".
	t.Run("right CA and name pass", func(t *testing.T) {
		if err := pollOnce(t, client(t, good, caPEM), good, 5*time.Second); err != nil {
			t.Fatalf("Poll over TLS with the serving CA: %v", err)
		}
	})
	// A spoofed "ready" must never be believed, so each case also proves Poll fails.
	verifyFails := func(t *testing.T, endpoint string, ca []byte) {
		t.Helper()
		c := client(t, endpoint, ca)
		resp, err := c.Get(endpointURL(endpoint, "node-1"))
		if err == nil {
			_ = resp.Body.Close()
		}
		var verr *tls.CertificateVerificationError
		if !errors.As(err, &verr) {
			t.Fatalf("GET %s = %v, want a certificate verification error", endpoint, err)
		}
		if err := pollOnce(t, c, endpoint, 200*time.Millisecond); err == nil {
			t.Fatalf("Poll %s returned ready over an unverified connection", endpoint)
		}
	}
	t.Run("wrong CA fails", func(t *testing.T) { verifyFails(t, good, otherCA) })
	t.Run("wrong name fails", func(t *testing.T) {
		// The leaf names localhost only; dialing the IP must not verify.
		verifyFails(t, "https://127.0.0.1:"+port, caPEM)
	})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	t.Run("TLS 1.2 client refused despite a 1.2 TLSOpts floor", func(t *testing.T) {
		conn, err := tls.Dial("tcp", "127.0.0.1:"+port,
			&tls.Config{RootCAs: pool, ServerName: "localhost", MaxVersion: tls.VersionTLS12}) // #nosec G402 -- deliberately below the server's forced MinVersion 1.3
		if err == nil {
			_ = conn.Close()
			t.Fatal("a TLS 1.2 client was served; the readiness server must be TLS 1.3 only")
		}
	})
	t.Run("TLSOpts applied", func(t *testing.T) {
		conn, err := tls.Dial("tcp", "127.0.0.1:"+port,
			&tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13, CurvePreferences: []tls.CurveID{tls.X25519}})
		if err == nil {
			_ = conn.Close()
			t.Fatal("an X25519-only client was served although TLSOpts limited the curves to P-384")
		}
	})
}

// TestClientRequiresTLS13: the readiness client refuses a server capped at TLS 1.2.
func TestClientRequiresTLS13(t *testing.T) {
	dir, caPEM := mintCertDir(t)
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{ // #nosec G402 -- deliberately capped at TLS 1.2 to prove the client refuses it
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	endpoint := "https://localhost:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	caFile := filepath.Join(t.TempDir(), CAKey)
	writeCA(t, caFile, caPEM)
	c, err := Client(endpoint, caFile)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(endpointURL(endpoint, "node-1"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the readiness client completed a TLS 1.2 handshake; it must require TLS 1.3")
	}
	if !strings.Contains(err.Error(), "protocol version") {
		t.Errorf("GET against a TLS 1.2 server = %v, want a protocol version error", err)
	}
}

// TestClientFollowsCAFile proves the CA file is re-read per request: late files and rotation work.
func TestClientFollowsCAFile(t *testing.T) {
	dir, caPEM := mintCertDir(t)
	_, otherCA := mintCertDir(t)
	port := freePort(t)
	startServer(t, &Server{
		Addr: "127.0.0.1:" + port, Secure: true, CertDir: dir,
		Handler: newHandler(t, balloonsDaemonSet(), balloonsPod("node-1", true, false)),
		Log:     logr.Discard(),
	})
	endpoint := "https://localhost:" + port
	caFile := filepath.Join(t.TempDir(), CAKey)
	c, err := Client(endpoint, caFile)
	if err != nil {
		t.Fatal(err)
	}

	// No file yet: the poll must keep retrying, then pass once it appears.
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.WriteFile(caFile, caPEM, 0o600)
	}()
	if err := pollOnce(t, c, endpoint, 5*time.Second); err != nil {
		t.Fatalf("Poll with a CA file written after start: %v", err)
	}

	writeCA(t, caFile, otherCA)
	if err := pollOnce(t, c, endpoint, 200*time.Millisecond); err == nil {
		t.Fatal("Poll still passed after the CA file changed to a CA that does not sign the server")
	}
	writeCA(t, caFile, caPEM)
	if err := pollOnce(t, c, endpoint, 5*time.Second); err != nil {
		t.Fatalf("Poll after the CA file changed back: %v", err)
	}
}

// TestServerPlaintextOptOut: Secure=false is the explicit plaintext fallback.
func TestServerPlaintextOptOut(t *testing.T) {
	port := freePort(t)
	startServer(t, &Server{
		Addr:    "127.0.0.1:" + port,
		Handler: newHandler(t, balloonsDaemonSet(), balloonsPod("node-1", true, false)),
		Log:     logr.Discard(),
	})
	plain := "http://127.0.0.1:" + port
	c, err := Client(plain, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := pollOnce(t, c, plain, 5*time.Second); err != nil {
		t.Fatalf("Poll over plaintext: %v", err)
	}
}

// TestServerTLSNeedsCert proves a TLS listener without a keypair fails Start.
func TestServerTLSNeedsCert(t *testing.T) {
	srv := &Server{Addr: "127.0.0.1:" + freePort(t), Secure: true, CertDir: t.TempDir(), Handler: &Handler{}, Log: logr.Discard()}
	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("Start with an empty cert dir returned nil, want an error")
	}
}

// TestClientNeverDowngrades proves https needs a valid CA file; http keeps the default client.
func TestClientNeverDowngrades(t *testing.T) {
	if _, err := Client("https://mgr.ns.svc:8082", ""); err == nil {
		t.Error("Client(https, no CA file) returned nil error")
	}
	garbage := filepath.Join(t.TempDir(), CAKey)
	writeCA(t, garbage, []byte("not a pem"))
	c, err := Client("https://mgr.ns.svc:8082", garbage)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := c.Get("https://mgr.ns.svc:8082" + Path); err == nil || !strings.Contains(err.Error(), "no PEM CA") {
		if err == nil {
			_ = resp.Body.Close()
		}
		t.Errorf("GET with a garbage CA = %v, want a no-PEM-CA error", err)
	}
	if c, err := Client("http://mgr.ns.svc:8082", ""); err != nil || c != http.DefaultClient {
		t.Errorf("Client(http) = %v, %v; want http.DefaultClient", c, err)
	}

	err = RunPoller([]string{"--endpoint=https://mgr.ns.svc:8082", "--node=n1"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--ca-file") {
		t.Errorf("RunPoller(https, no --ca-file) = %v, want an error naming the flag", err)
	}
}
