// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// SubcommandName is the operator-binary subcommand the injected init container runs.
const SubcommandName = "wait-for-balloons"

// The init container cannot mount the operator's cert Secret, so ca.crt is synced
// into a ConfigMap in its namespace. CAConfigMapEnv names it; CAKey is its data key.
const (
	CAConfigMapEnv = "READINESS_CA_CONFIGMAP"
	CAKey          = "ca.crt"
)

// PollConfig configures the init-container poller.
type PollConfig struct {
	// Endpoint is the manager readiness base URL, e.g. "https://<manager-service>:8082".
	Endpoint string
	// Node is the pod's spec.nodeName from the downward API, sent as ?node=.
	Node string
	// Interval is the delay between polls; Timeout is the overall budget.
	Interval time.Duration
	Timeout  time.Duration
}

// pollDefaults are the poll cadence and budget when the flags are unset.
const (
	defaultPollInterval = 5 * time.Second
	defaultPollTimeout  = 300 * time.Second
)

// RunPoller parses the subcommand's flags (args after the subcommand name) and
// blocks until the node is ready (nil) or the timeout or bad config (error).
func RunPoller(args []string, out io.Writer) error {
	fs := flag.NewFlagSet(SubcommandName, flag.ContinueOnError)
	fs.SetOutput(out)
	endpoint := fs.String("endpoint", "", "manager readiness base URL, e.g. https://<manager-service>:8082")
	caFile := fs.String("ca-file", "", "PEM CA an https endpoint is verified against, re-read on every poll")
	node := fs.String("node", "", "node the pod is scheduled to (spec.nodeName via the downward API)")
	interval := fs.Duration("interval", defaultPollInterval, "delay between readiness polls")
	timeout := fs.Duration("timeout", defaultPollTimeout, "overall budget before giving up")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := PollConfig{Endpoint: *endpoint, Node: *node, Interval: *interval, Timeout: *timeout}
	if cfg.Endpoint == "" {
		return fmt.Errorf("--endpoint is required")
	}
	if cfg.Node == "" {
		return fmt.Errorf("--node is required (wire it from the downward API spec.nodeName)")
	}
	httpClient, err := Client(cfg.Endpoint, *caFile)
	if err != nil {
		return err
	}
	return Poll(context.Background(), httpClient, cfg, out)
}

// Client returns the client for endpoint: http.DefaultClient for http://, and for
// https:// one trusting only the PEM CA in caFile, verifying the endpoint host as
// server name. An https endpoint without a CA file is an error, never a downgrade.
func Client(endpoint, caFile string) (*http.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parsing readiness endpoint: %w", err)
	}
	if u.Scheme != "https" {
		return http.DefaultClient, nil
	}
	if caFile == "" {
		return nil, fmt.Errorf("endpoint %s is https but no --ca-file names the CA to verify it", endpoint)
	}
	return &http.Client{Transport: &caFileTransport{file: caFile}}, nil
}

// caFileTransport re-reads its CA file per request, so rotation and late mounts
// work without a restart; a bad file fails the request, which is retried.
type caFileTransport struct {
	file string

	mu  sync.Mutex
	pem []byte
	rt  *http.Transport
}

func (t *caFileTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt, err := t.transport()
	if err != nil {
		return nil, err
	}
	return rt.RoundTrip(req)
}

func (t *caFileTransport) transport() (*http.Transport, error) {
	pem, err := os.ReadFile(t.file)
	if err != nil {
		return nil, fmt.Errorf("reading readiness CA: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.rt != nil && bytes.Equal(pem, t.pem) {
		return t.rt, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no PEM CA certificate in %s", t.file)
	}
	if t.rt != nil {
		t.rt.CloseIdleConnections()
	}
	t.rt = http.DefaultTransport.(*http.Transport).Clone()
	// TLS 1.3 only: the manager's readiness endpoint serves nothing older.
	t.rt.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
	t.pem = pem
	return t.rt, nil
}

// Poll hits the readiness endpoint every cfg.Interval until it returns 200 (nil),
// the timeout or ctx ends (error), or a 400 misconfiguration fails fast. Other
// responses are retried. Non-positive Interval/Timeout take defaults.
func Poll(parent context.Context, httpClient *http.Client, cfg PollConfig, out io.Writer) error {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultPollInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultPollTimeout
	}
	ctx, cancel := context.WithTimeout(parent, cfg.Timeout)
	defer cancel()

	target := endpointURL(cfg.Endpoint, cfg.Node)
	// Log write errors are discarded: they must not abort the poll.
	_, _ = fmt.Fprintf(out, "waiting for balloons plugin to be ready on node %q via %s\n", cfg.Node, target)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		ready, retriable, detail := probe(ctx, httpClient, target)
		if ready {
			_, _ = fmt.Fprintf(out, "balloons plugin ready on node %q\n", cfg.Node)
			return nil
		}
		if !retriable {
			return fmt.Errorf("readiness endpoint rejected the request: %s", detail)
		}
		_, _ = fmt.Fprintf(out, "not ready yet (%s); retrying in %s\n", detail, cfg.Interval)

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for balloons plugin on node %q: %w (last: %s)", cfg.Node, ctx.Err(), detail)
		case <-ticker.C:
		}
	}
}

// probe performs one readiness request. A 200 is ready; network errors and responses
// other than 200 or 400 are retriable; a 400 or a request that cannot be built is not.
func probe(ctx context.Context, httpClient *http.Client, target string) (ready, retriable bool, detail string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, false, fmt.Sprintf("building request: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Network errors are transient (manager not up yet).
		return false, true, fmt.Sprintf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return true, false, ""
	}
	// The body is diagnostic only; a failed read does not change the verdict.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	reason := trimBody(body)
	if err != nil {
		reason = fmt.Sprintf("body unreadable: %v", err)
	}
	if resp.StatusCode == http.StatusBadRequest {
		return false, false, fmt.Sprintf("HTTP 400: %s", reason)
	}
	return false, true, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, reason)
}

// endpointURL joins the base endpoint and the node query parameter, tolerating a
// trailing slash and an endpoint that already includes the path.
func endpointURL(endpoint, node string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		// Not a URL: join anyway so the failing request surfaces the bad value.
		return fmt.Sprintf("%s%s?node=%s", endpoint, Path, url.QueryEscape(node))
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = Path
	} else {
		// Strip a trailing slash: the mux registers the exact "/balloons/ready".
		u.Path = strings.TrimRight(u.Path, "/")
	}
	q := u.Query()
	q.Set("node", node)
	u.RawQuery = q.Encode()
	return u.String()
}

// trimBody renders a response body as a compact single-line string for logs.
func trimBody(b []byte) string {
	// One log line: collapse whitespace, then cap the length.
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
