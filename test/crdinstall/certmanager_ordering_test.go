// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package crdinstall

import (
	"context"
	"fmt"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/intel/cpu-pinning-operator/test/ctrlplane"
)

// certManagerPrereqCRDs builds stub cert-manager.io/v1 Issuer and Certificate CRDs with
// an open spec, standing in for the prerequisite cert-manager install.
func certManagerPrereqCRDs() []*apiextensionsv1.CustomResourceDefinition {
	openSpec := apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"spec": {
				Type:                   "object",
				XPreserveUnknownFields: ptrBool(true),
			},
		},
	}
	crd := func(plural, singular, kind string) *apiextensionsv1.CustomResourceDefinition {
		return &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: plural + ".cert-manager.io"},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "cert-manager.io",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Plural:   plural,
					Singular: singular,
					Kind:     kind,
					ListKind: kind + "List",
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
					Name:    "v1",
					Served:  true,
					Storage: true,
					Schema:  &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &openSpec},
				}},
			},
		}
	}
	return []*apiextensionsv1.CustomResourceDefinition{
		crd("issuers", "issuer", "Issuer"),
		crd("certificates", "certificate", "Certificate"),
	}
}

func ptrBool(b bool) *bool { return &b }

// startEnvWithCertManagerPrereq boots an apiserver with the operator and cert-manager CRDs.
func startEnvWithCertManagerPrereq(t *testing.T) (client.Client, func()) {
	t.Helper()
	return ctrlplane.Start(t, &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir(t)},
		CRDs:                  certManagerPrereqCRDs(),
		ErrorIfCRDPathMissing: true,
		Scheme:                operatorScheme(),
	})
}

// issuerObj mirrors the chart's selfSigned Issuer as an unstructured object.
func issuerObj(ns string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("cert-manager.io/v1")
	u.SetKind("Issuer")
	u.SetNamespace(ns)
	u.SetName("cpu-pinning-operator-selfsigned-issuer")
	_ = unstructured.SetNestedMap(u.Object, map[string]any{}, "spec", "selfSigned")
	return u
}

func certificateObj(ns string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("cert-manager.io/v1")
	u.SetKind("Certificate")
	u.SetNamespace(ns)
	u.SetName("cpu-pinning-operator-serving-cert")
	u.Object["spec"] = map[string]any{
		"secretName": "webhook-server-cert",
		"dnsNames": []any{
			fmt.Sprintf("cpu-pinning-operator-webhook.%s.svc", ns),
			fmt.Sprintf("cpu-pinning-operator-webhook.%s.svc.cluster.local", ns),
		},
		"issuerRef": map[string]any{
			"kind": "Issuer",
			"name": "cpu-pinning-operator-selfsigned-issuer",
		},
	}
	return u
}

// TestCertManagerPrerequisiteResolvesOperatorCertResources proves the operator's Issuer
// and Certificate apply once the cert-manager CRDs are installed.
func TestCertManagerPrerequisiteResolvesOperatorCertResources(t *testing.T) {
	c, stop := startEnvWithCertManagerPrereq(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "default"
	for _, obj := range []*unstructured.Unstructured{issuerObj(ns), certificateObj(ns)} {
		obj := obj
		t.Run(obj.GetKind(), func(t *testing.T) {
			if err := c.Create(ctx, obj); err != nil {
				if apimeta.IsNoMatchError(err) {
					t.Fatalf("%s did not resolve even though the cert-manager prerequisite CRDs are installed — the operator's serving-cert resources must apply once cert-manager is present: %v", obj.GetKind(), err)
				}
				t.Fatalf("create %s: %v", obj.GetKind(), err)
			}
			_ = c.Delete(context.Background(), obj)
		})
	}
}

// TestWithoutCertManagerPrerequisiteOperatorCertFailsNoMatch proves the Certificate fails
// with NoKindMatchError without cert-manager: the chart ships no cert-manager CRDs.
func TestWithoutCertManagerPrerequisiteOperatorCertFailsNoMatch(t *testing.T) {
	// Operator CRDs only, no cert-manager.
	c, stop := startEnv(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := c.Create(ctx, certificateObj("default"))
	if err == nil {
		t.Fatal("expected the operator's Certificate to FAIL to apply without the cert-manager prerequisite, but it was admitted")
	}
	if !apimeta.IsNoMatchError(err) {
		t.Fatalf("expected a NoKindMatchError (cert-manager.io/v1 not registered — install cert-manager first), got: %v", err)
	}
}
