// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/go-logr/logr"
)

// FuzzServeHTTP proves arbitrary request targets get a documented status and a
// decodable body with ready=false unless the status is 200.
func FuzzServeHTTP(f *testing.F) {
	f.Add("?node=node-a")
	f.Add("?node=")
	f.Add("")
	f.Add("?node=node-a&node=other")
	f.Add("?node=%00%ff")
	f.Add("?node=" + strings.Repeat("a", 512))
	f.Add("?nodes=node-a;node=node-b")
	f.Add(" ")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		f.Fatalf("add client-go scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
		balloonsDaemonSet(),
		balloonsPod("node-a", true, false),
	).Build()
	h := &Handler{Client: c, Namespace: testNamespace, DaemonSet: testDaemonSet, Log: logr.Discard()}

	f.Fuzz(func(t *testing.T, query string) {
		target := Path + query
		u, err := url.ParseRequestURI(target)
		if err != nil {
			// net/http rejects this before any handler sees it.
			return
		}
		// Assign the URL: httptest.NewRequest panics on targets a real server rejects earlier.
		req := httptest.NewRequest(http.MethodGet, Path, nil)
		req.URL = u

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		switch rec.Code {
		case http.StatusOK, http.StatusBadRequest, http.StatusServiceUnavailable, http.StatusTooManyRequests:
		default:
			t.Fatalf("query %q produced status %d, outside the documented set", query, rec.Code)
		}

		var res result
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("query %q produced a body that is not JSON: %v (%q)", query, err, rec.Body.String())
		}
		if res.Ready != (rec.Code == http.StatusOK) {
			t.Fatalf("query %q returned status %d with ready=%v", query, rec.Code, res.Ready)
		}
		if !res.Ready && res.Reason == "" {
			t.Fatalf("query %q reported not-ready without a reason the init container can log", query)
		}
		// The echoed node must be the one asked about; JSON mangles invalid UTF-8,
		// so only valid names are compared.
		want := requestedNode(t, target)
		if utf8.ValidString(want) && res.Node != want && rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q answered for node %q, want %q", query, res.Node, want)
		}
	})
}

// requestedNode returns the node query parameter of a request target.
func requestedNode(t *testing.T, target string) string {
	t.Helper()
	u, err := url.ParseRequestURI(target)
	if err != nil {
		t.Fatalf("parsing %q: %v", target, err)
	}
	return u.Query().Get("node")
}
