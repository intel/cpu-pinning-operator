// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"

	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/readiness"
	podwebhook "github.com/intel/cpu-pinning-operator/internal/webhook"
)

// TestBindFlagsZapWiring verifies the zap flags the chart's log values feed reach the zap Options.
func TestBindFlagsZapWiring(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		devel   bool
		wantLvl zapcore.Level // only checked when a level flag is set
		hasLvl  bool
	}{
		{
			name:  "defaults leave zap options unset",
			args:  nil,
			devel: false,
		},
		{
			name:    "log.level=debug sets debug verbosity",
			args:    []string{"--zap-log-level=debug"},
			wantLvl: zapcore.DebugLevel,
			hasLvl:  true,
		},
		{
			name:    "log.level=error raises the threshold",
			args:    []string{"--zap-log-level=error"},
			wantLvl: zapcore.ErrorLevel,
			hasLvl:  true,
		},
		{
			name:  "log.development=true enables development mode",
			args:  []string{"--zap-devel=true"},
			devel: true,
		},
		{
			name:    "both flags together (chart debug+development)",
			args:    []string{"--zap-log-level=debug", "--zap-devel=true"},
			devel:   true,
			wantLvl: zapcore.DebugLevel,
			hasLvl:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet(tc.name, flag.ContinueOnError)
			cfg := bindFlags(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}

			if cfg.zapOpts.Development != tc.devel {
				t.Errorf("Development = %v, want %v", cfg.zapOpts.Development, tc.devel)
			}

			if tc.hasLvl {
				if cfg.zapOpts.Level == nil {
					t.Fatalf("Level is nil, want a level enabler for %v", tc.wantLvl)
				}
				// Anything at or above the configured level is enabled.
				if !cfg.zapOpts.Level.Enabled(tc.wantLvl) {
					t.Errorf("Level does not enable %v", tc.wantLvl)
				}
				// One level below the threshold must be filtered out.
				if cfg.zapOpts.Level.Enabled(tc.wantLvl - 1) {
					t.Errorf("Level unexpectedly enables %v (below the configured threshold)", tc.wantLvl-1)
				}
			} else if cfg.zapOpts.Level != nil {
				t.Errorf("Level = %v, want nil (no --zap-log-level given)", cfg.zapOpts.Level)
			}
		})
	}
}

// TestMetricsServerOptions verifies metrics default to TLS with the serving-cert dir, and
// the plaintext fallback leaves SecureServing off.
func TestMetricsServerOptions(t *testing.T) {
	const certDir = "/tmp/k8s-webhook-server/serving-certs"

	t.Run("secure by default serves TLS from the cert dir", func(t *testing.T) {
		fs := flag.NewFlagSet("secure-default", flag.ContinueOnError)
		cfg := bindFlags(fs)
		if err := fs.Parse([]string{"--cert-dir=" + certDir}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !cfg.metricsSecure {
			t.Fatal("metrics-secure defaulted to false; the endpoint would serve plaintext HTTP")
		}
		opts := metricsServerOptions(cfg)
		if !opts.SecureServing {
			t.Error("SecureServing = false with metrics-secure default on; metrics would be served in the clear")
		}
		if opts.CertDir != certDir {
			t.Errorf("CertDir = %q, want %q (the mounted serving-cert dir)", opts.CertDir, certDir)
		}
		if opts.BindAddress != cfg.metricsAddr {
			t.Errorf("BindAddress = %q, want %q", opts.BindAddress, cfg.metricsAddr)
		}
		// TLSOpts must carry hardenTLS, not inherit the crypto/tls default.
		if len(opts.TLSOpts) == 0 {
			t.Fatal("secure metrics server has no TLSOpts; MinVersion/cipher policy left implicit")
		}
		cfgTLS := &tls.Config{}
		for _, op := range opts.TLSOpts {
			op(cfgTLS)
		}
		if cfgTLS.MinVersion != tls.VersionTLS12 {
			t.Errorf("metrics TLS MinVersion = %#x, want TLS 1.2 (%#x)", cfgTLS.MinVersion, tls.VersionTLS12)
		}
		// Must be controller-runtime's delegating authn/authz filter, not a weaker wrapper.
		if !cfg.metricsRequireAuth {
			t.Fatal("metrics-require-auth defaulted to false; the endpoint would accept unauthenticated scrapes")
		}
		if opts.FilterProvider == nil {
			t.Fatal("secure metrics server has no FilterProvider; /metrics would accept any client that can reach it")
		}
		if got, want := funcPointer(opts.FilterProvider), funcPointer(filters.WithAuthenticationAndAuthorization); got != want {
			t.Errorf("FilterProvider = %#x, want filters.WithAuthenticationAndAuthorization (%#x)", got, want)
		}
	})

	t.Run("auth opt-out keeps TLS and only drops the client check", func(t *testing.T) {
		// The auth opt-out must drop only the client check, never TLS.
		fs := flag.NewFlagSet("no-auth", flag.ContinueOnError)
		cfg := bindFlags(fs)
		if err := fs.Parse([]string{"--metrics-require-auth=false", "--cert-dir=" + certDir}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		opts := metricsServerOptions(cfg)
		if opts.FilterProvider != nil {
			t.Error("FilterProvider wired with --metrics-require-auth=false")
		}
		if !opts.SecureServing {
			t.Error("SecureServing = false with --metrics-require-auth=false; the auth opt-out must not drop TLS")
		}
		if opts.CertDir != certDir {
			t.Errorf("CertDir = %q, want %q; the auth opt-out must not drop the serving cert", opts.CertDir, certDir)
		}
		if len(opts.TLSOpts) == 0 {
			t.Error("TLSOpts empty with --metrics-require-auth=false; the TLS floor must still be pinned")
		}
	})

	t.Run("explicit opt-out falls back to plaintext without a cert dir", func(t *testing.T) {
		fs := flag.NewFlagSet("insecure", flag.ContinueOnError)
		cfg := bindFlags(fs)
		if err := fs.Parse([]string{"--metrics-secure=false", "--cert-dir=" + certDir}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		opts := metricsServerOptions(cfg)
		if opts.SecureServing {
			t.Error("SecureServing = true with --metrics-secure=false")
		}
		// Plaintext mode must not present the serving cert.
		if opts.CertDir != "" {
			t.Errorf("CertDir = %q, want empty in plaintext mode", opts.CertDir)
		}
		// No TLS to harden in plaintext mode.
		if len(opts.TLSOpts) != 0 {
			t.Errorf("plaintext metrics server carries %d TLSOpts, want 0", len(opts.TLSOpts))
		}
		// require-auth defaults true but must not wire the filter on plaintext: the
		// bearer token would cross the wire in the clear.
		if !cfg.metricsRequireAuth {
			t.Fatal("test no longer exercises the default metrics-require-auth=true against plaintext")
		}
		if opts.FilterProvider != nil {
			t.Error("plaintext metrics server wired a token-authenticating FilterProvider; tokens would travel in the clear")
		}
	})
}

// funcPointer identifies a func value; funcs are not comparable, so the code pointer is the identity.
func funcPointer(fn any) uintptr {
	return reflect.ValueOf(fn).Pointer()
}

// TestHardenTLS verifies the TLS 1.2 floor and exactly the TLS 1.2 cipher allow-list.
func TestHardenTLS(t *testing.T) {
	c := &tls.Config{}
	hardenTLS(c)

	if c.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2 (%#x)", c.MinVersion, tls.VersionTLS12)
	}
	// Exactly the ECDHE + AES-GCM allow-list, 256-bit suites first; no ChaCha20.
	wantSuites := []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	}
	if !reflect.DeepEqual(c.CipherSuites, wantSuites) {
		t.Errorf("CipherSuites = %v, want %v", c.CipherSuites, wantSuites)
	}
	// None of ours may appear in tls.InsecureCipherSuites().
	insecure := map[uint16]string{}
	for _, s := range tls.InsecureCipherSuites() {
		insecure[s.ID] = s.Name
	}
	for _, id := range c.CipherSuites {
		if name, bad := insecure[id]; bad {
			t.Errorf("cipher suite %#x (%s) is on the crypto/tls insecure list", id, name)
		}
	}
	// P-384 first is the load-bearing assertion.
	want := []tls.CurveID{tls.CurveP384, tls.CurveP521, tls.X25519, tls.CurveP256}
	if !reflect.DeepEqual(c.CurvePreferences, want) {
		t.Errorf("CurvePreferences = %v, want %v", c.CurvePreferences, want)
	}
	if len(c.CurvePreferences) == 0 || c.CurvePreferences[0] != tls.CurveP384 {
		t.Errorf("CurvePreferences[0] = %v, want P-384", c.CurvePreferences)
	}
}

// TestWebhookServerOptions verifies the webhook server is built with hardenTLS wired in.
func TestWebhookServerOptions(t *testing.T) {
	const certDir = "/tmp/k8s-webhook-server/serving-certs"
	fs := flag.NewFlagSet("webhook", flag.ContinueOnError)
	cfg := bindFlags(fs)
	if err := fs.Parse([]string{"--cert-dir=" + certDir}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts := webhookServerOptions(cfg)
	if opts.Port != 9443 {
		t.Errorf("webhook Port = %d, want 9443", opts.Port)
	}
	if err := fs.Parse([]string{"--webhook-port=9555"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := webhookServerOptions(cfg).Port; got != 9555 {
		t.Errorf("webhook Port with --webhook-port=9555 = %d, want 9555", got)
	}
	if opts.CertDir != certDir {
		t.Errorf("webhook CertDir = %q, want %q", opts.CertDir, certDir)
	}
	if len(opts.TLSOpts) == 0 {
		t.Fatal("webhook server has no TLSOpts; MinVersion/cipher policy left implicit")
	}
	cfgTLS := &tls.Config{}
	for _, op := range opts.TLSOpts {
		op(cfgTLS)
	}
	if cfgTLS.MinVersion != tls.VersionTLS12 {
		t.Errorf("webhook TLS MinVersion = %#x, want TLS 1.2 (%#x)", cfgTLS.MinVersion, tls.VersionTLS12)
	}
}

// TestBalloonsReadyServer: the readiness listener takes its address from the flags,
// TLS defaults on, and it carries hardenTLS.
func TestBalloonsReadyServer(t *testing.T) {
	const certDir = "/certs"
	parse := func(t *testing.T, args ...string) *readiness.Server {
		t.Helper()
		fs := flag.NewFlagSet("ready", flag.ContinueOnError)
		cfg := bindFlags(fs)
		if err := fs.Parse(append(args, "--cert-dir="+certDir)); err != nil {
			t.Fatalf("parse: %v", err)
		}
		return balloonsReadyServer(cfg, &readiness.Handler{})
	}

	s := parse(t)
	if s.Addr != ":8082" || !s.Secure || s.CertDir != certDir {
		t.Errorf("defaults: Addr=%q Secure=%v CertDir=%q, want :8082, true, %s", s.Addr, s.Secure, s.CertDir, certDir)
	}
	if p := parse(t, "--balloons-ready-secure=false"); p.Secure {
		t.Error("--balloons-ready-secure=false still serves TLS")
	}
	got, want := &tls.Config{}, &tls.Config{}
	for _, op := range s.TLSOpts {
		op(got)
	}
	hardenTLS(want)
	if got.MinVersion != want.MinVersion || !reflect.DeepEqual(got.CipherSuites, want.CipherSuites) ||
		!reflect.DeepEqual(got.CurvePreferences, want.CurvePreferences) {
		t.Errorf("readiness TLSOpts do not apply hardenTLS: got %+v", got)
	}
}

func TestLeaderElectionID(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		want       string
	}{
		{
			// Only reachable via an explicit --leader-election-id=.
			name:       "empty falls back to default",
			configured: "",
			want:       defaultLeaderElectionID,
		},
		{
			// Distinct release fullnames must yield distinct leases.
			name:       "release-derived name wins",
			configured: "canary-cpu-pinning-operator",
			want:       "canary-cpu-pinning-operator",
		},
		{
			name:       "second release gets its own lease",
			configured: "bluegreen-cpu-pinning-operator",
			want:       "bluegreen-cpu-pinning-operator",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := leaderElectionID(tc.configured); got != tc.want {
				t.Errorf("leaderElectionID(%q) = %q, want %q", tc.configured, got, tc.want)
			}
		})
	}
}

// TestBindFlagsReservationTTL verifies --reservation-ttl defaults to DefaultReservationTTL and parses an override.
func TestBindFlagsReservationTTL(t *testing.T) {
	t.Run("default matches the reservation store default", func(t *testing.T) {
		fs := flag.NewFlagSet("ttl-default", flag.ContinueOnError)
		cfg := bindFlags(fs)
		if err := fs.Parse(nil); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if cfg.reservationTTL != podwebhook.DefaultReservationTTL {
			t.Errorf("reservationTTL default = %v, want %v (the reservation store default)",
				cfg.reservationTTL, podwebhook.DefaultReservationTTL)
		}
	})

	t.Run("operator value is parsed", func(t *testing.T) {
		fs := flag.NewFlagSet("ttl-set", flag.ContinueOnError)
		cfg := bindFlags(fs)
		if err := fs.Parse([]string{"--reservation-ttl=4m"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if cfg.reservationTTL != 4*time.Minute {
			t.Errorf("reservationTTL = %v, want 4m", cfg.reservationTTL)
		}
	})
}

// TestBindFlagsReserveAuthority verifies the reserve-authority flags default off with
// DefaultReservePort, and parse overrides.
func TestBindFlagsReserveAuthority(t *testing.T) {
	t.Run("defaults are off with the package default port", func(t *testing.T) {
		fs := flag.NewFlagSet("reserve-default", flag.ContinueOnError)
		cfg := bindFlags(fs)
		if err := fs.Parse(nil); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if cfg.reserveAuthority {
			t.Error("reserveAuthority default = true, want false (the chart's render gate decides; " +
				"a bare manager must not open the listener unasked)")
		}
		if cfg.reservePort != podwebhook.DefaultReservePort {
			t.Errorf("reservePort default = %d, want %d (the webhook package default)",
				cfg.reservePort, podwebhook.DefaultReservePort)
		}
	})

	t.Run("operator values are parsed", func(t *testing.T) {
		fs := flag.NewFlagSet("reserve-set", flag.ContinueOnError)
		cfg := bindFlags(fs)
		if err := fs.Parse([]string{"--reserve-authority", "--reserve-port=9555"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !cfg.reserveAuthority {
			t.Error("reserveAuthority = false after --reserve-authority, want true")
		}
		if cfg.reservePort != 9555 {
			t.Errorf("reservePort = %d, want 9555", cfg.reservePort)
		}
	})
}

// TestBindFlagsDriftEviction verifies the drift-eviction flags default to the controller's
// exported defaults and parse overrides.
func TestBindFlagsDriftEviction(t *testing.T) {
	t.Run("defaults match the controller package defaults", func(t *testing.T) {
		fs := flag.NewFlagSet("drift-default", flag.ContinueOnError)
		cfg := bindFlags(fs)
		if err := fs.Parse(nil); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if cfg.driftEvictionInterval != controller.DefaultDriftEvictionInterval {
			t.Errorf("driftEvictionInterval default = %v, want %v",
				cfg.driftEvictionInterval, controller.DefaultDriftEvictionInterval)
		}
		if cfg.driftEvictionBurst != controller.DefaultDriftEvictionBurst {
			t.Errorf("driftEvictionBurst default = %d, want %d",
				cfg.driftEvictionBurst, controller.DefaultDriftEvictionBurst)
		}
	})

	t.Run("operator values are parsed", func(t *testing.T) {
		fs := flag.NewFlagSet("drift-set", flag.ContinueOnError)
		cfg := bindFlags(fs)
		if err := fs.Parse([]string{"--drift-eviction-interval=2s", "--drift-eviction-burst=5"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if cfg.driftEvictionInterval != 2*time.Second {
			t.Errorf("driftEvictionInterval = %v, want 2s", cfg.driftEvictionInterval)
		}
		if cfg.driftEvictionBurst != 5 {
			t.Errorf("driftEvictionBurst = %d, want 5", cfg.driftEvictionBurst)
		}
	})
}

// TestBindFlagsRejectsInvalidLevel verifies an unparseable log level is a flag error.
func TestBindFlagsRejectsInvalidLevel(t *testing.T) {
	fs := flag.NewFlagSet("invalid", flag.ContinueOnError)
	fs.SetOutput(&nopWriter{})
	bindFlags(fs)
	if err := fs.Parse([]string{"--zap-log-level=verbose"}); err == nil {
		t.Fatal("expected an error for an invalid --zap-log-level, got nil")
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestCacheSyncReadyChecker covers the cache-sync check, including a nil request.
func TestCacheSyncReadyChecker(t *testing.T) {
	t.Run("nil request does not panic and reports synced", func(t *testing.T) {
		check := cacheSyncReadyChecker("informer caches", func(ctx context.Context) bool {
			// A non-nil context proves the fallback kicked in.
			if ctx == nil {
				t.Error("waitFor received a nil context")
			}
			return true
		})
		if err := check(nil); err != nil {
			t.Fatalf("expected ready (nil) with a nil request, got %v", err)
		}
	})

	t.Run("not synced yields a subject-specific error", func(t *testing.T) {
		check := cacheSyncReadyChecker("scoped node cache", func(context.Context) bool { return false })
		err := check(&http.Request{})
		if err == nil {
			t.Fatal("expected not-ready error when caches are not synced, got nil")
		}
		// The message must name the cache.
		if !strings.Contains(err.Error(), "scoped node cache") {
			t.Fatalf("expected error to name the scoped node cache, got %q", err.Error())
		}
	})

	t.Run("synced with a real request is ready", func(t *testing.T) {
		check := cacheSyncReadyChecker("informer caches", func(context.Context) bool { return true })
		if err := check(&http.Request{}); err != nil {
			t.Fatalf("expected ready (nil) when caches are synced, got %v", err)
		}
	})
}

// syncRecordingCache records WaitForCacheSync calls and returns a canned result.
type syncRecordingCache struct {
	cache.Cache
	synced   bool
	waitSeen bool
}

func (c *syncRecordingCache) WaitForCacheSync(context.Context) bool {
	c.waitSeen = true
	return c.synced
}

// TestUnleaderedCacheNeedLeaderElection verifies the wrapper opts out of leader election,
// so followers sync the scoped caches and become Ready.
func TestUnleaderedCacheNeedLeaderElection(t *testing.T) {
	// Compile-time check that the wrapper is a LeaderElectionRunnable.
	var _ manager.LeaderElectionRunnable = unleaderedCache{}

	if (unleaderedCache{}).NeedLeaderElection() {
		t.Fatal("unleaderedCache.NeedLeaderElection() = true; the scoped node cache would be leader-gated and followers would never become Ready")
	}
}

// TestUnleaderedCacheDelegates verifies the wrapper forwards WaitForCacheSync to the embedded cache.
func TestUnleaderedCacheDelegates(t *testing.T) {
	for _, synced := range []bool{true, false} {
		underlying := &syncRecordingCache{synced: synced}
		wrapped := unleaderedCache{underlying}
		got := wrapped.WaitForCacheSync(context.Background())
		if !underlying.waitSeen {
			t.Error("WaitForCacheSync was not delegated to the embedded cache")
		}
		if got != synced {
			t.Errorf("WaitForCacheSync() = %v; want the embedded cache's result %v", got, synced)
		}
	}
}

// TestLeaderElectionIDResolvesChartContract verifies distinct release names stay distinct
// and never fold onto the shared default.
func TestLeaderElectionIDResolvesChartContract(t *testing.T) {
	const canary = "canary-cpu-pinning-operator"
	const bluegreen = "bluegreen-cpu-pinning-operator"

	gotCanary := leaderElectionID(canary)
	gotBluegreen := leaderElectionID(bluegreen)

	if gotCanary != canary {
		t.Errorf("leaderElectionID(%q) = %q; chart-derived value not preserved", canary, gotCanary)
	}
	if gotBluegreen != bluegreen {
		t.Errorf("leaderElectionID(%q) = %q; chart-derived value not preserved", bluegreen, gotBluegreen)
	}
	if gotCanary == defaultLeaderElectionID || gotBluegreen == defaultLeaderElectionID {
		t.Errorf("a chart-derived lease collapsed onto the default %q; releases would contend", defaultLeaderElectionID)
	}
	if gotCanary == gotBluegreen {
		t.Errorf("two releases resolved to the same lease %q; they would contend", gotCanary)
	}
}

// TestBalloonsEnabledNodeSelector verifies the scoped Node cache selector matches provisioned
// nodes only.
func TestBalloonsEnabledNodeSelector(t *testing.T) {
	enabled := labels.Set{controller.LabelBalloonsEnabled: "true"}
	if !balloonsEnabledNodeSelector.Matches(enabled) {
		t.Errorf("selector %q does not match a balloons-enabled node %v", balloonsEnabledNodeSelector, enabled)
	}

	if balloonsEnabledNodeSelector.Matches(labels.Set{}) {
		t.Errorf("selector %q matched an unlabelled node; a fresh node would wrongly enter the scoped cache", balloonsEnabledNodeSelector)
	}

	disabled := labels.Set{controller.LabelBalloonsEnabled: "false"}
	if balloonsEnabledNodeSelector.Matches(disabled) {
		t.Errorf("selector %q matched a balloons-enabled=false node %v", balloonsEnabledNodeSelector, disabled)
	}
}

func TestHealthCheckURL(t *testing.T) {
	for _, tc := range []struct {
		addr, want string
		wantErr    bool
	}{
		{addr: ":8081", want: "http://127.0.0.1:8081/healthz"},
		{addr: "0.0.0.0:8081", want: "http://127.0.0.1:8081/healthz"},
		{addr: "[::]:8081", want: "http://127.0.0.1:8081/healthz"},
		{addr: "127.0.0.1:9999", want: "http://127.0.0.1:9999/healthz"},
		{addr: "8081", wantErr: true},
	} {
		got, err := healthCheckURL(tc.addr)
		if tc.wantErr {
			if err == nil {
				t.Errorf("healthCheckURL(%q) = %q, want an error", tc.addr, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("healthCheckURL(%q): %v", tc.addr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("healthCheckURL(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// TestRunHealthCheckOnlyAcceptsSuccess pins the property the HEALTHCHECK depends on: a
// 500 from the healthz mux is a failed check, not a reachable endpoint.
func TestRunHealthCheckOnlyAcceptsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "200 is healthy", status: http.StatusOK},
		{name: "500 from a failed check is unhealthy", status: http.StatusInternalServerError, wantErr: true},
		{name: "404 is unhealthy", status: http.StatusNotFound, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("probed %q, want /healthz", r.URL.Path)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			err := runHealthCheck([]string{"-addr", strings.TrimPrefix(srv.URL, "http://")}, io.Discard)
			if tc.wantErr && err == nil {
				t.Fatalf("status %d: got nil error, want unhealthy", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("status %d: %v", tc.status, err)
			}
		})
	}
}

// TestRunHealthCheckUnreachable: nothing listening must be unhealthy, not a pass.
func TestRunHealthCheckUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	if err := runHealthCheck([]string{"-addr", addr}, io.Discard); err == nil {
		t.Fatal("got nil error probing a closed port, want unhealthy")
	}
}
