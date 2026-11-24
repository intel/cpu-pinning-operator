// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package docs checks that every relative markdown link resolves to an existing file
// and, for #anchors, to a heading slugged the way GitHub slugs it (punctuation dropped,
// not collapsed, so "read + re-validate" becomes "read--re-validate").
package docs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"testing"
	"unicode"
)

// linkRoots are the repo-relative paths whose links are checked; directories are walked for *.md.
var linkRoots = []string{
	"docs",
	"README.md",
	"CONTRIBUTING.md",
	filepath.Join("deploy", "cpu-pinning-operator", "README.md"),
}

var (
	// [text](target) and ![alt](target); the target stops at whitespace to skip a "title".
	linkRE = regexp.MustCompile(`\[[^\]]*\]\(\s*([^)\s]+)`)
	// An ATX heading; headingText trims any closing #s.
	headingRE = regexp.MustCompile(`(?m)^ {0,3}#{1,6}[ \t]+(.+?)[ \t]*$`)
	// An explicit <a id="x"> or <a name="x">.
	htmlAnchorRE = regexp.MustCompile(`<a\s+(?:id|name)\s*=\s*"([^"]+)"`)
	// An inline link inside a heading contributes its text, not its target.
	inlineLinkRE = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// A fenced code block delimiter.
	fenceRE = regexp.MustCompile("^ {0,3}(```|~~~)")
)

// repoRoot resolves the repository root from this file, independent of CWD.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller for repo-root path")
	}

	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// stripFences blanks fenced code blocks, keeping line numbers, so `#` comments and
// brackets in samples aren't read as headings or links.
func stripFences(content string) string {
	lines := strings.Split(content, "\n")
	inFence := false
	for i, line := range lines {
		if fenceRE.MatchString(line) {
			inFence = !inFence
			lines[i] = ""
			continue
		}
		if inFence {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// slug renders a heading the way GitHub renders it into an anchor.
func slug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// headingText is a heading's rendered text: inline links reduced to text, closing #s removed.
func headingText(raw string) string {
	text := inlineLinkRE.ReplaceAllString(raw, "$1")
	text = strings.TrimRight(text, "#")
	return strings.TrimSpace(text)
}

// anchorsOf returns the anchors a markdown file offers.
func anchorsOf(content string) map[string]bool {
	body := stripFences(content)
	found := map[string]bool{}
	seen := map[string]int{}
	for _, m := range headingRE.FindAllStringSubmatch(body, -1) {
		base := slug(headingText(m[1]))
		if base == "" {
			continue
		}
		anchor := base
		if n := seen[base]; n > 0 {
			anchor = fmt.Sprintf("%s-%d", base, n)
		}
		seen[base]++
		found[anchor] = true
	}
	for _, m := range htmlAnchorRE.FindAllStringSubmatch(body, -1) {
		found[strings.ToLower(m[1])] = true
	}
	return found
}

// markdownFiles expands the configured roots into the list of files to check.
func markdownFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	for _, r := range linkRoots {
		abs := filepath.Join(root, r)
		info, err := os.Stat(abs)
		if err != nil {
			t.Fatalf("link-check root %s is missing: %v", r, err)
		}
		if !info.IsDir() {
			files = append(files, abs)
			continue
		}
		err = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", r, err)
		}
	}
	if len(files) == 0 {
		t.Fatal("no markdown files found to check")
	}
	return files
}

// isExternal reports whether a link target points outside the repository.
func isExternal(target string) bool {
	return strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:")
}

// checkLinks returns one message per unresolved relative link and the count resolved.
func checkLinks(root string, files []string) (problems []string, resolved int) {
	anchorCache := map[string]map[string]bool{}
	anchors := func(path string) map[string]bool {
		if a, ok := anchorCache[path]; ok {
			return a
		}
		var a map[string]bool
		content, err := os.ReadFile(path)
		if err == nil {
			a = anchorsOf(string(content))
		}
		anchorCache[path] = a
		return a
	}

	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: cannot read: %v", mustRel(root, file), err))
			continue
		}
		body := stripFences(string(raw))
		for _, m := range linkRE.FindAllStringSubmatchIndex(body, -1) {
			target := body[m[2]:m[3]]
			if isExternal(target) {
				continue
			}
			where := fmt.Sprintf("%s:%d", mustRel(root, file), strings.Count(body[:m[0]], "\n")+1)

			path, anchor, _ := strings.Cut(target, "#")
			targetFile := file
			if path != "" {
				targetFile = filepath.Join(filepath.Dir(file), path)
				if _, err := os.Stat(targetFile); err != nil {
					problems = append(problems, fmt.Sprintf(
						"%s: link target %q does not exist (no file at %s)",
						where, target, mustRel(root, targetFile)))
					continue
				}
			}
			resolved++
			// Anchors in a non-markdown target are not ours to resolve.
			if anchor == "" || !strings.EqualFold(filepath.Ext(targetFile), ".md") {
				continue
			}
			if !anchors(targetFile)[strings.ToLower(anchor)] {
				problems = append(problems, fmt.Sprintf(
					"%s: link target %q has no such anchor — %s defines no heading slugging to %q",
					where, target, mustRel(root, targetFile), strings.ToLower(anchor)))
			}
		}
	}
	return problems, resolved
}

func TestDocLinksResolve(t *testing.T) {
	root := repoRoot(t)
	problems, resolved := checkLinks(root, markdownFiles(t, root))
	for _, p := range problems {
		t.Error(p)
	}
	t.Logf("resolved %d relative links", resolved)
}

// TestCheckLinksReportsBrokenTargets is the negative control: the checker still flags a
// wrong anchor and a missing file.
func TestCheckLinksReportsBrokenTargets(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		return path
	}
	write("target.md", "## A real heading\n")
	src := write("source.md", strings.Join([]string{
		"[good file and anchor](target.md#a-real-heading)",
		"[good same-file anchor](#only-here)",
		"[real file, wrong anchor](target.md#a-renamed-heading)",
		"[missing file](nowhere.md)",
		"[external](https://example.invalid#whatever)",
		"## Only here",
	}, "\n\n"))

	problems, resolved := checkLinks(dir, []string{src})
	if resolved != 3 {
		t.Errorf("resolved %d targets, want 3", resolved)
	}
	if len(problems) != 2 {
		t.Fatalf("got %d problems, want 2: %v", len(problems), problems)
	}
	for i, want := range []string{
		`source.md:5: link target "target.md#a-renamed-heading" has no such anchor`,
		`source.md:7: link target "nowhere.md" does not exist`,
	} {
		if !strings.HasPrefix(problems[i], want) {
			t.Errorf("problem %d = %q, want prefix %q", i, problems[i], want)
		}
	}
}

func mustRel(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ name, heading, want string }{
		{"colon and plus dropped without collapsing",
			"Admission critical section: lock-free read + re-validate-under-lock",
			"admission-critical-section-lock-free-read--re-validate-under-lock"},
		{"backticks contribute their text only",
			"The `KubeletPodResourcesGet` gate stays off", "the-kubeletpodresourcesget-gate-stays-off"},
		{"period dropped without a gap",
			"Reserved pool vs. reservation store", "reserved-pool-vs-reservation-store"},
		{"parens dropped, underscore kept",
			"Who is in which pool (`cpu_pinning_pool_membership`)", "who-is-in-which-pool-cpu_pinning_pool_membership"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := slug(headingText(tc.heading)); got != tc.want {
				t.Errorf("slug(%q) = %q, want %q", tc.heading, got, tc.want)
			}
		})
	}
}

func TestDuplicateHeadingsGetSuffixedAnchors(t *testing.T) {
	got := anchorsOf("# Modes\n\n## Modes\n\n```\n## Modes\n```\n")
	for _, want := range []string{"modes", "modes-1"} {
		if !got[want] {
			t.Errorf("anchor %q missing from %v", want, got)
		}
	}
	if got["modes-2"] {
		t.Error("a heading inside a code fence must not produce an anchor")
	}
}
