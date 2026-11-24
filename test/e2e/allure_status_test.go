// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ozontech/allure-go/pkg/allure"
)

// allureChildEnv arms the child fixture; only TestAllureFailedThenSkippedIsFailed sets it.
const allureChildEnv = "CPU_PINNING_ALLURE_ADAPTER_CHILD"

// fakeVerdict is a verdict with no testing.T behind it.
type fakeVerdict struct{ failed, skipped bool }

func (f fakeVerdict) Failed() bool  { return f.failed }
func (f fakeVerdict) Skipped() bool { return f.skipped }

// TestAllureStatus pins the verdict truth table: failed-and-skipped is FAILED, and the
// status comes from the verdict, not from which message was captured.
func TestAllureStatus(t *testing.T) {
	allureCase(t)
	for _, tc := range []struct {
		name         string
		v            fakeVerdict
		firstFailure string
		skipReason   string
		wantStatus   allure.Status
		wantMsgHas   string
	}{
		{"failed and skipped", fakeVerdict{failed: true, skipped: true}, "assert blew up", "gate not set",
			allure.Failed, "failed, then skipped: assert blew up"},
		// The skip reason must not become the failure headline.
		{"failed and skipped, only the skip reason captured", fakeVerdict{failed: true, skipped: true}, "", "gate not set",
			allure.Failed, "failed, then skipped: test failed (no failure message captured"},
		{"failed, message not captured", fakeVerdict{failed: true}, "", "",
			allure.Failed, "no failure message captured"},
		{"failed", fakeVerdict{failed: true}, "assert blew up", "",
			allure.Failed, "assert blew up"},
		{"skipped", fakeVerdict{skipped: true}, "", "gate not set",
			allure.Skipped, "gate not set"},
		{"skipped, reason not captured", fakeVerdict{skipped: true}, "", "",
			allure.Skipped, "no reason captured"},
		{"passed", fakeVerdict{}, "", "", allure.Passed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, msg := allureStatus(tc.v, tc.firstFailure, tc.skipReason)
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
			if !strings.Contains(msg, tc.wantMsgHas) {
				t.Errorf("message = %q, want it to contain %q", msg, tc.wantMsgHas)
			}
			if tc.wantMsgHas == "" && msg != "" {
				t.Errorf("message = %q, want empty so a passing result carries no status message", msg)
			}
		})
	}
}

// Distinct strings so the parent's exact-message assertion catches a swapped
// failure and skip reason.
const (
	childFailure = "deliberate assertion failure"
	childSkip    = "deliberate skip after the failure"
)

// TestAllureAdapterChildFixture is the scenario TestAllureFailedThenSkippedIsFailed
// runs in a child process. It relies on TestMain touching no cluster.
func TestAllureAdapterChildFixture(t *testing.T) {
	if os.Getenv(allureChildEnv) != "1" {
		skipNotSelected(t, "child fixture of TestAllureFailedThenSkippedIsFailed; runs only in its child process")
	}
	at := allureCase(t)
	at.Error(childFailure)
	// Bare t on purpose: the wrapper must read Skipped() from testing.T, not its buffer.
	t.Skip(childSkip) // bare skip: intentional
}

// TestAllureFailedThenSkippedIsFailed proves a failed-then-skipped test is published
// as FAILED. The fixture runs in a child process because t.Fail propagates to the parent.
func TestAllureFailedThenSkippedIsFailed(t *testing.T) {
	at := allureCase(t)
	out := t.TempDir()
	// A directly executed test binary has no default timeout. Later Cmd.Env duplicates win.
	cmd := exec.Command(os.Args[0], "-test.run=^TestAllureAdapterChildFixture$", "-test.v", "-test.count=1", "-test.timeout=60s")
	cmd.Env = append(os.Environ(),
		allureChildEnv+"=1",
		"ALLURE_OUTPUT_PATH="+out,
		"ALLURE_OUTPUT_FOLDER=allure-results",
	)
	childLog, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(childLog), "--- FAIL: TestAllureAdapterChildFixture") {
		at.Fatalf("child was expected to report the fixture as FAILED (exit %v); child log:\n%s", err, childLog)
	}

	results, globErr := filepath.Glob(filepath.Join(out, "allure-results", "*-result.json"))
	if globErr != nil {
		at.Fatalf("glob: %v", globErr)
	}
	if len(results) != 1 {
		at.Fatalf("want exactly 1 allure result, got %d %v; child log:\n%s", len(results), results, childLog)
	}
	raw, err := os.ReadFile(results[0])
	if err != nil {
		at.Fatalf("read result: %v", err)
	}
	var res struct {
		Status        string `json:"status"`
		StatusDetails struct {
			Message string `json:"message"`
		} `json:"statusDetails"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		at.Fatalf("decode result: %v", err)
	}

	if res.Status != string(allure.Failed) {
		at.Errorf("failed-then-skipped test published as %q, want %q: allureStatus must read Failed() before Skipped(), otherwise a real assertion failure is reported as an intentional skip. Result JSON:\n%s",
			res.Status, allure.Failed, raw)
	}
	// Exact match: pins the prefix and that the captured failure, not the skip reason, is used.
	if want := "failed, then skipped: " + childFailure; res.StatusDetails.Message != want {
		at.Errorf("status message is %q, want %q. Result JSON:\n%s", res.StatusDetails.Message, want, raw)
	}
}

// TestAllurePassName pins the per-pass identity: each E2E_PASS gets its own name,
// "pass" parameter and historyId, and an unset pass leaves the result unchanged.
func TestAllurePassName(t *testing.T) {
	at := allureCase(t)
	plain := newAllureResult("TestX", "")
	if plain.Name != "TestX" || plain.FullName != allurePackagePath+".TestX" || len(plain.Parameters) != 0 {
		at.Errorf("unset pass changed the result: name %q, fullName %q, parameters %v", plain.Name, plain.FullName, plain.Parameters)
	}
	if want := allure.NewResult("TestX", allurePackagePath+".TestX"); plain.HistoryID != want.HistoryID || plain.TestCaseID != want.TestCaseID {
		at.Errorf("unset pass changed historyId/testCaseId: got %s/%s, want %s/%s", plain.HistoryID, plain.TestCaseID, want.HistoryID, want.TestCaseID)
	}
	hard, scrape := newAllureResult("TestX", "hardened"), newAllureResult("TestX", "scrape")
	if hard.Name != "TestX [hardened]" || hard.FullName != allurePackagePath+".TestX[hardened]" {
		at.Errorf("hardened result: name %q, fullName %q", hard.Name, hard.FullName)
	}
	if len(hard.Parameters) != 1 || hard.Parameters[0].Name != "pass" || hard.Parameters[0].GetValue() != "hardened" {
		at.Errorf("hardened parameters = %v, want one pass=hardened", hard.Parameters)
	}
	for _, r := range []*allure.Result{hard, scrape} {
		if r.HistoryID == plain.HistoryID || r.TestCaseID == plain.TestCaseID {
			at.Errorf("%s shares historyId/testCaseId with the unset pass", r.Name)
		}
	}
	if hard.HistoryID == scrape.HistoryID || hard.TestCaseID == scrape.TestCaseID {
		at.Errorf("hardened and scrape share historyId/testCaseId %s/%s: one would show as a retry of the other", hard.HistoryID, hard.TestCaseID)
	}
}
