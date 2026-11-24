// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	nriv1alpha1 "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"
	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/controller"
	"github.com/intel/cpu-pinning-operator/internal/dashboard"
	"github.com/intel/cpu-pinning-operator/internal/match"
	"github.com/intel/cpu-pinning-operator/internal/readiness"
	"github.com/intel/cpu-pinning-operator/internal/topology"
	"github.com/intel/cpu-pinning-operator/internal/unlabel"
	podwebhook "github.com/intel/cpu-pinning-operator/internal/webhook"
)

// Chart defaults for the NRI balloons plugin; keep in sync with the chart.
const (
	defaultBalloonsNamespace = "kube-system"
	defaultBalloonsDaemonSet = "nri-resource-policy-balloons"
)

// healthCheckSubcommand backs the Dockerfile HEALTHCHECK: the image has no shell,
// so this binary is the only executable. Kubernetes uses the chart's probes instead.
const healthCheckSubcommand = "healthcheck"

const healthCheckTimeout = 3 * time.Second

// defaultProbeAddr is shared with --health-probe-bind-address so the two cannot drift.
const defaultProbeAddr = ":8081"

// preDeleteSubcommand must run before Helm deletes the default policy: the manager goes
// first, and the guard's failurePolicy:Fail webhook with no backend would deny the DELETE.
const preDeleteSubcommand = "pre-delete"

// postDeleteSubcommand strips operator-managed node labels (GC cannot remove label keys).
// It must run post-delete, or the live NodeReconciler re-labels the nodes.
const postDeleteSubcommand = "post-delete"

// validatingWebhookConfigEnv names the guard's ValidatingWebhookConfiguration
// for the pre-delete hook to delete. Empty skips the teardown.
const validatingWebhookConfigEnv = "VALIDATING_WEBHOOK_CONFIG_NAME"

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// getEnvInt parses a positive int, falling back to def; an invalid value is logged.
func getEnvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		setupLog.Info("ignoring invalid integer env var, using default", "env", key, "value", v, "default", def)
		return def
	}
	return n
}

// runPreDelete deletes the guard's ValidatingWebhookConfiguration; see preDeleteSubcommand.
func runPreDelete() error {
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("building pre-delete client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	if name := os.Getenv(validatingWebhookConfigEnv); name != "" {
		vwc := &admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		}
		if err := c.Delete(ctx, vwc); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("deleting validating webhook configuration %q: %w", name, err)
			}
			_, _ = fmt.Fprintf(os.Stdout, "validating webhook configuration %q already absent\n", name)
		} else {
			_, _ = fmt.Fprintf(os.Stdout, "deleted validating webhook configuration %q\n", name)
		}
	}
	return nil
}

// runPostDelete strips the operator-managed node labels; see postDeleteSubcommand.
func runPostDelete() error {
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("building post-delete client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	n, err := unlabel.StripManagedLabels(ctx, c, os.Stdout)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "node label cleanup complete: %d node(s) unlabelled\n", n)
	return nil
}

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// defaultLeaderElectionID is the fallback lease name. The chart's release-scoped name is
// truncated to 63 chars, so two releases must differ within those or they contend.
const defaultLeaderElectionID = "cpu-pinning-operator.intel.com"

// cacheSyncReadyTimeout bounds one readiness probe's cache-sync wait so it fails fast and the kubelet retries.
const cacheSyncReadyTimeout = 5 * time.Second

// cleanupTimeout bounds the hook Jobs so an unreachable API server fails them fast.
const cleanupTimeout = 60 * time.Second

func leaderElectionID(configured string) string {
	if configured != "" {
		return configured
	}
	return defaultLeaderElectionID
}

// approvedTLS12CipherSuites is the TLS 1.2 allow-list: ECDHE with AES-GCM only
// (forward secrecy plus AEAD, per NIST SP 800-52r2), 256-bit first; no ChaCha20. TLS 1.3 suites are fixed by the stdlib.
var approvedTLS12CipherSuites = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
}

// hardenTLS pins the TLS floor for the webhook, metrics and readiness servers. TLS 1.2
// stays for kube-apiserver and Prometheus (the readiness server raises its own floor
// to 1.3). P-384/P-521 lead the curves; X25519/P-256 remain as interop fallback.
func hardenTLS(c *tls.Config) {
	c.MinVersion = tls.VersionTLS12
	c.CipherSuites = approvedTLS12CipherSuites
	c.CurvePreferences = []tls.CurveID{tls.CurveP384, tls.CurveP521, tls.X25519, tls.CurveP256}
}

// metricsServerOptions serves metrics over HTTPS (default) with the webhook's serving cert.
// With metricsRequireAuth a scraper needs a bearer token (TokenReview) and RBAC for
// `get /metrics` (SubjectAccessReview), else 401/403. Auth is never applied without
// TLS: a token on a plaintext listener would be readable on the path.
func metricsServerOptions(cfg *managerConfig) metricsserver.Options {
	opts := metricsserver.Options{BindAddress: cfg.metricsAddr}
	if cfg.metricsSecure {
		opts.SecureServing = true
		// Default CertName/KeyName (tls.crt/tls.key) match the chart's secret.
		opts.CertDir = cfg.certDir
		opts.TLSOpts = []func(*tls.Config){hardenTLS}
		if cfg.metricsRequireAuth {
			opts.FilterProvider = filters.WithAuthenticationAndAuthorization
		}
	} else if cfg.metricsRequireAuth {
		setupLog.Info("metrics client authentication needs TLS and is NOT active: " +
			"--metrics-secure=false serves plaintext, so the endpoint accepts any client that can reach it " +
			"(restrict it with the chart's networkPolicy.metrics.from)")
	}
	return opts
}

// webhookServerOptions is separate so a test can assert the TLSOpts wiring.
func webhookServerOptions(cfg *managerConfig) webhook.Options {
	return webhook.Options{
		Port:    cfg.webhookPort,
		CertDir: cfg.certDir,
		TLSOpts: []func(*tls.Config){hardenTLS},
	}
}

// balloonsReadyServer is separate for the same reason as webhookServerOptions.
func balloonsReadyServer(cfg *managerConfig, h *readiness.Handler) *readiness.Server {
	return &readiness.Server{
		Addr:    cfg.balloonsReadyAddr,
		Secure:  cfg.balloonsReadySecure,
		CertDir: cfg.certDir,
		TLSOpts: []func(*tls.Config){hardenTLS},
		Handler: h,
		Log:     h.Log,
	}
}

// cacheSyncReadyChecker builds a cache-sync readiness check; subject names the lagging
// cache in the error. A nil request is tolerated.
func cacheSyncReadyChecker(subject string, waitFor func(context.Context) bool) healthz.Checker {
	return func(req *http.Request) error {
		baseCtx := context.Background()
		if req != nil {
			baseCtx = req.Context()
		}
		ctx, cancel := context.WithTimeout(baseCtx, cacheSyncReadyTimeout)
		defer cancel()
		if !waitFor(ctx) {
			return fmt.Errorf("%s not yet synced", subject)
		}
		return nil
	}
}

// balloonsEnabledNodeSelector scopes the admission path's Node cache to the same
// label set the reconcilers select the pinning pool with.
var balloonsEnabledNodeSelector = labels.SelectorFromSet(labels.Set(controller.ProvisionedNodeLabels()))

// newScopedNodeCache watches only balloons-enabled Nodes, bounding webhook scans by the
// provisioned count. It is separate because the manager's cache must stay unscoped, or
// the Node reconciler never sees a not-yet-labelled node to label it.
func newScopedNodeCache(cfg *rest.Config) (cache.Cache, error) {
	return cache.New(cfg, cache.Options{
		Scheme: scheme,
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Node{}: {Label: balloonsEnabledNodeSelector},
		},
	})
}

// newScopedLeaderCache backs LeaderDiscovery's Lease and leader-Pod reads, scoped to the
// operator namespace (the Lease to one name). A Get on the unscoped manager cache would
// start cluster-wide Lease and Pod watches.
func newScopedLeaderCache(cfg *rest.Config, namespace, leaseName string) (cache.Cache, error) {
	return cache.New(cfg, cache.Options{
		Scheme: scheme,
		ByObject: map[client.Object]cache.ByObject{
			&coordinationv1.Lease{}: {Namespaces: map[string]cache.Config{
				namespace: {FieldSelector: fields.OneTermEqualSelector("metadata.name", leaseName)},
			}},
			&corev1.Pod{}: {Namespaces: map[string]cache.Config{namespace: {}}},
		},
	})
}

// unleaderedCache makes the manager start a cache on every replica. A bare cache.Cache
// lands in the leader-gated runnable group, so on a follower it never syncs and readyz
// stays NotReady.
type unleaderedCache struct {
	cache.Cache
}

func (unleaderedCache) NeedLeaderElection() bool { return false }

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nriv1alpha1.AddToScheme(scheme))
	utilruntime.Must(cpopv1alpha1.AddToScheme(scheme))
}

// managerConfig holds the parsed manager flags; the flag names are the chart's contract.
type managerConfig struct {
	metricsAddr           string
	metricsSecure         bool
	metricsRequireAuth    bool
	probeAddr             string
	balloonsReadyAddr     string
	balloonsReadySecure   bool
	certDir               string
	enableLeaderElection  bool
	leaderElectionID      string
	reservationTTL        time.Duration
	reserveAuthority      bool
	reservePort           int
	webhookPort           int
	driftEvictionInterval time.Duration
	driftEvictionBurst    int
	zapOpts               zap.Options
}

// bindFlags registers every manager flag on fs.
func bindFlags(fs *flag.FlagSet) *managerConfig {
	cfg := &managerConfig{}
	fs.StringVar(&cfg.metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	fs.BoolVar(&cfg.metricsSecure, "metrics-secure", true,
		"Serve the metrics endpoint over HTTPS using the serving certificate in --cert-dir. "+
			"Defaults to on; set false only to fall back to plaintext HTTP metrics.")
	fs.BoolVar(&cfg.metricsRequireAuth, "metrics-require-auth", true,
		"Require every metrics client to authenticate. The scraper's bearer token is verified with a "+
			"TokenReview and authorized for `get /metrics` with a SubjectAccessReview, so a scraper needs "+
			"both a token and an RBAC grant (the chart's metrics-reader ClusterRole). Defaults to on, and "+
			"applies only with --metrics-secure: a token on a plaintext listener would be readable on the "+
			"path. Set false ONLY for a scraper that cannot present a token — the endpoint then keeps TLS "+
			"but accepts any client that can reach it, leaving the network policy as the only control.")
	fs.StringVar(&cfg.probeAddr, "health-probe-bind-address", defaultProbeAddr, "The address the probe endpoint binds to.")
	fs.StringVar(&cfg.balloonsReadyAddr, "balloons-ready-bind-address", ":8082",
		"The address the per-node balloons-readiness endpoint (/balloons/ready?node=) binds to. "+
			"The injected wait-for-balloons init container polls it to gate workload start.")
	fs.BoolVar(&cfg.balloonsReadySecure, "balloons-ready-secure", true,
		"Serve the balloons-readiness endpoint over HTTPS using the serving certificate in --cert-dir "+
			"(reloaded on rotation). Defaults to on; set false only to fall back to plaintext HTTP.")
	fs.BoolVar(&cfg.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	fs.StringVar(&cfg.leaderElectionID, "leader-election-id", defaultLeaderElectionID,
		"The name of the leader-election lease. Derive it from the release so two "+
			"releases of the operator in one cluster do not contend for the same lease.")
	fs.StringVar(&cfg.certDir, "cert-dir", "/tmp/k8s-webhook-server/serving-certs", "The directory containing TLS certificates.")
	fs.DurationVar(&cfg.reservationTTL, "reservation-ttl", podwebhook.DefaultReservationTTL,
		"How long the webhook's in-process admission reservation for a steered pod lives before "+
			"it is ignored. It must outlive the NRI balloons plugin's NRT publish latency (plugin "+
			"pin-to-CR write plus informer cache lag) so a burst of admissions subtracts each "+
			"other's not-yet-reflected demand; too short reopens the burst race, too long holds "+
			"phantom headroom for pods that never schedule. Raise it on clusters whose plugin "+
			"publishes slower than the default.")
	fs.BoolVar(&cfg.reserveAuthority, "reserve-authority", false,
		"Enable the leader-authoritative reserve step for an HA (multi-replica) install. "+
			"Every replica keeps serving admission and does the read + ranking locally, but "+
			"forwards ONLY the final headroom reservation to the lease holder over an internal "+
			"mTLS endpoint, so the fleet accounts headroom against ONE store instead of one per "+
			"replica. A commit the authority cannot take DENIES the pod under capacityCheck.mode "+
			"enforce, and falls back to this replica's local store under warn/off. Requires "+
			"--leader-elect, and a serving cert in --cert-dir carrying ca.crt and a DNS SAN. Off "+
			"by default; the chart passes it only at replicaCount > 1 with leader election on, so "+
			"a single-replica install pays zero cost.")
	fs.IntVar(&cfg.webhookPort, "webhook-port", 9443,
		"The port the admission webhook server listens on. It must match the webhook Service's targetPort.")
	fs.IntVar(&cfg.reservePort, "reserve-port", podwebhook.DefaultReservePort,
		"The port the reservation authority listens on (and followers dial) for the internal "+
			"mTLS reserve endpoint. Only used when --reserve-authority is set.")
	fs.DurationVar(&cfg.driftEvictionInterval, "drift-eviction-interval", controller.DefaultDriftEvictionInterval,
		"Steady-state pace of drift-driven pod evictions: one eviction token is replenished every "+
			"interval, so the whole fleet re-pins at 1/interval (the default 10s is 6/min). A single "+
			"config change can make every operator-pinned pod drift at once; this paces the restart "+
			"rollout instead of evicting the fleet in one burst. Tighten it on a small fleet for faster "+
			"convergence; widen it on a large fleet to keep the rollout gentle.")
	fs.IntVar(&cfg.driftEvictionBurst, "drift-eviction-burst", controller.DefaultDriftEvictionBurst,
		"How many drift-driven evictions may fire back-to-back before the --drift-eviction-interval "+
			"pace applies (the token bucket's burst size).")
	cfg.zapOpts.BindFlags(fs)
	return cfg
}

// healthCheckURL turns a bind address into the URL to probe; a missing or wildcard host becomes loopback.
func healthCheckURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("address %q is not host:port: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

// runHealthCheck probes /healthz; only 2xx is healthy (a failed check answers 500).
func runHealthCheck(args []string, out io.Writer) error {
	fs := flag.NewFlagSet(healthCheckSubcommand, flag.ContinueOnError)
	fs.SetOutput(out)
	addr := fs.String("addr", defaultProbeAddr,
		"host:port the manager serves /healthz on; match --health-probe-bind-address if it was changed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url, err := healthCheckURL(*addr)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: healthCheckTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("probe %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("probe %s: status %s", url, resp.Status)
	}
	_, _ = fmt.Fprintf(out, "healthy: %s\n", url)
	return nil
}

func main() {
	// The init container and the probe pod reuse this image, which has no shell, hence the subcommands.
	if len(os.Args) > 1 && os.Args[1] == readiness.SubcommandName {
		if err := readiness.RunPoller(os.Args[2:], os.Stdout); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if len(os.Args) > 1 && os.Args[1] == topology.ProbeSubcommandName {
		if err := topology.RunProbe(os.Stdout); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if len(os.Args) > 1 && os.Args[1] == healthCheckSubcommand {
		if err := runHealthCheck(os.Args[2:], os.Stdout); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if len(os.Args) > 1 && os.Args[1] == preDeleteSubcommand {
		if err := runPreDelete(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if len(os.Args) > 1 && os.Args[1] == postDeleteSubcommand {
		if err := runPostDelete(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	cfg := bindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&cfg.zapOpts)))

	// Client stays unset so unstructured reads are live: the node reconciler compares its
	// pre-SSA BalloonsPolicy read with the apply result, and a cached read would lag.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions(cfg),
		WebhookServer:          webhook.NewServer(webhookServerOptions(cfg)),
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.enableLeaderElection,
		LeaderElectionID:       leaderElectionID(cfg.leaderElectionID),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// unleaderedCache: the webhook serves on every replica and reads Nodes from here.
	nodeCache, err := newScopedNodeCache(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create scoped node cache")
		os.Exit(1)
	}
	if err := mgr.Add(unleaderedCache{nodeCache}); err != nil {
		setupLog.Error(err, "unable to add scoped node cache to manager")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create clientset for probe pod log retrieval")
		os.Exit(1)
	}
	logsGetter := &topology.ClientsetLogsGetter{Clientset: clientset}

	policyIndex := match.NewIndex()
	topoCache := topology.NewCache()

	// Disabled by default: Enabled false makes the Writer a no-op.
	dashboardWriter := &dashboard.Writer{
		Client: mgr.GetClient(),
		Config: dashboard.Config{
			Enabled:           os.Getenv("DASHBOARD_ENABLED") == "true",
			Namespace:         os.Getenv("DASHBOARD_NAMESPACE"),
			SidecarLabel:      os.Getenv("DASHBOARD_SIDECAR_LABEL"),
			SidecarLabelValue: os.Getenv("DASHBOARD_SIDECAR_LABEL_VALUE"),
			FolderAnnotation:  os.Getenv("DASHBOARD_FOLDER_ANNOTATION"),
			Folder:            os.Getenv("DASHBOARD_FOLDER"),
			InstanceName:      os.Getenv("DASHBOARD_INSTANCE_NAME"),
			DatasourceUID:     os.Getenv("DASHBOARD_DATASOURCE_UID"),
			// Zero value omits the Loki variable and log panel.
			Loki: dashboard.LokiConfig{
				Enabled:       os.Getenv("DASHBOARD_LOKI_ENABLED") == "true",
				DatasourceUID: os.Getenv("DASHBOARD_LOKI_DATASOURCE_UID"),
				NodeLabel:     os.Getenv("DASHBOARD_LOKI_NODE_LABEL"),
				PluginLabel:   os.Getenv("DASHBOARD_LOKI_PLUGIN_LABEL"),
				PluginName:    os.Getenv("DASHBOARD_LOKI_PLUGIN_NAME"),
				WorkloadLabel: os.Getenv("DASHBOARD_LOKI_WORKLOAD_LABEL"),
			},
		},
	}
	// Buffered so a probe never blocks on a busy metrics reconciler.
	topoEvents := make(chan event.GenericEvent, 16)

	nodeReconciler := &controller.NodeReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Log:         ctrl.Log.WithName("controller").WithName("node"),
		PolicyIndex: policyIndex,
		LogsGetter:  logsGetter,
		// Reads kubeadm's kubelet-config to flag a conflicting kubelet CPU Manager.
		// Uncached: a cached Get would watch every ConfigMap in the cluster.
		KubeletConfigReader: mgr.GetAPIReader(),
		TopoCache:           topoCache,
		TopoNotify:          topoEvents,
		Dashboards:          dashboardWriter,
		Recorder:            mgr.GetEventRecorderFor("cpu-pinning-operator"),
		// Must match the balloons plugin's namespace and the readiness endpoint's.
		BalloonsNamespace: getEnv("BALLOONS_NAMESPACE", defaultBalloonsNamespace),
		// Node reconciles are independent; non-positive keeps the default.
		MaxConcurrentReconciles: getEnvInt("NODE_RECONCILE_CONCURRENCY", 0),
	}

	if err := (&controller.BalloonsClusterConfigReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Log:         ctrl.Log.WithName("controller").WithName("balloonsclusterconfig"),
		PolicyIndex: policyIndex,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BalloonsClusterConfig")
		os.Exit(1)
	}

	if err := (&controller.BalloonPinningPolicyReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Log:         ctrl.Log.WithName("controller").WithName("balloonpinningpolicy"),
		PolicyIndex: policyIndex,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BalloonPinningPolicy")
		os.Exit(1)
	}

	// The init containers of an https readiness endpoint verify it against this copy.
	if name := os.Getenv(readiness.CAConfigMapEnv); name != "" && cfg.balloonsReadySecure {
		if err := (&controller.ReadinessCAReconciler{
			Client:        mgr.GetClient(),
			Reader:        mgr.GetAPIReader(),
			CAFile:        filepath.Join(cfg.certDir, readiness.CAKey),
			ConfigMapName: name,
			Log:           ctrl.Log.WithName("controller").WithName("readiness-ca"),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ReadinessCA")
			os.Exit(1)
		}
	}

	// Not leader-gated: the webhook serves on all replicas, and a follower with an empty
	// policyIndex would admit matched pods unmutated.
	if err := (&controller.PolicyIndexPopulator{
		Client:      mgr.GetClient(),
		Log:         ctrl.Log.WithName("controller").WithName("policyindexpopulator"),
		PolicyIndex: policyIndex,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PolicyIndexPopulator")
		os.Exit(1)
	}

	// Not leader-gated either: hydrates topology from the node annotation so followers
	// can run the live capacity check.
	if err := (&controller.TopoCachePopulator{
		Client:    mgr.GetClient(),
		Log:       ctrl.Log.WithName("controller").WithName("topocachepopulator"),
		TopoCache: topoCache,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TopoCachePopulator")
		os.Exit(1)
	}

	if err := nodeReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Node")
		os.Exit(1)
	}

	if err := (&controller.NRTMetricsReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Log:        ctrl.Log.WithName("controller").WithName("nrtmetrics"),
		TopoCache:  topoCache,
		TopoEvents: topoEvents,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NRTMetrics")
		os.Exit(1)
	}

	if err := (&controller.PodDriftReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Log:              ctrl.Log.WithName("controller").WithName("poddrift"),
		PolicyIndex:      policyIndex,
		Recorder:         mgr.GetEventRecorderFor("cpu-pinning-operator"),
		EvictionInterval: cfg.driftEvictionInterval,
		EvictionBurst:    cfg.driftEvictionBurst,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PodDrift")
		os.Exit(1)
	}

	setupLog.Info("setting up webhook server")
	hookServer := mgr.GetWebhookServer()

	// One store per replica; under --reserve-authority the leader's is authoritative and
	// each replica's is the fallback.
	reservations := podwebhook.NewReservationStore(cfg.reservationTTL)

	podMutator := &podwebhook.PodMutator{
		Client: mgr.GetClient(),
		// Label-scoped: admission work is bounded by the balloons-enabled count.
		NodeReader:   nodeCache,
		Log:          ctrl.Log.WithName("webhook").WithName("pod"),
		PolicyIndex:  policyIndex,
		Recorder:     mgr.GetEventRecorderFor("cpu-pinning-operator"),
		TopoCache:    topoCache,
		Reservations: reservations,
		CertDir:      cfg.certDir,
	}

	// HA reserve: replicas rank locally and forward the reservation to the leader;
	// discovery or transport errors fall back to the local store.
	if cfg.reserveAuthority {
		if !cfg.enableLeaderElection {
			setupLog.Error(fmt.Errorf("--reserve-authority requires --leader-elect"), "invalid configuration")
			os.Exit(1)
		}
		selfPod, err := os.Hostname()
		if err != nil {
			setupLog.Error(err, "unable to resolve self pod name (hostname) for reserve leader discovery")
			os.Exit(1)
		}
		reserveAddr := fmt.Sprintf(":%d", cfg.reservePort)
		if err := mgr.Add(&podwebhook.ReserveAuthority{
			Addr:    reserveAddr,
			CertDir: cfg.certDir,
			Store:   reservations,
			Log:     ctrl.Log.WithName("webhook").WithName("reserve-authority"),
			// Seeds a new leader's store from pinned-but-unreflected pods.
			Warm: podMutator.WarmReservations,
		}); err != nil {
			setupLog.Error(err, "unable to add reserve authority runnable")
			os.Exit(1)
		}
		leaseName := leaderElectionID(cfg.leaderElectionID)
		ownNamespace := getEnv("POD_NAMESPACE", "")
		// Without it no leader is ever resolved; under enforce every matched pod is denied.
		if ownNamespace == "" {
			setupLog.Error(fmt.Errorf("POD_NAMESPACE is empty"),
				"--reserve-authority needs the operator's own namespace to find the leader")
			os.Exit(1)
		}
		// Followers forward, so it must start there. A stale cached holder only yields a transport error.
		leaderCache, err := newScopedLeaderCache(mgr.GetConfig(), ownNamespace, leaseName)
		if err != nil {
			setupLog.Error(err, "unable to create scoped leader-discovery cache")
			os.Exit(1)
		}
		if err := mgr.Add(unleaderedCache{leaderCache}); err != nil {
			setupLog.Error(err, "unable to add scoped leader-discovery cache to manager")
			os.Exit(1)
		}
		// Otherwise a follower goes Ready before the sync and its first reserve blocks inside
		// the admission budget (a denial under enforce).
		if err := mgr.AddReadyzCheck("reserve-cache-sync",
			cacheSyncReadyChecker("scoped leader-discovery cache", leaderCache.WaitForCacheSync)); err != nil {
			setupLog.Error(err, "unable to set up reserve-cache-sync ready check")
			os.Exit(1)
		}
		discovery := &podwebhook.LeaderDiscovery{
			Reader:      leaderCache,
			Namespace:   ownNamespace,
			LeaseName:   leaseName,
			SelfPodName: selfPod,
		}
		reserveClient, err := podwebhook.NewLeaderReserveClient(
			cfg.certDir, cfg.reservePort, reservations, discovery.Leader,
			ctrl.Log.WithName("webhook").WithName("reserve-client"))
		if err != nil {
			setupLog.Error(err, "unable to build leader reserve client")
			os.Exit(1)
		}
		podMutator.SetReserver(reserveClient)
		setupLog.Info("leader-authoritative reserve enabled",
			"reservePort", cfg.reservePort, "leaseName", discovery.LeaseName, "selfPod", selfPod)
	}

	decoder := admission.NewDecoder(scheme)
	if err := podMutator.InjectDecoder(decoder); err != nil {
		setupLog.Error(err, "unable to inject decoder into pod mutator")
		os.Exit(1)
	}

	hookServer.Register("/mutate-v1-pod", &webhook.Admission{Handler: podMutator})

	// Rejects deleting or narrowing a guardActivePods policy that still holds pinned pods.
	policyGuard := &podwebhook.PolicyGuard{
		// Uncached: a lagging cache could miss a running pod and let a narrowing through.
		Client: mgr.GetAPIReader(),
		Log:    ctrl.Log.WithName("webhook").WithName("balloonpinningpolicy"),
	}
	if err := policyGuard.InjectDecoder(decoder); err != nil {
		setupLog.Error(err, "unable to inject decoder into policy guard")
		os.Exit(1)
	}
	hookServer.Register("/validate-v1alpha1-balloonpinningpolicy", &webhook.Admission{Handler: policyGuard})

	// Own listener: the healthz mux cannot take query parameters. Served on every replica.
	if err := mgr.Add(balloonsReadyServer(cfg, &readiness.Handler{
		// Uncached: a false-ready starts a workload before the plugin, and NRI pins only at creation.
		Client:    mgr.GetAPIReader(),
		Namespace: getEnv("BALLOONS_NAMESPACE", defaultBalloonsNamespace),
		DaemonSet: getEnv("BALLOONS_DAEMONSET", defaultBalloonsDaemonSet),
		Log:       ctrl.Log.WithName("readiness").WithName("balloons"),
	})); err != nil {
		setupLog.Error(err, "unable to add balloons-readiness endpoint")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}

	// Ready only once the webhook can mutate: the server starts before caches sync, and
	// under failurePolicy Ignore pods would be admitted unmutated. Needs synced caches,
	// the serving cert on disk, and a listener accepting TLS.
	if err := mgr.AddReadyzCheck("informer-cache-sync", cacheSyncReadyChecker("informer caches", mgr.GetCache().WaitForCacheSync)); err != nil {
		setupLog.Error(err, "unable to set up cache-sync ready check")
		os.Exit(1)
	}
	// Or a cold-start admission sees no nodes and skips capacity steering.
	if err := mgr.AddReadyzCheck("node-cache-sync", cacheSyncReadyChecker("scoped node cache", nodeCache.WaitForCacheSync)); err != nil {
		setupLog.Error(err, "unable to set up node-cache-sync ready check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("webhook-cert", podwebhook.CertPresenceChecker(cfg.certDir)); err != nil {
		setupLog.Error(err, "unable to set up webhook cert ready check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("webhook-serving", hookServer.StartedChecker()); err != nil {
		setupLog.Error(err, "unable to set up webhook serving ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
