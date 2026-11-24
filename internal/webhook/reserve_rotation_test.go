// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// copyCertDir overwrites dst's tls.crt/tls.key/ca.crt with src's, like a cert-manager renewal.
func copyCertDir(t *testing.T, src, dst string) {
	t.Helper()
	for _, name := range []string{defaultCertName, defaultKeyName, caCertName} {
		data, err := os.ReadFile(filepath.Join(src, name)) // #nosec G304 -- test temp dir
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o600); err != nil { // #nosec G703 -- test temp dir
			t.Fatal(err)
		}
	}
}

func leafDER(t *testing.T, certDir string) []byte {
	t.Helper()
	cert, _, err := loadReserveTLS(certDir)
	if err != nil {
		t.Fatal(err)
	}
	return cert.Certificate[0]
}

// TestReserveTransportPicksUpRenewedCerts: a renewal in the cert dir is used by the next
// reserve call on both ends without a restart, for leaf-only and CA + leaf renewals.
func TestReserveTransportPicksUpRenewedCerts(t *testing.T) {
	ekus := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	cases := []struct {
		name  string
		renew func(t *testing.T, ca reserveCA) string
	}{
		{"leaf", func(t *testing.T, ca reserveCA) string {
			return writeReserveCertDir(t, ca, ekus, reserveTestDNSName)
		}},
		{"CA and leaf", func(t *testing.T, _ reserveCA) string {
			return writeReserveCertDir(t, mintReserveCA(t), ekus, reserveTestDNSName)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ca := mintReserveCA(t)
			certDir := writeReserveCertDir(t, ca, ekus, reserveTestDNSName)
			// A follower that never renews.
			stale := t.TempDir()
			copyCertDir(t, certDir, stale)

			store := NewReservationStore(0)
			addr := startAuthority(t, certDir, store)
			host, portStr, _ := net.SplitHostPort(addr)
			port := atoiOrFatal(t, portStr)
			discover := func(context.Context) (string, bool, error) { return host, false, nil }
			c, err := newLeaderReserveClient(certDir, port, NewReservationStore(0), discover, logr.Discard())
			if err != nil {
				t.Fatal(err)
			}
			reserve := func(c *leaderReserveClient, node string) error {
				_, _, _, err := c.reserveBest(context.Background(), "",
					[]ReserveRequest{{Node: node, PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}})
				return err
			}
			if err := reserve(c, "n1"); err != nil {
				t.Fatalf("reserve before renewal: %v", err)
			}

			copyCertDir(t, tc.renew(t, ca), certDir)
			newLeaf := leafDER(t, certDir)
			// Drop keep-alive so the second call does a fresh handshake.
			c.httpc.CloseIdleConnections()

			if err := reserve(c, "n2"); err != nil {
				t.Fatalf("reserve after renewal: %v", err)
			}

			cfg, err := c.tlsConfig()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(cfg.Certificates[0].Certificate[0], newLeaf) {
				t.Error("follower still presents the pre-renewal leaf")
			}

			conn, err := tls.Dial("tcp", addr, authorityProbe(t, certDir))
			if err != nil {
				t.Fatalf("probe after renewal: %v", err)
			}
			got := conn.ConnectionState().PeerCertificates[0].Raw
			_ = conn.Close()
			if !bytes.Equal(got, newLeaf) {
				t.Error("authority still presents the pre-renewal leaf")
			}

			if tc.name != "CA and leaf" {
				return
			}
			// After a CA renewal a follower on the old leaf is refused. It gets the current ca.crt,
			// so the refusal can only come from the authority's client-cert check.
			current, err := os.ReadFile(filepath.Join(certDir, caCertName)) // #nosec G304 -- test temp dir
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stale, caCertName), current, 0o600); err != nil { // #nosec G703 -- test temp dir
				t.Fatal(err)
			}
			old, err := newLeaderReserveClient(stale, port, NewReservationStore(0), discover, logr.Discard())
			if err != nil {
				t.Fatal(err)
			}
			oldCfg, err := old.tlsConfig()
			if err != nil {
				t.Fatal(err)
			}
			serverLeaf, err := x509.ParseCertificate(newLeaf)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := serverLeaf.Verify(x509.VerifyOptions{
				Roots: oldCfg.RootCAs, DNSName: oldCfg.ServerName,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}); err != nil {
				t.Fatalf("stale follower given the current CA does not trust the authority's new leaf: %v", err)
			}
			if err := reserve(old, "n3"); err == nil {
				t.Fatal("a follower on the replaced CA reserved after the CA renewal")
			}
			if r := reservedCores(store, "n3"); len(r) != 0 {
				t.Fatalf("refused call touched the authority store: %v", r)
			}
		})
	}
}

// TestReserveTransportUnreadableCertFailsHandshake: an unreadable cert dir fails the next
// handshake instead of running without an identity.
func TestReserveTransportUnreadableCertFailsHandshake(t *testing.T) {
	certDir := writeReserveCerts(t)
	addr := startAuthority(t, certDir, NewReservationStore(0))
	probe := authorityProbe(t, certDir)
	host, portStr, _ := net.SplitHostPort(addr)
	c, err := newLeaderReserveClient(certDir, atoiOrFatal(t, portStr), NewReservationStore(0),
		func(context.Context) (string, bool, error) { return host, false, nil }, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(certDir, caCertName)); err != nil {
		t.Fatal(err)
	}
	if authorityServing(probe, addr) {
		t.Error("authority completed a handshake with its CA file gone")
	}
	if _, err := c.tlsConfig(); err == nil {
		t.Error("follower built a TLS config with its CA file gone")
	}
}

// recertifyReserveCA issues a renewed CA certificate for ca's existing key, as cert-manager
// does with privateKey.rotationPolicy: Never.
func recertifyReserveCA(t *testing.T, ca reserveCA) reserveCA {
	t.Helper()
	tmpl := *ca.cert
	tmpl.SerialNumber = big.NewInt(3)
	tmpl.NotBefore = time.Now().Add(-time.Minute)
	tmpl.NotAfter = time.Now().Add(2 * time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &ca.key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return reserveCA{cert: cert, der: der, key: ca.key}
}

// TestReserveTransportMixedCAVersionsDuringRenewal: while a renewal reaches the replicas at
// different times, a kept-key CA renewal keeps old and new leaves talking; a re-keyed CA cannot.
func TestReserveTransportMixedCAVersionsDuringRenewal(t *testing.T) {
	ekus := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	cases := []struct {
		name    string
		renewed func(t *testing.T, ca reserveCA) reserveCA
		wantOK  bool
	}{
		{"same key", recertifyReserveCA, true},
		{"new key", func(t *testing.T, _ reserveCA) reserveCA { return mintReserveCA(t) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ca := mintReserveCA(t)
			follower := writeReserveCertDir(t, ca, ekus, reserveTestDNSName)
			authority := writeReserveCertDir(t, tc.renewed(t, ca), ekus, reserveTestDNSName)

			store := NewReservationStore(0)
			addr := startAuthority(t, authority, store)
			host, portStr, _ := net.SplitHostPort(addr)
			c, err := newLeaderReserveClient(follower, atoiOrFatal(t, portStr), NewReservationStore(0),
				func(context.Context) (string, bool, error) { return host, false, nil }, logr.Discard())
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = c.reserveBest(context.Background(), "",
				[]ReserveRequest{{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}})
			if tc.wantOK && err != nil {
				t.Fatalf("follower on the pre-renewal CA could not reserve with an authority on the renewed one: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("a re-keyed CA kept mixed replicas talking; this case documents why the chart keeps the CA key")
			}
		})
	}
}
