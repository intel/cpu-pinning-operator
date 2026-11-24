// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ozontech/allure-go/pkg/allure"
)

// allurePackagePath is the FullName prefix; the historyId derives from it, so it must stay stable.
const allurePackagePath = "github.com/intel/cpu-pinning-operator/test/e2e"

// allureT wraps a *testing.T and captures the Log/Error/Fatal/Skip messages routed
// through it for the Allure report, because testing.T cannot read them back.
type allureT struct {
	*testing.T

	mu           sync.Mutex
	log          strings.Builder // full captured message stream, in call order
	firstFailure string          // first Error/Fatal message (the headline reason)
	skipReason   string          // last Skip message
}

// capture appends msg to the buffer and records the headline reason; kind is "FAIL", "SKIP" or "LOG".
func (a *allureT) capture(kind, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.log.WriteString(msg)
	if !strings.HasSuffix(msg, "\n") {
		a.log.WriteByte('\n')
	}
	switch kind {
	case "FAIL":
		if a.firstFailure == "" {
			a.firstFailure = msg
		}
	case "SKIP":
		a.skipReason = msg
	}
}

// Each method marks itself a helper, then captures before forwarding, because
// Fatal* and Skip* do not return.

func (a *allureT) Log(args ...any) {
	a.T.Helper()
	a.capture("LOG", fmt.Sprint(args...))
	a.T.Log(args...)
}
func (a *allureT) Logf(f string, a2 ...any) {
	a.T.Helper()
	a.capture("LOG", fmt.Sprintf(f, a2...))
	a.T.Logf(f, a2...)
}

func (a *allureT) Error(args ...any) {
	a.T.Helper()
	a.capture("FAIL", fmt.Sprint(args...))
	a.T.Error(args...)
}
func (a *allureT) Errorf(f string, a2 ...any) {
	a.T.Helper()
	a.capture("FAIL", fmt.Sprintf(f, a2...))
	a.T.Errorf(f, a2...)
}

func (a *allureT) Fatal(args ...any) {
	a.T.Helper()
	a.capture("FAIL", fmt.Sprint(args...))
	a.T.Fatal(args...)
}
func (a *allureT) Fatalf(f string, a2 ...any) {
	a.T.Helper()
	a.capture("FAIL", fmt.Sprintf(f, a2...))
	a.T.Fatalf(f, a2...)
}

func (a *allureT) Skip(args ...any) {
	a.T.Helper()
	a.capture("SKIP", fmt.Sprint(args...))
	a.T.Skip(args...) // bare skip: intentional
}
func (a *allureT) Skipf(f string, a2 ...any) {
	a.T.Helper()
	a.capture("SKIP", fmt.Sprintf(f, a2...))
	a.T.Skipf(f, a2...) // bare skip: intentional
}

// allureCase records the calling test as one Allure result, written on cleanup, and
// returns the capturing wrapper. Call it first in a test body. It never fails the test.
func allureCase(t *testing.T) *allureT {
	t.Helper()
	at := &allureT{T: t}
	start := allure.GetNow()
	name := t.Name()
	t.Cleanup(func() {
		res := newAllureResult(name, os.Getenv("E2E_PASS"))
		res.Start = start
		res.WithFrameWork("Go testing").
			WithPackage("test/e2e").
			WithSuite("e2e")

		at.mu.Lock()
		captured := at.log.String()
		firstFailure := at.firstFailure
		skipReason := at.skipReason
		at.mu.Unlock()

		status, msg := allureStatus(t, firstFailure, skipReason)
		res.Status = status
		res.SetStatusMessage(msg)

		if captured != "" {
			res.SetStatusTrace(captured)
			res.Attachments = append(res.Attachments,
				allure.NewAttachment("test log", allure.Text, []byte(captured)))
		}

		res.Finish()
		if err := res.Print(); err != nil {
			t.Logf("allure: failed to write result for %q: %v", name, err)
		}
	})
	return at
}

// newAllureResult builds the result for test name. A non-empty pass (E2E_PASS, set by
// hack/e2e-validation.sh) goes into the name, a "pass" parameter and the FullName, so
// testCaseId and historyId differ per pass and one pass never shows as a retry of
// another. An empty pass gives the same result as before the pass existed.
func newAllureResult(name, pass string) *allure.Result {
	if pass == "" {
		return allure.NewResult(name, fmt.Sprintf("%s.%s", allurePackagePath, name))
	}
	res := allure.NewResult(fmt.Sprintf("%s [%s]", name, pass), fmt.Sprintf("%s.%s[%s]", allurePackagePath, name, pass))
	res.Parameters = append(res.Parameters, allure.NewParameter("pass", pass))
	return res
}

// verdict is the part of *testing.T that allureStatus reads.
type verdict interface {
	Failed() bool
	Skipped() bool
}

// allureStatus maps a verdict to an Allure status and headline. Failed is read first:
// Errorf then Skipf leaves both true, and that is a failure.
func allureStatus(v verdict, firstFailure, skipReason string) (allure.Status, string) {
	switch {
	case v.Failed():
		msg := firstNonEmpty(firstFailure, "test failed (no failure message captured; route the failing assertion through the allureCase helper to surface it)")
		if v.Skipped() {
			msg = "failed, then skipped: " + msg
		}
		return allure.Failed, msg
	case v.Skipped():
		return allure.Skipped, firstNonEmpty(skipReason, "test skipped (no reason captured; route the skip through the allureCase helper to surface it)")
	}
	return allure.Passed, ""
}

// firstNonEmpty returns the first non-empty string of its arguments.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
