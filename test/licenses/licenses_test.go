// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package licenses checks the image's /licenses copy and NOTICE against the modules
// the operator binary links.
package licenses

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"testing"
)

// buildEnv matches the Dockerfile's build target: build constraints decide which modules link.
var buildEnv = []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64"}

var licenseNames = []string{"LICENSE", "LICENSE.txt", "LICENSE.md", "license.md", "COPYING", "COPYING.txt", "COPYING.md"}

// legalFile matches the license, NOTICE and PATENTS file names the image must carry.
var legalFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|patents)([.-][a-z0-9]+)?$`)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller for repo-root path")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), buildEnv...)
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
			stderr = ee.Stderr
		}
		t.Fatalf("%s %v: %v\n%s", name, args, err, stderr)
	}
	return string(out)
}

// linkedPackage is a package linked into ./cmd/..., with its module (by replacement path
// if replaced) and the module-cache directories of both.
type linkedPackage struct{ module, moduleDir, dir string }

func linkedPackages(t *testing.T, root string) []linkedPackage {
	t.Helper()
	out := run(t, root, "go", "list", "-deps", "-f",
		`{{with .Module}}{{if not .Main}}{{if .Replace}}{{.Replace.Path}}{{"\t"}}{{.Replace.Dir}}{{else}}{{.Path}}{{"\t"}}{{.Dir}}{{end}}{{"\t"}}{{$.Dir}}{{end}}{{end}}`,
		"./cmd/...")
	var pkgs []linkedPackage
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Split(line, "\t"); len(f) == 3 {
			pkgs = append(pkgs, linkedPackage{f[0], f[1], f[2]})
		}
	}
	if len(pkgs) == 0 {
		t.Fatal("go list -deps ./cmd/... reported no modules")
	}
	return pkgs
}

func hasAny(dir string, names []string) bool {
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return true
		}
	}
	return false
}

// TestImageLicenseCopyCoversLinkedModules proves the copy includes, for every linked
// module, every license, NOTICE and PATENTS file at the module root and in the directory
// of each linked package (such as a forked third_party package), plus the Go standard
// library license.
func TestImageLicenseCopyCoversLinkedModules(t *testing.T) {
	root := repoRoot(t)
	dest := t.TempDir()
	run(t, root, "bash", filepath.Join("hack", "list-third-party-licenses.sh"), "copy", dest)

	checked := map[string]bool{}
	for _, p := range linkedPackages(t, root) {
		got := filepath.Join(dest, filepath.FromSlash(p.module))
		if !hasAny(got, licenseNames) {
			t.Errorf("no license file copied for linked module %s (want one of %v in %s)", p.module, licenseNames, got)
		}
		for _, dir := range []string{p.moduleDir, p.dir} {
			if checked[dir] {
				continue
			}
			checked[dir] = true
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read %s: %v", dir, err)
			}
			for _, e := range entries {
				if e.IsDir() || strings.HasSuffix(e.Name(), ".go") || !legalFile.MatchString(e.Name()) {
					continue
				}
				rel, err := filepath.Rel(p.moduleDir, filepath.Join(dir, e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(got, rel)); err != nil {
					t.Errorf("linked module %s has %s, which was not copied", p.module, rel)
				}
			}
		}
	}

	goroot := strings.TrimSpace(run(t, root, "go", "env", "GOROOT"))
	want, err := os.ReadFile(filepath.Join(goroot, "LICENSE"))
	if err != nil {
		t.Fatalf("read the Go standard library LICENSE: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "std", "LICENSE"))
	if err != nil {
		t.Fatalf("the Go standard library LICENSE was not copied: %v", err)
	}
	if string(got) != string(want) {
		t.Error("the copied Go standard library LICENSE differs from $(go env GOROOT)/LICENSE")
	}
}

// TestNoticeListsLinkedModules proves NOTICE lists every linked module as "path (version)".
func TestNoticeListsLinkedModules(t *testing.T) {
	root := repoRoot(t)
	notice, err := os.ReadFile(filepath.Join(root, "NOTICE"))
	if err != nil {
		t.Fatalf("read NOTICE: %v", err)
	}
	out := run(t, root, "go", "list", "-deps", "-f",
		`{{with .Module}}{{if not .Main}}{{if .Replace}}{{.Replace.Path}} ({{.Replace.Version}}){{else}}{{.Path}} ({{.Version}}){{end}}{{end}}{{end}}`,
		"./cmd/...")
	seen := map[string]bool{}
	for _, entry := range strings.Split(out, "\n") {
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		if !strings.Contains(string(notice), entry+"\n") {
			t.Errorf("NOTICE does not list %s; regenerate it with ./hack/generate-notice.sh > NOTICE", entry)
		}
	}
}

// TestNoticeIsGenerated proves NOTICE is the generator's current output, so the text of
// every non-Apache license file the image ships is in NOTICE.
func TestNoticeIsGenerated(t *testing.T) {
	root := repoRoot(t)
	want := run(t, root, "bash", filepath.Join("hack", "generate-notice.sh"))
	got, err := os.ReadFile(filepath.Join(root, "NOTICE"))
	if err != nil {
		t.Fatalf("read NOTICE: %v", err)
	}
	if string(got) != want {
		t.Error("NOTICE is stale; regenerate it: ./hack/generate-notice.sh > NOTICE && cp NOTICE deploy/cpu-pinning-operator/")
	}
}

// canonicalApacheSHA256 is the SHA-256 of https://www.apache.org/licenses/LICENSE-2.0.txt.
const canonicalApacheSHA256 = "cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"

// TestLicenseIsCanonical proves LICENSE is the Apache License 2.0 text, unmodified.
func TestLicenseIsCanonical(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "LICENSE"))
	if err != nil {
		t.Fatalf("read LICENSE: %v", err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != canonicalApacheSHA256 {
		t.Error("LICENSE differs from the canonical Apache License 2.0 text")
	}
}
