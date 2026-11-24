// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const wcaGate = "E2E_WEBHOOK_CA"

// wcaProbeScript only fetches the chain (-k): the verdict is Go's, against the bundles.
// %{certs} needs curl 7.88+ on an OpenSSL backend, which the default image has.
const wcaProbeScript = `curl -sS -k -m 10 -o /dev/null -w '%{certs}' "https://$WEBHOOK_HOST/"`

// TestWebhookServingCertChainsToInjectedCABundle proves the cert the webhook mounts
// verifies, for its Service name, against every caBundle, and is the cert the Service presents.
func TestWebhookServingCertChainsToInjectedCABundle(t *testing.T) {
	at := allureCase(t)
	if os.Getenv(wcaGate) == "" {
		skipNotSelected(at, "set %s=1 to verify the webhook's serving certificate against the injected caBundles", wcaGate)
	}
	c := newClient(at)
	ctx := context.Background()
	opNS := operatorNamespace()

	webhookHost, _ := wiServiceHosts(at, c, opNS)
	svcName, _, _ := strings.Cut(webhookHost, ".")
	dnsName := svcName + "." + opNS + ".svc"
	bundles := wcaBundles(at, c, opNS, svcName)

	// The live Deployment, not chart values: the Secret the manager actually serves.
	var dep appsv1.Deployment
	depKey := client.ObjectKey{Namespace: opNS, Name: env("E2E_OPERATOR_DEPLOYMENT", operatorDeployment)}
	if err := c.Get(ctx, depKey, &dep); err != nil {
		skipNoConfig(at, "read operator Deployment %s (set E2E_OPERATOR_DEPLOYMENT): %v", depKey, err)
	}
	secretName, err := wcaCertSecret(&dep)
	if err != nil {
		at.Fatalf("Deployment %s: %v", depKey, err)
	}
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: opNS, Name: secretName}, &secret); err != nil {
		skipNoConfig(at, "read serving cert Secret %s/%s (the e2e kubeconfig needs get on secrets there): %v", opNS, secretName, err)
	}
	mounted := secret.Data["tls.crt"]
	for _, problem := range wcaChainVerdict(bundles, mounted, dnsName) {
		at.Error(problem)
	}
	at.Logf("%d caBundle(s) checked against Secret %s/%s for %s; identical: %v",
		len(bundles), opNS, secretName, dnsName, wcaIdentical(bundles))

	t.Run("presented chain is the mounted cert", func(t *testing.T) {
		st := allureCase(t)
		if wiIngressRestricted(st, c, opNS, "webhook-server") {
			skipNotSelected(st, "networkPolicy.webhook.from closes the webhook port to pods, so no probe pod can read the "+
				"certificate it presents; the mounted Secret was still verified")
		}
		code, log := wiRunProbe(st, c, "cpu-pinning-e2e-webhook-ca", "webhook-ca-probe", wcaProbeScript,
			corev1.EnvVar{Name: "WEBHOOK_HOST", Value: webhookHost})
		if code != 0 {
			skipNoConfig(st, "the probe pod could not complete a TLS handshake with %s (curl exit %d): %s", webhookHost, code, log)
		}
		if len(wcaPEMCerts([]byte(log))) == 0 {
			skipNoConfig(st, "the probe's curl printed no certificate chain; %%{certs} needs curl 7.88+ on an OpenSSL backend, "+
				"set E2E_WEBHOOK_PROBE_IMAGE to such an image. Log:\n%s", log)
		}
		if err := wcaSameLeaf(mounted, []byte(log)); err != nil {
			st.Errorf("%s: %v", webhookHost, err)
		}
	})
}

// wcaDefaultCertDir is cmd/main.go's --cert-dir default.
const wcaDefaultCertDir = "/tmp/k8s-webhook-server/serving-certs"

// wcaCertSecret names the Secret mounted at the manager's --cert-dir.
func wcaCertSecret(dep *appsv1.Deployment) (string, error) {
	spec := dep.Spec.Template.Spec
	if len(spec.Containers) == 0 {
		return "", fmt.Errorf("no containers")
	}
	ctr, dir := spec.Containers[0], wcaDefaultCertDir
	for _, cand := range spec.Containers {
		for i, arg := range cand.Args {
			if v, ok := strings.CutPrefix(arg, "--cert-dir="); ok {
				ctr, dir = cand, v
			} else if arg == "--cert-dir" && i+1 < len(cand.Args) {
				ctr, dir = cand, cand.Args[i+1]
			}
		}
	}
	dir = strings.TrimRight(dir, "/")
	for _, m := range ctr.VolumeMounts {
		if strings.TrimRight(m.MountPath, "/") != dir {
			continue
		}
		for _, v := range spec.Volumes {
			if v.Name == m.Name && v.Secret != nil {
				return v.Secret.SecretName, nil
			}
		}
		return "", fmt.Errorf("container %s mounts %s from volume %q, which is not a Secret", ctr.Name, dir, m.Name)
	}
	return "", fmt.Errorf("container %s mounts nothing at its --cert-dir %s", ctr.Name, dir)
}

// wcaSameLeaf fails unless both PEM texts start with the same certificate.
func wcaSameLeaf(mounted, presented []byte) error {
	m, p := wcaPEMCerts(mounted), wcaPEMCerts(presented)
	if len(m) == 0 || len(p) == 0 {
		return fmt.Errorf("mounted %d and presented %d certificate(s); need a leaf on both sides", len(m), len(p))
	}
	if fm, fp := sha256.Sum256(m[0].Raw), sha256.Sum256(p[0].Raw); fm != fp {
		return fmt.Errorf("presents leaf sha256 %x (subject %q), but the mounted Secret's leaf is sha256 %x (subject %q)",
			fp, p[0].Subject, fm, m[0].Subject)
	}
	return nil
}

// wcaBundles returns the caBundle of every mutating and validating webhook that calls
// the Service, keyed by kind/config/webhook; both kinds must be present.
func wcaBundles(t testing.TB, c client.Client, ns, svc string) map[string][]byte {
	t.Helper()
	ctx := context.Background()
	calls := func(cc admissionregistrationv1.WebhookClientConfig) bool {
		return cc.Service != nil && cc.Service.Namespace == ns && cc.Service.Name == svc
	}
	bundles := map[string][]byte{}
	var mutating admissionregistrationv1.MutatingWebhookConfigurationList
	if err := c.List(ctx, &mutating); err != nil {
		t.Fatalf("list MutatingWebhookConfigurations: %v", err)
	}
	for _, cfg := range mutating.Items {
		for _, wh := range cfg.Webhooks {
			if calls(wh.ClientConfig) {
				bundles["mutating "+cfg.Name+"/"+wh.Name] = wh.ClientConfig.CABundle
			}
		}
	}
	var validating admissionregistrationv1.ValidatingWebhookConfigurationList
	if err := c.List(ctx, &validating); err != nil {
		t.Fatalf("list ValidatingWebhookConfigurations: %v", err)
	}
	for _, cfg := range validating.Items {
		for _, wh := range cfg.Webhooks {
			if calls(wh.ClientConfig) {
				bundles["validating "+cfg.Name+"/"+wh.Name] = wh.ClientConfig.CABundle
			}
		}
	}
	kinds := map[string]bool{}
	for key := range bundles {
		kinds[strings.Fields(key)[0]] = true
	}
	if !kinds["mutating"] || !kinds["validating"] {
		t.Fatalf("webhooks calling Service %s/%s: %v — the chart installs a mutating and a validating one", ns, svc, kinds)
	}
	return bundles
}

// wcaPEMCerts returns the CERTIFICATE blocks in b; text around them is skipped.
func wcaPEMCerts(b []byte) []*x509.Certificate {
	// pem.Decode wants BEGIN at a line start; curl may print it after a "Cert:" label.
	b = bytes.ReplaceAll(b, []byte("-----BEGIN "), []byte("\n-----BEGIN "))
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		if block, b = pem.Decode(b); block == nil {
			return certs
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
			certs = append(certs, cert)
		}
	}
}

// wcaChainVerdict lists every bundle the presented chain does not verify against
// for dnsName as a server; empty means all hold.
func wcaChainVerdict(bundles map[string][]byte, presented []byte, dnsName string) []string {
	chain := wcaPEMCerts(presented)
	if len(chain) == 0 {
		return []string{"the webhook port presented no certificate"}
	}
	if len(bundles) == 0 {
		return []string{"no caBundle to verify against"}
	}
	intermediates := x509.NewCertPool()
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}
	keys := make([]string, 0, len(bundles))
	for key := range bundles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var problems []string
	for _, key := range keys {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(bundles[key]) {
			problems = append(problems, fmt.Sprintf("%s: caBundle holds no PEM certificate (%d bytes)", key, len(bundles[key])))
			continue
		}
		_, err := chain[0].Verify(x509.VerifyOptions{DNSName: dnsName, Roots: roots, Intermediates: intermediates})
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: the serving certificate (subject %q, SANs %v) does not verify for %s: %v",
				key, chain[0].Subject, chain[0].DNSNames, dnsName, err))
		}
	}
	return problems
}

// wcaIdentical reports whether every bundle carries the same bytes.
func wcaIdentical(bundles map[string][]byte) bool {
	var first []byte
	for _, b := range bundles {
		if first == nil {
			first = b
		} else if !bytes.Equal(first, b) {
			return false
		}
	}
	return true
}

// TestServingChainVerdict proves wcaChainVerdict accepts only a chain to every bundle
// with the Service name as SAN. No cluster.
func TestServingChainVerdict(t *testing.T) {
	const dnsName = "op-webhook-service.op.svc"
	ca, caKey, caPEM := wcaTestCert(t, "ca", nil, nil, nil)
	_, _, otherPEM := wcaTestCert(t, "other-ca", nil, nil, nil)
	_, _, leafPEM := wcaTestCert(t, "leaf", []string{dnsName}, ca, caKey)
	_, _, wrongSANPEM := wcaTestCert(t, "leaf", []string{"elsewhere.op.svc"}, ca, caKey)
	// curl's %{certs} wraps each PEM block in detail lines.
	presented := append(append([]byte("Subject:CN=leaf\nCert:"), leafPEM...), "\nSubject:CN=ca\nCert:"+string(caPEM)...)

	for _, tc := range []struct {
		name      string
		bundles   map[string][]byte
		presented []byte
		problems  int
	}{
		{"both bundles hold the signing CA", map[string][]byte{"mutating m/w": caPEM, "validating v/w": caPEM}, presented, 0},
		{"the validating bundle is another CA", map[string][]byte{"mutating m/w": caPEM, "validating v/w": otherPEM}, presented, 1},
		{"the chart's placeholder bundle", map[string][]byte{"mutating m/w": []byte("\n"), "validating v/w": caPEM}, presented, 1},
		{"the SAN is not the Service name", map[string][]byte{"mutating m/w": caPEM, "validating v/w": caPEM}, wrongSANPEM, 2},
		{"no certificate presented", map[string][]byte{"mutating m/w": caPEM}, []byte("curl: (35) handshake failed"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wcaChainVerdict(tc.bundles, tc.presented, dnsName); len(got) != tc.problems {
				t.Errorf("wcaChainVerdict() = %q, want %d problem(s)", got, tc.problems)
			}
		})
	}

	t.Run("presented leaf matches the mounted one", func(t *testing.T) {
		if err := wcaSameLeaf(append(append([]byte{}, leafPEM...), caPEM...), presented); err != nil {
			t.Errorf("same leaf: %v", err)
		}
		if err := wcaSameLeaf(wrongSANPEM, presented); err == nil {
			t.Error("a different leaf from the same CA was accepted")
		}
		if err := wcaSameLeaf(leafPEM, []byte("curl: (7) refused")); err == nil {
			t.Error("nothing presented was accepted")
		}
	})

	t.Run("Secret resolved from the live --cert-dir mount", func(t *testing.T) {
		dep := func(args []string, mountPath string, vol corev1.VolumeSource) *appsv1.Deployment {
			d := &appsv1.Deployment{}
			d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "manager", Args: args,
				VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}, {Name: "cert", MountPath: mountPath}}}}
			d.Spec.Template.Spec.Volumes = []corev1.Volume{
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "cert", VolumeSource: vol}}
			return d
		}
		secret := corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "webhook-server-cert"}}
		emptyDir := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
		for _, tc := range []struct {
			name    string
			d       *appsv1.Deployment
			want    string
			wantErr bool
		}{
			{"--cert-dir=", dep([]string{"--cert-dir=/certs/"}, "/certs", secret), "webhook-server-cert", false},
			{"--cert-dir as two args", dep([]string{"--cert-dir", "/certs"}, "/certs", secret), "webhook-server-cert", false},
			{"the binary's default dir", dep(nil, wcaDefaultCertDir, secret), "webhook-server-cert", false},
			{"nothing at --cert-dir", dep([]string{"--cert-dir=/certs"}, "/elsewhere", secret), "", true},
			{"not a Secret", dep([]string{"--cert-dir=/certs"}, "/certs", emptyDir), "", true},
		} {
			got, err := wcaCertSecret(tc.d)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("%s: wcaCertSecret() = %q, %v; want %q, error %v", tc.name, got, err, tc.want, tc.wantErr)
			}
		}
	})
}

// wcaTestCert issues a cert signed by parent, or a self-signed CA when parent is nil.
func wcaTestCert(t testing.TB, cn string, dnsNames []string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	signer, signerKey := tmpl, key
	if parent == nil {
		tmpl.IsCA, tmpl.BasicConstraintsValid = true, true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	} else {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
