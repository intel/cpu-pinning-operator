// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package chart_test

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every expected version is parsed from Chart.lock or the Makefile at run time, so these
// tests are not one more place a bump has to land.
const (
	chartLockRel    = "deploy/cpu-pinning-operator/Chart.lock"
	chartYAMLRel    = "deploy/cpu-pinning-operator/Chart.yaml"
	chartArchiveRel = "deploy/cpu-pinning-operator/charts"
	makefileRel     = "Makefile"
	upgradeDocRel   = "docs/upgrade.md"
)

// semverPattern matches a three-part version with an optional leading v.
var semverPattern = regexp.MustCompile(`v?[0-9]+\.[0-9]+\.[0-9]+`)

var (
	envtestPattern   = regexp.MustCompile(`(?m)^ENVTEST_K8S_VERSION\s*\?=\s*(\S+)`)
	archivePattern   = regexp.MustCompile(`[A-Za-z0-9._/:*-]*\.tgz`)
	tracePathPattern = regexp.MustCompile("`([A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)+)`")
)

// subchart is one resolved chart dependency.
type subchart struct {
	name    string
	version string
}

// repoFile resolves a repository-relative path from this package's directory.
func repoFile(rel string) string {
	return filepath.Join("..", "..", filepath.FromSlash(rel))
}

// readRepoFile reads a repository-relative file, failing if it is gone: a deleted
// restatement is drift too.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(repoFile(rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(body)
}

// lockedSubcharts returns the dependencies Chart.lock resolves. The lock, not Chart.yaml,
// because Chart.yaml may hold a range.
func lockedSubcharts(t *testing.T) []subchart {
	t.Helper()
	var lock struct {
		Dependencies []struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		} `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, chartLockRel)), &lock); err != nil {
		t.Fatalf("parsing %s: %v", chartLockRel, err)
	}
	if len(lock.Dependencies) == 0 {
		t.Fatalf("%s resolves no dependencies, so there is nothing to check restatements against", chartLockRel)
	}
	deps := make([]subchart, 0, len(lock.Dependencies))
	for _, d := range lock.Dependencies {
		if d.Name == "" || d.Version == "" {
			t.Fatalf("%s has a dependency with an empty name or version: %+v", chartLockRel, d)
		}
		deps = append(deps, subchart{name: d.Name, version: d.Version})
	}
	return deps
}

// lockedVersions indexes the resolved dependencies by name.
func lockedVersions(t *testing.T) map[string]string {
	t.Helper()
	byName := map[string]string{}
	for _, d := range lockedSubcharts(t) {
		byName[d.name] = d.version
	}
	return byName
}

// envtestVersion returns the API-server version the Makefile pins.
func envtestVersion(t *testing.T) string {
	t.Helper()
	m := envtestPattern.FindStringSubmatch(readRepoFile(t, makefileRel))
	if m == nil {
		t.Fatalf("%s no longer assigns ENVTEST_K8S_VERSION, so the version it pins cannot be read", makefileRel)
	}
	return m[1]
}

// archivesByDep maps each vendored archive to the lock dependency it carries (name
// prefix, version suffix); names matching no entry are returned as unclaimed.
func archivesByDep(t *testing.T, deps []subchart) (claimed map[string]subchart, unclaimed []string) {
	t.Helper()
	entries, err := os.ReadDir(repoFile(chartArchiveRel))
	if err != nil {
		t.Fatalf("reading %s: %v", chartArchiveRel, err)
	}
	claimed = map[string]subchart{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".tgz") {
			continue
		}
		matched := false
		for _, d := range deps {
			if strings.HasPrefix(name, d.name+"-") && strings.HasSuffix(name, "-"+d.version+".tgz") {
				claimed[name] = d
				matched = true
				break
			}
		}
		if !matched {
			unclaimed = append(unclaimed, name)
		}
	}
	return claimed, unclaimed
}

// TestChartYAMLAgreesWithLock proves Chart.yaml's dependencies agree with the lock.
func TestChartYAMLAgreesWithLock(t *testing.T) {
	locked := lockedVersions(t)

	var chart struct {
		Dependencies []struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		} `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, chartYAMLRel)), &chart); err != nil {
		t.Fatalf("parsing %s: %v", chartYAMLRel, err)
	}

	declared := map[string]string{}
	for _, d := range chart.Dependencies {
		declared[d.Name] = d.Version
		want, ok := locked[d.Name]
		if !ok {
			t.Errorf("%s declares dependency %q, which %s does not resolve", chartYAMLRel, d.Name, chartLockRel)
			continue
		}
		if d.Version != want {
			t.Errorf("%s pins %s %q but %s resolves %q", chartYAMLRel, d.Name, d.Version, chartLockRel, want)
		}
	}
	for name, want := range locked {
		if _, ok := declared[name]; !ok {
			t.Errorf("%s resolves %s %q but %s declares no such dependency", chartLockRel, name, want, chartYAMLRel)
		}
	}
}

// TestVendoredArchivesAgreeWithLock proves the archives under charts/ are exactly the
// ones the lock resolves.
func TestVendoredArchivesAgreeWithLock(t *testing.T) {
	deps := lockedSubcharts(t)
	claimed, unclaimed := archivesByDep(t, deps)

	for _, name := range unclaimed {
		if d, prefixed := depForArchiveName(deps, name); prefixed {
			t.Errorf("%s/%s does not carry the version %s resolves for %s (%q)",
				chartArchiveRel, name, chartLockRel, d.name, d.version)
			continue
		}
		t.Errorf("%s/%s matches no dependency %s resolves", chartArchiveRel, name, chartLockRel)
	}

	for _, d := range deps {
		var found []string
		for name, owner := range claimed {
			if owner == d {
				found = append(found, name)
			}
		}
		switch len(found) {
		case 1:
		case 0:
			t.Errorf("%s resolves %s %q but no archive under %s/ carries that version",
				chartLockRel, d.name, d.version, chartArchiveRel)
		default:
			t.Errorf("%s resolves %s %q but %d archives under %s/ carry it: %v",
				chartLockRel, d.name, d.version, len(found), chartArchiveRel, found)
		}
	}
}

// TestNoticeNamesVendoredArchives proves NOTICE and its generator script name exactly the
// archives under charts/.
func TestNoticeNamesVendoredArchives(t *testing.T) {
	claimed, _ := archivesByDep(t, lockedSubcharts(t))
	for _, rel := range []string{"NOTICE", "hack/generate-notice.sh"} {
		named := map[string]bool{}
		for _, m := range archivePattern.FindAllString(readRepoFile(t, rel), -1) {
			named[path.Base(m)] = true
		}
		for name := range claimed {
			if !named[name] {
				t.Errorf("%s does not name the vendored archive %s/%s", rel, chartArchiveRel, name)
			}
		}
		for name := range named {
			if _, ok := claimed[name]; !ok {
				t.Errorf("%s names %s, which is not an archive under %s/ that %s resolves",
					rel, name, chartArchiveRel, chartLockRel)
			}
		}
	}
}

// TestUpgradeCompatibilityTableAgreesWithSources proves each compatibility table row
// states a version its "Traces to" source carries. Columns are found by header; rows
// tracing to a cluster or a claim are skipped.
func TestUpgradeCompatibilityTableAgreesWithSources(t *testing.T) {
	locked := lockedVersions(t)
	resolved := invert(t, locked)
	wantEnvtest := envtestVersion(t)

	tracedToLock := map[string]bool{}
	sawEnvtestRow := false
	sawTable := false

	lines := strings.Split(readRepoFile(t, upgradeDocRel), "\n")
	versionCol, tracesCol := -1, -1
	for i, line := range lines {
		cells := markdownRowCells(line)
		if cells == nil {
			continue
		}
		if i+1 < len(lines) && isSeparatorRow(lines[i+1]) {
			versionCol, tracesCol = columnIndex(cells, "Version"), columnIndex(cells, "Traces to")
			if versionCol >= 0 && tracesCol >= 0 {
				sawTable = true
			}
			continue
		}
		if versionCol < 0 || tracesCol < 0 || len(cells) <= versionCol || len(cells) <= tracesCol {
			continue
		}
		version, traces := cells[versionCol], cells[tracesCol]
		found := semverPattern.FindString(version)
		switch {
		case strings.Contains(traces, path.Base(chartLockRel)):
			if found == "" {
				t.Errorf("%s:%d traces to %s but states no version: %q", upgradeDocRel, i+1, chartLockRel, version)
				continue
			}
			if _, ok := resolved[found]; !ok {
				t.Errorf("%s:%d states %q, which %s resolves for no dependency (resolved: %v)",
					upgradeDocRel, i+1, found, chartLockRel, locked)
				continue
			}
			tracedToLock[found] = true
		case strings.Contains(traces, "ENVTEST_K8S_VERSION"):
			sawEnvtestRow = true
			if found != wantEnvtest {
				t.Errorf("%s:%d states %q but %s pins ENVTEST_K8S_VERSION %q",
					upgradeDocRel, i+1, found, makefileRel, wantEnvtest)
			}
		default:

			src := tracePathPattern.FindStringSubmatch(traces)
			if src == nil || found == "" {
				continue
			}
			body, err := os.ReadFile(repoFile(src[1]))
			if err != nil {
				t.Errorf("%s:%d traces to %s, which cannot be read: %v", upgradeDocRel, i+1, src[1], err)
				continue
			}
			if !strings.Contains(string(body), found) {
				t.Errorf("%s:%d states %q but %s carries no such version", upgradeDocRel, i+1, found, src[1])
			}
		}
	}

	if !sawTable {
		t.Fatalf("no table in %s has both a Version and a Traces to column", upgradeDocRel)
	}
	for name, version := range locked {
		if !tracedToLock[version] {
			t.Errorf("%s resolves %s %q but no row in %s traces that version to %s",
				chartLockRel, name, version, upgradeDocRel, chartLockRel)
		}
	}
	if !sawEnvtestRow {
		t.Errorf("no row in %s traces a version to ENVTEST_K8S_VERSION in %s", upgradeDocRel, makefileRel)
	}
}

// columnIndex returns the position of a named column in a header row, or -1.
func columnIndex(header []string, name string) int {
	for i, cell := range header {
		if strings.EqualFold(cell, name) {
			return i
		}
	}
	return -1
}

// invert turns a name-to-version map into version-to-name, failing when two dependencies
// share a version (a restatement of one would then satisfy the other).
func invert(t *testing.T, byName map[string]string) map[string]string {
	t.Helper()
	byVersion := make(map[string]string, len(byName))
	for name, version := range byName {
		if other, clash := byVersion[version]; clash {
			t.Fatalf("%s resolves both %s and %s at %q, so a documented version cannot be attributed to either",
				chartLockRel, other, name, version)
		}
		byVersion[version] = name
	}
	return byVersion
}

// markdownRowCells splits a markdown table row into trimmed cells; nil for a separator or
// non-row.
func markdownRowCells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") || isSeparatorRow(line) {
		return nil
	}
	parts := strings.Split(strings.Trim(line, "|"), "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}

// isSeparatorRow reports whether a line is the dashed row under a table header.
func isSeparatorRow(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return false
	}
	return strings.Trim(line, "|-: ") == ""
}

// depForArchiveName returns the dependency whose name, followed by a dash, opens an
// archive file name.
func depForArchiveName(deps []subchart, name string) (subchart, bool) {
	for _, d := range deps {
		if strings.HasPrefix(name, d.name+"-") {
			return d, true
		}
	}
	return subchart{}, false
}
