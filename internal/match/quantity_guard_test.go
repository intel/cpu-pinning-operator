// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package match

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// quantityPathBudget bounds parse-then-Cmp for the worst guard-admitted input; Cmp
// is the expensive half. Generous slack for CI, far below the fuzz timeout.
const quantityPathBudget = 5 * time.Second

// comparesWithin reports whether parse-then-compare returns within the budget; on
// timeout the goroutine is deliberately leaked, since the work cannot be cancelled.
func comparesWithin(qty string, budget time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		q, err := resource.ParseQuantity(qty)
		if err != nil {
			return
		}
		one := resource.MustParse("1")
		_ = q.Cmp(one)
	}()
	select {
	case <-done:
		return true
	case <-time.After(budget):
		return false
	}
}

// TestExponentGuardBoundsQuantityPath proves skipUnparseableQuantity rejects what would
// hang and everything it admits terminates within the budget.
func TestExponentGuardBoundsQuantityPath(t *testing.T) {
	// The guard rejects what would hang; corpus seed e75f81a0d02e0d6c carries this string, keep it.
	hangs := []string{
		"92233720E6854775807",
		"1e9999999",             // 7 exponent digits, one past the cap
		"1e0000000000009999999", // leading zeros stripped → still 7 significant
		"9223372036854775807e99999999",
	}
	for _, q := range hangs {
		if !skipUnparseableQuantity(q) {
			t.Errorf("skipUnparseableQuantity(%q) = false; this reaches ParseQuantity and hangs the fuzz harness", q)
		}
	}

	// Everything the guard admits terminates.
	admitted := []string{
		"1e999999", // the worst case the cap allows: 6 exponent digits
		"9e999999",
		"1E999999",             // capital E takes the same path
		"1e0000000000999999",   // leading zeros stripped → 6 significant
		"9223372036854775807",  // int64 max, no exponent
		"-9223372036854775808", // int64 min
		"0.5", "1.5", "250m", "2Gi", "500n", "8", "", "not-a-quantity",
	}
	for _, q := range admitted {
		if skipUnparseableQuantity(q) {
			t.Errorf("skipUnparseableQuantity(%q) = true, but this parses fine; the cap is discarding fuzz coverage", q)
		}
		if !comparesWithin(q, quantityPathBudget) {
			t.Fatalf("parse+Cmp of %q did not return within %v, yet the guard admits it — the 6-digit cap is too loose", q, quantityPathBudget)
		}
	}
}

// TestExponentGuardIgnoresLeadingZeros proves the guard counts significant digits, so 1e0000000000 is admitted.
func TestExponentGuardIgnoresLeadingZeros(t *testing.T) {
	benign := "1e" + strings.Repeat("0", 50) + "9"
	if skipUnparseableQuantity(benign) {
		t.Errorf("skipUnparseableQuantity(%q) = true; 50 leading zeros make it 1e9, not a huge exponent", benign)
	}
	huge := "1e" + strings.Repeat("0", 50) + "1234567"
	if !skipUnparseableQuantity(huge) {
		t.Errorf("skipUnparseableQuantity(%q) = false; stripping zeros leaves 7 significant digits, past the cap", huge)
	}
}
