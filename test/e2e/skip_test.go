// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

// e2eStrictEnv turns a configuration gap into a failure; any non-empty value counts.
const e2eStrictEnv = "E2E_STRICT"

// skipTally counts skips per helper, since `go test` prints no total. Atomic: subtests skip independently.
var skipTally struct {
	noCapability atomic.Int64
	noConfig     atomic.Int64
	notSelected  atomic.Int64
}

// reportSkipTally prints the skip totals after the run; silent when nothing skipped.
func reportSkipTally() {
	hw, cfg, sel := skipTally.noCapability.Load(), skipTally.noConfig.Load(), skipTally.notSelected.Load()
	if hw+cfg+sel == 0 {
		return
	}
	fmt.Printf("e2e: %d skip(s) — %d no capability, %d no configuration, %d not selected\n",
		hw+cfg+sel, hw, cfg, sel)
}

// skipNoCapability skips when the cluster cannot present what the test needs
// (one NUMA node, no hyperthreads, no cluster). Always skips.
func skipNoCapability(t testing.TB, format string, args ...any) {
	t.Helper()
	skipTally.noCapability.Add(1)
	t.Skip(fmt.Sprintf(format, args...)) // bare skip: intentional
}

// skipNoConfig skips when the cluster could run the test but was not set up for it;
// fails under E2E_STRICT.
func skipNoConfig(t testing.TB, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv(e2eStrictEnv) != "" {
		t.Fatalf("%s — %s is set, so a run that cannot exercise this is a failure", msg, e2eStrictEnv)
	}
	skipTally.noConfig.Add(1)
	t.Skip(msg) // bare skip: intentional
}

// skipNotSelected skips a test whose opt-in gate is unset, even under E2E_STRICT.
func skipNotSelected(t testing.TB, format string, args ...any) {
	t.Helper()
	skipTally.notSelected.Add(1)
	t.Skip(fmt.Sprintf(format, args...)) // bare skip: intentional
}

// routingTB records a skip helper's Skip/Fatalf instead of ending the test. Both
// intercepts panic to unwind, because the real calls do not return.
type routingTB struct {
	testing.TB
	skips  int
	fatals int
}

// tbStop is the panic the intercepts raise; recoverStop swallows only it.
const tbStop = "routingTB: the test under check ended here"

func (r *routingTB) Skip(...any)           { r.skips++; panic(tbStop) }
func (r *routingTB) Fatalf(string, ...any) { r.fatals++; panic(tbStop) }

func (r *routingTB) recoverStop() {
	if p := recover(); p != nil && p != tbStop {
		panic(p)
	}
}

// TestMetricsSkipRoutingIsAuthAware proves an empty scrape is not-selected only when
// every replica refused it, and a strict no-config failure otherwise.
func TestMetricsSkipRoutingIsAuthAware(t *testing.T) {
	t.Setenv(e2eStrictEnv, "1")
	for _, tc := range []struct {
		name                          string
		attempted, refused            int
		wantNotSelected, wantNoConfig int64
		wantSkips, wantFatals         int
	}{
		{"every replica refused: a deployment mode", 2, 2, 1, 0, 1, 0},
		{"one refused and one broken: still a broken install", 2, 1, 0, 0, 0, 1},
		{"nothing reachable and nothing refused: a configuration gap", 2, 0, 0, 0, 0, 1},
		{"no ready replica to attempt: a configuration gap", 0, 0, 0, 0, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notSelected, noConfig := skipTally.notSelected.Load(), skipTally.noConfig.Load()
			rec := &routingTB{TB: t}
			authRefused := allScrapesRefused(tc.attempted, tc.refused)
			func() {
				defer rec.recoverStop()
				skipNoMetrics(rec, authRefused, "no metrics endpoint served the scrape")
			}()
			gotNotSelected := skipTally.notSelected.Load() - notSelected
			gotNoConfig := skipTally.noConfig.Load() - noConfig
			// This check's routing calls are not the run's skips, so take them back out
			// of the totals reportSkipTally prints.
			skipTally.notSelected.Add(-gotNotSelected)
			skipTally.noConfig.Add(-gotNoConfig)

			if gotNotSelected != tc.wantNotSelected || gotNoConfig != tc.wantNoConfig {
				t.Errorf("%d of %d replica(s) refused (authRefused=%v) routed to notSelected+%d noConfig+%d, want notSelected+%d noConfig+%d",
					tc.refused, tc.attempted, authRefused, gotNotSelected, gotNoConfig, tc.wantNotSelected, tc.wantNoConfig)
			}
			// skipNoConfig fails without counting, so only this catches a wrong route.
			if rec.skips != tc.wantSkips || rec.fatals != tc.wantFatals {
				t.Errorf("%d of %d replica(s) refused (authRefused=%v) under %s produced %d skip(s) and %d failure(s), want %d and %d",
					tc.refused, tc.attempted, authRefused, e2eStrictEnv, rec.skips, rec.fatals, tc.wantSkips, tc.wantFatals)
			}
		})
	}
}

// requireSourceScan skips when no sources ship with the binary (go test -c), and
// fails when sources exist but the anchor file is missing (a rename disarmed the guard).
func requireSourceScan(t testing.TB, files []string, sawHelperFile bool, helperFile string) {
	t.Helper()
	if len(files) == 0 {
		skipNoCapability(t, "this guard scans the e2e package sources (starting at %s) and the run has none next "+
			"to the test binary: a packaged binary (go test -c + scp to a node without a checkout) ships "+
			"without them, so it is enforced from a checkout by the pre-commit gate instead", helperFile)
	}
	if !sawHelperFile {
		t.Fatalf("scanned %d file(s) and none was %s: the guard is not reading the e2e package",
			len(files), helperFile)
	}
}

// TestSourceScanSkipRoutingNeedsSources proves requireSourceScan's routing holds under E2E_STRICT.
func TestSourceScanSkipRoutingNeedsSources(t *testing.T) {
	t.Setenv(e2eStrictEnv, "1")
	for _, tc := range []struct {
		name                  string
		files                 []string
		sawHelperFile         bool
		wantNoCapability      int64
		wantSkips, wantFatals int
	}{
		{"packaged binary: no sources shipped", nil, false, 1, 1, 0},
		{"sources present but the anchor file was renamed", []string{"helpers_test.go"}, false, 0, 0, 1},
		{"a checkout: scan the package", []string{"skip_test.go", "helpers_test.go"}, true, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noCapability := skipTally.noCapability.Load()
			rec := &routingTB{TB: t}
			func() {
				defer rec.recoverStop()
				requireSourceScan(rec, tc.files, tc.sawHelperFile, skipHelperFile)
			}()
			gotNoCapability := skipTally.noCapability.Load() - noCapability
			// This check's routing calls are not the run's skips, so take them back out
			// of the totals reportSkipTally prints.
			skipTally.noCapability.Add(-gotNoCapability)

			if gotNoCapability != tc.wantNoCapability {
				t.Errorf("%d file(s) scanned (sawHelperFile=%v) routed to noCapability+%d, want noCapability+%d",
					len(tc.files), tc.sawHelperFile, gotNoCapability, tc.wantNoCapability)
			}
			if rec.skips != tc.wantSkips || rec.fatals != tc.wantFatals {
				t.Errorf("%d file(s) scanned (sawHelperFile=%v) under %s produced %d skip(s) and %d failure(s), want %d and %d",
					len(tc.files), tc.sawHelperFile, e2eStrictEnv, rec.skips, rec.fatals, tc.wantSkips, tc.wantFatals)
			}
		})
	}
}

// TestReserveAuthorityModeSkipIsSymmetric proves both reserve-authority mismatches
// route to skipNoConfig with a message naming the Helm value to change.
func TestReserveAuthorityModeSkipIsSymmetric(t *testing.T) {
	t.Setenv(e2eStrictEnv, "1")
	const opNS, name = "cpu-pinning-operator-system", "cpu-pinning-operator-controller-manager"
	for _, tc := range []struct {
		name            string
		hasFlag         bool
		negative        bool
		wantSkip        bool
		wantMentionsSet string // the Helm value the message must point at
	}{
		{"flag wired, positive run: the mode asked for", true, false, false, ""},
		{"flag absent, positive run: a mis-set release", false, false, true, "webhook.reserveAuthority.enabled=false"},
		{"flag absent, negative control: the mode asked for", false, true, false, ""},
		{"flag wired, negative control: the control cannot run here", true, true, true, "webhook.reserveAuthority.enabled=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := raAuthorityModeSkip(opNS, name, tc.hasFlag, tc.negative)
			if (msg != "") != tc.wantSkip {
				t.Fatalf("hasFlag=%v negative=%v produced %q, want a skip reason: %v", tc.hasFlag, tc.negative, msg, tc.wantSkip)
			}
			if !tc.wantSkip {
				return
			}
			// The message must name the Deployment it read and the value to set.
			for _, want := range []string{opNS + "/" + name, raAuthorityFlag, tc.wantMentionsSet} {
				if !strings.Contains(msg, want) {
					t.Errorf("hasFlag=%v negative=%v skip reason does not mention %q: %s", tc.hasFlag, tc.negative, want, msg)
				}
			}

			noConfig := skipTally.noConfig.Load()
			rec := &routingTB{TB: t}
			func() {
				defer rec.recoverStop()
				skipNoConfig(rec, "%s", msg)
			}()
			gotNoConfig := skipTally.noConfig.Load() - noConfig
			skipTally.noConfig.Add(-gotNoConfig) // not one of the run's own skips
			if rec.fatals != 1 || rec.skips != 0 || gotNoConfig != 0 {
				t.Errorf("hasFlag=%v negative=%v under %s produced %d skip(s), %d failure(s), noConfig+%d; want the strict failure skipNoConfig gives",
					tc.hasFlag, tc.negative, e2eStrictEnv, rec.skips, rec.fatals, gotNoConfig)
			}
		})
	}
}

const (
	// scrapeHelperFile defines the scrapers, so it may discard a scrape outcome.
	scrapeHelperFile = "helpers_test.go"
	// scrapeExemption must be the whole tail of the line, like skipExemption.
	scrapeExemption = "// scrape outcome: intentional"
)

// scrapeOutcomeRE matches binding a scrapeMetrics* second value: branching on it
// lets an assertion silently pass having asserted nothing.
var scrapeOutcomeRE = regexp.MustCompile(`,\s*\w+\s*:?=\s*scrapeMetrics\w*\(`)

// TestMetricAssertionsAreNotSilentlyDropped fails on a scrape outcome taken apart
// outside the helper file without the exemption marker.
func TestMetricAssertionsAreNotSilentlyDropped(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob the e2e package: %v", err)
	}
	var sawHelperFile bool
	var dropped []string
	for _, f := range files {
		if f == scrapeHelperFile {
			sawHelperFile = true
			continue
		}
		// This file holds the pattern only as string literals.
		if f == skipHelperFile {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if scrapeOutcomeRE.MatchString(line) && !strings.HasSuffix(strings.TrimSpace(line), scrapeExemption) {
				dropped = append(dropped, fmt.Sprintf("%s:%d", f, i+1))
			}
		}
	}
	requireSourceScan(t, files, sawHelperFile, scrapeHelperFile)
	// Positive control: the guard must still recognise these shapes.
	for _, regressed := range []string{
		"\tafter, ok := scrapeMetricsAllReplicas(t)",
		"\tbefore, haveMetrics := scrapeMetricsAllReplicas(t)",
		"\tbyReplica, ok := scrapeMetricsPerReplica(t)",
	} {
		if !scrapeOutcomeRE.MatchString(regressed) {
			t.Errorf("the guard no longer recognises %q, so it would not catch the shape it exists for", strings.TrimSpace(regressed))
		}
	}
	if len(dropped) > 0 {
		t.Fatalf("%d site(s) take a scrape outcome apart outside %s: assert the metric in a subtest that scrapes with a …OrSkip form, so an "+
			"endpoint that did not serve costs a counted skip instead of a dropped assertion — or end the line with %q:\n\t%s",
			len(dropped), scrapeHelperFile, scrapeExemption, strings.Join(dropped, "\n\t"))
	}
}

// bareSkipRE matches a Skip call on any receiver; it over-matches by design.
var bareSkipRE = regexp.MustCompile(`\b[\w.]+\.(Skipf?|SkipNow)\(`)

const (
	// skipHelperFile is this file; its presence proves the glob read the right directory.
	skipHelperFile = "skip_test.go"
	// skipExemption must be the whole tail of the line.
	skipExemption = "// bare skip: intentional"
)

// TestNoBareSkips fails on a Skip call that bypasses the three skip helpers.
func TestNoBareSkips(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob the e2e package: %v", err)
	}
	var sawHelperFile bool
	var bare []string
	for _, f := range files {
		if f == skipHelperFile {
			sawHelperFile = true
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if bareSkipRE.MatchString(line) && !strings.HasSuffix(strings.TrimSpace(line), skipExemption) {
				bare = append(bare, fmt.Sprintf("%s:%d", f, i+1))
			}
		}
	}
	// Distinguish "clean" from "scanned nothing".
	requireSourceScan(t, files, sawHelperFile, skipHelperFile)
	if len(bare) > 0 {
		t.Fatalf("%d bare Skip call(s) among the %d file(s) scanned: route them through "+
			"skipNoCapability / skipNoConfig / skipNotSelected, or end the line with %q:\n\t%s",
			len(bare), len(files), skipExemption, strings.Join(bare, "\n\t"))
	}
}
